package bridge

import "time"

// StreamEventKind identifies a provider-neutral semantic event. Wire event
// names, Responses IDs, output indexes, and sequence numbers deliberately do
// not cross this boundary.
type StreamEventKind string

const (
	StreamResponseStarted        StreamEventKind = "response_started"
	StreamTextDelta              StreamEventKind = "text_delta"
	StreamToolCallStarted        StreamEventKind = "tool_call_started"
	StreamToolCallMetadataDelta  StreamEventKind = "tool_call_metadata_delta"
	StreamToolCallArgumentsDelta StreamEventKind = "tool_call_arguments_delta"
	StreamToolCallCompleted      StreamEventKind = "tool_call_completed"
	StreamReasoningDelta         StreamEventKind = "reasoning_delta"
	StreamUsageUpdated           StreamEventKind = "usage_updated"
	StreamCompleted              StreamEventKind = "completed"
	StreamIncomplete             StreamEventKind = "incomplete"
	StreamFailed                 StreamEventKind = "failed"
)

// StreamEvent is the semantic stream boundary consumed by an output adapter.
// The interface is intentionally small so a later provider can add an
// adapter-specific event without importing a Responses wire package.
type StreamEvent interface {
	StreamEventKind() StreamEventKind
}

type ToolCallKey struct {
	ChoiceIndex int
	ToolIndex   int
}

type ResponseStarted struct {
	ID        string
	CreatedAt time.Time
	Model     string
}

func (ResponseStarted) StreamEventKind() StreamEventKind { return StreamResponseStarted }

type TextDelta struct {
	ChoiceIndex int
	Text        string
}

func (TextDelta) StreamEventKind() StreamEventKind { return StreamTextDelta }

type ToolCallStarted struct {
	Key    ToolCallKey
	Kind   ToolKind
	CallID string
	Name   string
}

func (ToolCallStarted) StreamEventKind() StreamEventKind { return StreamToolCallStarted }

// ToolCallMetadataDelta carries fragmented provider ID/name fields. The
// values are fragments, not a replacement for the accumulated metadata.
type ToolCallMetadataDelta struct {
	Key    ToolCallKey
	CallID string
	Name   string
}

func (ToolCallMetadataDelta) StreamEventKind() StreamEventKind { return StreamToolCallMetadataDelta }

type ToolCallArgumentsDelta struct {
	Key       ToolCallKey
	Arguments string
}

func (ToolCallArgumentsDelta) StreamEventKind() StreamEventKind {
	return StreamToolCallArgumentsDelta
}

type ToolCallCompleted struct {
	Key       ToolCallKey
	Kind      ToolKind
	CallID    string
	Name      string
	Arguments string
}

func (ToolCallCompleted) StreamEventKind() StreamEventKind { return StreamToolCallCompleted }

type ReasoningDelta struct {
	ChoiceIndex int
	Text        string
}

func (ReasoningDelta) StreamEventKind() StreamEventKind { return StreamReasoningDelta }

// Usage is provider-neutral token accounting. A provider may omit any or all
// fields; zero means unavailable, not a synthesized count.
type Usage struct {
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
	ReasoningTokens       int
}

type UsageUpdated struct {
	Usage Usage
}

func (UsageUpdated) StreamEventKind() StreamEventKind { return StreamUsageUpdated }

type Completed struct {
	Reason string
}

func (Completed) StreamEventKind() StreamEventKind { return StreamCompleted }

type Incomplete struct {
	Reason string
}

func (Incomplete) StreamEventKind() StreamEventKind { return StreamIncomplete }

// Failed contains a stable semantic code and a safe, adapter-selected
// message. Provider payloads must not be copied into logs or user-visible
// diagnostics without an explicit privacy review.
type Failed struct {
	Code    string
	Message string
}

func (Failed) StreamEventKind() StreamEventKind { return StreamFailed }
