package opencodego

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestNewToolRegistryRegistersApplyPatchPerRequestAndRejectsReservedCollision(t *testing.T) {
	request := minimalRequest()
	request.Tools = []bridge.Tool{
		bridge.FunctionTool{Name: "lookup", Parameters: mustSchema(t, `{"type":"object"}`)},
		bridge.DeferredTool{ToolKind: bridge.ToolNamespace, Name: "mcp"},
	}
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil {
		t.Fatal("registry is nil")
	}
	registration, ok := registry.Inbound(ApplyPatchToolName)
	if !ok || registration.UpstreamName != ApplyPatchUpstreamName || registration.WrapperField != ApplyPatchWrapperField {
		t.Fatalf("apply_patch registration = %#v, %v", registration, ok)
	}

	request.Tools = append(request.Tools, bridge.FunctionTool{Name: ApplyPatchUpstreamName, Parameters: mustSchema(t, `{"type":"object"}`)})
	if _, err := NewToolRegistry(request); err == nil {
		t.Fatal("reserved synthetic function name was accepted")
	} else {
		assertProviderCode(t, err, ErrorInvalidRequest)
	}
	for _, name := range []string{ApplyPatchToolName, ApplyPatchUpstreamName} {
		collisionRequest := minimalRequest()
		collisionRequest.Tools = []bridge.Tool{bridge.FunctionTool{Name: name, Parameters: mustSchema(t, `{"type":"object"}`)}}
		if _, err := MapRequest(collisionRequest, DefaultModel); err == nil {
			t.Fatalf("direct mapping accepted reserved name %q", name)
		}
	}
}

func TestMapRequestWrapsCustomApplyPatchAndPreservesExactInput(t *testing.T) {
	patch := "*** Begin Patch\r\n*** Update File: café.txt\r\n+Olá, 世界 \"quoted\" \\ literal\r\n*** End Patch"
	request := minimalRequest()
	request.Tools = []bridge.Tool{bridge.FunctionTool{Name: "lookup", Parameters: mustSchema(t, `{"type":"object"}`)}}
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolRegistry = registry
	request.Input = []bridge.InputItem{
		bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "continue"}}},
		bridge.CustomToolCall{CallID: "custom-call", Name: ApplyPatchToolName, Input: patch, Status: "completed"},
		bridge.CustomToolCallOutput{CallID: "custom-call", Output: "applied"},
	}

	mapped, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Tools) != 2 {
		t.Fatalf("mapped tools = %#v", mapped.Tools)
	}
	var wrapped ChatCompletionTool
	for _, tool := range mapped.Tools {
		if tool.Function.Name == ApplyPatchUpstreamName {
			wrapped = tool
		}
	}
	if wrapped.Function.Name != ApplyPatchUpstreamName || wrapped.Function.Strict == nil || !*wrapped.Function.Strict {
		t.Fatalf("wrapped function = %#v", wrapped)
	}
	var schema map[string]any
	if err := json.Unmarshal(wrapped.Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("wrapper schema = %#v", schema)
	}
	properties := schema["properties"].(map[string]any)
	if len(properties) != 1 || properties[ApplyPatchWrapperField].(map[string]any)["type"] != "string" {
		t.Fatalf("wrapper properties = %#v", properties)
	}

	if len(mapped.Messages) != 3 {
		t.Fatalf("mapped messages = %#v", mapped.Messages)
	}
	call := mapped.Messages[1].ToolCalls[0]
	if call.ID != "custom-call" || call.Function.Name != ApplyPatchUpstreamName {
		t.Fatalf("wrapped call = %#v", call)
	}
	var arguments map[string]string
	if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
		t.Fatal(err)
	}
	if arguments[ApplyPatchWrapperField] != patch {
		t.Fatalf("patch changed through wrapper: got %q want %q", arguments[ApplyPatchWrapperField], patch)
	}
	if mapped.Messages[2].ToolCallID != "custom-call" || mapped.Messages[2].Content == nil || *mapped.Messages[2].Content != "applied" {
		t.Fatalf("custom result = %#v", mapped.Messages[2])
	}
	encoded, _ := json.Marshal(mapped)
	if !strings.Contains(string(encoded), `"name":"`+ApplyPatchUpstreamName+`"`) {
		t.Fatalf("synthetic function name missing from provider request: %s", encoded)
	}
}

