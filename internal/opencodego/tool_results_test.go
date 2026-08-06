package opencodego

import (
	"errors"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestTranslateToolResultsPreservesKindsTextAndSemanticStatus(t *testing.T) {
	items := []bridge.InputItem{
		bridge.FunctionCall{CallID: "function-call", Name: "lookup", Arguments: `{}`, Status: "failed"},
		bridge.CustomToolCall{CallID: "custom-call", Name: ApplyPatchToolName, Input: "patch"},
		bridge.FunctionCallOutput{CallID: "function-call", Output: "", Status: "completed"},
		bridge.CustomToolCallOutput{CallID: "custom-call", Output: "exact\nerror", Status: "failed"},
	}
	results, err := TranslateToolResults(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	if results[0] != (bridge.ToolResult{CallID: "function-call", Kind: bridge.ToolFunction, Output: "", Status: "completed", Error: true}) {
		t.Fatalf("empty result = %#v", results[0])
	}
	want := bridge.ToolResult{CallID: "custom-call", Kind: bridge.ToolCustom, Output: "exact\nerror", Status: "failed", Error: true}
	if results[1] != want {
		t.Fatalf("error result = %#v, want %#v", results[1], want)
	}
}

func TestTranslateToolResultsRejectsCorrelationViolations(t *testing.T) {
	tests := []struct {
		name string
		item []bridge.InputItem
		want error
	}{
		{
			name: "unknown",
			item: []bridge.InputItem{bridge.FunctionCallOutput{CallID: "missing", Output: "x"}},
			want: nil,
		},
		{
			name: "duplicate",
			item: []bridge.InputItem{
				bridge.FunctionCall{CallID: "call", Name: "lookup"},
				bridge.FunctionCallOutput{CallID: "call", Output: "one"},
				bridge.FunctionCallOutput{CallID: "call", Output: "two"},
			},
			want: ErrToolResultDuplicate,
		},
		{
			name: "output before call",
			item: []bridge.InputItem{
				bridge.CustomToolCallOutput{CallID: "call", Output: "x"},
			},
			want: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results, err := TranslateToolResults(test.item)
			if test.want == nil {
				if err != nil || len(results) != 1 || results[0].CallID == "" {
					t.Fatalf("output-only translation = %#v, error = %v", results, err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestTranslateToolResultsChecksLocalKindButDefersStoredCorrelation(t *testing.T) {
	items := []bridge.InputItem{
		bridge.FunctionCallOutput{CallID: "function-call", Output: "function output"},
		bridge.FunctionCall{CallID: "function-call", Name: "lookup", Arguments: "{}"},
		bridge.CustomToolCallOutput{CallID: "custom-call", Output: "custom output"},
	}
	results, err := TranslateToolResults(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Kind != bridge.ToolFunction || results[1].Kind != bridge.ToolCustom {
		t.Fatalf("results = %#v", results)
	}

	_, err = TranslateToolResults([]bridge.InputItem{
		bridge.FunctionCall{CallID: "call", Name: "lookup"},
		bridge.CustomToolCallOutput{CallID: "call", Output: "wrong kind"},
	})
	if !errors.Is(err, ErrToolResultKindMismatch) {
		t.Fatalf("kind error = %v, want %v", err, ErrToolResultKindMismatch)
	}
}
