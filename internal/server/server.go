package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

const (
	defaultReadHeaderTimeout      = 5 * time.Second
	defaultRequestBodyReadTimeout = 30 * time.Second
	defaultDownstreamWriteTimeout = 30 * time.Second
	defaultIdleTimeout            = 60 * time.Second
	defaultStreamIdleTimeout      = 60 * time.Second
	defaultMaxBodyBytes           = int64(16 << 20)
	defaultMaxHeaderBytes         = 64 << 10
	defaultMaxInputItems          = bridge.DefaultMaxInputItems
	defaultMaxCollectionItems     = bridge.DefaultMaxCollectionItems
	defaultMaxTools               = bridge.DefaultMaxProviderTools
	defaultMaxSchemaBytes         = int64(bridge.DefaultMaxFunctionSchemaBytes)
	defaultMaxActiveRequests      = bridge.DefaultMaxActiveRequests
	defaultMaxPendingRecords      = 128
	defaultMaxPendingAggregate    = int64(128 << 20)

	routeLive      = "live"
	routeReady     = "ready"
	routeResponses = "responses"
	routeUnknown   = "unknown"
)

// Config is the HTTP server portion of the validated application settings.
type Config struct {
	ListenAddr               string
	AllowNonLoopback         bool
	Upstream                 UpstreamClient
	ReadHeaderTimeout        time.Duration
	IdleTimeout              time.Duration
	RequestBodyReadTimeout   time.Duration
	StreamIdleTimeout        time.Duration
	DownstreamWriteTimeout   time.Duration
	MaxBodyBytes             int64
	MaxHeaderBytes           int
	MaxInputItems            int
	MaxCollectionItems       int
	MaxTools                 int
	MaxSchemaBytes           int64
	MaxSSELineBytes          int
	MaxSSEEventBytes         int
	MaxSSEBufferedBytes      int
	MaxSSEReadBufferBytes    int
	MaxOutputBytes           int
	MaxTextBytes             int
	MaxReasoningBytes        int
	MaxToolCallArgumentBytes int
	MaxPendingTurnBytes      int64
	MaxPendingRecords        int
	MaxPendingAggregateBytes int64
	MaxActiveRequests        int
	// ContinuationStore may be injected by an embedding application or test.
	// When nil, New creates and owns a standard-library in-memory store using
	// ContinuationStoreConfig.
	ContinuationStore       *opencodego.ContinuationStore
	ContinuationStoreConfig opencodego.ContinuationStoreConfig
}

// Server owns the loopback HTTP listener and the Codex-facing route surface.
type Server struct {
	config   Config
	ln       net.Listener
	http     *http.Server
	logger   *slog.Logger
	upstream UpstreamClient
	ready    atomic.Bool

	requestID uint64

	// lifecycleMu serializes shutdown admission with gateway-owned error writes.
	// It is deliberately separate from activeRequestsMu so a bounded network
	// write cannot block request accounting or active-request inspection.
	lifecycleMu        sync.Mutex
	activeRequestsMu   sync.Mutex
	activeRequests     map[uint64]activeRequest
	activeRequestsDone chan struct{}
	shuttingDown       bool

	continuations         *opencodego.ContinuationStore
	ownsContinuations     bool
	continuationCloseOnce sync.Once

	shutdownOnce sync.Once
	shutdownErr  error
}

type activeRequest struct {
	cancel    context.CancelFunc
	closeBody func()
}

