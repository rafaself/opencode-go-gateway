package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestRoutesReturnStableJSONContracts(t *testing.T) {
	server := newTestServer(t, nil)

	tests := []struct {
		name            string
		method          string
		path            string
		body            string
		wantStatus      int
		wantAllow       string
		wantType        string
		wantCode        string
		wantStatusField string
	}{
		{name: "live", method: http.MethodGet, path: "/health/live", wantStatus: http.StatusOK, wantStatusField: "ok"},
		{name: "ready", method: http.MethodGet, path: "/health/ready", wantStatus: http.StatusOK, wantStatusField: "ready"},
		{name: "responses without provider", method: http.MethodPost, path: "/v1/responses", body: textRequestBodyForServerTest(), wantStatus: http.StatusInternalServerError, wantType: "provider_unavailable", wantCode: "upstream_not_configured"},
		{name: "unknown path", method: http.MethodGet, path: "/unknown", wantStatus: http.StatusNotFound, wantType: "invalid_request", wantCode: "not_found"},
		{name: "live method", method: http.MethodPost, path: "/health/live", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodGet, wantType: "invalid_request", wantCode: "method_not_allowed"},
		{name: "responses method", method: http.MethodGet, path: "/v1/responses", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodPost, wantType: "invalid_request", wantCode: "method_not_allowed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, "http://127.0.0.1"+test.path, strings.NewReader(test.body))
			if test.path == "/v1/responses" {
				request.Header.Set("Content-Type", "application/json")
			}
			server.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q", got)
			}
			if got := recorder.Header().Get("X-Request-ID"); !strings.HasPrefix(got, "req-") {
				t.Fatalf("request ID = %q", got)
			}
			if test.wantAllow != "" && recorder.Header().Get("Allow") != test.wantAllow {
				t.Fatalf("allow = %q, want %q", recorder.Header().Get("Allow"), test.wantAllow)
			}

			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if test.wantStatusField != "" && payload["status"] != test.wantStatusField {
				t.Fatalf("payload = %v", payload)
			}
			if test.wantType != "" {
				errorPayload, ok := payload["error"].(map[string]any)
				wantCode := test.wantCode
				if wantCode == "" {
					wantCode = test.wantType
				}
				if !ok || errorPayload["type"] != test.wantType || errorPayload["code"] != wantCode || errorPayload["param"] != "" {
					t.Fatalf("error payload = %v", payload["error"])
				}
			}
			if strings.Contains(recorder.Body.String(), "private prompt") {
				t.Fatal("response body echoed request content")
			}
		})
	}
}

func TestResponsesEnforceBodyLimit(t *testing.T) {
	server := newTestServerWithConfig(t, Config{ListenAddr: "127.0.0.1:0", MaxBodyBytes: 16}, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(`{"prompt":"body is too large"}`))
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	errorPayload := payload["error"].(map[string]any)
	if errorPayload["type"] != "request_too_large" || errorPayload["code"] != "request_too_large" || errorPayload["param"] != "body" || errorPayload["message"] == "" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestRequestBodyReadTimeoutReturnsSafeJSON(t *testing.T) {
	upstreamCalled := make(chan struct{}, 1)
	server := newTestServerWithConfigAndUpstream(t, Config{
		ListenAddr:             "127.0.0.1:0",
		RequestBodyReadTimeout: 100 * time.Millisecond,
	}, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		upstreamCalled <- struct{}{}
		return nil, nil
	}), nil)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	connection, err := net.Dial("tcp", server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintf(connection, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\nConnection: close\r\n\r\n{\"model\":\"", server.Addr()); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusRequestTimeout || !bytes.Contains(body, []byte(`"type":"timeout"`)) || !bytes.Contains(body, []byte(`"code":"timeout"`)) || !bytes.Contains(body, []byte(`"param":"body"`)) {
		t.Fatalf("body timeout response = %d %s", response.StatusCode, body)
	}
	select {
	case <-upstreamCalled:
		t.Fatal("upstream was called before the request body completed")
	default:
	}
	if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve error after body timeout test: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not exit after body timeout test")
	}
}

