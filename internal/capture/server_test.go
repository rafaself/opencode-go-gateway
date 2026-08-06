package capture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRedactRemovesSensitiveValuesAndNormalizesIdentity(t *testing.T) {
	input := map[string]any{
		"model":         "gpt-5.3-codex",
		"instructions":  "private developer instructions",
		"input":         []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "input_text", "text": "private prompt"}}}},
		"path":          "/home/rafa/private/source.go",
		"environment":   map[string]any{"HOME": "/home/rafa", "OPENAI_API_KEY": "sk-secret"},
		"authorization": "Bearer secret",
		"id":            "real-id",
		"created_at":    "2026-08-05T00:00:00Z",
		"tools":         []any{map[string]any{"type": "function", "name": "exec_command"}},
	}

	redacted, err := json.Marshal(Redact(input))
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(redacted)
	for _, forbidden := range []string{"private developer instructions", "private prompt", "/home/rafa/private/source.go", "sk-secret", "Bearer secret", "real-id"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("redacted fixture contains %q: %s", forbidden, serialized)
		}
	}
	if !strings.Contains(serialized, `"model":"gpt-5.3-codex"`) {
		t.Fatalf("semantic model field was not preserved: %s", serialized)
	}
	if !strings.Contains(serialized, `"type":"function"`) || !strings.Contains(serialized, `"name":"exec_command"`) {
		t.Fatalf("tool shape was not preserved: %s", serialized)
	}
}

func TestCaptureWritesRedactedFixtureAndValidSSE(t *testing.T) {
	server, err := Listen(Config{OutputDir: t.TempDir(), FixturePrefix: "test", ResponseMode: ResponseText})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	body := `{
  "model":"gpt-5.3-codex",
  "instructions":"private instructions",
  "input":[{"id":"input-real","type":"message","role":"user","content":[{"type":"input_text","text":"private prompt"}]}],
  "tools":[],
  "tool_choice":"auto",
  "parallel_tool_calls":false,
  "reasoning":{"summary":"private reasoning"},
  "stream":true,
  "store":false,
  "authorization":"Bearer body-secret",
  "path":"/home/rafa/private.go",
  "environment":{"OPENAI_API_KEY":"sk-body-secret"}
}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer header-secret")
	request.Header.Set("ChatGPT-Account-Id", "account-secret")
	request.Header.Set("User-Agent", "codex_exec/0.146.0 (private path)")
	request.Header.Set("X-Codex-Window-Id", "window-real")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatal("capture response unexpectedly depends on a [DONE] marker")
	}
	events := parseSSE(t, recorder.Body.String())
	if got := sseEventTypes(events); !reflect.DeepEqual(got, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}) {
		t.Fatalf("event sequence = %v", got)
	}

	entries, err := os.ReadDir(server.config.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("fixture count = %d", len(entries))
	}
	fixtureBytes, err := os.ReadFile(filepath.Join(server.config.OutputDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var fixture Fixture
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.CodexVersion != "0.146.0" {
		t.Fatalf("Codex version = %q", fixture.CodexVersion)
	}
	if !reflect.DeepEqual(fixture.Request.TopLevelFields, []string{"authorization", "environment", "input", "instructions", "model", "parallel_tool_calls", "path", "reasoning", "store", "stream", "tool_choice", "tools"}) {
		t.Fatalf("top-level fields = %v", fixture.Request.TopLevelFields)
	}
	for _, forbidden := range []string{"private instructions", "private prompt", "/home/rafa/private.go", "sk-body-secret", "Bearer header-secret", "account-secret", "window-real", "input-real"} {
		if strings.Contains(string(fixtureBytes), forbidden) {
			t.Fatalf("fixture contains forbidden value %q", forbidden)
		}
	}
	if _, ok := fixture.Request.Headers["authorization"]; ok {
		t.Fatalf("fixture contains authorization header: %s", fixtureBytes)
	}
	if _, ok := fixture.Request.Headers["chatgpt-account-id"]; ok {
		t.Fatalf("fixture contains ChatGPT account header: %s", fixtureBytes)
	}
	if body, ok := fixture.Request.Body.(map[string]any); ok {
		if _, present := body["authorization"]; present {
			t.Fatalf("fixture contains authorization body field: %s", fixtureBytes)
		}
	}
}

func TestCaptureRejectsOversizedAndMalformedBodies(t *testing.T) {
	server, err := Listen(Config{OutputDir: "", MaxBodyBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	for name, body := range map[string]string{
		"oversized": `{"model":"this body is too large"}`,
		"malformed": `{"model":`,
		"trailing":  `{"model":"ok"}{"extra":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(body))
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestListenRejectsNonLoopbackAndAcceptsLoopback(t *testing.T) {
	if _, err := Listen(Config{ListenAddr: "0.0.0.0:0", OutputDir: ""}); err == nil {
		t.Fatal("non-loopback listener was accepted")
	}
	if _, err := Listen(Config{ListenAddr: "127.0.0.1:0", CodexVersion: "not-a-version", OutputDir: ""}); err == nil {
		t.Fatal("invalid Codex version was accepted")
	}
	server, err := Listen(Config{ListenAddr: "127.0.0.1:0", OutputDir: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.BaseURL() == "" || !strings.HasPrefix(server.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("BaseURL = %q", server.BaseURL())
	}
}

func TestResponseModesHaveIncreasingSequenceNumbers(t *testing.T) {
	for _, mode := range []ResponseMode{ResponseText, ResponseFunction, ResponseParallel, ResponseCustom, ResponseIncomplete, ResponseFailed} {
		t.Run(string(mode), func(t *testing.T) {
			events, err := responseEvents(mode, "gpt-5.3-codex", "capture acknowledged", 1)
			if err != nil {
				t.Fatal(err)
			}
			previous := -1
			for _, event := range events {
				sequence, ok := sequenceNumber(event["sequence_number"])
				if !ok || sequence <= previous {
					t.Fatalf("invalid sequence number %v after %d", event["sequence_number"], previous)
				}
				previous = sequence
			}
			if mode != ResponseIncomplete && mode != ResponseFailed && events[len(events)-1]["type"] != "response.completed" {
				t.Fatalf("terminal event = %v", events[len(events)-1]["type"])
			}
		})
	}
}

func parseSSE(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("invalid SSE event: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func sseEventTypes(events []map[string]any) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event["type"].(string))
	}
	return result
}

func sequenceNumber(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case float64:
		return int(number), number == float64(int(number))
	default:
		return 0, false
	}
}
