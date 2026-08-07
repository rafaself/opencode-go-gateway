package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

func TestResponsesStreamsTextIncrementallyThroughTheGateway(t *testing.T) {
	providerStream := strings.Join([]string{
		`data: {"id":"provider-secret-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`,
		`data: {"id":"provider-secret-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"世界"},"finish_reason":null}]}`,
		`data: {"id":"provider-secret-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"","reasoning_content":"private provider reasoning"},"finish_reason":null}]}`,
		`data: {"id":"provider-secret-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"provider-secret-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":2}}}`,
		"data: [DONE]",
	}, "\n\n") + "\n\n"

	var gotRequest bridge.Request
	client := UpstreamClientFunc(func(_ context.Context, request bridge.Request) (*UpstreamResponse, error) {
		gotRequest = request
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader(providerStream)),
		}, nil
	})
	gateway := newIntegrationGateway(t, client, nil)

	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}

	events := readResponseEvents(t, response.Body)
	if got := responseEventTypes(events); !equalStrings(got, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}) {
		t.Fatalf("event types = %v events=%s", got, mustJSON(t, events))
	}

	var output strings.Builder
	for _, event := range events {
		if event["type"] == "response.output_text.delta" {
			output.WriteString(event["delta"].(string))
		}
	}
	if got := output.String(); got != "Hello 世界" {
		t.Fatalf("incremental output = %q", got)
	}
	if strings.Contains(string(mustJSON(t, events)), "private provider reasoning") {
		t.Fatal("provider reasoning_content was exposed downstream")
	}
	terminal := events[len(events)-1]
	responseObject := terminal["response"].(map[string]any)
	usage := responseObject["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) || usage["total_tokens"] != float64(18) {
		t.Fatalf("usage = %#v", usage)
	}
	if responseObject["model"] != "deepseek-v4-flash" {
		t.Fatalf("response model = %#v, want deepseek-v4-flash", responseObject["model"])
	}
	if gotRequest.Model != "deepseek-v4-flash (go)" || !gotRequest.Generation.Stream || gotRequest.Instructions != "system instruction" {
		t.Fatalf("bridge request = %#v", gotRequest)
	}
	if len(gotRequest.Input) != 2 {
		t.Fatalf("bridge input count = %d", len(gotRequest.Input))
	}
}

func TestResponsesTreatsFirstProviderTerminalAsAuthoritative(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"provider-terminal","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	gateway := newIntegrationGateway(t, staticUpstream(stream), nil)
	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	events := readResponseEvents(t, response.Body)
	if countResponseTerminals(events) != 1 || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("events = %#v, want one completed response without read-ahead", events)
	}
}

func TestResponsesEmitsLifecycleBeforeQuietUpstreamProducesData(t *testing.T) {
	body := &blockingBody{closed: make(chan struct{})}
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	}), nil)

	response := postTextRequest(t, gateway)
	reader := bufio.NewReader(response.Body)
	defer response.Body.Close()

	types := make([]string, 0, 2)
	for len(types) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading lifecycle event %d: %v", len(types), err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &event); err != nil {
			t.Fatalf("decoding lifecycle event %q: %v", line, err)
		}
		eventType, ok := event["type"].(string)
		if !ok {
			t.Fatalf("lifecycle event has no type: %#v", event)
		}
		types = append(types, eventType)
	}
	if !equalStrings(types, []string{"response.created", "response.in_progress"}) {
		t.Fatalf("lifecycle events = %v", types)
	}

	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("quiet upstream body was not closed after the client disconnected")
	}
}

