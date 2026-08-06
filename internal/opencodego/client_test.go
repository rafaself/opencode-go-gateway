package opencodego

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestMapRequestPreservesDomainOrderingAndProviderShape(t *testing.T) {
	schema, err := bridge.NewJSONSchema([]byte(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}

	request := bridge.Request{
		Instructions: "gateway instructions",
		Input: []bridge.InputItem{
			bridge.Message{Role: bridge.RoleDeveloper, Content: []bridge.ContentPart{bridge.TextContent{Text: "developer policy"}}},
			bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "user question"}}},
			bridge.FunctionCall{ID: "item-id", CallID: "call-id", Name: "exec_command", Arguments: `{"cmd":"pwd"}`},
			bridge.FunctionCallOutput{CallID: "call-id", Output: "result"},
		},
		Tools: []bridge.Tool{
			bridge.FunctionTool{Name: "exec_command", Description: "run a command", Parameters: schema},
		},
		ToolChoice: bridge.ToolChoice{Kind: bridge.ToolChoiceAuto},
		Generation: bridge.GenerationOptions{
			Stream:            true,
			ParallelToolCalls: true,
			Reasoning:         bridge.ReasoningOptions{Effort: "medium"},
		},
	}

	got, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != DefaultModel || !got.Stream || !got.ParallelToolCalls {
		t.Fatalf("request metadata = %#v", got)
	}
	if got.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q, want high", got.ReasoningEffort)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" {
		t.Fatalf("thinking options = %#v", got.Thinking)
	}
	if got.ToolChoice != nil {
		t.Fatalf("auto tool choice must be omitted in thinking mode: %#v", got.ToolChoice)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("message count = %d, want 5", len(got.Messages))
	}
	wantRoles := []string{"system", "system", "user", "assistant", "tool"}
	for index, want := range wantRoles {
		if got.Messages[index].Role != want {
			t.Fatalf("message[%d].role = %q, want %q", index, got.Messages[index].Role, want)
		}
	}
	if got.Messages[0].Content == nil || *got.Messages[0].Content != "gateway instructions" {
		t.Fatalf("instructions message = %#v", got.Messages[0])
	}
	if got.Messages[1].Content == nil || *got.Messages[1].Content != "developer policy" {
		t.Fatalf("developer message = %#v", got.Messages[1])
	}
	if len(got.Messages[3].ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %#v", got.Messages[3].ToolCalls)
	}
	call := got.Messages[3].ToolCalls[0]
	if call.ID != "call-id" || call.Type != "function" || call.Function.Name != "exec_command" || call.Function.Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("assistant tool call = %#v", call)
	}
	if got.Messages[4].ToolCallID != "call-id" || got.Messages[4].Content == nil || *got.Messages[4].Content != "result" {
		t.Fatalf("tool result = %#v", got.Messages[4])
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "exec_command" {
		t.Fatalf("tools = %#v", got.Tools)
	}
	if !bytes.Equal(got.Tools[0].Function.Parameters, schema.RawJSON()) {
		t.Fatalf("tool schema changed: got %s want %s", got.Tools[0].Function.Parameters, schema.RawJSON())
	}
}

