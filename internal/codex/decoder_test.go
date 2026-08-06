package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestRequestFixturesDecodeDeterministically(t *testing.T) {
	fixtures := map[string]struct {
		input []bridge.InputKind
		tools []bridge.ToolKind
	}{
		"apply-patch-request.json": {
			input: []bridge.InputKind{bridge.InputMessage},
			tools: []bridge.ToolKind{bridge.ToolFunction, bridge.ToolNamespace, bridge.ToolWebSearch},
		},
		"cancellation-request.json": {
			input: []bridge.InputKind{bridge.InputMessage},
		},
		"continuation-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputFunctionCall, bridge.InputFunctionCallOutput},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
		"custom-tool-result-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputCustomToolCall, bridge.InputCustomToolCallOutput},
			tools: []bridge.ToolKind{bridge.ToolFunction, bridge.ToolNamespace, bridge.ToolWebSearch},
		},
		"developer-instructions-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputMessage},
		},
		"empty-tool-result-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputFunctionCall, bridge.InputFunctionCallOutput},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
		"function-tools-request.json": {
			input: []bridge.InputKind{bridge.InputMessage},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
		"parallel-tools-request.json": {
			input: []bridge.InputKind{bridge.InputMessage},
			tools: []bridge.ToolKind{bridge.ToolFunction, bridge.ToolFunction},
		},
		"shell-command-request.json": {
			input: []bridge.InputKind{bridge.InputMessage},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
		"simple-request.json": {
			input: []bridge.InputKind{bridge.InputMessage},
			tools: []bridge.ToolKind{bridge.ToolFunction, bridge.ToolNamespace, bridge.ToolWebSearch},
		},
		"tool-error-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputFunctionCall, bridge.InputFunctionCallOutput},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
		"tool-results-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputFunctionCall, bridge.InputFunctionCallOutput},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
		"workspace-file-read-request.json": {
			input: []bridge.InputKind{bridge.InputMessage, bridge.InputFunctionCall, bridge.InputFunctionCallOutput},
			tools: []bridge.ToolKind{bridge.ToolFunction},
		},
	}

	decoder := mustDecoder(t, 1<<20)
	requestDir := filepath.Join("..", "..", "testdata", "codex", "requests")
	entries, err := os.ReadDir(requestDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		expectation, ok := fixtures[entry.Name()]
		if !ok {
			t.Fatalf("fixture %s has no bridge expectation", entry.Name())
		}
		seen[entry.Name()] = true

		t.Run(entry.Name(), func(t *testing.T) {
			body := fixtureBody(t, filepath.Join(requestDir, entry.Name()))
			first, err := decoder.Decode(bytes.NewReader(body), "application/json")
			if err != nil {
				t.Fatal(err)
			}
			second, err := decoder.Decode(bytes.NewReader(body), "application/json; charset=utf-8")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("decoding is not deterministic:\nfirst:  %#v\nsecond: %#v", first, second)
			}
			if first.Model == "" {
				t.Fatal("decoded model is empty")
			}
			if got := inputKinds(first.Input); !reflect.DeepEqual(got, expectation.input) {
				t.Fatalf("input kinds = %v, want %v", got, expectation.input)
			}
			if got := toolKinds(first.Tools); !slices.Equal(got, expectation.tools) {
				t.Fatalf("tool kinds = %v, want %v", got, expectation.tools)
			}
			if !first.Generation.Stream {
				t.Fatal("fixture did not preserve stream=true")
			}
			serialized, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			serializedAgain, err := json.Marshal(second)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(serialized, serializedAgain) {
				t.Fatalf("bridge serialization is not deterministic:\nfirst: %s\nsecond: %s", serialized, serializedAgain)
			}
		})
	}
	for name := range fixtures {
		if !seen[name] {
			t.Errorf("missing request fixture %s", name)
		}
	}
}