func TestResponsesUsesTheRealOpenCodeGoHTTPAdapter(t *testing.T) {
	const providerKey = "provider-api-key-secret"
	var providerRequest struct {
		Authorization string
		Body          map[string]any
	}
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerRequest.Authorization = request.Header.Get("Authorization")
		if err := json.NewDecoder(request.Body).Decode(&providerRequest.Body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "data: {\"id\":\"provider-id\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"from provider\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer, "data: {\"id\":\"provider-id\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer provider.Close()

	client, err := opencodego.NewClient(opencodego.ClientConfig{
		APIKey:     providerKey,
		BaseURL:    provider.URL,
		HTTPClient: provider.Client(),
		UserAgent:  "opencode-go-gateway/test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gateway := newIntegrationGateway(t, NewOpenCodeUpstreamClient(client), slog.New(slog.NewTextHandler(&logs, nil)))
	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	events := readResponseEvents(t, response.Body)
	if events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("terminal event = %#v", events[len(events)-1])
	}
	if providerRequest.Authorization != "Bearer "+providerKey {
		t.Fatalf("provider authorization = %q", providerRequest.Authorization)
	}
	if providerRequest.Body["model"] != "deepseek-v4-flash" || providerRequest.Body["stream"] != true || providerRequest.Body["reasoning_effort"] != "high" {
		t.Fatalf("provider request metadata = %#v", providerRequest.Body)
	}
	messages := providerRequest.Body["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" || messages[len(messages)-1].(map[string]any)["role"] != "user" {
		t.Fatalf("provider message roles = %#v", messages)
	}
	if strings.Contains(logs.String(), providerKey) || strings.Contains(string(mustJSON(t, events)), providerKey) {
		t.Fatal("provider API key crossed into logs or downstream response bytes")
	}
}

func TestResponsesStreamsFunctionToolsThroughTheRealProviderAdapter(t *testing.T) {
	const providerKey = "function-provider-key"
	var providerRequest map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&providerRequest); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		tools, ok := providerRequest["tools"].([]any)
		if !ok || len(tools) != 3 {
			http.Error(writer, "function and custom tools missing", http.StatusBadRequest)
			return
		}
		seenNames := map[string]bool{}
		for _, rawTool := range tools {
			tool := rawTool.(map[string]any)
			function := tool["function"].(map[string]any)
			name, _ := function["name"].(string)
			seenNames[name] = true
			if tool["type"] != "function" || (name != "lookup" && name != "other_tool" && name != opencodego.ApplyPatchUpstreamName) {
				http.Error(writer, "function tool changed", http.StatusBadRequest)
				return
			}
			if name == opencodego.ApplyPatchUpstreamName && function["strict"] != true {
				http.Error(writer, "custom wrapper is not strict", http.StatusBadRequest)
				return
			}
			if name == "lookup" {
				if function["description"] != "look up a value" || function["strict"] != true {
					http.Error(writer, "function tool changed", http.StatusBadRequest)
					return
				}
				parameters, parametersOK := function["parameters"].(map[string]any)
				required, requiredOK := parameters["required"].([]any)
				if !parametersOK || parameters["type"] != "object" || !requiredOK || len(required) != 1 || required[0] != "q" {
					http.Error(writer, "function schema changed", http.StatusBadRequest)
					return
				}
			}
		}
		if !seenNames["lookup"] || !seenNames["other_tool"] || !seenNames[opencodego.ApplyPatchUpstreamName] {
			http.Error(writer, "function tools changed", http.StatusBadRequest)
			return
		}
		if _, exists := providerRequest["tool_choice"]; exists {
			http.Error(writer, "thinking-mode auto choice was not omitted", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		providerStream := strings.Join([]string{
			`data: {"id":"provider-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"prefix","tool_calls":[{"index":0,"id":"provider-call-","type":"function","function":{"name":"look","arguments":"[1"}}]},"finish_reason":null}]}`,
			`data: {"id":"provider-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"type":"function","function":{"name":"other","arguments":"not"}}]},"finish_reason":null}]}`,
			`data: {"id":"provider-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"suffix","tool_calls":[{"index":0,"id":"id","function":{"name":"up","arguments":"]"}},{"index":1,"function":{"name":"_tool","arguments":"-json"}}]},"finish_reason":null}]}`,
			`data: {"id":"provider-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		_, _ = io.WriteString(writer, providerStream)
	}))
	defer provider.Close()

	client, err := opencodego.NewClient(opencodego.ClientConfig{
		APIKey:     providerKey,
		BaseURL:    provider.URL,
		HTTPClient: provider.Client(),
		UserAgent:  "opencode-go-gateway/function-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gateway := newIntegrationGateway(t, NewOpenCodeUpstreamClient(client), slog.New(slog.NewTextHandler(&logs, nil)))
	requestBody := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"look up"}],"tools":[{"type":"function","name":"lookup","description":"look up a value","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"strict":true},{"type":"function","name":"other_tool","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":true,"stream":true}`
	response := postRequest(t, gateway, requestBody)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, readBody(t, response.Body))
	}
	events := readResponseEvents(t, response.Body)
	if got := responseEventTypes(events); !equalStrings(got, []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}) {
		t.Fatalf("event types = %v", got)
	}
	var output strings.Builder
	var calls []map[string]any
	var addedCalls []map[string]any
	for _, event := range events {
		if event["type"] == "response.output_text.delta" {
			output.WriteString(event["delta"].(string))
		}
		if event["type"] == "response.output_item.done" {
			item := event["item"].(map[string]any)
			if item["type"] == "function_call" {
				calls = append(calls, item)
			}
		}
		if event["type"] == "response.output_item.added" {
			item := event["item"].(map[string]any)
			if item["type"] == "function_call" {
				addedCalls = append(addedCalls, item)
			}
		}
	}
	if output.String() != "prefixsuffix" || len(calls) != 2 || len(addedCalls) != len(calls) {
		t.Fatalf("output=%q calls=%#v added=%#v", output.String(), calls, addedCalls)
	}
	for index := range calls {
		if calls[index]["call_id"] != addedCalls[index]["call_id"] || calls[index]["id"] != addedCalls[index]["id"] {
			t.Fatalf("function identity changed between added and done: added=%#v done=%#v", addedCalls[index], calls[index])
		}
	}
	if calls[0]["call_id"] != "provider-call-" || calls[0]["name"] != "lookup" || calls[0]["arguments"] != `[1]` {
		t.Fatalf("first function call = %#v", calls[0])
	}
	if calls[1]["call_id"] != "call_0_1" || calls[1]["name"] != "other_tool" || calls[1]["arguments"] != "not-json" {
		t.Fatalf("second function call = %#v", calls[1])
	}
	if calls[0]["id"] == calls[1]["id"] || calls[0]["id"] == nil || calls[1]["id"] == nil || calls[0]["status"] != "completed" || calls[1]["status"] != "completed" {
		t.Fatalf("function item identity/status = %#v", calls)
	}
	if strings.Contains(logs.String(), "not-json") || strings.Contains(logs.String(), providerKey) {
		t.Fatalf("tool data or provider credential appeared in logs: %s", logs.String())
	}
}

func TestResponsesStreamsCheckedApplyPatchFixtureThroughProvider(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: café.txt\n+Olá, \"world\" \\ literal\n*** End Patch"
	wrapper, err := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: patch})
	if err != nil {
		t.Fatal(err)
	}
	fragments := make([]string, 0, (len(wrapper)+2)/3)
	for offset := 0; offset < len(wrapper); {
		end := offset + 3
		if end > len(wrapper) {
			end = len(wrapper)
		}
		for end < len(wrapper) && !utf8.RuneStart(wrapper[end]) {
			end++
		}
		fragments = append(fragments, string(wrapper[offset:end]))
		offset = end
	}
	providerStream := providerToolStream(t, opencodego.ApplyPatchUpstreamName, "provider-custom-call", fragments...)

	var providerRequest map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&providerRequest); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		tools, ok := providerRequest["tools"].([]any)
		if !ok || len(tools) != 2 {
			http.Error(writer, "fixture tools were not translated", http.StatusBadRequest)
			return
		}
		seen := make(map[string]map[string]any)
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok || tool["type"] != "function" {
				http.Error(writer, "unexpected provider tool", http.StatusBadRequest)
				return
			}
			function, ok := tool["function"].(map[string]any)
			if !ok {
				http.Error(writer, "missing provider function", http.StatusBadRequest)
				return
			}
			name, _ := function["name"].(string)
			seen[name] = function
		}
		if _, ok := seen["exec_command"]; !ok {
			http.Error(writer, "standard function was not preserved", http.StatusBadRequest)
			return
		}
		customFunction, ok := seen[opencodego.ApplyPatchUpstreamName]
		if !ok || customFunction["strict"] != true {
			http.Error(writer, "synthetic custom function was not declared strictly", http.StatusBadRequest)
			return
		}
		parameters, ok := customFunction["parameters"].(map[string]any)
		properties, propertiesOK := parameters["properties"].(map[string]any)
		inputSchema, inputOK := properties[opencodego.ApplyPatchWrapperField].(map[string]any)
		required, requiredOK := parameters["required"].([]any)
		if !ok || !propertiesOK || !inputOK || inputSchema["type"] != "string" || parameters["type"] != "object" || parameters["additionalProperties"] != false || !requiredOK || len(required) != 1 || required[0] != opencodego.ApplyPatchWrapperField {
			http.Error(writer, "custom wrapper schema changed", http.StatusBadRequest)
			return
		}
		if _, exists := seen["mcp"]; exists {
			http.Error(writer, "namespace metadata was sent upstream", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, providerStream)
	}))
	defer provider.Close()

	client, err := opencodego.NewClient(opencodego.ClientConfig{
		APIKey:     "apply-patch-provider-key",
		BaseURL:    provider.URL,
		HTTPClient: provider.Client(),
		UserAgent:  "opencode-go-gateway/apply-patch-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gateway := newIntegrationGateway(t, NewOpenCodeUpstreamClient(client), slog.New(slog.NewTextHandler(&logs, nil)))
	response := postRequest(t, gateway, checkedFixtureRequestBody(t, "apply-patch-request.json"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, readBody(t, response.Body))
	}
	rawResponse := readBody(t, response.Body)
	events := readResponseEvents(t, strings.NewReader(rawResponse))
	wantTypes := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
		"response.completed",
	}
	if got := responseEventTypes(events); !equalStrings(got, wantTypes) {
		t.Fatalf("event types = %v, want %v; bytes = %s", got, wantTypes, rawResponse)
	}
	for index, event := range events {
		if event["sequence_number"] != float64(index) {
			t.Fatalf("event %d sequence = %#v", index, event["sequence_number"])
		}
	}
	addedItem := events[2]["item"].(map[string]any)
	addedID, _ := addedItem["id"].(string)
	if !strings.HasPrefix(addedID, "ctc_") || addedItem["call_id"] != "provider-custom-call" || addedItem["name"] != opencodego.ApplyPatchToolName || addedItem["input"] != "" || events[2]["output_index"] != float64(0) {
		t.Fatalf("custom added item = %#v", addedItem)
	}
	if events[3]["item_id"] != addedID || events[3]["output_index"] != float64(0) || events[3]["delta"] != patch {
		t.Fatalf("custom input delta = %#v", events[3])
	}
	if events[4]["item_id"] != addedID || events[4]["output_index"] != float64(0) || events[4]["input"] != patch {
		t.Fatalf("custom input done = %#v", events[4])
	}
	doneItem := events[5]["item"].(map[string]any)
	if doneItem["id"] != addedID || doneItem["call_id"] != "provider-custom-call" || doneItem["input"] != patch || doneItem["status"] != "completed" {
		t.Fatalf("custom done item = %#v", doneItem)
	}
	if strings.Contains(rawResponse, opencodego.ApplyPatchUpstreamName) || strings.Contains(string(mustJSON(t, providerRequest)), patch) || strings.Contains(logs.String(), patch) {
		t.Fatalf("synthetic name or patch leaked into a forbidden boundary: response=%s logs=%s", rawResponse, logs.String())
	}
}

func TestResponsesRejectsUnknownToolResultHistory(t *testing.T) {
	called := false
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		called = true
		return nil, errors.New("upstream must not be called")
	}), nil)
	response := postRequest(t, gateway, checkedFixtureRequestBody(t, "custom-tool-result-request.json"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.StatusCode, readBody(t, response.Body))
	}
	body := readBody(t, response.Body)
	if !strings.Contains(body, `"continuation_unknown"`) || strings.Contains(body, "<normalized:id>") {
		t.Fatalf("unknown continuation response = %s", body)
	}
	if called {
		t.Fatal("upstream was called for an unknown continuation")
	}
}

