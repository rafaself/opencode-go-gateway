package bridge

import (
	"testing"
	"time"
)

func TestStreamEventsExposeProviderNeutralSemantics(t *testing.T) {
	events := []StreamEvent{
		ResponseStarted{ID: "chatcmpl-1", CreatedAt: time.Unix(42, 0).UTC(), Model: "deepseek-v4-flash"},
		TextDelta{ChoiceIndex: 0, Text: "hello"},
		ReasoningDelta{ChoiceIndex: 0, Text: "private"},
		ToolCallStarted{Key: ToolCallKey{ChoiceIndex: 0, ToolIndex: 1}, Kind: ToolFunction, CallID: "call-1", Name: "exec"},
		ToolCallMetadataDelta{Key: ToolCallKey{ChoiceIndex: 0, ToolIndex: 1}, CallID: "call-1", Name: "exec"},
		ToolCallArgumentsDelta{Key: ToolCallKey{ChoiceIndex: 0, ToolIndex: 1}, Arguments: "{}"},
		ToolCallCompleted{Key: ToolCallKey{ChoiceIndex: 0, ToolIndex: 1}, Kind: ToolFunction, CallID: "call-1", Name: "exec", Arguments: "{}"},
		UsageUpdated{Usage: Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
		Completed{Reason: "stop"},
	}

	for _, event := range events {
		if event == nil || event.StreamEventKind() == "" {
			t.Fatalf("event %#v has no semantic kind", event)
		}
	}
}
