package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	if gotRequest.Model != "gpt-5.3-codex" || !gotRequest.Generation.Stream || gotRequest.Instructions != "system instruction" {
		t.Fatalf("bridge request = %#v", gotRequest)
	}
	if len(gotRequest.Input) != 2 {
		t.Fatalf("bridge input count = %d", len(gotRequest.Input))
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
		if !ok || len(tools) != 2 {
			http.Error(writer, "function tools missing", http.StatusBadRequest)
			return
		}
		seenNames := map[string]bool{}
		for _, rawTool := range tools {
			tool := rawTool.(map[string]any)
			function := tool["function"].(map[string]any)
			name, _ := function["name"].(string)
			seenNames[name] = true
			if tool["type"] != "function" || (name != "lookup" && name != "other_tool") {
				http.Error(writer, "function tool changed", http.StatusBadRequest)
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
		if !seenNames["lookup"] || !seenNames["other_tool"] {
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
	requestBody := `{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":"look up"}],"tools":[{"type":"function","name":"lookup","description":"look up a value","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"strict":true},{"type":"function","name":"other_tool","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":true,"stream":true}`
	response := postRequest(t, gateway, requestBody)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, readBody(t, response.Body))
	}
	events := readResponseEvents(t, response.Body)
	if got := responseEventTypes(events); !equalStrings(got, []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.output_text.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
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

func TestResponsesFailsWhenProviderReturnsUndeclaredFunctionTool(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"provider-id","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"other_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	gateway := newIntegrationGateway(t, staticUpstream(stream), nil)
	requestBody := `{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":"use lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`
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
		{name: "deferred tool definition", body: `{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"namespace","name":"secret_tool"}],"stream":true}`},
		{name: "prior tool call", body: `{"model":"gpt-5.3-codex","input":[{"type":"function_call","call_id":"secret-call","name":"secret_tool","arguments":"{}"}],"stream":true}`},
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
			body := `{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":` + choice + `,"stream":true}`
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
		name       string
		status     int
		wantStatus int
		wantCode   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantStatus: http.StatusBadGateway, wantCode: "upstream_unauthorized"},
		{name: "rate limited", status: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests, wantCode: "upstream_rate_limited"},
		{name: "server", status: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantCode: "upstream_server_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(`{"provider":"secret payload"}`)}
			gateway := newIntegrationGateway(t, UpstreamClientFunc(func(_ context.Context, _ bridge.Request) (*UpstreamResponse, error) {
				return &UpstreamResponse{StatusCode: test.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
			}), nil)
			response := postTextRequest(t, gateway)
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if got := readBody(t, response.Body); !strings.Contains(got, `"`+test.wantCode+`"`) || strings.Contains(got, "secret payload") {
				t.Fatalf("provider error body = %s", got)
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

func TestServerShutdownCancelsBlockedResponseWithinGracePeriod(t *testing.T) {
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

	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown did not complete within its grace period: %v", err)
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
	requestBody := `{"model":"gpt-5.3-codex","instructions":"instruction-secret","input":[{"type":"message","role":"user","content":"prompt-secret"}],"stream":true}`
	response := postRequest(t, gateway, requestBody)
	defer response.Body.Close()
	_ = readBody(t, response.Body)
	if strings.Contains(logs.String(), "instruction-secret") || strings.Contains(logs.String(), "prompt-secret") {
		t.Fatalf("logs exposed request secrets: %s", logs.String())
	}
}

func newIntegrationGateway(t *testing.T, client UpstreamClient, logger *slog.Logger) *httptest.Server {
	t.Helper()
	server, err := New(Config{ListenAddr: "127.0.0.1:0", Upstream: client}, logger)
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
	return `{"model":"gpt-5.3-codex","instructions":"system instruction","input":[{"type":"message","role":"assistant","content":[{"type":"input_text","text":"prior answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"reasoning":{"effort":"medium"},"stream":true}`
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