func TestResponsesPreservesContinuationErrorsForDeferredResultCorrelation(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "duplicate result",
			body: `{"model":"deepseek-v4-flash (go)","input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"one"},{"type":"function_call_output","call_id":"call-1","output":"two"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`,
			code: "continuation_duplicate",
		},
		{
			name: "mismatched result kind",
			body: `{"model":"deepseek-v4-flash (go)","input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"call-1","output":"wrong kind"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`,
			code: "continuation_kind_mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
				called = true
				return nil, errors.New("upstream must not be called")
			}), nil)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
			if called {
				t.Fatal("upstream was called for a continuation validation error")
			}
		})
	}
}

func TestResponsesRejectsOutputOnlyContinuationKindMismatch(t *testing.T) {
	called := 0
	server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		called++
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(providerToolStream(t, "lookup", "stored-function-call", `{}`))),
		}, nil
	}), nil)
	initial := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	first := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(initial))
	firstRequest.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(first, firstRequest)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"response.completed"`) {
		t.Fatalf("initial response = %d %s", first.Code, first.Body.String())
	}

	continuation := `{"model":"deepseek-v4-flash (go)","input":[{"type":"custom_tool_call_output","call_id":"stored-function-call","output":"wrong kind"}],"tools":[],"stream":true}`
	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(continuation))
	secondRequest.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(second, secondRequest)
	if second.Code != http.StatusBadRequest || !strings.Contains(second.Body.String(), `"code":"continuation_kind_mismatch"`) {
		t.Fatalf("output-only mismatch = %d %s", second.Code, second.Body.String())
	}
	if called != 1 {
		t.Fatalf("upstream calls = %d, want only initial capture", called)
	}
}

func TestResponsesRejectsOutputOnlyContinuationDuplicate(t *testing.T) {
	called := false
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		called = true
		return nil, errors.New("upstream must not be called")
	}), nil)
	body := `{"model":"deepseek-v4-flash (go)","input":[{"type":"function_call_output","call_id":"stored-call","output":"one"},{"type":"function_call_output","call_id":"stored-call","output":"two"}],"tools":[],"stream":true}`
	response := postRequest(t, gateway, body)
	defer response.Body.Close()
	responseBody := readBody(t, response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(responseBody, `"code":"continuation_duplicate"`) {
		t.Fatalf("output-only duplicate = %d %s", response.StatusCode, responseBody)
	}
	if called {
		t.Fatal("upstream was called for a duplicate output-only result")
	}
}

func TestResponsesRejectsUnknownOutputOnlyContinuation(t *testing.T) {
	called := false
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		called = true
		return nil, errors.New("upstream must not be called")
	}), nil)
	body := `{"model":"deepseek-v4-flash (go)","input":[{"type":"custom_tool_call_output","call_id":"missing-call","output":"result"}],"tools":[],"stream":true}`
	response := postRequest(t, gateway, body)
	defer response.Body.Close()
	responseBody := readBody(t, response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(responseBody, `"code":"continuation_unknown"`) {
		t.Fatalf("output-only unknown = %d %s", response.StatusCode, responseBody)
	}
	if called {
		t.Fatal("upstream was called for an unknown output-only result")
	}
}

func TestResponsesReplaysDeepSeekReasoningAndToolResultsEndToEnd(t *testing.T) {
	var providerRequests []map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		providerRequests = append(providerRequests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if len(providerRequests) == 1 {
			_, _ = io.WriteString(writer, strings.Join([]string{
				`data: {"id":"provider-first","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"deep reasoning"},"finish_reason":null}]}`,
				`data: {"id":"provider-first","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"provider-call","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"provider-first","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}, "\n\n")+"\n\n")
			return
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 3 {
			http.Error(writer, "continuation messages were not reconstructed", http.StatusBadRequest)
			return
		}
		assistant, ok := messages[1].(map[string]any)
		if !ok || assistant["role"] != "assistant" || assistant["content"] != "" || assistant["reasoning_content"] != "deep reasoning" {
			http.Error(writer, "assistant reasoning turn was not replayed", http.StatusBadRequest)
			return
		}
		calls, ok := assistant["tool_calls"].([]any)
		if !ok || len(calls) != 1 {
			http.Error(writer, "assistant tool call was not replayed", http.StatusBadRequest)
			return
		}
		call := calls[0].(map[string]any)
		function := call["function"].(map[string]any)
		if call["id"] != "provider-call" || function["name"] != "lookup" || function["arguments"] != `{"query":"x"}` {
			http.Error(writer, "assistant tool call changed during replay", http.StatusBadRequest)
			return
		}
		result := messages[2].(map[string]any)
		if result["role"] != "tool" || result["tool_call_id"] != "provider-call" || result["content"] != "exact tool output" {
			http.Error(writer, "tool result changed during replay", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, strings.Join([]string{
			`data: {"id":"provider-second","object":"chat.completion.chunk","created":1700000001,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"final answer"},"finish_reason":null}]}`,
			`data: {"id":"provider-second","object":"chat.completion.chunk","created":1700000001,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n")+"\n\n")
	}))
	defer provider.Close()
	client, err := opencodego.NewClient(opencodego.ClientConfig{
		APIKey:     "continuation-provider-key",
		BaseURL:    provider.URL,
		HTTPClient: provider.Client(),
		UserAgent:  "opencode-go-gateway/continuation-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway := newIntegrationGateway(t, NewOpenCodeUpstreamClient(client), nil)
	initial := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"stream":true}`
	firstResponse := postRequest(t, gateway, initial)
	firstEvents := readResponseEvents(t, firstResponse.Body)
	firstResponse.Body.Close()
	if len(firstEvents) == 0 || firstEvents[len(firstEvents)-1]["type"] != "response.completed" {
		t.Fatalf("initial tool response = %#v", firstEvents)
	}
	if strings.Contains(string(mustJSON(t, firstEvents)), "deep reasoning") {
		t.Fatal("reasoning content leaked to Responses")
	}
	continuation := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"},{"type":"function_call_output","call_id":"provider-call","output":"exact tool output"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"stream":true}`
	secondResponse := postRequest(t, gateway, continuation)
	secondEvents := readResponseEvents(t, secondResponse.Body)
	secondResponse.Body.Close()
	if len(secondEvents) == 0 || secondEvents[len(secondEvents)-1]["type"] != "response.completed" {
		t.Fatalf("continued response = %#v", secondEvents)
	}
	if len(providerRequests) != 2 {
		t.Fatalf("provider request count = %d", len(providerRequests))
	}
}