func New(config Config, logger *slog.Logger) (*Server, error) {
	config = withDefaults(config)
	address, err := resolveListenAddress(config.ListenAddr, config.AllowNonLoopback)
	if err != nil {
		return nil, err
	}
	if config.ReadHeaderTimeout <= 0 || config.RequestBodyReadTimeout <= 0 || config.DownstreamWriteTimeout <= 0 || config.StreamIdleTimeout <= 0 || config.IdleTimeout <= 0 {
		return nil, fmt.Errorf("HTTP timeouts must be positive")
	}
	if config.MaxBodyBytes <= 0 || config.MaxHeaderBytes <= 0 {
		return nil, fmt.Errorf("HTTP limits must be positive")
	}
	if config.MaxInputItems <= 0 || config.MaxCollectionItems <= 0 || config.MaxTools <= 0 || config.MaxSchemaBytes <= 0 || config.MaxActiveRequests <= 0 || config.MaxPendingRecords <= 0 {
		return nil, fmt.Errorf("request limits must be positive")
	}
	if config.MaxTools > bridge.DefaultMaxProviderTools {
		return nil, fmt.Errorf("MaxTools must not exceed %d", bridge.DefaultMaxProviderTools)
	}
	if config.MaxSchemaBytes > int64(bridge.DefaultMaxFunctionSchemaBytes) {
		return nil, fmt.Errorf("MaxSchemaBytes must not exceed %d", bridge.DefaultMaxFunctionSchemaBytes)
	}
	if config.MaxSSELineBytes <= 0 || config.MaxSSEEventBytes <= 0 || config.MaxSSEBufferedBytes <= 0 || config.MaxSSEReadBufferBytes <= 0 || config.MaxOutputBytes <= 0 || config.MaxTextBytes <= 0 || config.MaxReasoningBytes <= 0 || config.MaxToolCallArgumentBytes <= 0 || config.MaxPendingTurnBytes <= 0 || config.MaxPendingAggregateBytes <= 0 {
		return nil, fmt.Errorf("stream limits must be positive")
	}
	continuations := config.ContinuationStore
	ownsContinuations := false
	if continuations == nil {
		continuations, err = opencodego.NewContinuationStore(config.ContinuationStoreConfig)
		if err != nil {
			return nil, fmt.Errorf("configure continuation store: %w", err)
		}
		ownsContinuations = true
	}

	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		if ownsContinuations {
			_ = continuations.Close()
		}
		return nil, fmt.Errorf("listen on %s: %w", config.ListenAddr, err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	upstream := config.Upstream
	if upstream == nil {
		upstream = UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
			return nil, &UpstreamError{Code: upstreamErrorNotConfigured}
		})
	}
	server := &Server{
		config:             config,
		ln:                 listener,
		logger:             logger,
		upstream:           upstream,
		continuations:      continuations,
		ownsContinuations:  ownsContinuations,
		activeRequests:     make(map[uint64]activeRequest),
		activeRequestsDone: closedSignal(),
	}
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		// ReadTimeout covers the socket-level request phase. handleResponses also
		// applies the same phase-specific deadline through ResponseController so
		// embedded callers and direct handler tests receive the same behavior.
		ReadTimeout: config.RequestBodyReadTimeout,
		// A net/http WriteTimeout would cover the entire streamed response.
		// StreamSession applies a fresh deadline to each write/flush instead.
		WriteTimeout:   0,
		IdleTimeout:    config.IdleTimeout,
		MaxHeaderBytes: config.MaxHeaderBytes,
	}
	server.ready.Store(true)
	return server, nil
}

