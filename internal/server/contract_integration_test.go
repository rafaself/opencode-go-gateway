package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

// TestCheckedRequestFixturesThroughGateway exercises every request capture at
// the actual HTTP boundary. Normal turns must produce a valid Responses SSE
// stream; result-only captures must produce the documented unknown
// continuation response because no prior provider turn exists in the fresh
// test server.
func TestCheckedRequestFixturesThroughGateway(t *testing.T) {
	textStream := providerTextStream(t, "fixture text")
	applyPatchStream, err := os.ReadFile("../../testdata/opencodego/apply-patch-fragmented.sse")
	if err != nil {
		t.Fatal(err)
	}
	parallelStream := providerParallelToolStream(t)
	cases := []struct {
		name          string
		fixture       string
		provider      string
		wantStatus    int
		wantErrorCode string
	}{
		{name: "apply-patch", fixture: "apply-patch-request.json", provider: string(applyPatchStream), wantStatus: http.StatusOK},
		{name: "cancellation", fixture: "cancellation-request.json", provider: textStream, wantStatus: http.StatusOK},
		{name: "developer-instructions", fixture: "developer-instructions-request.json", provider: textStream, wantStatus: http.StatusOK},
		{name: "function-tools", fixture: "function-tools-request.json", provider: providerToolStream(t, "exec_command", "call-function", `{"cmd":"true"}`), wantStatus: http.StatusOK},
		{name: "parallel-tools", fixture: "parallel-tools-request.json", provider: parallelStream, wantStatus: http.StatusOK},
		{name: "shell-command", fixture: "shell-command-request.json", provider: providerToolStream(t, "exec_command", "call-shell", `{"cmd":"true"}`), wantStatus: http.StatusOK},
		{name: "simple", fixture: "simple-request.json", provider: textStream, wantStatus: http.StatusOK},
		{name: "custom-tool-result", fixture: "custom-tool-result-request.json", provider: textStream, wantStatus: http.StatusBadRequest, wantErrorCode: "continuation_unknown"},
		{name: "continuation", fixture: "continuation-request.json", provider: textStream, wantStatus: http.StatusBadRequest, wantErrorCode: "continuation_unknown"},
		{name: "empty-tool-result", fixture: "empty-tool-result-request.json", provider: textStream, wantStatus: http.StatusBadRequest, wantErrorCode: "continuation_unknown"},
		{name: "tool-error", fixture: "tool-error-request.json", provider: textStream, wantStatus: http.StatusBadRequest, wantErrorCode: "continuation_unknown"},
		{name: "tool-results", fixture: "tool-results-request.json", provider: textStream, wantStatus: http.StatusBadRequest, wantErrorCode: "continuation_unknown"},
		{name: "workspace-file-read", fixture: "workspace-file-read-request.json", provider: textStream, wantStatus: http.StatusBadRequest, wantErrorCode: "continuation_unknown"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			gateway := newIntegrationGateway(t, staticUpstream(test.provider), nil)
			response := postRequest(t, gateway, checkedFixtureRequestBody(t, test.fixture))
			defer response.Body.Close()
			raw := readBody(t, response.Body)
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.StatusCode, test.wantStatus, raw)
			}
			if test.wantStatus == http.StatusOK {
				events := decodeResponsesWireContract(t, raw)
				independent := readResponseEvents(t, strings.NewReader(raw))
				if !equalStrings(responseEventTypes(events), responseEventTypes(independent)) {
					t.Fatalf("independent SSE parsers disagree: wire=%v scanner=%v", responseEventTypes(events), responseEventTypes(independent))
				}
				return
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
				t.Fatalf("error response is not JSON: %v; body=%s", err, raw)
			}
			if envelope.Error.Code != test.wantErrorCode {
				t.Fatalf("error code = %q, want %q; body=%s", envelope.Error.Code, test.wantErrorCode, raw)
			}
		})
	}
}

func TestResponsesWireContractIsIndependentOfScannerParsing(t *testing.T) {
	provider := providerTextStream(t, "Olá, 世界")
	gateway := newIntegrationGateway(t, staticUpstream(provider), nil)
	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	raw := readBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, raw)
	}
	events := decodeResponsesWireContract(t, raw)
	scanned := readResponseEvents(t, strings.NewReader(raw))
	if len(events) != len(scanned) {
		t.Fatalf("event count = %d, scanner count = %d", len(events), len(scanned))
	}
	for index := range events {
		if events[index]["type"] != scanned[index]["type"] || events[index]["sequence_number"] != scanned[index]["sequence_number"] {
			t.Fatalf("event %d differs: wire=%#v scanner=%#v", index, events[index], scanned[index])
		}
	}
	if countResponseTerminals(events) != 1 {
		t.Fatalf("terminal count = %d", countResponseTerminals(events))
	}
}

