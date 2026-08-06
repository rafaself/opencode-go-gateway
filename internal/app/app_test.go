package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/config"
)

func TestRunStopsTheHTTPServerOnContextCancellation(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	settings := config.Defaults()
	settings = settings.WithAPIKey("test-api-key")
	settings.Port = 0
	settings.ShutdownTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, settings, logger, func(address string) { ready <- address })
	}()

	var address string
	select {
	case address = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not become ready")
	}

	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}

	if got := logs.String(); !bytes.Contains([]byte(got), []byte("server listening")) || !bytes.Contains([]byte(got), []byte("server shutdown")) {
		t.Fatalf("lifecycle logs = %s", got)
	}
}

func TestRunWaitsForActiveRequestDuringGracefulShutdown(t *testing.T) {
	settings := config.Defaults().WithAPIKey("test-api-key")
	settings.Port = 0
	settings.ShutdownTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, settings, nil, func(address string) { ready <- address })
	}()

	var address string
	select {
	case address = <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not become ready")
	}

	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintf(connection, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{", address); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-runErr:
		t.Fatalf("server stopped before the active request completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := connection.Write([]byte("}")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotImplemented)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("server shutdown failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down after the active request completed")
	}
}

func TestNewLoggerRedactsSensitiveAttributes(t *testing.T) {
	var logs bytes.Buffer
	logger := NewLogger(&logs, slog.LevelInfo)
	logger.Info("request observed",
		slog.String("api_key", "sk-log-secret"),
		slog.String("request_body", "private prompt"),
		slog.String("instructions", "private instructions"),
		slog.String("route", "responses"),
	)

	output := logs.String()
	for _, forbidden := range []string{"sk-log-secret", "private prompt", "private instructions"} {
		if bytes.Contains([]byte(output), []byte(forbidden)) {
			t.Fatalf("logger exposed %q: %s", forbidden, output)
		}
	}
	if !bytes.Contains([]byte(output), []byte("route=responses")) || !bytes.Contains([]byte(output), []byte("<redacted>")) {
		t.Fatalf("logger did not preserve safe metadata and redaction: %s", output)
	}
}

func TestNewLoggerRedactsNestedStructuredValues(t *testing.T) {
	var logs bytes.Buffer
	logger := NewLogger(&logs, slog.LevelInfo)
	logger.Info("nested request",
		slog.Any("details", map[string]any{
			"safe": "visible",
			"nested": map[string]any{
				"api_key":        "nested-api-key-secret",
				"accessToken":    "camel-access-token-secret",
				"clientMetadata": "camel-client-metadata-secret",
				"environment": map[string]any{
					"MODEL":   "nested-model-secret",
					"attempt": 7,
					"enabled": true,
				},
			},
			"items": []any{
				map[string]any{"password": "nested-password-secret", "safe_item": "visible-item"},
			},
		}),
		slog.Group("environment", slog.Int("port", 8787), slog.Bool("debug", true)),
		slog.Group("request", slog.Any("headers", map[string]string{
			"Authorization": "Bearer nested-header-secret",
			"safe-header":   "visible-header",
		})),
	)

	output := logs.String()
	for _, forbidden := range []string{
		"nested-api-key-secret",
		"camel-access-token-secret",
		"camel-client-metadata-secret",
		"nested-model-secret",
		"nested-password-secret",
		"nested-header-secret",
		"attempt=7",
		"enabled=true",
		"port=8787",
		"debug=true",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("logger exposed nested sensitive value %q: %s", forbidden, output)
		}
	}
	for _, expected := range []string{"safe:visible", "safe_item:visible-item", "safe-header:visible-header", "<redacted>"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("logger omitted expected safe metadata or redaction marker %q: %s", expected, output)
		}
	}
}