func TestLogsContainRequestMetadataButNoSensitiveValues(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := newTestServer(t, logger)
	secretBody := `{"model":"deepseek-v4-flash (go)","instructions":"private prompt","input":[{"type":"message","role":"user","content":"private prompt"}],"stream":true}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses?secret=query", strings.NewReader(secretBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer header-secret")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
	logOutput := logs.String()
	for _, expected := range []string{"component=server", "request_id=", "method=POST", "route=responses", "status=500", "error_code=upstream_not_configured"} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("logs do not contain %q: %s", expected, logOutput)
		}
	}
	for _, forbidden := range []string{"private prompt", "sk-request-secret", "Bearer header-secret", "secret=query", "authorization"} {
		if strings.Contains(logOutput, forbidden) {
			t.Fatalf("logs contain forbidden value %q: %s", forbidden, logOutput)
		}
	}
}

func textRequestBodyForServerTest() string {
	return `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"hello"}],"stream":true}`
}

func TestLogsClassifyUnknownRoutesWithoutLoggingTheRawPath(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := newTestServer(t, logger)
	secretPath := "/workspace/private-prompt.txt?api_key=path-secret"
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+secretPath, nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, "route=unknown") {
		t.Fatalf("logs do not contain the safe unknown route classification: %s", logOutput)
	}
	for _, forbidden := range []string{"private-prompt.txt", "path-secret", "workspace", "path="} {
		if strings.Contains(logOutput, forbidden) {
			t.Fatalf("logs contain untrusted path content %q: %s", forbidden, logOutput)
		}
	}
}

func TestReadyBecomesUnavailableDuringShutdown(t *testing.T) {
	server := newTestServer(t, nil)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/health/ready", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestServerWiresPendingLimitsIntoItsOwnedContinuationStore(t *testing.T) {
	server, err := New(Config{
		ListenAddr:               "127.0.0.1:0",
		MaxPendingTurnBytes:      1024,
		MaxPendingRecords:        2,
		MaxPendingAggregateBytes: 2048,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.continuations == nil {
		t.Fatal("server did not create a continuation store")
	}
	maxRecords, maxRecordBytes, maxAggregateBytes := server.continuations.CapacityLimits()
	if maxRecordBytes != 1024 || maxRecords != 2 || maxAggregateBytes != 2048 {
		t.Fatalf("continuation limits = records:%d record_bytes:%d aggregate_bytes:%d", maxRecords, maxRecordBytes, maxAggregateBytes)
	}
}

func TestServerRejectsProviderBudgetOverflow(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
	}{
		{
			name:   "tool slots",
			config: Config{ListenAddr: "127.0.0.1:0", MaxTools: bridge.DefaultMaxProviderTools + 1},
		},
		{
			name:   "schema bytes",
			config: Config{ListenAddr: "127.0.0.1:0", MaxSchemaBytes: int64(bridge.DefaultMaxFunctionSchemaBytes) + 1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.config, nil); err == nil {
				t.Fatal("provider budget overflow was accepted")
			}
		})
	}
}

func TestServerShutdownAdmissionCheckIsAtomicWithActiveRequestAccounting(t *testing.T) {
	server := &Server{
		config:         Config{MaxActiveRequests: 1},
		activeRequests: make(map[uint64]activeRequest),
	}
	firstCancel := func() {}
	unregister, accepted, shuttingDown := server.trackRequest(1, firstCancel, nil)
	if !accepted || shuttingDown {
		t.Fatalf("first request admission = accepted %v shutting_down %v", accepted, shuttingDown)
	}
	defer unregister()
	server.markShuttingDown()

	secondUnregister, accepted, shuttingDown := server.trackRequest(2, func() {}, nil)
	if accepted || !shuttingDown {
		t.Fatalf("racing request admission = accepted %v shutting_down %v", accepted, shuttingDown)
	}
	secondUnregister()
}

func TestBodyTimeoutWriteGatePreservesRequestContext(t *testing.T) {
	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	writer := &statusWriter{ResponseWriter: recorder, context: requestContext}
	server := &Server{}

	if !server.writeJSONErrorAfterBodyTimeout(writer, http.StatusRequestTimeout, "timeout", "body", "timed out") {
		t.Fatal("body-timeout write was unexpectedly rejected")
	}
	if writer.context != requestContext {
		t.Fatal("body-timeout path replaced the effective request context")
	}
	if writer.status != http.StatusRequestTimeout || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":"timeout"`)) {
		t.Fatalf("body-timeout response = status %d body %s", writer.status, recorder.Body.String())
	}
}