func TestResponsesRetriesContinuationAfterUpstreamBadRequest(t *testing.T) {
	var upstreamCalls int
	var continuationRequests int
	upstream := UpstreamClientFunc(func(_ context.Context, request bridge.Request) (*UpstreamResponse, error) {
		upstreamCalls++
		if request.Continuation != nil {
			continuationRequests++
		}
		switch upstreamCalls {
		case 1:
			return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(providerToolStream(t, "lookup", "retry-call", `{"x":1}`)))}, nil
		case 2:
			return &UpstreamResponse{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"provider rejected retry"}`))}, nil
		default:
			return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"id":"retry-success","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"retried"},"finish_reason":null}]}`,
				`data: {"id":"retry-success","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}, "\n\n") + "\n\n"))}, nil
		}
	})
	gateway := newIntegrationGateway(t, upstream, nil)
	initial := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	first := postRequest(t, gateway, initial)
	firstEvents := readResponseEvents(t, first.Body)
	first.Body.Close()
	if len(firstEvents) == 0 || firstEvents[len(firstEvents)-1]["type"] != "response.completed" {
		t.Fatalf("initial response = %#v", firstEvents)
	}
	continuation := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"},{"type":"function_call","call_id":"retry-call","name":"lookup","arguments":"{\"x\":1}"},{"type":"function_call_output","call_id":"retry-call","output":"retry output"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	failed := postRequest(t, gateway, continuation)
	failedBody := readBody(t, failed.Body)
	failed.Body.Close()
	if failed.StatusCode != http.StatusBadRequest || !strings.Contains(failedBody, `"upstream_bad_request"`) {
		t.Fatalf("upstream rejection = %d %s", failed.StatusCode, failedBody)
	}
	retried := postRequest(t, gateway, continuation)
	retriedEvents := readResponseEvents(t, retried.Body)
	retried.Body.Close()
	if len(retriedEvents) == 0 || retriedEvents[len(retriedEvents)-1]["type"] != "response.completed" {
		t.Fatalf("retried response = %#v", retriedEvents)
	}
	if upstreamCalls != 3 || continuationRequests != 2 {
		t.Fatalf("upstream calls = %d, continuation requests = %d", upstreamCalls, continuationRequests)
	}
}

func TestResponsesReleasesContinuationBeforeFirstAcceptedUpstreamEvent(t *testing.T) {
	const initial = `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	const continuation = `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"},{"type":"function_call","call_id":"retry-call","name":"lookup","arguments":"{\"x\":1}"},{"type":"function_call_output","call_id":"retry-call","output":"retry output"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	tests := []struct {
		name   string
		stream string
		cancel bool
	}{
		{name: "malformed SSE", stream: "data: {not-json\n\n"},
		{name: "immediate EOF", stream: ""},
		{name: "provider stream error", stream: "data: {\"error\":{\"type\":\"server_error\"}}\n\n"},
		{name: "client cancellation", cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var continuationAttempts int
			entered := make(chan struct{})
			var blocked *blockingBody
			upstream := UpstreamClientFunc(func(_ context.Context, request bridge.Request) (*UpstreamResponse, error) {
				if request.Continuation == nil {
					return &UpstreamResponse{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader(providerToolStream(t, "lookup", "retry-call", `{"x":1}`))),
					}, nil
				}
				continuationAttempts++
				if continuationAttempts == 1 {
					if test.cancel {
						blocked = &blockingBody{closed: make(chan struct{})}
						close(entered)
						return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: blocked}, nil
					}
					return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(test.stream))}, nil
				}
				success := strings.Join([]string{
					`data: {"id":"retry-success","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"retried"},"finish_reason":null}]}`,
					`data: {"id":"retry-success","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`data: [DONE]`,
				}, "\n\n") + "\n\n"
				return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(success))}, nil
			})
			server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, upstream, nil)

			initialRecorder := httptest.NewRecorder()
			initialRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(initial))
			initialRequest.Header.Set("Content-Type", "application/json")
			server.ServeHTTP(initialRecorder, initialRequest)
			if initialRecorder.Code != http.StatusOK || !strings.Contains(initialRecorder.Body.String(), `"response.completed"`) {
				t.Fatalf("initial response = %d %s", initialRecorder.Code, initialRecorder.Body.String())
			}

			firstRecorder := httptest.NewRecorder()
			firstRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(continuation))
			firstRequest.Header.Set("Content-Type", "application/json")
			if test.cancel {
				requestContext, cancel := context.WithCancel(firstRequest.Context())
				firstRequest = firstRequest.WithContext(requestContext)
				done := make(chan struct{})
				go func() {
					server.ServeHTTP(firstRecorder, firstRequest)
					close(done)
				}()
				select {
				case <-entered:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("cancellation upstream was not reached")
				}
				select {
				case <-blocked.closed:
				case <-time.After(time.Second):
					t.Fatal("cancellation did not close the upstream body")
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("cancelled response handler did not finish")
				}
			} else {
				server.ServeHTTP(firstRecorder, firstRequest)
			}
			if test.cancel {
				if strings.Contains(firstRecorder.Body.String(), `"response.failed"`) {
					t.Fatalf("canceled response emitted a terminal failure: %s", firstRecorder.Body.String())
				}
			} else if !strings.Contains(firstRecorder.Body.String(), `"response.failed"`) {
				t.Fatalf("pre-acceptance response = %s", firstRecorder.Body.String())
			}

			retryRecorder := httptest.NewRecorder()
			retryRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(continuation))
			retryRequest.Header.Set("Content-Type", "application/json")
			server.ServeHTTP(retryRecorder, retryRequest)
			if retryRecorder.Code != http.StatusOK || !strings.Contains(retryRecorder.Body.String(), `"response.completed"`) {
				t.Fatalf("retry response = %d %s", retryRecorder.Code, retryRecorder.Body.String())
			}
			if continuationAttempts != 2 {
				t.Fatalf("continuation attempts = %d, want 2", continuationAttempts)
			}
		})
	}

}

func TestResponsesConsumesContinuationAfterFirstAcceptedEvent(t *testing.T) {
	const initial = `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	const continuation = `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"},{"type":"function_call","call_id":"accepted-call","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"accepted-call","output":"result"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	var continuationAttempts int
	upstream := UpstreamClientFunc(func(_ context.Context, request bridge.Request) (*UpstreamResponse, error) {
		if request.Continuation == nil {
			return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(providerToolStream(t, "lookup", "accepted-call", `{}`)))}, nil
		}
		continuationAttempts++
		if continuationAttempts == 1 {
			acceptedThenEOF := "data: {\"id\":\"accepted\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
			return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(acceptedThenEOF))}, nil
		}
		return nil, errors.New("provider must not be retried after acceptance")
	})
	server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, upstream, nil)

	initialRecorder := httptest.NewRecorder()
	initialRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(initial))
	initialRequest.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(initialRecorder, initialRequest)

	firstRecorder := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(continuation))
	firstRequest.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(firstRecorder, firstRequest)
	if !strings.Contains(firstRecorder.Body.String(), `"response.failed"`) {
		t.Fatalf("accepted upstream failure response = %s", firstRecorder.Body.String())
	}

	secondRecorder := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(continuation))
	secondRequest.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusBadRequest || !strings.Contains(secondRecorder.Body.String(), `"continuation_consumed"`) {
		t.Fatalf("accepted continuation retry = %d %s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if continuationAttempts != 1 {
		t.Fatalf("provider continuation attempts = %d, want 1", continuationAttempts)
	}
}

func TestResponsesReplaysCustomApplyPatchContinuationThroughProvider(t *testing.T) {
	var providerRequests []map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		providerRequests = append(providerRequests, body)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if len(providerRequests) == 1 {
			_, _ = io.WriteString(writer, providerToolStream(t, opencodego.ApplyPatchUpstreamName, "custom-retry-call", `{"input":"patch"}`))
			return
		}
		messages := body["messages"].([]any)
		assistant := messages[1].(map[string]any)
		calls := assistant["tool_calls"].([]any)
		call := calls[0].(map[string]any)
		function := call["function"].(map[string]any)
		if assistant["content"] != "" || assistant["reasoning_content"] != "" || call["id"] != "custom-retry-call" || function["name"] != opencodego.ApplyPatchUpstreamName || function["arguments"] != `{"input":"patch"}` {
			http.Error(writer, "custom continuation was not reconstructed", http.StatusBadRequest)
			return
		}
		result := messages[2].(map[string]any)
		if result["role"] != "tool" || result["tool_call_id"] != "custom-retry-call" || result["content"] != "applied exactly" {
			http.Error(writer, "custom result was not reconstructed", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, strings.Join([]string{
			`data: {"id":"custom-second","object":"chat.completion.chunk","created":1700000001,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`,
			`data: {"id":"custom-second","object":"chat.completion.chunk","created":1700000001,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n")+"\n\n")
	}))
	defer provider.Close()
	client, err := opencodego.NewClient(opencodego.ClientConfig{APIKey: "custom-continuation-key", BaseURL: provider.URL, HTTPClient: provider.Client()})
	if err != nil {
		t.Fatal(err)
	}
	gateway := newIntegrationGateway(t, NewOpenCodeUpstreamClient(client), nil)
	initial := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"apply patch"}],"tools":[],"stream":true}`
	first := postRequest(t, gateway, initial)
	firstEvents := readResponseEvents(t, first.Body)
	first.Body.Close()
	if len(firstEvents) == 0 || firstEvents[len(firstEvents)-1]["type"] != "response.completed" {
		t.Fatalf("custom initial response = %#v", firstEvents)
	}
	continuation := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"apply patch"},{"type":"custom_tool_call_output","call_id":"custom-retry-call","output":"applied exactly"}],"tools":[],"stream":true}`
	second := postRequest(t, gateway, continuation)
	secondEvents := readResponseEvents(t, second.Body)
	second.Body.Close()
	if len(secondEvents) == 0 || secondEvents[len(secondEvents)-1]["type"] != "response.completed" {
		t.Fatalf("custom continuation response = %#v", secondEvents)
	}
	if len(providerRequests) != 2 {
		t.Fatalf("provider requests = %d", len(providerRequests))
	}
}