func withDefaults(config Config) Config {
	if config.ListenAddr == "" {
		config.ListenAddr = "127.0.0.1:0"
	}
	if config.ReadHeaderTimeout == 0 {
		config.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.RequestBodyReadTimeout == 0 {
		config.RequestBodyReadTimeout = defaultRequestBodyReadTimeout
	}
	if config.StreamIdleTimeout == 0 {
		config.StreamIdleTimeout = defaultStreamIdleTimeout
	}
	if config.DownstreamWriteTimeout == 0 {
		config.DownstreamWriteTimeout = defaultDownstreamWriteTimeout
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if config.MaxInputItems == 0 {
		config.MaxInputItems = defaultMaxInputItems
	}
	if config.MaxCollectionItems == 0 {
		config.MaxCollectionItems = defaultMaxCollectionItems
	}
	if config.MaxTools == 0 {
		config.MaxTools = defaultMaxTools
	}
	if config.MaxSchemaBytes == 0 {
		config.MaxSchemaBytes = defaultMaxSchemaBytes
	}
	if config.MaxSSELineBytes == 0 {
		config.MaxSSELineBytes = opencodego.DefaultSSEMaxLineBytes
	}
	if config.MaxSSEEventBytes == 0 {
		config.MaxSSEEventBytes = opencodego.DefaultSSEMaxEventBytes
	}
	if config.MaxSSEBufferedBytes == 0 {
		config.MaxSSEBufferedBytes = opencodego.DefaultSSEMaxBufferedBytes
	}
	if config.MaxSSEReadBufferBytes == 0 {
		config.MaxSSEReadBufferBytes = opencodego.DefaultSSEReadBufferBytes
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = bridge.DefaultMaxOutputBytes
	}
	if config.MaxTextBytes == 0 {
		config.MaxTextBytes = bridge.DefaultMaxTextBytes
	}
	if config.MaxReasoningBytes == 0 {
		config.MaxReasoningBytes = bridge.DefaultMaxReasoningBytes
	}
	if config.MaxToolCallArgumentBytes == 0 {
		config.MaxToolCallArgumentBytes = bridge.DefaultMaxToolCallArgumentBytes
	}
	if config.MaxPendingTurnBytes == 0 {
		config.MaxPendingTurnBytes = int64(opencodego.DefaultContinuationMaxRecordBytes)
	}
	if config.MaxPendingRecords == 0 {
		config.MaxPendingRecords = defaultMaxPendingRecords
	}
	if config.MaxPendingAggregateBytes == 0 {
		config.MaxPendingAggregateBytes = defaultMaxPendingAggregate
	}
	if config.MaxActiveRequests == 0 {
		config.MaxActiveRequests = defaultMaxActiveRequests
	}
	if config.ContinuationStoreConfig.MaxBytesPerRecord == 0 {
		config.ContinuationStoreConfig.MaxBytesPerRecord = config.MaxPendingTurnBytes
	}
	if config.ContinuationStoreConfig.MaxRecords == 0 {
		config.ContinuationStoreConfig.MaxRecords = config.MaxPendingRecords
	}
	if config.ContinuationStoreConfig.MaxAggregateBytes == 0 {
		config.ContinuationStoreConfig.MaxAggregateBytes = config.MaxPendingAggregateBytes
	}
	return config
}

func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Serve() error {
	if s == nil || s.http == nil || s.ln == nil {
		return nil
	}
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting new connections and waits for active handlers until
// ctx expires. The caller supplies the bounded grace period.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.shutdownOnce.Do(func() {
		s.ready.Store(false)
		s.markShuttingDown()
		// Shutdown is cancellation-first: existing handlers stop provider work and
		// release their bodies immediately, while net/http still performs its
		// normal graceful handler cleanup within the caller's grace period.
		s.cancelActiveRequests()
		shutdownWatchDone := make(chan struct{})
		shutdownWatchExited := make(chan struct{})
		go func() {
			defer close(shutdownWatchExited)
			select {
			case <-ctx.Done():
				s.cancelActiveRequests()
			case <-shutdownWatchDone:
			}
		}()
		s.shutdownErr = s.http.Shutdown(ctx)
		if s.shutdownErr == nil {
			s.shutdownErr = s.waitActiveRequests(ctx)
		}
		close(shutdownWatchDone)
		<-shutdownWatchExited
		if s.shutdownErr != nil {
			// Graceful shutdown has reached its deadline. Release every
			// provider body and handler before returning the timeout error.
			s.cancelActiveRequests()
			_ = s.http.Close()
		}
		s.closeContinuations()
	})
	return s.shutdownErr
}

func (s *Server) Close() error {
	if s == nil || s.http == nil {
		return nil
	}
	s.ready.Store(false)
	s.markShuttingDown()
	s.cancelActiveRequests()
	err := s.http.Close()
	s.closeContinuations()
	return err
}

func (s *Server) closeContinuations() {
	if s == nil || !s.ownsContinuations || s.continuations == nil {
		return
	}
	s.continuationCloseOnce.Do(func() { _ = s.continuations.Close() })
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := atomic.AddUint64(&s.requestID, 1)
	requestContext, cancel := context.WithCancel(r.Context())
	r = r.WithContext(requestContext)
	if r.Body != nil {
		r.Body = &requestBodyCloser{ReadCloser: r.Body}
	}

	started := time.Now()
	response := &statusWriter{ResponseWriter: w, context: requestContext}
	requestIDValue := fmt.Sprintf("req-%06d", requestID)
	response.Header().Set("X-Request-ID", requestIDValue)
	route := routeForPath(r.URL.Path)
	if err := validateRequestHostAndOrigin(s.config, r); err != nil {
		closeRequestBody(r)
		writeJSONError(response, http.StatusForbidden, gatewayPermissionCode, "request host or origin is not allowed")
		cancel()
		s.logRequest(requestIDValue, r, route, response, started)
		return
	}
	if route == routeReady && s.isShuttingDown() {
		closeRequestBody(r)
		s.route(response, r)
		s.logRequest(requestIDValue, r, route, response, started)
		cancel()
		return
	}
	unregister, accepted, shuttingDown := s.trackRequest(requestID, cancel, func() {
		if r.Body != nil {
			_ = r.Body.Close()
		}
	})
	if !accepted {
		closeRequestBody(r)
		if shuttingDown {
			writeJSONError(response, http.StatusServiceUnavailable, gatewayShutdownCode, "the gateway is shutting down")
		} else {
			response.Header().Set("Retry-After", "1")
			writeJSONError(response, http.StatusTooManyRequests, gatewayActiveLimitCode, "the gateway is at its active request limit")
		}
		cancel()
		s.logRequest(requestIDValue, r, route, response, started)
		return
	}
	defer func() {
		unregister()
		cancel()
	}()

	s.route(response, r)
	s.writeFallbackIfNeeded(response, r)

	s.logRequest(requestIDValue, r, route, response, started)
}

