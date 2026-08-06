package opencodego

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

var (
	ErrDuplicateStreamTerminal = errors.New("duplicate upstream stream terminal")
	ErrMalformedStream         = errors.New("malformed upstream stream")
	ErrStreamAggregateLimit    = errors.New("stream aggregate limit exceeded")
)

// ChatCompletionStreamEvent is the provider-owned result of decoding one SSE
// data event. Done is true only for the upstream [DONE] marker; callers must
// consume it and must never forward it to a downstream Responses stream.
type ChatCompletionStreamEvent struct {
	Chunk *ChatCompletionChunk
	Error *ProviderStreamError
	Done  bool
}

// ProviderStreamError is the structured error payload accepted from an
// upstream Chat Completions stream. It is kept typed so an orchestrator can
// choose a policy without copying the raw JSON into diagnostics.
type ProviderStreamError struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type ChatCompletionStreamDecoder struct {
	sse       *SSEDecoder
	doneSeen  bool
	errorSeen bool
}

func NewChatCompletionStreamDecoder(reader io.Reader, options SSEDecoderOptions) *ChatCompletionStreamDecoder {
	return &ChatCompletionStreamDecoder{sse: NewSSEDecoder(reader, options)}
}

// Next decodes one provider SSE event. A provider error is returned as a
// typed event with a nil Go error; malformed transport/JSON is returned as a
// stable error without embedding untrusted payload text.
func (decoder *ChatCompletionStreamDecoder) Next() (ChatCompletionStreamEvent, error) {
	if decoder == nil || decoder.sse == nil {
		return ChatCompletionStreamEvent{}, io.EOF
	}
	if decoder.doneSeen || decoder.errorSeen {
		_, err := decoder.sse.Next()
		if errors.Is(err, io.EOF) {
			return ChatCompletionStreamEvent{}, io.EOF
		}
		if err != nil {
			return ChatCompletionStreamEvent{}, err
		}
		return ChatCompletionStreamEvent{}, ErrDuplicateStreamTerminal
	}
	event, err := decoder.sse.Next()
	if err != nil {
		return ChatCompletionStreamEvent{}, err
	}
	if strings.TrimSpace(event.Data) == "[DONE]" {
		decoder.doneSeen = true
		return ChatCompletionStreamEvent{Done: true}, nil
	}
	if event.Data == "" {
		return ChatCompletionStreamEvent{}, ErrMalformedStream
	}

	var envelope struct {
		Error *ProviderStreamError `json:"error"`
	}
	if err := json.Unmarshal([]byte(event.Data), &envelope); err != nil {
		return ChatCompletionStreamEvent{}, wrapStreamDecodeError(err)
	}
	if envelope.Error != nil {
		decoder.errorSeen = true
		return ChatCompletionStreamEvent{Error: envelope.Error}, nil
	}

	var chunk ChatCompletionChunk
	if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
		return ChatCompletionStreamEvent{}, wrapStreamDecodeError(err)
	}
	if chunk.ID == "" && chunk.Object == "" && chunk.Model == "" && chunk.Created == 0 && len(chunk.Choices) == 0 && chunk.Usage == nil {
		return ChatCompletionStreamEvent{}, ErrMalformedStream
	}
	return ChatCompletionStreamEvent{Chunk: &chunk}, nil
}

func wrapStreamDecodeError(cause error) error {
	return &streamDecodeError{cause: cause}
}

type streamDecodeError struct {
	cause error
}

func (err *streamDecodeError) Error() string { return ErrMalformedStream.Error() }

func (err *streamDecodeError) Unwrap() error { return errors.Join(ErrMalformedStream, err.cause) }

// BridgeStreamDecoder translates provider chunks into semantic events. It
// owns only provider reconstruction state (choice/tool indexes and fragments);
// Responses IDs, output indexes, and sequence numbers remain in internal/codex.
type BridgeStreamDecoder struct {
	provider *ChatCompletionStreamDecoder
	pending  []bridge.StreamEvent
	calls    map[bridge.ToolCallKey]*providerToolCall

	started           bool
	terminal          bool
	finishReason      string
	finishConflict    bool
	finishedChoices   map[int]bool
	aggregateBytes    int
	maxAggregateBytes int
}

type providerToolCall struct {
	key       bridge.ToolCallKey
	callID    []byte
	name      []byte
	arguments []byte
	completed bool
}

func NewBridgeStreamDecoder(reader io.Reader, options SSEDecoderOptions) *BridgeStreamDecoder {
	options = options.withDefaults()
	return &BridgeStreamDecoder{
		provider:          NewChatCompletionStreamDecoder(reader, options),
		calls:             make(map[bridge.ToolCallKey]*providerToolCall),
		finishedChoices:   make(map[int]bool),
		maxAggregateBytes: options.MaxAggregateBytes,
	}
}

