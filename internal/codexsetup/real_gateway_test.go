package codexsetup

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
	"github.com/rafaself/opencode-go-gateway/internal/server"
)

// TestRealGatewaySetupDoctorAndResponseFlow runs the real gateway HTTP server
// on an ephemeral loopback listener against a real provider endpoint, then
// drives the real setup, the real doctor probes, and one full Responses
// request through the live gateway. Every server started by the test is
// closed at the end.
func TestRealGatewaySetupDoctorAndResponseFlow(t *testing.T) {
	const apiKey = "real-gateway-test-key"

	// Real provider endpoint over loopback TCP with TLS. It enforces the
	// forwarded credential, serves the model list, and streams one completion.
	// The gateway runtime contract requires an HTTPS upstream, so the provider
	// must use a real TLS listener rather than plain HTTP.
	provider := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+apiKey {
			http.Error(writer, "missing bearer credential", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "/models"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"data":[{"id":"deepseek-v4-flash"},{"id":"deepseek-v4-flash-free"}]}`)
		case strings.HasSuffix(request.URL.Path, "/chat/completions"):
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, strings.Join([]string{
				`data: {"id":"real-provider","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"hello from provider"},"finish_reason":null}]}`,
				`data: {"id":"real-provider","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}, "\n\n")+"\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(provider.Close)

	// The real gateway server bound to an ephemeral loopback listener.
	client, err := opencodego.NewClient(opencodego.ClientConfig{
		APIKey:     apiKey,
		BaseURL:    provider.URL,
		HTTPClient: provider.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Upstream:   server.NewOpenCodeUpstreamClient(client),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- gateway.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := gateway.Shutdown(ctx); err != nil {
			t.Errorf("gateway shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("gateway serve returned: %v", err)
		}
	})
	gatewayURL := "http://" + gateway.Addr() + "/v1"

	// Real setup against the running gateway: both the Go and Zen backends
	// point at the same live listener.
	home := t.TempDir()
	setup, err := SetupCodex(SetupOptions{CodexHome: home, GatewayURL: gatewayURL, ZenGatewayURL: gatewayURL})
	if err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(setup.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, providerID := range []string{GoProviderID, ZenProviderID} {
		values, err := InspectProviderFor(configData, providerID)
		if err != nil {
			t.Fatalf("%s provider is not declared: %v", providerID, err)
		}
		if values.BaseURL != gatewayURL {
			t.Fatalf("%s base URL = %q, want %q", providerID, values.BaseURL, gatewayURL)
		}
	}
	goProfile, err := InspectProfile(readTestFile(t, setup.ProfilePath))
	if err != nil {
		t.Fatal(err)
	}
	if goProfile.ModelProvider != GoProviderID || goProfile.Model != GoModelID {
		t.Fatalf("Go profile values = %+v", goProfile)
	}
	zenProfile, err := InspectProfile(readTestFile(t, setup.ZenFreeProfilePath))
	if err != nil {
		t.Fatal(err)
	}
	if zenProfile.ModelProvider != ZenProviderID || zenProfile.Model != ZenFreeModelID {
		t.Fatalf("Zen Free profile values = %+v", zenProfile)
	}

	// Real doctor: default real dial and executable lookup, a real HTTP
	// client trusted for the test TLS provider, and real probes against the
	// live gateway and provider.
	report := Diagnose(context.Background(), DoctorOptions{
		Environment: Environment{LookupEnv: func(name string) (string, bool) {
			switch name {
			case "OPENCODE_GO_API_KEY":
				return apiKey, true
			case "OPENCODE_GO_BASE_URL":
				return provider.URL, true
			default:
				return "", false
			}
		}},
		CodexHome:  home,
		GatewayURL: gatewayURL,
		HTTPClient: provider.Client(),
	})
	if report.Failures() != 0 {
		t.Fatalf("doctor found failures against the real gateway: %+v", report.Checks)
	}
	for _, name := range []string{
		"Gateway configuration", "OpenCode Go API key", "Codex config",
		"Gateway Go provider name", "Gateway Go provider wire API", "Gateway Go provider transport policy",
		"Gateway Zen provider name", "Gateway Zen provider wire API", "Gateway Zen provider transport policy", "Gateway Zen provider URL",
		"Gateway profile", "Codex model selection", "Model catalog",
		"Gateway port", "Gateway live", "Gateway ready",
		"OpenCode Go authentication", "OpenCode Go model",
	} {
		if !hasCheck(report, name, SeverityPass) {
			t.Fatalf("doctor missing PASS for %q: %+v", name, report.Checks)
		}
	}

	// One real Responses request through the live gateway listener. The
	// tagged model routes through the Go backend.
	request, err := http.NewRequest(http.MethodPost, gatewayURL+"/responses", strings.NewReader(
		`{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d, body = %s", response.StatusCode, readTestBody(t, response.Body))
	}
	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("responses content type = %q", response.Header.Get("Content-Type"))
	}
	events := readTestSSEEvents(t, response.Body)
	if len(events) == 0 || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("responses events = %#v", events)
	}
	var text strings.Builder
	for _, event := range events {
		if event["type"] == "response.output_text.delta" {
			if delta, ok := event["delta"].(string); ok {
				text.WriteString(delta)
			}
		}
	}
	if !strings.Contains(text.String(), "hello from provider") {
		t.Fatalf("responses output text = %q, events = %#v", text.String(), events)
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readTestBody(t *testing.T, body io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readTestSSEEvents(t *testing.T, body io.Reader) []map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024), 4<<20)
	events := make([]map[string]any, 0)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("invalid response SSE event %q: %v", line, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