func TestRequestFixturesPreserveDomainValues(t *testing.T) {
	decoder := mustDecoder(t, 1<<20)
	requestDir := filepath.Join("..", "..", "testdata", "codex", "requests")

	t.Run("message roles and text order", func(t *testing.T) {
		body := fixtureBody(t, filepath.Join(requestDir, "developer-instructions-request.json"))
		request, err := decoder.Decode(bytes.NewReader(body), "application/json")
		if err != nil {
			t.Fatal(err)
		}
		first, ok := request.Input[0].(bridge.Message)
		if !ok {
			t.Fatalf("first input = %T, want bridge.Message", request.Input[0])
		}
		if first.Role != bridge.RoleDeveloper || len(first.Content) != 2 {
			t.Fatalf("first message = %#v", first)
		}
		second, ok := request.Input[1].(bridge.Message)
		if !ok || second.Role != bridge.RoleUser || len(second.Content) != 1 {
			t.Fatalf("second message = %#v", request.Input[1])
		}
		firstText, ok := first.Content[0].(bridge.TextContent)
		if !ok {
			t.Fatalf("first content = %T", first.Content[0])
		}
		secondText, ok := first.Content[1].(bridge.TextContent)
		if !ok {
			t.Fatalf("second content = %T", first.Content[1])
		}
		if firstText.Text != "<redacted:string>" || secondText.Text != "<redacted:string>" {
			t.Fatalf("message text order was not preserved: %#v", first.Content)
		}
	})

	t.Run("function call and output correlation", func(t *testing.T) {
		body := fixtureBody(t, filepath.Join(requestDir, "continuation-request.json"))
		request, err := decoder.Decode(bytes.NewReader(body), "application/json")
		if err != nil {
			t.Fatal(err)
		}
		call, ok := request.Input[1].(bridge.FunctionCall)
		if !ok {
			t.Fatalf("function call = %T", request.Input[1])
		}
		output, ok := request.Input[2].(bridge.FunctionCallOutput)
		if !ok {
			t.Fatalf("function output = %T", request.Input[2])
		}
		if call.CallID == "" || call.CallID != output.CallID || call.Name != "exec_command" {
			t.Fatalf("correlation values were not preserved: call=%#v output=%#v", call, output)
		}
		if request.PreviousResponseID == "" {
			t.Fatal("previous_response_id was not preserved")
		}
	})

	t.Run("function schema and deferred tool kind", func(t *testing.T) {
		body := fixtureBody(t, filepath.Join(requestDir, "simple-request.json"))
		request, err := decoder.Decode(bytes.NewReader(body), "application/json")
		if err != nil {
			t.Fatal(err)
		}
		function, ok := request.Tools[0].(bridge.FunctionTool)
		if !ok {
			t.Fatalf("first tool = %T", request.Tools[0])
		}
		if function.Name != "exec_command" || !json.Valid(function.Parameters.RawJSON()) {
			t.Fatalf("function tool schema was not preserved: %#v", function)
		}
		if _, ok := request.Tools[1].(bridge.DeferredTool); !ok {
			t.Fatalf("namespace tool = %T, want explicit deferred tool", request.Tools[1])
		}
	})
}

func TestRequestFixturesMatchBridgeGolden(t *testing.T) {
	goldenBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", "bridge-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]bridgeGolden
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatal(err)
	}
	decoder := mustDecoder(t, 1<<20)
	requestDir := filepath.Join("..", "..", "testdata", "codex", "requests")
	entries, err := os.ReadDir(requestDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		want, ok := golden[entry.Name()]
		if !ok {
			t.Errorf("missing bridge golden for %s", entry.Name())
			continue
		}
		seen[entry.Name()] = true
		t.Run(entry.Name(), func(t *testing.T) {
			request, err := decoder.Decode(bytes.NewReader(fixtureBody(t, filepath.Join(requestDir, entry.Name()))), "application/json")
			if err != nil {
				t.Fatal(err)
			}
			got := bridgeGoldenFromRequest(request)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("bridge output = %#v, want golden %#v", got, want)
			}
		})
	}
	for name := range golden {
		if !seen[name] {
			t.Errorf("bridge golden has no request fixture %s", name)
		}
	}
}

