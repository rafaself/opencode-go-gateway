package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestDecodeErrorsDoNotExposeUntrustedValues(t *testing.T) {
	const marker = "attacker-secret-marker"

	tests := []struct {
		name  string
		body  string
		code  ErrorCode
		param string
	}{
		{
			name:  "unknown top-level field name and value",
			body:  `{"model":"gpt-5.3-codex","stream":true,"` + marker + `":"` + marker + `"}`,
			code:  ErrorUnsupportedField,
			param: "request.<unknown_field>",
		},
		{
			name:  "unknown nested field name and value",
			body:  `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":"safe","` + marker + `":"` + marker + `"}]}`,
			code:  ErrorUnsupportedField,
			param: "input[0].<unknown_field>",
		},
		{
			name:  "unsupported input type",
			body:  `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"` + marker + `"}]}`,
			code:  ErrorUnsupportedItemType,
			param: "input[0].type",
		},
		{
			name:  "unsupported content type",
			body:  `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"` + marker + `","text":"safe"}]}]}`,
			code:  ErrorUnsupportedItemType,
			param: "input[0].content[0].type",
		},
		{
			name:  "unsupported message role",
			body:  `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"` + marker + `","content":"safe"}]}`,
			code:  ErrorInvalidRequest,
			param: "input[0].role",
		},
		{
			name:  "unsupported tool type",
			body:  `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"` + marker + `"}]}`,
			code:  ErrorUnsupportedToolType,
			param: "tools[0].type",
		},
		{
			name:  "unsupported string tool choice",
			body:  `{"model":"gpt-5.3-codex","stream":true,"tool_choice":"` + marker + `"}`,
			code:  ErrorInvalidRequest,
			param: "tool_choice",
		},
		{
			name:  "unsupported object tool choice type",
			body:  `{"model":"gpt-5.3-codex","stream":true,"tool_choice":{"type":"` + marker + `","name":"safe"}}`,
			code:  ErrorInvalidRequest,
			param: "tool_choice.type",
		},
		{
			name:  "undeclared tool choice name",
			body:  `{"model":"gpt-5.3-codex","stream":true,"tool_choice":{"type":"function","name":"` + marker + `"}}`,
			code:  ErrorInvalidRequest,
			param: "tool_choice.name",
		},
		{
			name:  "unsupported text format",
			body:  `{"model":"gpt-5.3-codex","stream":true,"text":{"format":{"type":"` + marker + `"}}}`,
			code:  ErrorInvalidRequest,
			param: "text.format.type",
		},
		{
			name:  "unsupported schema type",
			body:  `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"safe","parameters":{"type":"` + marker + `"}}]}`,
			code:  ErrorInvalidRequest,
			param: "tools[0].parameters",
		},
		{
			name:  "schema property name and type",
			body:  `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"safe","parameters":{"type":"object","properties":{"` + marker + `":{"type":"` + marker + `"}}}}]}`,
			code:  ErrorInvalidRequest,
			param: "tools[0].parameters",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(test.body), "application/json")
			assertDecodeError(t, err, test.code, test.param)
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("decoder error exposed untrusted marker: %v", err)
			}
			var decodeErr *Error
			if !errors.As(err, &decodeErr) {
				t.Fatalf("error type = %T, want *codex.Error", err)
			}
			if strings.Contains(decodeErr.Message, marker) || strings.Contains(decodeErr.Param, marker) {
				t.Fatalf("structured error exposed untrusted marker: %#v", decodeErr)
			}
		})
	}
}