func TestMapRequestGroupsContiguousFunctionCallsAndPreservesBoundaries(t *testing.T) {
	request := bridge.Request{
		Input: []bridge.InputItem{
			bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "before"}}},
			bridge.FunctionCall{CallID: "call-1", Name: "first_tool", Arguments: `{"one":1}`},
			bridge.FunctionCall{CallID: "call-2", Name: "second_tool", Arguments: `{"two":2}`},
			bridge.FunctionCallOutput{CallID: "call-1", Output: "first result"},
			bridge.FunctionCallOutput{CallID: "call-2", Output: "second result"},
			bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "after"}}},
		},
		Generation: bridge.GenerationOptions{Stream: true, ParallelToolCalls: true},
	}

	got, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("message count = %d, want 5: %#v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content == nil || *got.Messages[0].Content != "before" {
		t.Fatalf("message before tool group = %#v", got.Messages[0])
	}
	group := got.Messages[1]
	if group.Role != "assistant" || len(group.ToolCalls) != 2 {
		t.Fatalf("parallel assistant group = %#v", group)
	}
	for index, want := range []struct {
		id, name, arguments string
	}{
		{id: "call-1", name: "first_tool", arguments: `{"one":1}`},
		{id: "call-2", name: "second_tool", arguments: `{"two":2}`},
	} {
		call := group.ToolCalls[index]
		if call.ID != want.id || call.Function.Name != want.name || call.Function.Arguments != want.arguments {
			t.Fatalf("parallel tool call[%d] = %#v, want %#v", index, call, want)
		}
	}
	if got.Messages[2].Role != "tool" || got.Messages[2].ToolCallID != "call-1" || got.Messages[2].Content == nil || *got.Messages[2].Content != "first result" {
		t.Fatalf("first tool result = %#v", got.Messages[2])
	}
	if got.Messages[3].Role != "tool" || got.Messages[3].ToolCallID != "call-2" || got.Messages[3].Content == nil || *got.Messages[3].Content != "second result" {
		t.Fatalf("second tool result = %#v", got.Messages[3])
	}
	if got.Messages[4].Role != "user" || got.Messages[4].Content == nil || *got.Messages[4].Content != "after" {
		t.Fatalf("message after tool group = %#v", got.Messages[4])
	}
}

func TestMapRequestSupportsThinkingPolicyAndRejectsForcedChoices(t *testing.T) {
	base := bridge.Request{
		Input:      []bridge.InputItem{bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "hello"}}}},
		Tools:      []bridge.Tool{bridge.FunctionTool{Name: "lookup", Parameters: mustSchema(t, `{"type":"object"}`)}},
		Generation: bridge.GenerationOptions{Stream: true},
	}

	for _, test := range []struct {
		name       string
		choice     bridge.ToolChoice
		mode       ThinkingMode
		wantChoice any
		wantError  ErrorCode
	}{
		{name: "thinking none", choice: bridge.ToolChoice{Kind: bridge.ToolChoiceNone}, mode: ThinkingEnabled, wantChoice: "none"},
		{name: "thinking required rejected", choice: bridge.ToolChoice{Kind: bridge.ToolChoiceRequired}, mode: ThinkingEnabled, wantError: ErrorUnsupportedToolChoice},
		{name: "thinking named rejected", choice: bridge.ToolChoice{Kind: bridge.ToolChoiceFunction, FunctionName: "lookup"}, mode: ThinkingEnabled, wantError: ErrorUnsupportedToolChoice},
		{name: "non-thinking auto", choice: bridge.ToolChoice{Kind: bridge.ToolChoiceAuto}, mode: ThinkingDisabled, wantChoice: "auto"},
		{name: "non-thinking required rejected", choice: bridge.ToolChoice{Kind: bridge.ToolChoiceRequired}, mode: ThinkingDisabled, wantError: ErrorUnsupportedToolChoice},
		{name: "non-thinking named rejected", choice: bridge.ToolChoice{Kind: bridge.ToolChoiceFunction, FunctionName: "lookup"}, mode: ThinkingDisabled, wantError: ErrorUnsupportedToolChoice},
		{name: "unsupported choice rejected", choice: bridge.ToolChoice{Kind: "future_choice"}, mode: ThinkingEnabled, wantError: ErrorUnsupportedToolChoice},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.ToolChoice = test.choice
			got, err := MapRequestWithThinking(request, DefaultModel, test.mode)
			if test.wantError != "" {
				assertProviderCode(t, err, test.wantError)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ToolChoice == nil {
				t.Fatal("tool choice was omitted")
			}
			var raw any
			encoded, err := json.Marshal(got.ToolChoice)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &raw); err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(test.wantChoice)
			if err != nil {
				t.Fatal(err)
			}
			var wantRaw any
			if err := json.Unmarshal(want, &wantRaw); err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(raw, wantRaw) {
				t.Fatalf("tool choice = %s, want %s", encoded, want)
			}
			if test.mode == ThinkingDisabled && got.Thinking == nil {
				t.Fatal("explicit disabled thinking option was omitted")
			}
		})
	}

	request := base
	request.Generation.Reasoning.Effort = "low"
	if _, err := MapRequestWithThinking(request, DefaultModel, ThinkingDisabled); err == nil {
		t.Fatal("reasoning effort with disabled thinking unexpectedly succeeded")
	}
}