func TestDecoderPolicyMatchesCheckedInFieldPolicy(t *testing.T) {
	policyBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", "field-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		TopLevel map[string]struct {
			Policy string `json:"policy"`
		} `json:"top_level"`
		ItemTypes map[string]string `json:"item_types"`
		ToolTypes map[string]string `json:"tool_types"`
	}
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.TopLevel) != len(topLevelPolicy) {
		t.Fatalf("decoder top-level policy has %d fields, checked-in policy has %d", len(topLevelPolicy), len(policy.TopLevel))
	}
	for field, entry := range policy.TopLevel {
		if got, ok := topLevelPolicy[field]; !ok || string(got) != entry.Policy {
			t.Errorf("top-level policy %q = %q, decoder = %q", field, entry.Policy, topLevelPolicy[field])
		}
	}
	for itemType, policyValue := range policy.ItemTypes {
		if policyValue != string(policyTranslate) {
			t.Fatalf("input item %q has unsupported test policy %q", itemType, policyValue)
		}
	}
	for toolType, policyValue := range policy.ToolTypes {
		switch toolType {
		case string(bridge.ToolFunction), string(bridge.ToolCustom), string(bridge.ToolNamespace), string(bridge.ToolWebSearch):
			if policyValue != string(policyTranslate) && policyValue != string(policyDefer) {
				t.Errorf("tool %q has unsupported test policy %q", toolType, policyValue)
			}
		default:
			t.Errorf("checked-in policy contains tool type %q without a decoder policy", toolType)
		}
	}
}