func TestResponsesRejectsApplyPatchSyntheticNameCollisions(t *testing.T) {
	for _, name := range []string{opencodego.ApplyPatchToolName, opencodego.ApplyPatchUpstreamName} {
		t.Run(name, func(t *testing.T) {
			called := false
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
				called = true
				return nil, errors.New("provider must not be called")
			}), nil)
			body := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"function","name":"` + name + `","parameters":{"type":"object"}}],"stream":true}`
			response := postRequest(t, gateway, body)
			defer response.Body.Close()
			responseBody := readBody(t, response.Body)
			if response.StatusCode != http.StatusBadRequest || !strings.Contains(responseBody, `"code":"invalid_request"`) || strings.Contains(responseBody, name) {
				t.Fatalf("collision response = %d %s", response.StatusCode, responseBody)
			}
			if called {
				t.Fatal("provider was called for a synthetic-name collision")
			}
		})
	}
}

func TestResponsesMapsMalformedApplyPatchWrappersToSafeFailures(t *testing.T) {
	for name, arguments := range map[string]string{
		"missing":    `{"other":"patch"}`,
		"non-string": `{"input":42}`,
		"trailing":   `{"input":"patch"} {}`,
		"malformed":  `{"input":`,
	} {
		t.Run(name, func(t *testing.T) {
			gateway := newIntegrationGateway(t, staticUpstream(providerToolStream(t, opencodego.ApplyPatchUpstreamName, "provider-malformed-call", arguments)), nil)
			response := postTextRequest(t, gateway)
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			rawResponse := readBody(t, response.Body)
			events := readResponseEvents(t, strings.NewReader(rawResponse))
			if len(events) == 0 || events[len(events)-1]["type"] != "response.failed" {
				t.Fatalf("events = %#v", events)
			}
			terminalResponse := events[len(events)-1]["response"].(map[string]any)
			responseError := terminalResponse["error"].(map[string]any)
			if responseError["code"] != "upstream_custom_tool_invalid" || strings.Contains(rawResponse, opencodego.ApplyPatchUpstreamName) || strings.Contains(rawResponse, `"input":42`) || strings.Contains(rawResponse, `"other":"patch"`) || strings.Contains(rawResponse, `"input":"patch"} {`) {
				t.Fatalf("malformed custom response = %s", rawResponse)
			}
		})
	}
}

func TestResponsesFailsWhenProviderReturnsUndeclaredFunctionTool(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"provider-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"other_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	gateway := newIntegrationGateway(t, staticUpstream(stream), nil)
	requestBody := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
	response := postRequest(t, gateway, requestBody)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	events := readResponseEvents(t, response.Body)
	if len(events) == 0 || events[len(events)-1]["type"] != "response.failed" {
		t.Fatalf("events = %#v", events)
	}
	for _, event := range events {
		if event["type"] == "response.completed" {
			t.Fatal("undeclared provider tool produced a successful response")
		}
		if event["type"] != "response.output_item.done" {
			continue
		}
		item, _ := event["item"].(map[string]any)
		if item["type"] == "function_call" && item["status"] == "completed" {
			t.Fatalf("undeclared provider tool produced a completed function item: %#v", item)
		}
	}
	terminalResponse := events[len(events)-1]["response"].(map[string]any)
	responseError := terminalResponse["error"].(map[string]any)
	if responseError["code"] != "upstream_tool_not_declared" {
		t.Fatalf("provider tool failure = %#v", responseError)
	}
}

func TestResponsesRejectsToolBearingRequestsBeforeCallingUpstream(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "prior tool call", body: `{"model":"deepseek-v4-flash (go)","input":[{"type":"function_call","call_id":"secret-call","name":"secret_tool","arguments":"{}"}],"stream":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
				called = true
				return nil, errors.New("upstream must not be called")
			}), nil)

			response := postRequest(t, gateway, test.body)
			defer response.Body.Close()
			if response.StatusCode != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501", response.StatusCode)
			}
			if got := readBody(t, response.Body); !strings.Contains(got, `"feature_not_implemented"`) || strings.Contains(got, "secret_tool") {
				t.Fatalf("tool rejection = %s", got)
			}
			if called {
				t.Fatal("upstream was called for a tool-bearing request")
			}
		})
	}
}

func TestResponsesRejectsForcedAndNamedToolChoicesExplicitly(t *testing.T) {
	for _, choice := range []string{
		`"required"`,
		`{"type":"function","name":"lookup"}`,
	} {
		t.Run(choice, func(t *testing.T) {
			called := false
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
				called = true
				return nil, errors.New("upstream must not be called")
			}), nil)
			body := `{"model":"deepseek-v4-flash (go)","input":[{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":` + choice + `,"stream":true}`
			response := postRequest(t, gateway, body)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			if got := readBody(t, response.Body); !strings.Contains(got, `"invalid_request"`) || strings.Contains(got, "lookup") {
				t.Fatalf("choice rejection = %s", got)
			}
			if called {
				t.Fatal("upstream was called for a forced/named tool choice")
			}
		})
	}
}

func TestResponsesMapsPreStreamProviderFailuresToJSON(t *testing.T) {
	for _, test := range []struct {
		name           string
		status         int
		wantStatus     int
		wantType       string
		wantCode       string
		retryAfter     string
		wantRetryAfter string
	}{
		{name: "bad request", status: http.StatusBadRequest, wantStatus: http.StatusBadRequest, wantType: "provider_bad_request", wantCode: "upstream_bad_request"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantStatus: http.StatusBadGateway, wantType: "authentication_error", wantCode: "upstream_unauthorized"},
		{name: "forbidden", status: http.StatusForbidden, wantStatus: http.StatusBadGateway, wantType: "permission_error", wantCode: "upstream_forbidden"},
		{name: "rate limited", status: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", wantCode: "upstream_rate_limited", retryAfter: "17", wantRetryAfter: "17"},
		{name: "unsafe retry after", status: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", wantCode: "upstream_rate_limited", retryAfter: "provider-secret\n", wantRetryAfter: ""},
		{name: "timeout", status: http.StatusGatewayTimeout, wantStatus: http.StatusGatewayTimeout, wantType: "timeout", wantCode: "upstream_timeout"},
		{name: "server", status: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantType: "provider_unavailable", wantCode: "upstream_server_error"},
		{name: "unexpected", status: http.StatusTeapot, wantStatus: http.StatusBadGateway, wantType: "provider_protocol_error", wantCode: "upstream_unexpected_status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(`{"provider":"secret payload"}`)}
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
				header := http.Header{"Content-Type": []string{"application/json"}}
				if test.retryAfter != "" {
					header.Set("Retry-After", test.retryAfter)
				}
				return &UpstreamResponse{StatusCode: test.status, Header: header, Body: body}, nil
			}), nil)
			response := postTextRequest(t, gateway)
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			got := readBody(t, response.Body)
			if !strings.Contains(got, `"type":"`+test.wantType+`"`) || !strings.Contains(got, `"code":"`+test.wantCode+`"`) || strings.Contains(got, "secret payload") {
				t.Fatalf("provider error body = %s", got)
			}
			if gotRetryAfter := response.Header.Get("Retry-After"); gotRetryAfter != test.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", gotRetryAfter, test.wantRetryAfter)
			}
			if !body.wasClosed() {
				t.Fatal("pre-stream provider body was not closed")
			}
		})
	}

	t.Run("unsupported content type", func(t *testing.T) {
		body := &trackedBody{Reader: strings.NewReader(`{"provider":"secret payload"}`)}
		gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
			return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
		}), nil)
		response := postTextRequest(t, gateway)
		defer response.Body.Close()
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", response.StatusCode)
		}
		if got := readBody(t, response.Body); !strings.Contains(got, `"upstream_unsupported_content_type"`) || strings.Contains(got, "secret payload") {
			t.Fatalf("content-type failure = %s", got)
		}
		if !body.wasClosed() {
			t.Fatal("unsupported content-type body was not closed")
		}
	})
}

