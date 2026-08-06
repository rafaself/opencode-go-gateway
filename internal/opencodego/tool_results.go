package opencodego

import (
	"errors"
	"strings"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

var (
	ErrToolResultInvalid      = errors.New("tool result is invalid")
	ErrToolResultUnknownCall  = errors.New("tool result does not correlate to a tool call")
	ErrToolResultDuplicate    = errors.New("tool result is duplicated")
	ErrToolResultKindMismatch = errors.New("tool result kind does not match the tool call")
)

type toolCallResultMetadata struct {
	kind  bridge.ToolKind
	error bool
}

// TranslateToolResults translates the exact Codex result item shapes into
// provider-neutral values. It preserves output text, including empty strings,
// while reducing optional error/status wire forms to safe semantic metadata.
// Correlation to a retained provider turn is performed separately by
// ContinuationStore.Begin.
func TranslateToolResults(items []bridge.InputItem) ([]bridge.ToolResult, error) {
	calls := make(map[string]toolCallResultMetadata)
	seenResults := make(map[string]struct{})
	results := make([]bridge.ToolResult, 0)
	for _, item := range items {
		switch value := item.(type) {
		case bridge.FunctionCall:
			if value.CallID == "" || value.Name == "" {
				return nil, ErrToolResultInvalid
			}
			if _, exists := calls[value.CallID]; exists {
				return nil, ErrToolResultInvalid
			}
			calls[value.CallID] = toolCallResultMetadata{kind: bridge.ToolFunction, error: toolResultStatusIsError(value.Status)}
		case bridge.CustomToolCall:
			if value.CallID == "" || value.Name == "" {
				return nil, ErrToolResultInvalid
			}
			if _, exists := calls[value.CallID]; exists {
				return nil, ErrToolResultInvalid
			}
			calls[value.CallID] = toolCallResultMetadata{kind: bridge.ToolCustom, error: toolResultStatusIsError(value.Status)}
		case bridge.FunctionCallOutput:
			result, err := toolResult(value.CallID, bridge.ToolFunction, value.Output, value.Status, value.Error, calls, seenResults)
			if err != nil {
				return nil, err
			}
			results = append(results, result)
		case bridge.CustomToolCallOutput:
			result, err := toolResult(value.CallID, bridge.ToolCustom, value.Output, value.Status, value.Error, calls, seenResults)
			if err != nil {
				return nil, err
			}
			results = append(results, result)
		}
	}
	return results, nil
}

func toolResult(callID string, kind bridge.ToolKind, output, status string, errorMarker bool, calls map[string]toolCallResultMetadata, seenResults map[string]struct{}) (bridge.ToolResult, error) {
	if callID == "" {
		return bridge.ToolResult{}, ErrToolResultInvalid
	}
	callMetadata, exists := calls[callID]
	if !exists {
		return bridge.ToolResult{}, ErrToolResultUnknownCall
	}
	if callMetadata.kind != kind {
		return bridge.ToolResult{}, ErrToolResultKindMismatch
	}
	if _, exists := seenResults[callID]; exists {
		return bridge.ToolResult{}, ErrToolResultDuplicate
	}
	seenResults[callID] = struct{}{}
	return bridge.ToolResult{
		CallID: callID,
		Kind:   kind,
		Output: output,
		Status: status,
		Error:  errorMarker || callMetadata.error || toolResultStatusIsError(status),
	}, nil
}

func toolResultStatusIsError(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "failure", "cancelled", "canceled", "incomplete":
		return true
	default:
		return false
	}
}