func TestDecodeCustomApplyPatchToolDeclarationUsesResponsesTextFormat(t *testing.T) {
	body := `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":"patch"}],"tools":[{"type":"custom","name":"apply_patch","description":"patches","format":{"type":"text"}}]}`
	request, err := Decode(strings.NewReader(body), "application/json", DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	custom, ok := request.Tools[0].(bridge.CustomTool)
	if !ok || custom.Name != "apply_patch" || custom.Description != "patches" || custom.Format.Kind != bridge.CustomToolFormatText {
		t.Fatalf("custom tool = %#v", request.Tools[0])
	}

	body = `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":"patch"}],"tools":[{"type":"custom","name":"apply_patch"}]}`
	request, err = Decode(strings.NewReader(body), "application/json", DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	custom, ok = request.Tools[0].(bridge.CustomTool)
	if !ok || custom.Format.Kind != bridge.CustomToolFormatText {
		t.Fatalf("default custom format = %#v", request.Tools[0])
	}

	body = `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":"patch"}],"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: \"x\""}}]}`
	if _, err := Decode(strings.NewReader(body), "application/json", DefaultMaxBodyBytes); err == nil || !strings.Contains(err.Error(), "tools[0].format") {
		t.Fatalf("grammar custom format error = %v", err)
	}
}

func TestDecodeRequestBoundaryAndPolicyErrors(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		maxBytes    int64
		code        ErrorCode
		param       string
		messagePart string
	}{
		{name: "missing content type", body: `{"model":"gpt-5.3-codex","stream":true}`, code: ErrorInvalidRequest, param: "content-type", messagePart: "application/json"},
		{name: "wrong content type", body: `{"model":"gpt-5.3-codex","stream":true}`, contentType: "text/plain", code: ErrorInvalidRequest, param: "content-type", messagePart: "application/json"},
		{name: "malformed json", body: `{"model":`, contentType: "application/json", code: ErrorMalformedJSON, param: "body", messagePart: "malformed"},
		{name: "trailing json", body: `{"model":"gpt-5.3-codex","stream":true}{}`, contentType: "application/json", code: ErrorMalformedJSON, param: "body", messagePart: "one JSON"},
		{name: "duplicate json key", body: `{"model":"gpt-5.3-codex","model":"other","stream":true}`, contentType: "application/json", code: ErrorMalformedJSON, param: "body", messagePart: "duplicate"},
		{name: "oversized body", body: `{"model":"gpt-5.3-codex","stream":true}`, contentType: "application/json", maxBytes: 8, code: ErrorRequestTooLarge, param: "body", messagePart: "configured limit"},
		{name: "invalid utf8", body: string([]byte{'{', '"', 'm', 'o', 'd', 'e', 'l', '"', ':', '"', 0xff, '"', '}'}), contentType: "application/json", code: ErrorMalformedJSON, param: "body", messagePart: "UTF-8"},
		{name: "unknown top level", body: `{"model":"gpt-5.3-codex","stream":true,"temperature":0.2}`, contentType: "application/json", code: ErrorUnsupportedField, param: "request.<unknown_field>", messagePart: "not classified"},
		{name: "deferred top level", body: `{"model":"gpt-5.3-codex","stream":true,"background":false}`, contentType: "application/json", code: ErrorUnsupportedField, param: "background", messagePart: "deferred"},
		{name: "unknown input type", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"computer_call"}]}`, contentType: "application/json", code: ErrorUnsupportedItemType, param: "input[0].type", messagePart: "not supported"},
		{name: "unknown tool type", body: `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"hosted_tool"}]}`, contentType: "application/json", code: ErrorUnsupportedToolType, param: "tools[0].type", messagePart: "not supported"},
		{name: "missing function call id", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"function_call","name":"exec_command","arguments":"{}"}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "input[0].call_id", messagePart: "required"},
		{name: "missing output correlation", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"function_call_output","call_id":"call_missing","output":""}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "input[0].call_id", messagePart: "does not correlate"},
		{name: "mismatched output kind", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"call_1","output":""}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "input[1].call_id", messagePart: "kind"},
		{name: "duplicate output correlation", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"one"},{"type":"function_call_output","call_id":"call_1","output":"two"}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "input[2].call_id", messagePart: "more than one"},
		{name: "duplicate tool name", body: `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"exec","parameters":{}},{"type":"function","name":"exec","parameters":{}}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "tools[1].name", messagePart: "duplicate"},
		{name: "invalid schema root", body: `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"exec","parameters":[]}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "tools[0].parameters", messagePart: "JSON Schema"},
		{name: "invalid nested schema", body: `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{"arg":"bad"}}}]}`, contentType: "application/json", code: ErrorInvalidRequest, param: "tools[0].parameters", messagePart: "JSON Schema"},
		{name: "stream false", body: `{"model":"gpt-5.3-codex","stream":false}`, contentType: "application/json", code: ErrorUnsupportedField, param: "stream", messagePart: "true"},
		{name: "stream missing", body: `{"model":"gpt-5.3-codex"}`, contentType: "application/json", code: ErrorUnsupportedField, param: "stream", messagePart: "true"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			maxBytes := test.maxBytes
			if maxBytes == 0 {
				maxBytes = 1 << 20
			}
			decoder := mustDecoder(t, maxBytes)
			_, err := decoder.Decode(strings.NewReader(test.body), test.contentType)
			if err == nil {
				t.Fatal("Decode unexpectedly succeeded")
			}
			var decodeErr *Error
			if !errors.As(err, &decodeErr) {
				t.Fatalf("error type = %T, want *codex.Error: %v", err, err)
			}
			if decodeErr.Code != test.code || decodeErr.Param != test.param {
				t.Fatalf("error = %#v, want code=%q param=%q", decodeErr, test.code, test.param)
			}
			if decodeErr.Message == "" || !strings.Contains(decodeErr.Message, test.messagePart) {
				t.Fatalf("message = %q, want substring %q", decodeErr.Message, test.messagePart)
			}
		})
	}
}

func TestDecodeAcceptsCompatibilityNoOpsAndUTF8(t *testing.T) {
	body := `{"model":"gpt-5.3-codex","instructions":"Olá, 世界","input":[],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"low","summary":"auto"},"text":{"format":{"type":"text"},"verbosity":"high"},"stream":true,"stream_options":{"include_obfuscation":true},"store":false,"include":["reasoning.encrypted_content"],"service_tier":"default","prompt_cache_key":"opaque-key","metadata":{"tenant":"private"},"client_metadata":{"session_id":"opaque"}}`
	request, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(body), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if request.Instructions != "Olá, 世界" || !request.Generation.ParallelToolCalls || request.Generation.Reasoning.Effort != "low" {
		t.Fatalf("translated generation values = %#v", request)
	}
	if !reflect.DeepEqual(request.Generation.Include, []string{"reasoning.encrypted_content"}) {
		t.Fatalf("include = %v", request.Generation.Include)
	}
	if request.Generation.Text.Format.Kind != bridge.TextFormatText {
		t.Fatalf("text format = %#v", request.Generation.Text.Format)
	}
}

func TestDecodePreservesFunctionSchemaRawJSONAndDoesNotExposeBody(t *testing.T) {
	rawSchema := `{ "type" : "object", "properties" : { "arg" : { "type" : "string" } } }`
	body := `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"exec","description":"private schema description","parameters":` + rawSchema + `}]}`
	request, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(body), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	function, ok := request.Tools[0].(bridge.FunctionTool)
	if !ok {
		t.Fatalf("tool = %T", request.Tools[0])
	}
	if string(function.Parameters.RawJSON()) != rawSchema {
		t.Fatalf("raw schema = %q, want exact %q", function.Parameters.RawJSON(), rawSchema)
	}

	_, err = mustDecoder(t, 1<<20).Decode(strings.NewReader(`{"model":"gpt-5.3-codex","stream":true,"private_body":"do not expose"}`), "application/json")
	if err == nil {
		t.Fatal("unknown private field unexpectedly decoded")
	}
	if strings.Contains(err.Error(), "do not expose") {
		t.Fatalf("decoder error exposed request body: %v", err)
	}
}

func TestDecodeEnforcesFunctionToolCountAndSchemaLimits(t *testing.T) {
	tooMany := make([]string, bridge.DefaultMaxFunctionTools+1)
	for index := range tooMany {
		tooMany[index] = `{"type":"function","name":"tool_` + strconv.Itoa(index) + `","parameters":{"type":"object"}}`
	}
	body := `{"model":"gpt-5.3-codex","stream":true,"tools":[` + strings.Join(tooMany, ",") + `]}`
	_, err := mustDecoder(t, 2<<20).Decode(strings.NewReader(body), "application/json")
	assertDecodeError(t, err, ErrorInvalidRequest, "tools")

	largeSchema := `{"type":"object","description":"` + strings.Repeat("x", bridge.DefaultMaxFunctionSchemaBytes) + `"}`
	body = `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"large","parameters":` + largeSchema + `}]}`
	_, err = mustDecoder(t, 2<<20).Decode(strings.NewReader(body), "application/json")
	assertDecodeError(t, err, ErrorInvalidRequest, "tools")
}

func TestDecodeRejectsUnknownFieldsInsideKnownObjects(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{name: "message", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":[],"future":true}]}`, param: "input[0].<unknown_field>"},
		{name: "content", body: `{"model":"gpt-5.3-codex","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ok","future":true}]}]}`, param: "input[0].content[0].<unknown_field>"},
		{name: "function tool", body: `{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"exec","parameters":{},"future":true}]}`, param: "tools[0].<unknown_field>"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := mustDecoder(t, 1<<20).Decode(strings.NewReader(test.body), "application/json")
			assertDecodeError(t, err, ErrorUnsupportedField, test.param)
		})
	}
}