func TestResponsesMapsPreStreamTimeoutAndNetworkErrorsToSafeTaxonomy(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		wantType string
		wantCode string
	}{
		{name: "timeout", err: context.DeadlineExceeded, wantType: "timeout", wantCode: "upstream_timeout"},
		{name: "network", err: errors.New("private transport detail"), wantType: "provider_unavailable", wantCode: "upstream_network_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
				return nil, test.err
			}), nil)
			response := postTextRequest(t, gateway)
			defer response.Body.Close()
			body := readBody(t, response.Body)
			if !strings.Contains(body, `"type":"`+test.wantType+`"`) || !strings.Contains(body, `"code":"`+test.wantCode+`"`) || strings.Contains(body, "private transport detail") {
				t.Fatalf("safe upstream error = %s", body)
			}
		})
	}
}

func TestResponsesEmitsOneTimeoutFailureAfterSSEStarts(t *testing.T) {
	body := &blockingBody{closed: make(chan struct{})}
	gateway := newIntegrationGatewayWithConfig(t, Config{ListenAddr: "127.0.0.1:0", StreamIdleTimeout: 100 * time.Millisecond}, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}), nil)
	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	events := readResponseEvents(t, response.Body)
	if countResponseTerminals(events) != 1 || events[len(events)-1]["type"] != "response.failed" {
		t.Fatalf("timeout events = %#v", events)
	}
	errorObject := events[len(events)-1]["response"].(map[string]any)["error"].(map[string]any)
	if errorObject["type"] != "timeout" || errorObject["code"] != "timeout" {
		t.Fatalf("timeout error = %#v", errorObject)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("stream idle timeout did not close the upstream body")
	}
}

func TestResponsesCancellationBeforeHeadersClosesBodyWithoutFallbackJSON(t *testing.T) {
	started := make(chan struct{})
	body := &trackedBody{Reader: strings.NewReader("private provider body")}
	server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, UpstreamClientFunc(func(ctx context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		close(started)
		<-ctx.Done()
		return &UpstreamResponse{Body: body}, ctx.Err()
	}), nil)

	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(textRequestBody())).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled pre-header handler did not finish")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("canceled pre-header response wrote fallback bytes: %s", recorder.Body.String())
	}
	if !body.wasClosed() {
		t.Fatal("canceled pre-header response body was not closed")
	}
}

func TestServerShutdownBeforeHeadersDoesNotWriteFallbackJSON(t *testing.T) {
	started := make(chan struct{})
	upstream := UpstreamClientFunc(func(ctx context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	server, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: upstream}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://"+server.Addr()+"/v1/responses", strings.NewReader(textRequestBody()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	type requestOutcome struct {
		err  error
		body []byte
	}
	requestResult := make(chan requestOutcome, 1)
	go func() {
		response, requestErr := client.Do(request)
		if response != nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if requestErr == nil {
				requestErr = readErr
			}
			requestResult <- requestOutcome{err: requestErr, body: body}
			return
		}
		requestResult <- requestOutcome{err: requestErr}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	shutdownErr := server.Shutdown(shutdownContext)
	cancel()
	if shutdownErr != nil {
		t.Fatalf("shutdown error = %v, want graceful completion after immediate cancellation", shutdownErr)
	}
	select {
	case outcome := <-requestResult:
		if outcome.err == nil && len(outcome.body) != 0 {
			// net/http may synthesize an empty 200 when a handler returns without
			// writing headers; the application must still never emit fallback JSON.
			t.Fatalf("pre-header shutdown returned fallback bytes: %s", outcome.body)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-header client request did not finish")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve error after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not exit after shutdown")
	}
}

func TestServerShutdownMidstreamDoesNotWriteFallbackOrTerminalEvents(t *testing.T) {
	first := `data: {"id":"provider","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"
	body := &blockingBody{first: []byte(first), closed: make(chan struct{}), contextCanceledC: make(chan struct{})}
	upstream := UpstreamClientFunc(func(ctx context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		go func() {
			<-ctx.Done()
			body.contextCanceled()
		}()
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	})
	server, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: upstream}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Post("http://"+server.Addr()+"/v1/responses", "application/json", strings.NewReader(textRequestBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	shutdownErr := server.Shutdown(shutdownContext)
	cancel()
	if shutdownErr != nil {
		t.Fatalf("shutdown error = %v, want graceful completion after immediate cancellation", shutdownErr)
	}
	remaining, readErr := io.ReadAll(response.Body)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, net.ErrClosed) {
		t.Fatal(readErr)
	}
	events := readResponseEvents(t, bytes.NewReader(remaining))
	if countResponseTerminals(events) != 0 {
		t.Fatalf("shutdown emitted a terminal event: %#v", events)
	}
	for _, event := range events {
		if event["type"] == "response.failed" {
			t.Fatalf("shutdown emitted response.failed: %#v", event)
		}
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not close the upstream body")
	}
	select {
	case <-body.contextCanceledC:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the upstream context")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve error after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not exit after shutdown")
	}
}

func TestServerRejectsActiveRequestOverflowWithRetryAfter(t *testing.T) {
	entered := make(chan struct{})
	body := &blockingBody{closed: make(chan struct{})}
	server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0", MaxActiveRequests: 1}, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		close(entered)
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}), nil)

	firstRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(textRequestBody()))
	firstRequest.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		server.ServeHTTP(firstRecorder, firstRequest)
		close(firstDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first active request did not reach upstream")
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(textRequestBody()))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	server.ServeHTTP(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusTooManyRequests || secondRecorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("active limit response = %d headers=%v body=%s", secondRecorder.Code, secondRecorder.Header(), secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), `"type":"rate_limit_error"`) || !strings.Contains(secondRecorder.Body.String(), `"code":"active_request_limit"`) {
		t.Fatalf("active limit error = %s", secondRecorder.Body.String())
	}
	body.Close()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first active request did not release after body close")
	}
	server.activeRequestsMu.Lock()
	active := len(server.activeRequests)
	server.activeRequestsMu.Unlock()
	if active != 0 {
		t.Fatalf("active request registry = %d, want 0", active)
	}
}

func TestServerRejectsNonLoopbackHostAndOriginWithoutCORS(t *testing.T) {
	server := newTestServer(t, nil)
	tests := []struct {
		name   string
		host   string
		origin string
	}{
		{name: "host", host: "attacker.example"},
		{name: "origin", host: "127.0.0.1", origin: "https://attacker.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/health/live", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden || recorder.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("security response = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `"type":"permission_error"`) || !strings.Contains(recorder.Body.String(), `"code":"permission_error"`) {
				t.Fatalf("security error = %s", recorder.Body.String())
			}
		})
	}
}

func TestResponsesRepeatedCancellationReleasesBodiesAndRegistry(t *testing.T) {
	const iterations = 5
	entered := make(chan struct{}, iterations)
	bodies := make([]*blockingBody, 0, iterations)
	var bodyMu sync.Mutex
	upstream := UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		body := &blockingBody{closed: make(chan struct{})}
		bodyMu.Lock()
		bodies = append(bodies, body)
		bodyMu.Unlock()
		entered <- struct{}{}
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	})
	server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, upstream, nil)
	for index := 0; index < iterations; index++ {
		requestContext, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(textRequestBody())).WithContext(requestContext)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			server.ServeHTTP(recorder, request)
			close(done)
		}()
		select {
		case <-entered:
			cancel()
		case <-time.After(time.Second):
			t.Fatalf("iteration %d did not reach upstream", index)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d did not finish after cancellation", index)
		}
		cancel()
	}
	bodyMu.Lock()
	completedBodies := append([]*blockingBody(nil), bodies...)
	bodyMu.Unlock()
	if len(completedBodies) != iterations {
		t.Fatalf("upstream body count = %d, want %d", len(completedBodies), iterations)
	}
	for index, body := range completedBodies {
		select {
		case <-body.closed:
		default:
			t.Fatalf("iteration %d body remained open", index)
		}
	}
	server.activeRequestsMu.Lock()
	active := len(server.activeRequests)
	server.activeRequestsMu.Unlock()
	if active != 0 {
		t.Fatalf("active request registry = %d, want 0", active)
	}
}

func TestResponsesAppliesCodexBoundaryBeforeCallingUpstream(t *testing.T) {
	called := false
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		called = true
		return nil, errors.New("upstream must not be called")
	}), nil)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "trailing JSON", body: textRequestBody() + `{}`},
		{name: "invalid JSON", body: `{"model":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusBadRequest {
				response.Body.Close()
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
			if got := readBody(t, response.Body); !strings.Contains(got, `"invalid_request"`) && !strings.Contains(got, `"malformed_json"`) {
				t.Fatalf("boundary error = %s", got)
			}
			response.Body.Close()
		})
	}
	if called {
		t.Fatal("upstream was called before request boundary validation")
	}
}

