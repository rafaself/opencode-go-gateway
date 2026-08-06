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
	"sync"
	"sync/atomic"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultMaxBodyBytes      = int64(16 << 20)
	defaultMaxHeaderBytes    = 64 << 10

	routeLive      = "live"
	routeReady     = "ready"
	routeResponses = "responses"
	routeUnknown   = "unknown"
)

// Config is the HTTP server portion of the validated application settings.
type Config struct {
	ListenAddr        string
	AllowNonLoopback  bool
	Upstream          UpstreamClient
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxBodyBytes      int64
	MaxHeaderBytes    int
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

	activeRequestsMu sync.Mutex
	activeRequests   map[uint64]context.CancelFunc
	shuttingDown     bool

	shutdownOnce sync.Once
	shutdownErr  error
}

func New(config Config, logger *slog.Logger) (*Server, error) {
	config = withDefaults(config)
	address, err := resolveListenAddress(config.ListenAddr, config.AllowNonLoopback)
	if err != nil {
		return nil, err
	}
	if config.ReadHeaderTimeout <= 0 || config.ReadTimeout <= 0 || config.WriteTimeout <= 0 || config.IdleTimeout <= 0 {
		return nil, fmt.Errorf("HTTP timeouts must be positive")
	}
	if config.MaxBodyBytes <= 0 || config.MaxHeaderBytes <= 0 {
		return nil, fmt.Errorf("HTTP limits must be positive")
	}

	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
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
		config:         config,
		ln:             listener,
		logger:         logger,
		upstream:       upstream,
		activeRequests: make(map[uint64]context.CancelFunc),
	}
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		ReadTimeout:       config.ReadTimeout,
		WriteTimeout:      config.WriteTimeout,
		IdleTimeout:       config.IdleTimeout,
		MaxHeaderBytes:    config.MaxHeaderBytes,
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
	if config.ReadTimeout == 0 {
		config.ReadTimeout = defaultReadTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = defaultMaxHeaderBytes
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
		s.cancelActiveRequests()
		s.shutdownErr = s.http.Shutdown(ctx)
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
	return s.http.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := atomic.AddUint64(&s.requestID, 1)
	requestContext, cancel := context.WithCancel(r.Context())
	unregister := s.trackRequest(requestID, cancel)
	defer func() {
		unregister()
		cancel()
	}()
	r = r.WithContext(requestContext)

	started := time.Now()
	response := &statusWriter{ResponseWriter: w}
	requestIDValue := fmt.Sprintf("req-%06d", requestID)
	response.Header().Set("X-Request-ID", requestIDValue)
	route := routeForPath(r.URL.Path)

	s.route(response, r)
	if response.status == 0 {
		response.WriteHeader(http.StatusInternalServerError)
	}

	s.logger.Info("http request",
		slog.String("component", "server"),
		slog.String("request_id", requestIDValue),
		slog.String("method", r.Method),
		slog.String("route", route),
		slog.Int("status", response.status),
		slog.Int64("latency_ms", time.Since(started).Milliseconds()),
		slog.String("error_code", response.errorCode),
		slog.String("response_model", response.responseModel),
		slog.String("response_terminal", response.responseTerminal),
	)
}

func (s *Server) trackRequest(requestID uint64, cancel context.CancelFunc) func() {
	s.activeRequestsMu.Lock()
	if s.shuttingDown {
		s.activeRequestsMu.Unlock()
		cancel()
		return func() {}
	}
	s.activeRequests[requestID] = cancel
	s.activeRequestsMu.Unlock()
	return func() {
		s.activeRequestsMu.Lock()
		delete(s.activeRequests, requestID)
		s.activeRequestsMu.Unlock()
	}
}

func (s *Server) markShuttingDown() {
	s.activeRequestsMu.Lock()
	s.shuttingDown = true
	s.activeRequestsMu.Unlock()
}

func (s *Server) cancelActiveRequests() {
	s.activeRequestsMu.Lock()
	if !s.shuttingDown {
		s.shuttingDown = true
	}
	cancels := make([]context.CancelFunc, 0, len(s.activeRequests))
	for _, cancel := range s.activeRequests {
		cancels = append(cancels, cancel)
	}
	s.activeRequestsMu.Unlock()

	for _, cancel := range cancels {
		cancel()
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
	status           int
	errorCode        string
	responseModel    string
	responseTerminal string
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(payload)
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func writeJSON(w *statusWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w *statusWriter, status int, code, message string) {
	w.errorCode = code
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    code,
			"code":    code,
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