type bridgeGolden struct {
	Model              string                 `json:"model"`
	Instructions       string                 `json:"instructions"`
	InputKinds         []bridge.InputKind     `json:"input_kinds"`
	Inputs             []bridgeInputGolden    `json:"inputs"`
	ToolKinds          []bridge.ToolKind      `json:"tool_kinds"`
	ToolNames          []string               `json:"tool_names"`
	ToolDetails        []bridgeToolGolden     `json:"tool_details"`
	ParallelToolCalls  bool                   `json:"parallel_tool_calls"`
	ToolChoice         bridge.ToolChoiceKind  `json:"tool_choice"`
	ToolChoiceName     string                 `json:"tool_choice_name"`
	Stream             bool                   `json:"stream"`
	Include            []string               `json:"include"`
	PreviousResponseID bool                   `json:"previous_response_id"`
	ReasoningEffort    string                 `json:"reasoning_effort"`
	TextFormat         bridgeTextFormatGolden `json:"text_format"`
}

type bridgeInputGolden struct {
	Kind                 bridge.InputKind                  `json:"kind"`
	Message              *bridgeMessageGolden              `json:"message,omitempty"`
	FunctionCall         *bridgeFunctionCallGolden         `json:"function_call,omitempty"`
	FunctionCallOutput   *bridgeFunctionCallOutputGolden   `json:"function_call_output,omitempty"`
	CustomToolCall       *bridgeCustomToolCallGolden       `json:"custom_tool_call,omitempty"`
	CustomToolCallOutput *bridgeCustomToolCallOutputGolden `json:"custom_tool_call_output,omitempty"`
}

