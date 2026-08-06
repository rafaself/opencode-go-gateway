package opencodego

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestMapRequestPreservesFunctionToolContractAndStrictness(t *testing.T) {
	schema := mustSchema(t, `{ "type": "object", "properties": { "query": { "type": "string" } }, "required": ["query"] }`)
	strict := true
	request := minimalRequest()
	request.Tools = []bridge.Tool{bridge.FunctionTool{
		Name:        "lookup",
		Description: "look up a value",
		Parameters:  schema,
		Strict:      &strict,
	}}

	mapped, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Tools) != 1 || mapped.Tools[0].Type != "function" {
		t.Fatalf("mapped tools = %#v", mapped.Tools)
	}
	function := mapped.Tools[0].Function
	if function.Name != "lookup" || function.Description != "look up a value" || function.Strict == nil || !*function.Strict {
		t.Fatalf("mapped function = %#v", function)
	}
	if !bytes.Equal(function.Parameters, schema.RawJSON()) {
		t.Fatalf("schema bytes changed: got %s want %s", function.Parameters, schema.RawJSON())
	}
	if mapped.ToolChoice != nil {
		t.Fatalf("thinking-mode auto choice must be omitted: %#v", mapped.ToolChoice)
	}
}

func TestMapRequestRejectsToolCountAndSchemaAggregateLimits(t *testing.T) {
	schema := mustSchema(t, `{"type":"object"}`)
	tooMany := minimalRequest()
	tooMany.Tools = make([]bridge.Tool, bridge.DefaultMaxFunctionTools+1)
	for index := range tooMany.Tools {
		tooMany.Tools[index] = bridge.FunctionTool{Name: "tool_" + strconv.Itoa(index), Parameters: schema}
	}
	_, err := MapRequest(tooMany, DefaultModel)
	assertProviderCode(t, err, ErrorRequestTooLarge)

	largeSchema := mustSchema(t, `{"type":"object","description":"`+strings.Repeat("x", bridge.DefaultMaxFunctionSchemaBytes)+`"}`)
	tooLarge := minimalRequest()
	tooLarge.Tools = []bridge.Tool{bridge.FunctionTool{Name: "large", Parameters: largeSchema}}
	_, err = MapRequest(tooLarge, DefaultModel)
	assertProviderCode(t, err, ErrorRequestTooLarge)
}

func TestMapRequestProviderToolBudgetIncludesImplicitApplyPatch(t *testing.T) {
	for _, test := range []struct {
		name           string
		functionCount  int
		explicitCustom bool
		wantProvider   int
		wantError      bool
	}{
		{name: "127 implicit", functionCount: 127, wantProvider: 128},
		{name: "128 implicit", functionCount: 128, wantError: true},
		{name: "127 explicit", functionCount: 127, explicitCustom: true, wantProvider: 128},
		{name: "128 explicit", functionCount: 128, explicitCustom: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := requestWithFunctionTools(t, test.functionCount, test.explicitCustom)
			mapped, err := MapRequest(request, DefaultModel)
			if test.wantError {
				if err == nil {
					t.Fatal("provider tool budget overflow was accepted")
				}
				assertProviderCode(t, err, ErrorRequestTooLarge)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(mapped.Tools) != test.wantProvider {
				t.Fatalf("provider tool count = %d, want %d", len(mapped.Tools), test.wantProvider)
			}
			wrapperCount := 0
			for _, tool := range mapped.Tools {
				if tool.Function.Name == ApplyPatchUpstreamName {
					wrapperCount++
				}
			}
			if wrapperCount != 1 {
				t.Fatalf("apply_patch wrapper count = %d, want 1", wrapperCount)
			}
		})
	}
}

func TestMapRequestChargesApplyPatchWrapperSchemaOnceAtAggregateBoundary(t *testing.T) {
	wrapperBytes := int(ApplyPatchWrapperSchemaBytes())
	for _, test := range []struct {
		name       string
		schemaSize int
		wantError  bool
	}{
		{name: "exact total", schemaSize: bridge.DefaultMaxFunctionSchemaBytes - wrapperBytes},
		{name: "one byte over total", schemaSize: bridge.DefaultMaxFunctionSchemaBytes - wrapperBytes + 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := minimalRequest()
			request.Tools = []bridge.Tool{bridge.FunctionTool{
				Name:       "lookup",
				Parameters: schemaWithExactLength(t, test.schemaSize),
			}}
			registry, err := NewToolRegistry(request)
			if err != nil {
				t.Fatal(err)
			}
			request.ToolRegistry = registry

			mapped, err := MapRequest(request, DefaultModel)
			if test.wantError {
				if err == nil {
					t.Fatal("schema budget overflow was accepted")
				}
				assertProviderCode(t, err, ErrorRequestTooLarge)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(mapped.Tools) != 2 {
				t.Fatalf("mapped tools = %d, want 2", len(mapped.Tools))
			}
		})
	}
}

func requestWithFunctionTools(t *testing.T, count int, explicitCustom bool) bridge.Request {
	t.Helper()
	request := minimalRequest()
	request.Tools = make([]bridge.Tool, 0, count+1)
	for index := 0; index < count; index++ {
		request.Tools = append(request.Tools, bridge.FunctionTool{
			Name:       "tool_" + strconv.Itoa(index),
			Parameters: mustSchema(t, `{"type":"object"}`),
		})
	}
	if explicitCustom {
		request.Tools = append(request.Tools, bridge.CustomTool{Name: ApplyPatchToolName, Format: bridge.CustomToolFormat{Kind: bridge.CustomToolFormatText}})
	}
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolRegistry = registry
	return request
}

func schemaWithExactLength(t *testing.T, size int) bridge.JSONSchema {
	t.Helper()
	const prefix = `{"type":"object","description":"`
	const suffix = `"}`
	if size < len(prefix)+len(suffix) {
		t.Fatalf("schema size %d is too small", size)
	}
	raw := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
	if len(raw) != size {
		t.Fatalf("schema length = %d, want %d", len(raw), size)
	}
	return mustSchema(t, raw)
}

func TestMapRequestRejectsForcedAndNamedToolChoices(t *testing.T) {
	request := minimalRequest()
	request.Tools = []bridge.Tool{bridge.FunctionTool{Name: "lookup", Parameters: mustSchema(t, `{"type":"object"}`)}}
	for _, choice := range []bridge.ToolChoice{
		{Kind: bridge.ToolChoiceRequired},
		{Kind: bridge.ToolChoiceFunction, FunctionName: "lookup"},
	} {
		request.ToolChoice = choice
		_, err := MapRequest(request, DefaultModel)
		var providerErr *ProviderError
		if !errors.As(err, &providerErr) || providerErr.Code != ErrorUnsupportedToolChoice {
			t.Fatalf("choice %#v error = %v, want %s", choice, err, ErrorUnsupportedToolChoice)
		}
	}
}