func TestDecodeRequestHTTPWrapper(t *testing.T) {
	validBody := []byte(`{"model":"gpt-5.3-codex","stream":true}`)

	t.Run("content type accepts media type parameters", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		if _, err := DecodeRequest(req, int64(len(validBody))); err != nil {
			t.Fatalf("DecodeRequest returned error: %v", err)
		}
	})

	for _, contentType := range []string{"", "text/plain"} {
		t.Run("rejects content type "+contentType, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(validBody))
			req.Header.Set("Content-Type", contentType)
			assertDecodeError(t, decodeHTTPRequest(req, int64(len(validBody))), ErrorInvalidRequest, "content-type")
		})
	}

	t.Run("content length rejects before reading", func(t *testing.T) {
		body := &countingReadCloser{Reader: bytes.NewReader(validBody)}
		req := &http.Request{
			Method:        http.MethodPost,
			Body:          body,
			ContentLength: int64(len(validBody) + 1),
			Header:        http.Header{"Content-Type": []string{"application/json"}},
		}
		assertDecodeError(t, decodeHTTPRequest(req, int64(len(validBody))), ErrorRequestTooLarge, "body")
		if body.reads != 0 {
			t.Fatalf("body was read %d times after Content-Length rejection", body.reads)
		}
	})

	t.Run("unknown length is bounded by the decoder", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(validBody))
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		if _, err := DecodeRequest(req, int64(len(validBody))); err != nil {
			t.Fatalf("unknown-length request returned error: %v", err)
		}
	})

	t.Run("body exactly at the limit succeeds with unknown length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(validBody))
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		if _, err := DecodeRequest(req, int64(len(validBody))); err != nil {
			t.Fatalf("boundary request returned error: %v", err)
		}
	})

	t.Run("body over the limit is rejected with unknown length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(validBody))
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		assertDecodeError(t, decodeHTTPRequest(req, int64(len(validBody)-1)), ErrorRequestTooLarge, "body")
	})

	t.Run("trailing JSON is rejected", func(t *testing.T) {
		body := append(append([]byte(nil), validBody...), []byte(`{}`)...)
		req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(body))
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		assertDecodeError(t, decodeHTTPRequest(req, int64(len(body))), ErrorMalformedJSON, "body")
	})
}

func decodeHTTPRequest(request *http.Request, maxBodyBytes int64) error {
	_, err := DecodeRequest(request, maxBodyBytes)
	return err
}

type countingReadCloser struct {
	*bytes.Reader
	reads int
}

func (body *countingReadCloser) Read(p []byte) (int, error) {
	body.reads++
	return body.Reader.Read(p)
}

func (body *countingReadCloser) Close() error { return nil }

func TestPolicyListedInputTypesHaveBehaviorDrivenDecisions(t *testing.T) {
	policy := readContractPolicy(t)
	expected := map[string]struct {
		policy string
		body   string
		kind   bridge.InputKind
	}{
		string(bridge.InputMessage): {
			policy: string(policyTranslate),
			body:   `{"type":"message","id":"message-id","role":"user","content":"domain text"}`,
			kind:   bridge.InputMessage,
		},
		string(bridge.InputFunctionCall): {
			policy: string(policyTranslate),
			body:   `{"type":"function_call","id":"function-item-id","call_id":"function-call-id","name":"exec_command","arguments":"{}","status":"completed"}`,
			kind:   bridge.InputFunctionCall,
		},
		string(bridge.InputFunctionCallOutput): {
			policy: string(policyTranslate),
			body:   `{"type":"function_call_output","call_id":"function-call-id","output":"result"}`,
			kind:   bridge.InputFunctionCallOutput,
		},
		string(bridge.InputCustomToolCall): {
			policy: string(policyTranslate),
			body:   `{"type":"custom_tool_call","id":"custom-item-id","call_id":"custom-call-id","name":"apply_patch","input":"{}","status":"completed"}`,
			kind:   bridge.InputCustomToolCall,
		},
		string(bridge.InputCustomToolCallOutput): {
			policy: string(policyTranslate),
			body:   `{"type":"custom_tool_call_output","call_id":"custom-call-id","output":"result"}`,
			kind:   bridge.InputCustomToolCallOutput,
		},
	}

	for itemType, entry := range policy.ItemTypes {
		t.Run(itemType, func(t *testing.T) {
			expectation, ok := expected[itemType]
			if !ok {
				t.Fatalf("policy item type %q has no behavior-driven decision", itemType)
			}
			if entry != expectation.policy {
				t.Fatalf("policy item type %q = %q, behavior decision = %q", itemType, entry, expectation.policy)
			}
			items := expectation.body
			if itemType == string(bridge.InputFunctionCallOutput) {
				items = `{"type":"function_call","call_id":"function-call-id","name":"exec_command","arguments":"{}"},` + items
			}
			if itemType == string(bridge.InputCustomToolCallOutput) {
				items = `{"type":"custom_tool_call","call_id":"custom-call-id","name":"apply_patch","input":"{}"},` + items
			}
			body := `{"model":"gpt-5.3-codex","stream":true,"input":[` + items + `]}`
			request, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(body), "application/json")
			if err != nil {
				t.Fatalf("policy-listed item did not decode: %v", err)
			}
			found := false
			for _, item := range request.Input {
				if item.Kind() == expectation.kind {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("decoded input kinds = %v, want %q", inputKinds(request.Input), expectation.kind)
			}
		})
	}
	for itemType := range expected {
		if _, ok := policy.ItemTypes[itemType]; !ok {
			t.Errorf("behavior decision for %q is missing from field-policy.json", itemType)
		}
	}
}