func TestBodyTimeoutWriteGateRejectsShutdownAtTheWriteBoundary(t *testing.T) {
	server := &Server{}
	recorder := httptest.NewRecorder()
	writer := &statusWriter{ResponseWriter: recorder, context: context.Background()}

	server.markShuttingDown()
	if wrote := server.writeJSONErrorAfterBodyTimeout(writer, http.StatusRequestTimeout, "timeout", "body", "timed out"); wrote {
		t.Fatal("body-timeout writer emitted a response after shutdown began")
	}
	if writer.status != 0 || recorder.Body.Len() != 0 {
		t.Fatalf("post-shutdown body-timeout response = status %d body %s", writer.status, recorder.Body.String())
	}
}

func TestBodyTimeoutWriteDoesNotHoldActiveRequestAccountingDuringIO(t *testing.T) {
	underlying := &blockingResponseWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	server := &Server{
		config:         Config{MaxActiveRequests: 1, DownstreamWriteTimeout: time.Second},
		activeRequests: make(map[uint64]activeRequest),
	}
	writer := &statusWriter{ResponseWriter: underlying, context: context.Background()}
	writeDone := make(chan bool, 1)
	go func() {
		writeDone <- server.writeJSONErrorAfterBodyTimeout(writer, http.StatusRequestTimeout, "timeout", "body", "timed out")
	}()

	select {
	case <-underlying.started:
	case <-time.After(time.Second):
		t.Fatal("body-timeout response did not reach the blocking write")
	}
	if underlying.deadline.IsZero() {
		t.Fatal("body-timeout response did not apply the configured write deadline")
	}

	admission := make(chan struct {
		unregister   func()
		accepted     bool
		shuttingDown bool
	}, 1)
	go func() {
		unregister, accepted, shuttingDown := server.trackRequest(1, func() {}, nil)
		admission <- struct {
			unregister   func()
			accepted     bool
			shuttingDown bool
		}{unregister: unregister, accepted: accepted, shuttingDown: shuttingDown}
	}()
	select {
	case result := <-admission:
		if !result.accepted || result.shuttingDown {
			t.Fatalf("request admission while error response was blocked = accepted %v shutting_down %v", result.accepted, result.shuttingDown)
		}
		result.unregister()
	case <-time.After(250 * time.Millisecond):
		close(underlying.release)
		t.Fatal("active request accounting was blocked by body-timeout network I/O")
	}
	shutdownDone := make(chan struct{})
	go func() {
		server.markShuttingDown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(250 * time.Millisecond):
		close(underlying.release)
		t.Fatal("shutdown state tracking was blocked by body-timeout network I/O")
	}

	close(underlying.release)
	select {
	case wrote := <-writeDone:
		if !wrote {
			t.Fatal("body-timeout response was not admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("body-timeout response did not finish after the writer was released")
	}
}

func TestShutdownStopsNetworkServer(t *testing.T) {
	server := newTestServer(t, nil)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + server.Addr() + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}

	_, err = client.Get("http://" + server.Addr() + "/health/live")
	if err == nil {
		t.Fatal("request succeeded after shutdown")
	}
}

func newTestServer(t *testing.T, logger *slog.Logger) *Server {
	t.Helper()
	return newTestServerWithConfig(t, Config{ListenAddr: "127.0.0.1:0"}, logger)
}

func newTestServerWithConfig(t *testing.T, config Config, logger *slog.Logger) *Server {
	t.Helper()
	server, err := New(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	})
	return server
}

type blockingResponseWriter struct {
	header    http.Header
	status    int
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	deadline  time.Time
}

func (writer *blockingResponseWriter) Header() http.Header { return writer.header }

func (writer *blockingResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *blockingResponseWriter) Write(payload []byte) (int, error) {
	writer.startOnce.Do(func() { close(writer.started) })
	<-writer.release
	return len(payload), nil
}

func (writer *blockingResponseWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadline = deadline
	return nil
}