func (decoder *BridgeStreamDecoder) Next() (bridge.StreamEvent, error) {
	if decoder == nil || decoder.provider == nil {
		return nil, io.EOF
	}
	if len(decoder.pending) > 0 {
		event := decoder.pending[0]
		decoder.pending = decoder.pending[1:]
		return event, nil
	}
	if decoder.terminal {
		return nil, io.EOF
	}

	providerEvent, err := decoder.provider.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			decoder.terminal = true
			return bridge.Failed{Code: "upstream_eof", Message: "upstream stream ended before a terminal event"}, nil
		}
		decoder.terminal = true
		if errors.Is(err, ErrSSELimitExceeded) {
			return bridge.Failed{Code: "stream_limit_exceeded", Message: "The upstream stream exceeded its limit."}, nil
		}
		return bridge.Failed{Code: "upstream_stream_error", Message: "upstream stream could not be decoded"}, nil
	}
	if providerEvent.Error != nil {
		decoder.terminal = true
		return bridge.Failed{Code: "upstream_error", Message: "upstream provider returned a stream error"}, nil
	}
	if providerEvent.Done {
		if err := decoder.finish(); err != nil {
			decoder.terminal = true
			decoder.pending = nil
			return decoder.bridgeFailure(err), nil
		}
		decoder.terminal = true
		if len(decoder.pending) == 0 {
			return bridge.Failed{Code: "upstream_terminal_error", Message: "upstream stream did not report a terminal reason"}, nil
		}
		event := decoder.pending[0]
		decoder.pending = decoder.pending[1:]
		return event, nil
	}
	if providerEvent.Chunk == nil {
		decoder.terminal = true
		return bridge.Failed{Code: "upstream_stream_error", Message: "upstream stream contained no response chunk"}, nil
	}
	if err := decoder.consumeChunk(*providerEvent.Chunk); err != nil {
		decoder.terminal = true
		decoder.pending = nil
		return decoder.bridgeFailure(err), nil
	}
	if len(decoder.pending) == 0 {
		return decoder.Next()
	}
	event := decoder.pending[0]
	decoder.pending = decoder.pending[1:]
	return event, nil
}

func (decoder *BridgeStreamDecoder) bridgeFailure(err error) bridge.Failed {
	if errors.Is(err, ErrStreamAggregateLimit) {
		return bridge.Failed{Code: "stream_limit_exceeded", Message: "The upstream stream exceeded its limit."}
	}
	return bridge.Failed{Code: "upstream_stream_error", Message: "upstream stream could not be decoded"}
}