func TestPolicyListedToolTypesHaveBehaviorDrivenDecisions(t *testing.T) {
	policy := readContractPolicy(t)
	expected := map[string]struct {
		policy string
		body   string
		kind   bridge.ToolKind
	}{
		string(bridge.ToolFunction): {
			policy: string(policyTranslate),
			body:   `{"type":"function","name":"domain_function","description":"domain description","parameters":{"type":"object"},"strict":true}`,
			kind:   bridge.ToolFunction,
		},
		string(bridge.ToolCustom): {
			policy: string(policyTranslate),
			body:   `{"type":"custom","name":"apply_patch","description":"domain description","format":{"type":"text"}}`,
			kind:   bridge.ToolCustom,
		},
		string(bridge.ToolNamespace): {
			policy: string(policyDefer),
			body:   `{"type":"namespace","name":"domain_namespace","description":"domain description","tools":[]}`,
			kind:   bridge.ToolNamespace,
		},
		string(bridge.ToolWebSearch): {
			policy: string(policyDefer),
			body:   `{"type":"web_search","external_web_access":false}`,
			kind:   bridge.ToolWebSearch,
		},
	}

	for toolType, entry := range policy.ToolTypes {
		t.Run(toolType, func(t *testing.T) {
			expectation, ok := expected[toolType]
			if !ok {
				t.Fatalf("policy tool type %q has no behavior-driven decision", toolType)
			}
			if entry != expectation.policy {
				t.Fatalf("policy tool type %q = %q, behavior decision = %q", toolType, entry, expectation.policy)
			}
			body := `{"model":"gpt-5.3-codex","stream":true,"tools":[` + expectation.body + `]}`
			request, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(body), "application/json")
			if err != nil {
				t.Fatalf("policy-listed tool did not reach its decision: %v", err)
			}
			if len(request.Tools) != 1 || request.Tools[0].Kind() != expectation.kind {
				t.Fatalf("decoded tools = %#v, want one %q tool", request.Tools, expectation.kind)
			}
			if expectation.kind == bridge.ToolFunction {
				function, ok := request.Tools[0].(bridge.FunctionTool)
				if !ok || function.Strict == nil || !*function.Strict {
					t.Fatalf("function tool decision lost strictness: %#v", request.Tools[0])
				}
			}
		})
	}
	for toolType := range expected {
		if _, ok := policy.ToolTypes[toolType]; !ok {
			t.Errorf("behavior decision for %q is missing from field-policy.json", toolType)
		}
	}
}