func TestGatewayConcurrentRequestsKeepIndependentStreams(t *testing.T) {
	const requestCount = 12
	gateway := newIntegrationGateway(t, staticUpstream(providerTextStream(t, "concurrent")), nil)
	results := make(chan error, requestCount)
	var wait sync.WaitGroup
	wait.Add(requestCount)
	for index := 0; index < requestCount; index++ {
		go func() {
			defer wait.Done()
			request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(textRequestBody()))
			if err != nil {
				results <- err
				return
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := gateway.Client().Do(request)
			if err != nil {
				results <- err
				return
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err == nil && response.StatusCode != http.StatusOK {
				err = errors.New("concurrent request returned a non-200 status")
			}
			if err == nil {
				_, err = parseResponsesWireContract(raw)
			}
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func decodeResponsesWireContract(t *testing.T, raw string) []map[string]any {
	t.Helper()
	events, err := parseResponsesWireContract([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func parseResponsesWireContract(raw []byte) ([]map[string]any, error) {
	decoder := opencodego.NewSSEDecoder(bytes.NewReader(raw), opencodego.SSEDecoderOptions{})
	events := make([]map[string]any, 0)
	previousSequence := -1
	terminals := 0
	lastType := ""
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if event.Data == "[DONE]" {
			return nil, errors.New("Responses stream contains [DONE]")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			return nil, err
		}
		eventType, ok := payload["type"].(string)
		if !ok || !strings.HasPrefix(eventType, "response.") {
			return nil, errors.New("Responses event has an invalid type")
		}
		sequence, ok := payload["sequence_number"].(float64)
		if !ok || sequence != float64(int(sequence)) || int(sequence) <= previousSequence {
			return nil, errors.New("Responses sequence numbers are not increasing")
		}
		previousSequence = int(sequence)
		switch eventType {
		case "response.completed", "response.incomplete", "response.failed":
			terminals++
		}
		lastType = eventType
		events = append(events, payload)
	}
	if len(events) == 0 || terminals != 1 || (lastType != "response.completed" && lastType != "response.incomplete" && lastType != "response.failed") {
		return nil, errors.New("Responses stream does not contain exactly one terminal event")
	}
	return events, nil
}

func providerTextStream(t *testing.T, text string) string {
	t.Helper()
	stop := "stop"
	chunks := []opencodego.ChatCompletionChunk{
		{ID: "provider-text", Object: "chat.completion.chunk", Created: 1, Model: opencodego.DefaultModel, Choices: []opencodego.ChatCompletionChunkChoice{{Index: 0, Delta: opencodego.ChatMessage{Content: &text}}}},
		{ID: "provider-text", Object: "chat.completion.chunk", Created: 1, Model: opencodego.DefaultModel, Choices: []opencodego.ChatCompletionChunkChoice{{Index: 0, FinishReason: &stop}}},
	}
	return encodeProviderChunks(t, chunks)
}

func providerParallelToolStream(t *testing.T) string {
	firstIndex, secondIndex := 0, 1
	finish := "tool_calls"
	chunks := []opencodego.ChatCompletionChunk{
		{ID: "provider-parallel", Object: "chat.completion.chunk", Created: 1, Model: opencodego.DefaultModel, Choices: []opencodego.ChatCompletionChunkChoice{{Index: 0, Delta: opencodego.ChatMessage{ToolCalls: []opencodego.ToolCall{
			{Index: &firstIndex, ID: "call-exec", Type: "function", Function: opencodego.ToolCallFunction{Name: "exec_command", Arguments: `{"cmd":"true"}`}},
			{Index: &secondIndex, ID: "call-stdin", Type: "function", Function: opencodego.ToolCallFunction{Name: "write_stdin", Arguments: `{"session_id":1}`}},
		}}}}},
		{ID: "provider-parallel", Object: "chat.completion.chunk", Created: 1, Model: opencodego.DefaultModel, Choices: []opencodego.ChatCompletionChunkChoice{{Index: 0, FinishReason: &finish}}},
	}
	return encodeProviderChunks(t, chunks)
}

func encodeProviderChunks(t *testing.T, chunks []opencodego.ChatCompletionChunk) string {
	t.Helper()
	var stream strings.Builder
	for _, chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: ")
		stream.Write(data)
		stream.WriteString("\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	return stream.String()
}
