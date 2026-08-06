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
	assertProviderCode(t, err, ErrorInvalidRequest)

	largeSchema := mustSchema(t, `{"type":"object","description":"`+strings.Repeat("x", bridge.DefaultMaxFunctionSchemaBytes)+`"}`)
	tooLarge := minimalRequest()
	tooLarge.Tools = []bridge.Tool{bridge.FunctionTool{Name: "large", Parameters: largeSchema}}
	_, err = MapRequest(tooLarge, DefaultModel)
	assertProviderCode(t, err, ErrorInvalidRequest)
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