func TestResponseWirePreservesReasoningContent(t *testing.T) {
	var response ChatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"completion-1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning_content":"provider-only reasoning","tool_calls":[]}}]}`), &response); err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].Message.ReasoningContent == nil || *response.Choices[0].Message.ReasoningContent != "provider-only reasoning" {
		t.Fatalf("reasoning content was not preserved: %#v", response.Choices[0].Message)
	}
	if response.Choices[0].Message.Content == nil || *response.Choices[0].Message.Content != "answer" {
		t.Fatalf("content was not preserved: %#v", response.Choices[0].Message)
	}
}

func TestResponseWirePreservesChoiceAndToolCallIndexes(t *testing.T) {
	var chunk ChatCompletionChunk
	raw := []byte(`{"id":"chunk-1","choices":[{"index":2,"delta":{"role":"assistant","tool_calls":[{"index":3,"id":"call-3","type":"function","function":{"name":"third","arguments":""}},{"index":0,"id":"call-0","type":"function","function":{"name":"first","arguments":"{\"x\":1}"}}]},"finish_reason":null}]}`)
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatal(err)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 2 {
		t.Fatalf("choice index was not preserved: %#v", chunk.Choices)
	}
	toolCalls := chunk.Choices[0].Delta.ToolCalls
	if len(toolCalls) != 2 {
		t.Fatalf("tool calls = %#v", toolCalls)
	}
	for index, want := range []struct {
		index int
		id    string
	}{
		{index: 3, id: "call-3"},
		{index: 0, id: "call-0"},
	} {
		call := toolCalls[index]
		if call.Index == nil || *call.Index != want.index || call.ID != want.id {
			t.Fatalf("tool call[%d] = %#v, want index %d and id %q", index, call, want.index, want.id)
		}
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip ChatCompletionChunk
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Choices[0].Delta.ToolCalls[1].Index == nil || *roundTrip.Choices[0].Delta.ToolCalls[1].Index != 0 {
		t.Fatalf("tool call index was lost after marshal round trip: %s", encoded)
	}
}

func TestMapRequestEncodesParallelToolCallsExplicitly(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "false", true: "true"}[enabled], func(t *testing.T) {
			request := minimalRequest()
			request.Generation.ParallelToolCalls = enabled
			mapped, err := MapRequest(request, DefaultModel)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(mapped)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			value, ok := object["parallel_tool_calls"]
			if !ok {
				t.Fatalf("parallel_tool_calls omitted from outbound JSON: %s", encoded)
			}
			var got bool
			if err := json.Unmarshal(value, &got); err != nil {
				t.Fatal(err)
			}
			if got != enabled {
				t.Fatalf("parallel_tool_calls = %t, want %t: %s", got, enabled, encoded)
			}
		})
	}
}

func TestReasoningEffortCompatibilityMapping(t *testing.T) {
	request := minimalRequest()
	for _, test := range []struct {
		model string
		input string
		want  string
	}{
		{model: DefaultModel, input: "low", want: "high"},
		{model: DefaultModel, input: "medium", want: "high"},
		{model: DefaultModel, input: "high", want: "high"},
		{model: DefaultModel, input: "xhigh", want: "max"},
		{model: DefaultModel, input: "max", want: "max"},
		{model: "deepseek-v4-pro", input: "low", want: "high"},
		{model: "deepseek-v4-pro", input: "xhigh", want: "max"},
	} {
		t.Run(test.model+"/"+test.input, func(t *testing.T) {
			request.Generation.Reasoning.Effort = test.input
			got, err := MapRequest(request, test.model)
			if err != nil {
				t.Fatal(err)
			}
			if got.ReasoningEffort != test.want {
				t.Fatalf("reasoning effort = %q, want %q", got.ReasoningEffort, test.want)
			}
		})
	}
	request.Generation.Reasoning.Effort = "unsupported"
	if _, err := MapRequest(request, DefaultModel); err == nil {
		t.Fatal("unsupported reasoning effort unexpectedly succeeded")
	}
}

func TestClientBuildsSafeStreamingRequestAndKeepsSuccessBodyOpen(t *testing.T) {
	const apiKey = "opencode-test-key"
	var received ChatCompletionRequest
	var receivedHeader http.Header
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		receivedHeader = r.Header.Clone()
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if err := json.Unmarshal(receivedBody, &received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{APIKey: apiKey, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), bridge.Request{
		Input:      []bridge.InputItem{bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "hello"}}}},
		Generation: bridge.GenerationOptions{Stream: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Body == nil {
		t.Fatalf("response = %#v", response)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data: {}\n\n" {
		t.Fatalf("response body = %q", data)
	}
	if err := response.Close(); err != nil {
		t.Fatal(err)
	}
	if received.Model != DefaultModel || !received.Stream {
		t.Fatalf("upstream body metadata = %#v", received)
	}
	if receivedHeader.Get("Authorization") != "Bearer "+apiKey {
		t.Fatalf("authorization = %q", receivedHeader.Get("Authorization"))
	}
	if receivedHeader.Get("Content-Type") != "application/json" || receivedHeader.Get("Accept") != DefaultAccept {
		t.Fatalf("request headers = %#v", receivedHeader)
	}
	if receivedHeader.Get("User-Agent") != DefaultUserAgent {
		t.Fatalf("user-agent = %q", receivedHeader.Get("User-Agent"))
	}
	if strings.Contains(receivedHeader.Get("User-Agent"), apiKey) {
		t.Fatal("API key leaked into user-agent")
	}
	if bytes.Contains(receivedBody, []byte(apiKey)) {
		t.Fatal("API key leaked into request body")
	}
	var receivedObject map[string]json.RawMessage
	if err := json.Unmarshal(receivedBody, &receivedObject); err != nil {
		t.Fatal(err)
	}
	parallelValue, ok := receivedObject["parallel_tool_calls"]
	if !ok {
		t.Fatalf("parallel_tool_calls missing from outbound body: %s", receivedBody)
	}
	var parallelToolCalls bool
	if err := json.Unmarshal(parallelValue, &parallelToolCalls); err != nil {
		t.Fatal(err)
	}
	if parallelToolCalls {
		t.Fatalf("parallel_tool_calls = true, want explicit false: %s", receivedBody)
	}
}

func TestClientClosesErrorBodiesAndClassifiesStatusesWithoutPayloadLeak(t *testing.T) {
	const marker = "provider-secret-marker"
	for _, test := range []struct {
		status httpStatus
		code   ErrorCode
	}{
		{status: httpStatus{Code: http.StatusBadRequest}, code: ErrorBadRequest},
		{status: httpStatus{Code: http.StatusUnauthorized}, code: ErrorUnauthorized},
		{status: httpStatus{Code: http.StatusForbidden}, code: ErrorForbidden},
		{status: httpStatus{Code: http.StatusTooManyRequests, RetryAfter: "17"}, code: ErrorRateLimited},
		{status: httpStatus{Code: http.StatusInternalServerError}, code: ErrorServer},
	} {
		t.Run(test.code.String(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status.RetryAfter != "" {
					w.Header().Set("Retry-After", test.status.RetryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status.Code)
				_, _ = io.WriteString(w, `{"error":"`+marker+`"}`)
			}))
			defer server.Close()
			client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Do(context.Background(), minimalRequest())
			assertProviderCode(t, err, test.code)
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("provider payload leaked through error: %v", err)
			}
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("error type = %T", err)
			}
			if test.status.RetryAfter != "" && providerErr.RetryAfter != test.status.RetryAfter {
				t.Fatalf("Retry-After = %q, want %q", providerErr.RetryAfter, test.status.RetryAfter)
			}
		})
	}
}

func TestDefaultHTTPClientDoesNotFollowRedirectsOrReplayCredentials(t *testing.T) {
	const apiKey = "redirect-secret-key"
	const marker = "redirect-provider-secret"

	for _, test := range []struct {
		name       string
		crossHost  bool
		wantStatus int
	}{
		{name: "same host", wantStatus: http.StatusTemporaryRedirect},
		{name: "cross host", crossHost: true, wantStatus: http.StatusTemporaryRedirect},
	} {
		t.Run(test.name, func(t *testing.T) {
			secondaryRequests := 0
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryRequests++
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("redirected request replayed authorization: %q", got)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
			}))
			defer secondary.Close()

			primaryRequests := 0
			var primaryURL string
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryRequests++
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("redirect target reached same host: %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
					t.Errorf("initial authorization = %q, want bearer credential", got)
				}
				location := primaryURL + "/same-host-target"
				if test.crossHost {
					location = secondary.URL + "/cross-host-target"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(test.wantStatus)
				_, _ = io.WriteString(w, marker)
			}))
			defer primary.Close()
			primaryURL = primary.URL

			client, err := NewClient(ClientConfig{APIKey: apiKey, BaseURL: primary.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Do(context.Background(), minimalRequest())
			assertProviderCode(t, err, ErrorUnexpectedStatus)
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.StatusCode != test.wantStatus {
				t.Fatalf("redirect error = %#v", providerErr)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), apiKey) {
				t.Fatalf("redirect response leaked sensitive data: %v", err)
			}
			if primaryRequests != 1 {
				t.Fatalf("primary requests = %d, want 1", primaryRequests)
			}
			if secondaryRequests != 0 {
				t.Fatalf("secondary requests = %d, want 0", secondaryRequests)
			}
		})
	}
}

func TestClientBoundsOversizedErrorBodies(t *testing.T) {
	const limit int64 = 32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", int(limit)+1000))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: server.URL, MaxErrorBodyBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), minimalRequest())
	assertProviderCode(t, err, ErrorServer)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || !providerErr.BodyTruncated {
		t.Fatalf("oversized body classification = %#v", providerErr)
	}
}

func TestClientDoesNotParseOrExposeMalformedHTTPErrorBodies(t *testing.T) {
	const marker = "malformed-provider-marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, marker+" is not JSON")
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), minimalRequest())
	assertProviderCode(t, err, ErrorBadRequest)
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("malformed provider body leaked through error: %v", err)
	}
}

func TestClientClassifiesMalformedErrorBodyAndClosesIt(t *testing.T) {
	body := &failingBody{}
	client, err := NewClient(ClientConfig{
		APIKey:  "key",
		BaseURL: "https://example.com",
		HTTPClient: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Body: body, Header: make(http.Header)}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), minimalRequest())
	assertProviderCode(t, err, ErrorBadRequest)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || !providerErr.BodyReadFailed {
		t.Fatalf("malformed error body classification = %#v", providerErr)
	}
	if !body.closed {
		t.Fatal("malformed error body was not closed")
	}
}

func TestClientRejectsUnexpectedContentTypeAndClosesBody(t *testing.T) {
	for _, contentType := range []string{"", "application/json", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				}
				_, _ = io.WriteString(w, "not an SSE stream")
			}))
			defer server.Close()
			client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Do(context.Background(), minimalRequest())
			assertProviderCode(t, err, ErrorUnsupportedContentType)
		})
	}
}

func TestClientCancellationStopsUpstreamRequest(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	injected := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		close(finished)
		return nil, request.Context().Err()
	})
	client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: "https://example.com", HTTPClient: injected})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, requestErr := client.Do(ctx, minimalRequest())
		done <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case err := <-done:
		assertProviderCode(t, err, ErrorCanceled)
	case <-time.After(2 * time.Second):
		t.Fatal("client did not return after cancellation")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler did not observe cancellation")
	}
}

func TestNewClientValidatesCredentialsModelURLAndLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		config ClientConfig
	}{
		{name: "missing key", config: ClientConfig{BaseURL: "https://example.com"}},
		{name: "header injection key", config: ClientConfig{APIKey: "key\nvalue", BaseURL: "https://example.com"}},
		{name: "bad model", config: ClientConfig{APIKey: "key", BaseURL: "https://example.com", Model: "bad model"}},
		{name: "path traversal", config: ClientConfig{APIKey: "key", BaseURL: "https://example.com/v1/../private"}},
		{name: "query is not a base URL", config: ClientConfig{APIKey: "key", BaseURL: "https://example.com/v1?token=secret"}},
		{name: "negative request limit", config: ClientConfig{APIKey: "key", BaseURL: "https://example.com", MaxRequestBodyBytes: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewClient(test.config); err == nil {
				t.Fatal("invalid client configuration unexpectedly succeeded")
			}
		})
	}
}

func TestModelPolicyDefaultsAndRejectsUnsupportedProviderModels(t *testing.T) {
	for _, model := range []string{"", DefaultModel, "deepseek-v4-pro"} {
		client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: "https://example.com", Model: model})
		if err != nil {
			t.Fatalf("model %q rejected: %v", model, err)
		}
		mapped, err := client.BuildRequest(minimalRequest())
		if err != nil {
			t.Fatalf("model %q could not build request: %v", model, err)
		}
		want := model
		if want == "" {
			want = DefaultModel
		}
		if mapped.Model != want {
			t.Fatalf("mapped model = %q, want %q", mapped.Model, want)
		}
	}

	for _, model := range []string{"gpt-5.3-codex", "deepseek-chat", "deepseek-v4-flash-preview", " deepseek-v4-flash"} {
		t.Run(model, func(t *testing.T) {
			_, err := NewClient(ClientConfig{APIKey: "key", BaseURL: "https://example.com", Model: model})
			assertProviderCode(t, err, ErrorInvalidConfiguration)
			_, err = MapRequest(minimalRequest(), model)
			assertProviderCode(t, err, ErrorInvalidConfiguration)
		})
	}
}

func TestClientDoesNotRouteTheInboundBridgeModelToTheProvider(t *testing.T) {
	client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	request := minimalRequest()
	request.Model = "gpt-5.3-codex"
	mapped, err := client.BuildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.Model != DefaultModel {
		t.Fatalf("provider model = %q, want configured default %q", mapped.Model, DefaultModel)
	}
}

func TestFunctionNamesFollowProviderASCIIContract(t *testing.T) {
	validNames := []string{"a", "A-9_z", strings.Repeat("a", 64)}
	for _, name := range validNames {
		t.Run("valid/"+name, func(t *testing.T) {
			request := minimalRequest()
			request.Tools = []bridge.Tool{bridge.FunctionTool{Name: name, Parameters: mustSchema(t, `{"type":"object"}`)}}
			if _, err := MapRequest(request, DefaultModel); err != nil {
				t.Fatalf("valid function name %q rejected: %v", name, err)
			}
		})
	}

	for _, name := range []string{"café", "工具", "with space", "with.dot", strings.Repeat("a", 65)} {
		t.Run("invalid/"+name, func(t *testing.T) {
			request := minimalRequest()
			request.Tools = []bridge.Tool{bridge.FunctionTool{Name: name, Parameters: mustSchema(t, `{"type":"object"}`)}}
			_, err := MapRequest(request, DefaultModel)
			assertProviderCode(t, err, ErrorInvalidRequest)

			request.Input = []bridge.InputItem{bridge.FunctionCall{CallID: "call", Name: name, Arguments: `{}`}}
			_, err = MapRequest(request, DefaultModel)
			assertProviderCode(t, err, ErrorInvalidRequest)
		})
	}
}

func TestEndpointURLNormalizesVersionedBasesWithoutTraversal(t *testing.T) {
	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{name: "root test base", base: "http://127.0.0.1:8787", want: "http://127.0.0.1:8787/v1/chat/completions"},
		{name: "versioned test base", base: "http://127.0.0.1:8787/v1/", want: "http://127.0.0.1:8787/v1/chat/completions"},
		{name: "OpenCode default", base: DefaultBaseURL, want: "https://opencode.ai/zen/go/v1/chat/completions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := endpointURL(test.base)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClientDoesNotUseGlobalDefaultClient(t *testing.T) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("global client must not be used")
	})}
	defer func() { http.DefaultClient = previous }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), minimalRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Close()
}

func TestClientInjectedHTTPClientAndBodyOwnership(t *testing.T) {
	var body trackingBody
	injected := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/chat/completions" {
			return nil, errors.New("unexpected endpoint")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &body, Request: request}, nil
	})
	client, err := NewClient(ClientConfig{APIKey: "key", BaseURL: "https://example.com", HTTPClient: injected})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), minimalRequest())
	if err != nil {
		t.Fatal(err)
	}
	if body.closed {
		t.Fatal("success response body was closed before caller ownership")
	}
	if err := response.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("response.Close did not close the upstream body")
	}
}

func TestClientRejectsOversizedRequestBeforeCallingHTTPClient(t *testing.T) {
	calls := 0
	client, err := NewClient(ClientConfig{
		APIKey:              "key",
		BaseURL:             "https://example.com",
		MaxRequestBodyBytes: 64,
		HTTPClient: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("HTTP client should not be called")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := minimalRequest()
	request.Input = []bridge.InputItem{bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: strings.Repeat("x", 256)}}}}
	_, err = client.Do(context.Background(), request)
	assertProviderCode(t, err, ErrorRequestTooLarge)
	if calls != 0 {
		t.Fatalf("HTTP client calls = %d, want 0", calls)
	}
}

func TestClientClassifiesNetworkFailureWithoutRetry(t *testing.T) {
	calls := 0
	client, err := NewClient(ClientConfig{
		APIKey:  "key",
		BaseURL: "https://example.com",
		HTTPClient: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("transport marker")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), minimalRequest())
	assertProviderCode(t, err, ErrorNetwork)
	if strings.Contains(err.Error(), "transport marker") {
		t.Fatal("network error leaked the underlying transport message")
	}
	if calls != 1 {
		t.Fatalf("HTTP client calls = %d, want exactly one", calls)
	}
}

func TestDefaultHTTPClientUsesStreamingSafeTransportSettings(t *testing.T) {
	httpClient := newDefaultHTTPClient()
	if httpClient.Timeout != 0 {
		t.Fatalf("default HTTP client timeout = %s, want zero", httpClient.Timeout)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport = %T, want *http.Transport", httpClient.Transport)
	}
	if !transport.DisableCompression {
		t.Fatal("streaming transport unexpectedly enables automatic compression")
	}
	if transport.Proxy == nil || transport.ResponseHeaderTimeout <= 0 || transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf("transport phase settings are incomplete: %#v", transport)
	}
}

func TestClientRejectsUnsupportedBridgeShapes(t *testing.T) {
	request := minimalRequest()
	request.Tools = []bridge.Tool{bridge.DeferredTool{ToolKind: bridge.ToolNamespace, Name: "mcp"}}
	_, err := MapRequest(request, DefaultModel)
	assertProviderCode(t, err, ErrorUnsupportedTool)

	request = minimalRequest()
	request.Generation.Text.Format = bridge.TextFormat{Kind: bridge.TextFormatJSONSchema, Name: "answer", Schema: mustSchema(t, `{"type":"object"}`)}
	_, err = MapRequest(request, DefaultModel)
	assertProviderCode(t, err, ErrorUnsupportedResponseFormat)
}

func minimalRequest() bridge.Request {
	return bridge.Request{
		Input:      []bridge.InputItem{bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "hello"}}}},
		ToolChoice: bridge.ToolChoice{Kind: bridge.ToolChoiceAuto},
		Generation: bridge.GenerationOptions{Stream: true},
	}
}

func mustSchema(t *testing.T, raw string) bridge.JSONSchema {
	t.Helper()
	schema, err := bridge.NewJSONSchema([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func assertProviderCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error type = %T, want *ProviderError: %v", err, err)
	}
	if providerErr.Code != want {
		t.Fatalf("error code = %s, want %s (%v)", providerErr.Code, want, err)
	}
}

func jsonEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

type httpStatus struct {
	Code       int
	RetryAfter string
}

func (status httpStatus) String() string {
	return http.StatusText(status.Code)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type trackingBody struct {
	mu     sync.Mutex
	closed bool
}

func (body *trackingBody) Read([]byte) (int, error) { return 0, io.EOF }

func (body *trackingBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.closed = true
	return nil
}

type failingBody struct {
	closed bool
}

func (body *failingBody) Read([]byte) (int, error) { return 0, errors.New("malformed body marker") }

func (body *failingBody) Close() error {
	body.closed = true
	return nil
}