func (s *Server) writeFallbackIfNeeded(response *statusWriter, request *http.Request) {
	if s == nil || response == nil || request == nil {
		return
	}
	s.activeRequestsMu.Lock()
	defer s.activeRequestsMu.Unlock()
	if response.status != 0 || s.shuttingDown || request.Context().Err() != nil {
		return
	}
	response.WriteHeader(http.StatusInternalServerError)
}

func (s *Server) isShuttingDown() bool {
	s.activeRequestsMu.Lock()
	defer s.activeRequestsMu.Unlock()
	return s.shuttingDown
}

func (s *Server) logRequest(requestIDValue string, r *http.Request, route string, response *statusWriter, started time.Time) {
	s.logger.Info("http request",
		slog.String("component", "server"),
		slog.String("request_id", requestIDValue),
		slog.String("method", r.Method),
		slog.String("route", route),
		slog.Int("status", response.status),
		slog.Int64("latency_ms", time.Since(started).Milliseconds()),
		slog.String("error_code", response.errorCode),
		slog.String("response_id", response.responseID),
		slog.String("response_model", response.responseModel),
		slog.String("response_terminal", response.responseTerminal),
		slog.Int64("response_bytes", response.bytesWritten),
		slog.Bool("canceled", r.Context().Err() != nil),
	)
}

func (s *Server) trackRequest(requestID uint64, cancel context.CancelFunc, closeBody func()) (func(), bool, bool) {
	s.activeRequestsMu.Lock()
	if s.activeRequests == nil {
		s.activeRequests = make(map[uint64]activeRequest)
	}
	if s.activeRequestsDone == nil {
		s.activeRequestsDone = closedSignal()
	}
	if s.shuttingDown {
		s.activeRequestsMu.Unlock()
		return func() {}, false, true
	}
	if len(s.activeRequests) >= s.config.MaxActiveRequests {
		s.activeRequestsMu.Unlock()
		return func() {}, false, false
	}
	if len(s.activeRequests) == 0 {
		s.activeRequestsDone = make(chan struct{})
	}
	requestDone := s.activeRequestsDone
	s.activeRequests[requestID] = activeRequest{cancel: cancel, closeBody: closeBody}
	s.activeRequestsMu.Unlock()
	return func() {
		s.activeRequestsMu.Lock()
		if _, exists := s.activeRequests[requestID]; exists {
			delete(s.activeRequests, requestID)
			if len(s.activeRequests) == 0 {
				close(requestDone)
			}
		}
		s.activeRequestsMu.Unlock()
	}, true, false
}

func (s *Server) waitActiveRequests(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.activeRequestsMu.Lock()
	if len(s.activeRequests) == 0 {
		s.activeRequestsMu.Unlock()
		return nil
	}
	done := s.activeRequestsDone
	s.activeRequestsMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closedSignal() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func validateRequestHostAndOrigin(config Config, request *http.Request) error {
	if request == nil {
		return errors.New("request is required")
	}
	host := request.Host
	if host == "" && request.URL != nil {
		host = request.URL.Host
	}
	if strings.ContainsAny(host, "\r\n") {
		return errors.New("invalid host")
	}
	hostname := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostname = parsedHost
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		hostname = strings.Trim(host, "[]")
	}
	if hostname == "" {
		return errors.New("host is required")
	}
	if !config.AllowNonLoopback && !isLoopbackRequestHost(hostname) {
		return errors.New("non-loopback host")
	}
	if origin := strings.TrimSpace(request.Header.Get("Origin")); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("invalid origin")
		}
		if !config.AllowNonLoopback && !isLoopbackRequestHost(parsed.Hostname()) {
			return errors.New("non-loopback origin")
		}
	}
	return nil
}

func isLoopbackRequestHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (s *Server) markShuttingDown() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.activeRequestsMu.Lock()
	s.shuttingDown = true
	s.activeRequestsMu.Unlock()
}

func (s *Server) cancelActiveRequests() {
	s.activeRequestsMu.Lock()
	requests := make([]activeRequest, 0, len(s.activeRequests))
	for _, request := range s.activeRequests {
		requests = append(requests, request)
	}
	s.activeRequestsMu.Unlock()

	for _, request := range requests {
		if request.cancel != nil {
			request.cancel()
		}
		if request.closeBody != nil {
			// net/http may hold its request-body mutex while blocked in a socket
			// read. Closing it synchronously from shutdown can therefore wait on
			// the very handler shutdown is trying to release. Cancellation is
			// immediate; body cleanup proceeds independently and is released by
			// the connection close if the reader is not context-aware.
			go request.closeBody()
		}
	}
}

func (s *Server) route(w *statusWriter, r *http.Request) {
	switch routeForPath(r.URL.Path) {
	case routeLive:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET /health/live is supported")
			return
		}
		s.handleLive(w)
	case routeReady:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET /health/ready is supported")
			return
		}
		s.handleReady(w)
	case routeResponses:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST /v1/responses is supported")
			return
		}
		s.handleResponses(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func routeForPath(path string) string {
	switch path {
	case "/health/live":
		return routeLive
	case "/health/ready":
		return routeReady
	case "/v1/responses":
		return routeResponses
	default:
		return routeUnknown
	}
}

type statusWriter struct {
	http.ResponseWriter
	context            context.Context
	allowCanceledWrite bool
	status             int
	errorCode          string
	responseID         string
	responseModel      string
	responseTerminal   string
	bytesWritten       int64
}

type requestBodyCloser struct {
	io.ReadCloser
	once sync.Once
}

func closeRequestBody(request *http.Request) {
	if request != nil && request.Body != nil {
		_ = request.Body.Close()
	}
}

func (body *requestBodyCloser) Close() error {
	if body == nil || body.ReadCloser == nil {
		return nil
	}
	var err error
	body.once.Do(func() { err = body.ReadCloser.Close() })
	return err
}

func (w *statusWriter) WriteHeader(status int) {
	if w == nil || w.status != 0 || !w.writeAllowed() {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(payload []byte) (int, error) {
	if w == nil {
		return 0, errors.New("nil status writer")
	}
	if !w.writeAllowed() {
		if w.context != nil {
			return 0, w.context.Err()
		}
		return 0, context.Canceled
	}
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(payload)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if w == nil || !w.writeAllowed() {
		return
	}
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) writeAllowed() bool {
	return w != nil && (w.allowCanceledWrite || w.context == nil || w.context.Err() == nil)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func setReadDeadline(w *statusWriter, ctx context.Context, timeout time.Duration) {
	if w == nil || timeout <= 0 || (ctx != nil && ctx.Err() != nil) {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(timeout))
}

func clearReadDeadline(w *statusWriter) {
	if w == nil {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

func setWriteDeadline(w *statusWriter, timeout time.Duration) {
	if w == nil || timeout <= 0 {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
}

func clearWriteDeadline(w *statusWriter) {
	if w == nil {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func writeJSON(w *statusWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w *statusWriter, status int, code, message string) {
	writeJSONErrorWithParam(w, status, code, "", message)
}

func writeJSONErrorWithParam(w *statusWriter, status int, code, param, message string) {
	w.errorCode = code
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    gatewayErrorType(code),
			"code":    code,
			"param":   param,
			"message": message,
		},
	})
}

func resolveListenAddress(address string, allowNonLoopback bool) (*net.TCPAddr, error) {
	resolved, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address: %w", err)
	}
	if !allowNonLoopback && (resolved.IP == nil || !resolved.IP.IsLoopback()) {
		return nil, fmt.Errorf("listen address must resolve to a loopback host")
	}
	return resolved, nil
}