func (decoder *BridgeStreamDecoder) consumeChunk(chunk ChatCompletionChunk) error {
	if !decoder.started {
		decoder.started = true
		createdAt := time.Time{}
		if chunk.Created != 0 {
			createdAt = time.Unix(chunk.Created, 0).UTC()
		}
		if err := decoder.queue(bridge.ResponseStarted{
			ID:        chunk.ID,
			CreatedAt: createdAt,
			Model:     chunk.Model,
		}, len(chunk.ID)+len(chunk.Model)); err != nil {
			return err
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Index < 0 || decoder.finishedChoices[choice.Index] {
			return ErrMalformedStream
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			decoder.recordFinishReason(*choice.FinishReason)
		}
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if err := decoder.queue(bridge.TextDelta{ChoiceIndex: choice.Index, Text: *choice.Delta.Content}, len(*choice.Delta.Content)); err != nil {
				return err
			}
		}
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			if err := decoder.queue(bridge.ReasoningDelta{ChoiceIndex: choice.Index, Text: *choice.Delta.ReasoningContent}, len(*choice.Delta.ReasoningContent)); err != nil {
				return err
			}
		}
		for _, tool := range choice.Delta.ToolCalls {
			if err := decoder.consumeToolCall(choice.Index, tool); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			decoder.finishedChoices[choice.Index] = true
		}
	}
	if chunk.Usage != nil {
		usage := bridge.Usage{
			PromptTokens:          chunk.Usage.PromptTokens,
			CompletionTokens:      chunk.Usage.CompletionTokens,
			TotalTokens:           chunk.Usage.TotalTokens,
			PromptCacheHitTokens:  chunk.Usage.PromptCacheHitTokens,
			PromptCacheMissTokens: chunk.Usage.PromptCacheMissTokens,
		}
		if chunk.Usage.PromptTokensDetails != nil {
			usage.PromptCacheHitTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		if chunk.Usage.CompletionTokensDetails != nil {
			usage.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		if err := decoder.queue(bridge.UsageUpdated{Usage: usage}, 0); err != nil {
			return err
		}
	}
	return nil
}

func (decoder *BridgeStreamDecoder) consumeToolCall(choiceIndex int, tool ToolCall) error {
	if tool.Index == nil || *tool.Index < 0 {
		return ErrMalformedStream
	}
	toolIndex := *tool.Index
	key := bridge.ToolCallKey{ChoiceIndex: choiceIndex, ToolIndex: toolIndex}
	state, exists := decoder.calls[key]
	if !exists {
		state = &providerToolCall{key: key}
		decoder.calls[key] = state
		if err := decoder.queue(bridge.ToolCallStarted{
			Key:    key,
			Kind:   bridge.ToolFunction,
			CallID: tool.ID,
			Name:   tool.Function.Name,
		}, len(tool.ID)+len(tool.Function.Name)); err != nil {
			return err
		}
	} else if tool.ID != "" || tool.Function.Name != "" {
		if err := decoder.queue(bridge.ToolCallMetadataDelta{
			Key:    key,
			CallID: tool.ID,
			Name:   tool.Function.Name,
		}, len(tool.ID)+len(tool.Function.Name)); err != nil {
			return err
		}
	}
	state.callID = append(state.callID, tool.ID...)
	state.name = append(state.name, tool.Function.Name...)
	state.arguments = append(state.arguments, tool.Function.Arguments...)
	if tool.Function.Arguments != "" {
		if err := decoder.queue(bridge.ToolCallArgumentsDelta{Key: key, Arguments: tool.Function.Arguments}, len(tool.Function.Arguments)); err != nil {
			return err
		}
	}
	return nil
}

const bridgeStreamEventOverhead = 64

func (decoder *BridgeStreamDecoder) queue(event bridge.StreamEvent, payloadBytes int) error {
	if payloadBytes < 0 || bridgeStreamEventOverhead > decoder.maxAggregateBytes || payloadBytes > decoder.maxAggregateBytes-bridgeStreamEventOverhead || decoder.aggregateBytes > decoder.maxAggregateBytes-(bridgeStreamEventOverhead+payloadBytes) {
		return ErrStreamAggregateLimit
	}
	decoder.aggregateBytes += bridgeStreamEventOverhead + payloadBytes
	decoder.pending = append(decoder.pending, event)
	return nil
}

func (decoder *BridgeStreamDecoder) recordFinishReason(reason string) {
	if decoder.finishReason == "" {
		decoder.finishReason = reason
		return
	}
	if decoder.finishReason != reason {
		decoder.finishConflict = true
	}
}

func (decoder *BridgeStreamDecoder) finish() error {
	if decoder.finishConflict || decoder.finishReason == "" {
		return decoder.queue(bridge.Failed{Code: "upstream_terminal_error", Message: "upstream stream reported inconsistent terminal state"}, 0)
	}
	for _, key := range sortedToolKeys(decoder.calls) {
		state := decoder.calls[key]
		if state.completed {
			continue
		}
		state.completed = true
		if err := decoder.queue(bridge.ToolCallCompleted{
			Key:       state.key,
			Kind:      bridge.ToolFunction,
			CallID:    string(state.callID),
			Name:      string(state.name),
			Arguments: string(state.arguments),
		}, len(state.callID)+len(state.name)+len(state.arguments)); err != nil {
			return err
		}
	}
	switch decoder.finishReason {
	case "stop", "tool_calls":
		return decoder.queue(bridge.Completed{Reason: decoder.finishReason}, 0)
	case "length":
		return decoder.queue(bridge.Incomplete{Reason: "max_output_tokens"}, 0)
	case "content_filter", "refusal":
		return decoder.queue(bridge.Incomplete{Reason: "other"}, 0)
	default:
		return decoder.queue(bridge.Failed{Code: "upstream_terminal_error", Message: "upstream stream reported an unsupported terminal reason"}, 0)
	}
}

func sortedToolKeys(calls map[bridge.ToolCallKey]*providerToolCall) []bridge.ToolCallKey {
	keys := make([]bridge.ToolCallKey, 0, len(calls))
	for key := range calls {
		keys = append(keys, key)
	}
	for index := 1; index < len(keys); index++ {
		for position := index; position > 0 && toolKeyLess(keys[position], keys[position-1]); position-- {
			keys[position], keys[position-1] = keys[position-1], keys[position]
		}
	}
	return keys
}

func toolKeyLess(left, right bridge.ToolCallKey) bool {
	if left.ChoiceIndex != right.ChoiceIndex {
		return left.ChoiceIndex < right.ChoiceIndex
	}
	return left.ToolIndex < right.ToolIndex
}