func TestResponsesRejectsImplicitProviderToolOverflowBeforeUpstream(t *testing.T) {
	called := false
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		called = true
		return nil, &UpstreamError{Code: upstreamErrorServer}
	}), nil)

	tools := make([]map[string]any, bridge.DefaultMaxFunctionTools)
	for index := range tools {
		tools[index] = map[string]any{
			"type":       "function",
			"name":       "tool_" + strconv.Itoa(index),
			"parameters": map[string]any{"type": "object"},
		}
	}
	body, err := json.Marshal(map[string]any{
		"model":  "deepseek-v4-flash (go)",
		"input":  []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
		"tools":  tools,
		"stream": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := postRequest(t, gateway, string(body))
	defer response.Body.Close()
	responseBody := readBody(t, response.Body)
	if response.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(responseBody, `"code":"request_too_large"`) {
		t.Fatalf("provider tool overflow response = %d %s", response.StatusCode, responseBody)
	}
	if called {
		t.Fatal("upstream was called after the implicit provider-tool budget was exceeded")
	}
}

func TestResponsesChargesImplicitApplyPatchSchemaAtExactAggregateBoundary(t *testing.T) {
	const wrapperName = "lookup"
	wrapperBytes := int(opencodego.ApplyPatchWrapperSchemaBytes())
	for _, test := range []struct {
		name       string
		schemaSize int
		wantStatus int
		wantCalled bool
	}{
		{name: "exact total", schemaSize: bridge.DefaultMaxFunctionSchemaBytes - wrapperBytes, wantStatus: http.StatusBadGateway, wantCalled: true},
		{name: "one byte over", schemaSize: bridge.DefaultMaxFunctionSchemaBytes - wrapperBytes + 1, wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
				called = true
				return nil, &UpstreamError{Code: upstreamErrorServer}
			}), nil)
			const prefix = `{"type":"object","description":"`
			const suffix = `"}`
			schema := prefix + strings.Repeat("x", test.schemaSize-len(prefix)-len(suffix)) + suffix
			body, err := json.Marshal(map[string]any{
				"model":  "deepseek-v4-flash (go)",
				"input":  []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
				"tools":  []map[string]any{{"type": "function", "name": wrapperName, "parameters": json.RawMessage(schema)}},
				"stream": true,
			})
			if err != nil {
				t.Fatal(err)
			}
			response := postRequest(t, gateway, string(body))
			defer response.Body.Close()
			responseBody := readBody(t, response.Body)
			if response.StatusCode != test.wantStatus {
				t.Fatalf("schema boundary response = %d want %d body=%s", response.StatusCode, test.wantStatus, responseBody)
			}
			if called != test.wantCalled {
				t.Fatalf("upstream called = %t want %t", called, test.wantCalled)
			}
		})
	}
}

func TestResponsesEmitsExactlyOneFailureAfterValidDeltasOrTruncatedEOF(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		forbidden string
	}{
		{name: "malformed json", body: "data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" + "data: {malformed}\n\n"},
		{name: "provider error", body: "data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" + "data: {\"error\":{\"message\":\"provider-secret-payload\"}}\n\n", forbidden: "provider-secret-payload"},
		{name: "eof before done", body: "data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := newIntegrationGateway(t, staticUpstream(test.body), nil)
			response := postTextRequest(t, gateway)
			defer response.Body.Close()
			events := readResponseEvents(t, response.Body)
			if countResponseTerminals(events) != 1 || events[len(events)-1]["type"] != "response.failed" {
				t.Fatalf("events = %#v", events)
			}
			if !strings.Contains(string(mustJSON(t, events)), "partial") {
				t.Fatal("valid text delta was not preserved before failure")
			}
			if test.forbidden != "" && strings.Contains(string(mustJSON(t, events)), test.forbidden) {
				t.Fatal("raw provider error payload was exposed downstream")
			}
		})
	}
}

func TestResponsesMapsProviderLengthToIncomplete(t *testing.T) {
	stream := "data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	gateway := newIntegrationGateway(t, staticUpstream(stream), nil)
	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	events := readResponseEvents(t, response.Body)
	if countResponseTerminals(events) != 1 || events[len(events)-1]["type"] != "response.incomplete" {
		t.Fatalf("events = %#v", events)
	}
	terminalResponse := events[len(events)-1]["response"].(map[string]any)
	if terminalResponse["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete details = %#v", terminalResponse["incomplete_details"])
	}
}

func TestResponsesPreservesUnicodeAcrossProviderReadFragments(t *testing.T) {
	stream := "data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Olá 世界\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &chunkedBody{data: []byte(stream), chunkSize: 1},
		}, nil
	}), nil)
	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	events := readResponseEvents(t, response.Body)
	var output string
	for _, event := range events {
		if event["type"] == "response.output_text.delta" {
			output += event["delta"].(string)
		}
	}
	if output != "Olá 世界" {
		t.Fatalf("fragmented unicode output = %q", output)
	}
}

func TestResponsesForwardsFirstSSEBytesBeforeUpstreamCompletes(t *testing.T) {
	body := newGatedBody(
		[]byte("data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n\n"),
		[]byte("data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n"),
	)
	defer body.releaseRest()
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	}), nil)

	response := postTextRequest(t, gateway)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	firstLines := make(chan []string, 1)
	readErr := make(chan error, 1)
	go func() {
		lines := make([]string, 0)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				readErr <- err
				return
			}
			lines = append(lines, line)
			if strings.Contains(line, `"response.output_text.delta"`) {
				firstLines <- lines
				return
			}
		}
	}()

	var received []string
	select {
	case received = <-firstLines:
	case err := <-readErr:
		t.Fatalf("reading first downstream event: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("first downstream delta was buffered until upstream completion")
	}
	if !strings.Contains(strings.Join(received, ""), `"delta":"first"`) {
		t.Fatalf("first downstream bytes = %q", strings.Join(received, ""))
	}

	body.releaseRest()
	remaining := readResponseEvents(t, reader)
	if len(remaining) == 0 || remaining[len(remaining)-1]["type"] != "response.completed" {
		t.Fatalf("remaining downstream events = %#v", remaining)
	}
}

func TestResponsesClosesUpstreamBodyWhenDownstreamFlushFails(t *testing.T) {
	body := &blockingBody{
		first:  []byte("data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n\n"),
		closed: make(chan struct{}),
	}
	server := newTestServerWithConfigAndUpstream(t, Config{ListenAddr: "127.0.0.1:0"}, UpstreamClientFunc(func(context.Context, bridge.Request) (*UpstreamResponse, error) {
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	}), nil)
	writer := &flushFailWriter{header: make(http.Header)}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(textRequestBody()))
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(writer, request)
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream body was not closed after downstream flush failure")
	}
	if writer.status != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200", writer.status)
	}
}

func TestResponsesClosesUpstreamBodyOnClientCancellation(t *testing.T) {
	body := &blockingBody{
		first:            []byte("data: {\"id\":\"id\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"),
		closed:           make(chan struct{}),
		contextCanceledC: make(chan struct{}),
	}
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(ctx context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		go func() {
			<-ctx.Done()
			body.contextCanceled()
		}()
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}), nil)

	client := &http.Client{Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(textRequestBody()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(line, `"response.output_text.delta"`) {
			break
		}
	}
	_ = response.Body.Close()
	select {
	case <-body.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream body was not closed after downstream cancellation")
	}
	select {
	case <-body.contextCanceledC:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream context was not canceled after downstream cancellation")
	}
}