func TestDecodePreservesFullBridgeDomainValues(t *testing.T) {
	const rawSchema = `{ "type" : "object", "properties" : { "arg" : { "type" : "string" } }, "required" : ["arg"] }`
	body := `{"model":"gpt-5.3-codex","instructions":"domain instructions","previous_response_id":"response-id","input":[` +
		`{"type":"message","id":"message-id","role":"assistant","content":[{"type":"input_text","text":"first"},{"type":"input_text","text":"second"}]},` +
		`{"type":"function_call","id":"function-item-id","call_id":"function-call-id","name":"exec_command","arguments":"{\"cmd\":\"printf\"}","status":"completed"},` +
		`{"type":"function_call_output","call_id":"function-call-id","output":"function output"},` +
		`{"type":"custom_tool_call","id":"custom-item-id","call_id":"custom-call-id","name":"apply_patch","input":"{\"patch\":\"...\"}","status":"in_progress"},` +
		`{"type":"custom_tool_call_output","call_id":"custom-call-id","output":"custom output"}],` +
		`"tools":[{"type":"function","name":"exec_command","description":"function description","parameters":` + rawSchema + `,"strict":true},` +
		`{"type":"namespace","name":"mcp","description":"namespace description","tools":[]},` +
		`{"type":"web_search","external_web_access":false}],` +
		`"tool_choice":{"type":"function","name":"exec_command"},"parallel_tool_calls":true,` +
		`"reasoning":{"effort":"high","summary":"auto"},"text":{"format":{"type":"json_schema","name":"result","description":"result description","schema":` + rawSchema + `,"strict":false}},` +
		`"stream":true,"include":["reasoning.encrypted_content"]}`

	request, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(body), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "gpt-5.3-codex" || request.Instructions != "domain instructions" || request.PreviousResponseID != "response-id" {
		t.Fatalf("request identity fields = %#v", request)
	}
	if !request.Generation.ParallelToolCalls || request.Generation.Reasoning.Effort != "high" || request.Generation.Text.Format.Kind != bridge.TextFormatJSONSchema {
		t.Fatalf("generation fields = %#v", request.Generation)
	}
	if request.ToolChoice.Kind != bridge.ToolChoiceFunction || request.ToolChoice.FunctionName != "exec_command" {
		t.Fatalf("tool choice = %#v", request.ToolChoice)
	}

	message, ok := request.Input[0].(bridge.Message)
	if !ok || message.ID != "message-id" || message.Role != bridge.RoleAssistant || len(message.Content) != 2 {
		t.Fatalf("message = %#v", request.Input[0])
	}
	for index, want := range []string{"first", "second"} {
		content, ok := message.Content[index].(bridge.TextContent)
		if !ok || content.Text != want {
			t.Fatalf("message content[%d] = %#v, want %q", index, message.Content[index], want)
		}
	}
	functionCall, ok := request.Input[1].(bridge.FunctionCall)
	if !ok || functionCall.ID != "function-item-id" || functionCall.CallID != "function-call-id" || functionCall.Name != "exec_command" || functionCall.Arguments != `{"cmd":"printf"}` || functionCall.Status != "completed" {
		t.Fatalf("function call = %#v", request.Input[1])
	}
	functionOutput, ok := request.Input[2].(bridge.FunctionCallOutput)
	if !ok || functionOutput.CallID != "function-call-id" || functionOutput.Output != "function output" {
		t.Fatalf("function output = %#v", request.Input[2])
	}
	customCall, ok := request.Input[3].(bridge.CustomToolCall)
	if !ok || customCall.ID != "custom-item-id" || customCall.CallID != "custom-call-id" || customCall.Name != "apply_patch" || customCall.Input != `{"patch":"..."}` || customCall.Status != "in_progress" {
		t.Fatalf("custom call = %#v", request.Input[3])
	}
	customOutput, ok := request.Input[4].(bridge.CustomToolCallOutput)
	if !ok || customOutput.CallID != "custom-call-id" || customOutput.Output != "custom output" {
		t.Fatalf("custom output = %#v", request.Input[4])
	}

	functionTool, ok := request.Tools[0].(bridge.FunctionTool)
	if !ok || functionTool.Name != "exec_command" || functionTool.Description != "function description" || string(functionTool.Parameters.RawJSON()) != rawSchema || functionTool.Strict == nil || !*functionTool.Strict {
		t.Fatalf("function tool = %#v", request.Tools[0])
	}
	namespace, ok := request.Tools[1].(bridge.DeferredTool)
	if !ok || namespace.Kind() != bridge.ToolNamespace || namespace.Name != "mcp" {
		t.Fatalf("namespace tool = %#v", request.Tools[1])
	}
	webSearch, ok := request.Tools[2].(bridge.DeferredTool)
	if !ok || webSearch.Kind() != bridge.ToolWebSearch {
		t.Fatalf("web search tool = %#v", request.Tools[2])
	}
	format := request.Generation.Text.Format
	if format.Name != "result" || format.Description != "result description" || string(format.Schema.RawJSON()) != rawSchema || format.Strict == nil || *format.Strict {
		t.Fatalf("text format = %#v", format)
	}
}

type contractPolicy struct {
	TopLevel map[string]struct {
		Policy string `json:"policy"`
	} `json:"top_level"`
	ItemTypes map[string]string `json:"item_types"`
	ToolTypes map[string]string `json:"tool_types"`
}

func readContractPolicy(t *testing.T) contractPolicy {
	t.Helper()
	policyBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", "field-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy contractPolicy
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}
