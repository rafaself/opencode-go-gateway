package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		wantStatusField string
	}{
		{name: "live", method: http.MethodGet, path: "/health/live", wantStatus: http.StatusOK, wantStatusField: "ok"},
		{name: "ready", method: http.MethodGet, path: "/health/ready", wantStatus: http.StatusOK, wantStatusField: "ready"},
		{name: "responses placeholder", method: http.MethodPost, path: "/v1/responses", body: `{"model":"deepseek-v4-flash","prompt":"private prompt"}`, wantStatus: http.StatusNotImplemented, wantType: "not_implemented"},
		{name: "unknown path", method: http.MethodGet, path: "/unknown", wantStatus: http.StatusNotFound, wantType: "not_found"},
		{name: "live method", method: http.MethodPost, path: "/health/live", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodGet, wantType: "method_not_allowed"},
		{name: "responses method", method: http.MethodGet, path: "/v1/responses", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodPost, wantType: "method_not_allowed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, "http://127.0.0.1"+test.path, strings.NewReader(test.body))
			server.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q", got)
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
				if !ok || errorPayload["type"] != test.wantType || errorPayload["code"] != test.wantType {
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
	if !strings.Contains(recorder.Body.String(), `"code":"request_entity_too_large"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestLogsContainRequestMetadataButNoSensitiveValues(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := newTestServer(t, logger)
	secretBody := `{"prompt":"private prompt","api_key":"sk-request-secret"}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses?secret=query", strings.NewReader(secretBody))
	request.Header.Set("Authorization", "Bearer header-secret")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", recorder.Code)
	}
	logOutput := logs.String()
	for _, expected := range []string{"component=server", "request_id=", "method=POST", "route=responses", "status=501", "error_code=not_implemented"} {
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