func TestServerShutdownCancelsBlockedResponseImmediately(t *testing.T) {
	body := &blockingBody{
		closed:           make(chan struct{}),
		contextCanceledC: make(chan struct{}),
	}
	upstream := UpstreamClientFunc(func(ctx context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		go func() {
			<-ctx.Done()
			body.contextCanceled()
		}()
		return &UpstreamResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	})
	server, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: upstream}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Post("http://"+server.Addr()+"/v1/responses", "application/json", strings.NewReader(textRequestBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for lifecycleEvents := 0; lifecycleEvents < 2; {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("reading lifecycle event: %v", readErr)
		}
		if strings.HasPrefix(line, "data: ") {
			lifecycleEvents++
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown error = %v, want graceful completion after immediate cancellation", err)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not close the blocked upstream body")
	}
	select {
	case <-body.contextCanceledC:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the upstream request context")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve error after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP serve loop did not exit after shutdown")
	}
}

func TestResponsesDoesNotLogSecretsOnFailure(t *testing.T) {
	var logs bytes.Buffer
	gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		return nil, &UpstreamError{Code: upstreamErrorServer}
	}), slog.New(slog.NewTextHandler(&logs, nil)))
	requestBody := `{"model":"deepseek-v4-flash (go)","instructions":"instruction-secret","input":[{"type":"message","role":"user","content":"prompt-secret"}],"stream":true}`
	response := postRequest(t, gateway, requestBody)
	defer response.Body.Close()
	_ = readBody(t, response.Body)
	if strings.Contains(logs.String(), "instruction-secret") || strings.Contains(logs.String(), "prompt-secret") {
		t.Fatalf("logs exposed request secrets: %s", logs.String())
	}
}

func newIntegrationGateway(t *testing.T, client UpstreamClient, logger *slog.Logger) *httptest.Server {
	t.Helper()
	return newIntegrationGatewayWithConfig(t, Config{ListenAddr: "127.0.0.1:0"}, client, logger)
}

func newIntegrationGatewayWithConfig(t *testing.T, config Config, client UpstreamClient, logger *slog.Logger) *httptest.Server {
	t.Helper()
	config.Upstream = client
	server, err := New(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func newTestServerWithConfigAndUpstream(t *testing.T, config Config, client UpstreamClient, logger *slog.Logger) *Server {
	t.Helper()
	config.Upstream = client
	server, err := New(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func staticUpstream(stream string) UpstreamClient {
	return UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
		return &UpstreamResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
}

func postTextRequest(t *testing.T, gateway *httptest.Server) *http.Response {
	t.Helper()
	return postRequest(t, gateway, textRequestBody())
}

func postRequest(t *testing.T, gateway *httptest.Server, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func textRequestBody() string {
	return `{"model":"deepseek-v4-flash (go)","instructions":"system instruction","input":[{"type":"message","role":"assistant","content":[{"type":"input_text","text":"prior answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"reasoning":{"effort":"medium"},"stream":true}`
}

func readResponseEvents(t *testing.T, body io.Reader) []map[string]any {
	t.Helper()
	decoder := bufio.NewScanner(body)
	decoder.Buffer(make([]byte, 1024), 4<<20)
	result := make([]map[string]any, 0)
	for decoder.Scan() {
		line := decoder.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("invalid response SSE event %q: %v", line, err)
		}
		result = append(result, event)
	}
	if err := decoder.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func responseEventTypes(events []map[string]any) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event["type"].(string))
	}
	return result
}

func countResponseTerminals(events []map[string]any) int {
	count := 0
	for _, event := range events {
		switch event["type"] {
		case "response.completed", "response.incomplete", "response.failed":
			count++
		}
	}
	return count
}

func readBody(t *testing.T, body io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func checkedFixtureRequestBody(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../../testdata/codex/requests/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request struct {
			Body json.RawMessage `json:"body"`
		} `json:"request"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Request.Body) == 0 {
		t.Fatalf("fixture %q has no request body", name)
	}
	return string(fixture.Request.Body)
}

func providerToolStream(t *testing.T, name, callID string, fragments ...string) string {
	t.Helper()
	toolIndex := 0
	chunks := make([]opencodego.ChatCompletionChunk, 0, len(fragments)+2)
	for index, fragment := range fragments {
		tool := opencodego.ToolCall{
			Index: &toolIndex,
			Function: opencodego.ToolCallFunction{
				Arguments: fragment,
			},
		}
		if index == 0 {
			tool.ID = callID
			tool.Type = "function"
			tool.Function.Name = name
		}
		chunks = append(chunks, opencodego.ChatCompletionChunk{
			ID: "provider-stream",
			Choices: []opencodego.ChatCompletionChunkChoice{{
				Index: 0,
				Delta: opencodego.ChatMessage{ToolCalls: []opencodego.ToolCall{tool}},
			}},
			Created: 1700000000,
			Model:   "deepseek-v4-flash",
		})
	}
	finishReason := "tool_calls"
	chunks = append(chunks, opencodego.ChatCompletionChunk{
		ID: "provider-stream",
		Choices: []opencodego.ChatCompletionChunkChoice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
		Created: 1700000000,
		Model:   "deepseek-v4-flash",
	})
	var builder strings.Builder
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		builder.WriteString("data: ")
		builder.Write(encoded)
		builder.WriteString("\n\n")
	}
	builder.WriteString("data: [DONE]\n\n")
	return builder.String()
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type trackedBody struct {
	io.Reader
	mu     sync.Mutex
	closed bool
}

func (body *trackedBody) Close() error {
	body.mu.Lock()
	body.closed = true
	body.mu.Unlock()
	return nil
}

func (body *trackedBody) wasClosed() bool {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closed
}

type blockingBody struct {
	first            []byte
	closed           chan struct{}
	closeOnce        sync.Once
	contextOnce      sync.Once
	contextCanceledC chan struct{}
	mu               sync.Mutex
}

type gatedBody struct {
	first       []byte
	rest        []byte
	release     chan struct{}
	closed      chan struct{}
	releaseOnce sync.Once
	closeOnce   sync.Once
	mu          sync.Mutex
	released    bool
}

func newGatedBody(first, rest []byte) *gatedBody {
	return &gatedBody{
		first:   first,
		rest:    rest,
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (body *gatedBody) Read(target []byte) (int, error) {
	for {
		body.mu.Lock()
		if len(body.first) > 0 {
			n := copy(target, body.first)
			body.first = body.first[n:]
			body.mu.Unlock()
			return n, nil
		}
		if body.released {
			if len(body.rest) == 0 {
				body.mu.Unlock()
				return 0, io.EOF
			}
			n := copy(target, body.rest)
			body.rest = body.rest[n:]
			body.mu.Unlock()
			return n, nil
		}
		body.mu.Unlock()

		select {
		case <-body.release:
			body.mu.Lock()
			body.released = true
			body.mu.Unlock()
		case <-body.closed:
			return 0, io.EOF
		}
	}
}

func (body *gatedBody) releaseRest() {
	body.releaseOnce.Do(func() { close(body.release) })
}

func (body *gatedBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}

type chunkedBody struct {
	data      []byte
	chunkSize int
	position  int
}

func (body *chunkedBody) Read(target []byte) (int, error) {
	if body.position == len(body.data) {
		return 0, io.EOF
	}
	size := body.chunkSize
	if size <= 0 || size > len(target) {
		size = len(target)
	}
	if remaining := len(body.data) - body.position; size > remaining {
		size = remaining
	}
	n := copy(target[:size], body.data[body.position:body.position+size])
	body.position += n
	return n, nil
}

func (body *chunkedBody) Close() error { return nil }

type flushFailWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (writer *flushFailWriter) Header() http.Header { return writer.header }

func (writer *flushFailWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *flushFailWriter) Write(payload []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return 0, errors.New("downstream write failed")
}

func (writer *flushFailWriter) FlushError() error { return errors.New("downstream flush failed") }

func (writer *flushFailWriter) Flush() {}

func (body *blockingBody) Read(target []byte) (int, error) {
	body.mu.Lock()
	if len(body.first) > 0 {
		n := copy(target, body.first)
		body.first = body.first[n:]
		body.mu.Unlock()
		return n, nil
	}
	body.mu.Unlock()
	<-body.closed
	return 0, io.EOF
}

func (body *blockingBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}

func (body *blockingBody) contextCanceled() {
	body.contextOnce.Do(func() {
		if body.contextCanceledC != nil {
			close(body.contextCanceledC)
		}
	})
}