func TestMapRequestPreservesEmptyCustomApplyPatchInput(t *testing.T) {
	request := minimalRequest()
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolRegistry = registry
	request.Input = []bridge.InputItem{
		bridge.CustomToolCall{CallID: "empty-call", Name: ApplyPatchToolName, Input: ""},
	}
	mapped, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if got := mapped.Messages[0].ToolCalls[0].Function.Arguments; got != `{"input":""}` {
		t.Fatalf("empty wrapper = %q", got)
	}
}

func TestMapRequestAutoRegistersDeclaredResponsesCustomTool(t *testing.T) {
	request := minimalRequest()
	request.Tools = []bridge.Tool{bridge.CustomTool{
		Name:        ApplyPatchToolName,
		Description: "patches",
		Format:      bridge.CustomToolFormat{Kind: bridge.CustomToolFormatText},
	}}
	mapped, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Tools) != 1 || mapped.Tools[0].Function.Name != ApplyPatchUpstreamName {
		t.Fatalf("declared custom mapping = %#v", mapped.Tools)
	}
	if mapped.Tools[0].Function.Description != "Apply the exact patch text supplied by Codex to the current workspace." {
		t.Fatalf("synthetic description = %q", mapped.Tools[0].Function.Description)
	}
}

func TestMapRequestRejectsInvalidCustomApplyPatchInputAndKeepsGenericMappingUnchanged(t *testing.T) {
	request := minimalRequest()
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolRegistry = registry
	for _, item := range []bridge.InputItem{
		bridge.CustomToolCall{CallID: "call", Name: "other", Input: "patch"},
		bridge.CustomToolCall{CallID: "call", Name: ApplyPatchToolName, Input: strings.Repeat("x", DefaultMaxApplyPatchInputBytes+1)},
	} {
		request.Input = []bridge.InputItem{item}
		_, err := MapRequest(request, DefaultModel)
		if err == nil {
			t.Fatalf("invalid custom input %#v was accepted", item)
		}
		assertProviderCode(t, err, ErrorInvalidRequest)
	}

	generic := minimalRequest()
	if mapped, err := MapRequest(generic, DefaultModel); err != nil || len(mapped.Tools) != 0 {
		t.Fatalf("generic mapping changed: mapped=%#v err=%v", mapped, err)
	}
}

func TestApplyPatchInputLimitIsExclusiveThroughWrapper(t *testing.T) {
	const marker = "sensitive-patch-marker"
	for _, test := range []struct {
		name string
		size int
		ok   bool
	}{
		{name: "one byte below", size: DefaultMaxApplyPatchInputBytes - 1, ok: true},
		{name: "exact boundary", size: DefaultMaxApplyPatchInputBytes},
		{name: "one byte above", size: DefaultMaxApplyPatchInputBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.Repeat("x", test.size-len(marker)) + marker
			wrapped, err := wrapApplyPatchInput(input)
			if test.ok {
				if err != nil {
					t.Fatalf("wrap error = %v", err)
				}
				var decoded struct {
					Input string `json:"input"`
				}
				if err := json.Unmarshal([]byte(wrapped), &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.Input != input {
					t.Fatal("accepted wrapper changed the exact input")
				}
				return
			}
			if err == nil || !errors.Is(err, ErrApplyPatchInputLimit) {
				t.Fatalf("wrap error = %v, want the stable input-limit error", err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("input marker leaked through the wrapper error")
			}
		})
	}
}