type bridgeMessageGolden struct {
	ID      string      `json:"id"`
	Role    bridge.Role `json:"role"`
	Content []string    `json:"content"`
}

type bridgeFunctionCallGolden struct {
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type bridgeFunctionCallOutputGolden struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type bridgeCustomToolCallGolden struct {
	ID     string `json:"id"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
	Status string `json:"status"`
}

type bridgeCustomToolCallOutputGolden struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type bridgeToolGolden struct {
	Kind        bridge.ToolKind `json:"kind"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  string          `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type bridgeTextFormatGolden struct {
	Kind        bridge.TextFormatKind `json:"kind"`
	Name        string                `json:"name,omitempty"`
	Description string                `json:"description,omitempty"`
	Schema      string                `json:"schema,omitempty"`
	Strict      *bool                 `json:"strict,omitempty"`
}

func bridgeGoldenFromRequest(request bridge.Request) bridgeGolden {
	result := bridgeGolden{
		Model:              request.Model,
		Instructions:       request.Instructions,
		InputKinds:         inputKinds(request.Input),
		Inputs:             bridgeInputGoldens(request.Input),
		ToolKinds:          toolKinds(request.Tools),
		ParallelToolCalls:  request.Generation.ParallelToolCalls,
		ToolChoice:         request.ToolChoice.Kind,
		ToolChoiceName:     request.ToolChoice.FunctionName,
		Stream:             request.Generation.Stream,
		Include:            request.Generation.Include,
		PreviousResponseID: request.PreviousResponseID != "",
		ReasoningEffort:    request.Generation.Reasoning.Effort,
		TextFormat: bridgeTextFormatGolden{
			Kind:        request.Generation.Text.Format.Kind,
			Name:        request.Generation.Text.Format.Name,
			Description: request.Generation.Text.Format.Description,
			Schema:      string(request.Generation.Text.Format.Schema.RawJSON()),
			Strict:      request.Generation.Text.Format.Strict,
		},
		ToolNames:   make([]string, 0, len(request.Tools)),
		ToolDetails: bridgeToolGoldens(request.Tools),
	}
	for _, tool := range request.Tools {
		switch tool := tool.(type) {
		case bridge.FunctionTool:
			result.ToolNames = append(result.ToolNames, tool.Name)
		case bridge.DeferredTool:
			result.ToolNames = append(result.ToolNames, tool.Name)
		}
	}
	return result
}

func bridgeInputGoldens(items []bridge.InputItem) []bridgeInputGolden {
	result := make([]bridgeInputGolden, 0, len(items))
	for _, item := range items {
		golden := bridgeInputGolden{Kind: item.Kind()}
		switch item := item.(type) {
		case bridge.Message:
			message := bridgeMessageGolden{ID: item.ID, Role: item.Role, Content: make([]string, 0, len(item.Content))}
			for _, content := range item.Content {
				text, ok := content.(bridge.TextContent)
				if !ok {
					panic("bridge message contains an unsupported content union")
				}
				message.Content = append(message.Content, text.Text)
			}
			golden.Message = &message
		case bridge.FunctionCall:
			golden.FunctionCall = &bridgeFunctionCallGolden{ID: item.ID, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Status: item.Status}
		case bridge.FunctionCallOutput:
			golden.FunctionCallOutput = &bridgeFunctionCallOutputGolden{CallID: item.CallID, Output: item.Output}
		case bridge.CustomToolCall:
			golden.CustomToolCall = &bridgeCustomToolCallGolden{ID: item.ID, CallID: item.CallID, Name: item.Name, Input: item.Input, Status: item.Status}
		case bridge.CustomToolCallOutput:
			golden.CustomToolCallOutput = &bridgeCustomToolCallOutputGolden{CallID: item.CallID, Output: item.Output}
		default:
			panic("bridge input contains an unsupported union")
		}
		result = append(result, golden)
	}
	return result
}

func bridgeToolGoldens(tools []bridge.Tool) []bridgeToolGolden {
	result := make([]bridgeToolGolden, 0, len(tools))
	for _, tool := range tools {
		golden := bridgeToolGolden{Kind: tool.Kind()}
		switch tool := tool.(type) {
		case bridge.FunctionTool:
			golden.Name = tool.Name
			golden.Description = tool.Description
			golden.Parameters = string(tool.Parameters.RawJSON())
			golden.Strict = tool.Strict
		case bridge.DeferredTool:
			golden.Name = tool.Name
		default:
			panic("bridge tools contain an unsupported union")
		}
		result = append(result, golden)
	}
	return result
}

func mustDecoder(t *testing.T, maxBytes int64) *Decoder {
	t.Helper()
	decoder, err := NewDecoder(maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return decoder
}

func fixtureBody(t *testing.T, path string) []byte {
	t.Helper()
	fixture, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Request struct {
			Body json.RawMessage `json:"body"`
		} `json:"request"`
	}
	if err := json.Unmarshal(fixture, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Request.Body) == 0 {
		t.Fatalf("%s has no request body", path)
	}
	return envelope.Request.Body
}

func inputKinds(items []bridge.InputItem) []bridge.InputKind {
	result := make([]bridge.InputKind, 0, len(items))
	for _, item := range items {
		result = append(result, item.Kind())
	}
	return result
}

func toolKinds(tools []bridge.Tool) []bridge.ToolKind {
	result := make([]bridge.ToolKind, 0, len(tools))
	for _, tool := range tools {
		result = append(result, tool.Kind())
	}
	return result
}

func assertDecodeError(t *testing.T, err error, code ErrorCode, param string) {
	t.Helper()
	if err == nil {
		t.Fatal("Decode unexpectedly succeeded")
	}
	var decodeErr *Error
	if !errors.As(err, &decodeErr) {
		t.Fatalf("error type = %T, want *codex.Error: %v", err, err)
	}
	if decodeErr.Code != code || decodeErr.Param != param || decodeErr.Message == "" {
		t.Fatalf("error = %#v, want code=%q param=%q and non-empty message", decodeErr, code, param)
	}
}
