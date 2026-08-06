package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

var (
	ErrDuplicateStreamTerminal = errors.New("duplicate upstream stream terminal")
	ErrMalformedStream         = errors.New("malformed upstream stream")
	ErrStreamAggregateLimit    = errors.New("stream aggregate limit exceeded")
	ErrToolCallArgumentLimit   = errors.New("tool call argument limit exceeded")
	ErrToolCallLimit           = errors.New("tool call limit exceeded")
	ErrToolCallMetadataLimit   = errors.New("tool call metadata limit exceeded")
	ErrUndeclaredToolCall      = errors.New("upstream tool call was not declared")
	ErrStreamInterrupted       = errors.New("upstream stream was interrupted")
	ErrStreamTimeout           = errors.New("upstream stream timed out")
	ErrStreamCanceled          = errors.New("upstream stream was canceled")
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
		return ChatCompletionStreamEvent{}, classifyStreamReadError(err)
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

type streamTransportError struct {
	kind  error
	cause error
}

func (err *streamTransportError) Error() string {
	if err == nil || err.kind == nil {
		return ErrStreamInterrupted.Error()
	}
	return err.kind.Error()
}

func (err *streamTransportError) Unwrap() error {
	if err == nil {
		return nil
	}
	return errors.Join(err.kind, err.cause)
}

func classifyStreamReadError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, ErrSSELimitExceeded) || errors.Is(err, ErrSSEUnexpectedEOF) || errors.Is(err, ErrSSEInvalidUTF8) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return &streamTransportError{kind: ErrStreamCanceled, cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &streamTransportError{kind: ErrStreamTimeout, cause: err}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &streamTransportError{kind: ErrStreamTimeout, cause: err}
	}
	return &streamTransportError{kind: ErrStreamInterrupted, cause: err}
}

func wrapStreamDecodeError(cause error) error {
	return &streamDecodeError{cause: cause}
}

type streamDecodeError struct {
	cause error
}

func (err *streamDecodeError) Error() string { return ErrMalformedStream.Error() }

func (err *streamDecodeError) Unwrap() error { return errors.Join(ErrMalformedStream, err.cause) }

// BridgeStreamDecoderOptions configures provider-to-bridge stream translation.
// AllowedToolNames is copied when the decoder is constructed and is scoped to
// one upstream request. Provider tool names are validated before any tool
// event is emitted. A fragmented name remains private until the provider's
// terminal boundary, and may remain buffered only while it is a prefix of a
// declared function or registered synthetic tool name; an empty allowlist
// rejects every provider function tool call.
type BridgeStreamDecoderOptions struct {
	SSE                    SSEDecoderOptions
	AllowedToolNames       []string
	MaxToolCalls           int
	MaxChoices             int
	MaxToolNameBytes       int
	MaxProviderCallIDBytes int
	MaxOutputBytes         int
	MaxTextBytes           int
	MaxReasoningBytes      int
	// ToolRegistry is scoped to the request/continuation chain. Synthetic
	// provider names are translated only while this decoder is alive.
	ToolRegistry *bridge.ToolRegistry
}

// BridgeStreamDecoder translates provider chunks into semantic events. It
// owns only provider reconstruction state (choice/tool indexes and fragments);
// Responses IDs, output indexes, and sequence numbers remain in internal/codex.
type BridgeStreamDecoder struct {
	provider *ChatCompletionStreamDecoder
	pending  []bridge.StreamEvent
	calls    map[bridge.ToolCallKey]*providerToolCall

	started                  bool
	terminal                 bool
	finishReason             string
	finishConflict           bool
	choiceFinishReasons      map[int]string
	allowedToolNames         map[string]struct{}
	aggregateBytes           int
	maxAggregateBytes        int
	maxToolCalls             int
	maxChoices               int
	maxToolNameBytes         int
	maxProviderCallIDBytes   int
	maxToolCallArgumentBytes int
	maxOutputBytes           int
	maxTextBytes             int
	maxReasoningBytes        int
	outputBytes              int
	textBytes                int
	reasoningBytes           int
	toolRegistry             *bridge.ToolRegistry
	providerModel            string
	createdAt                time.Time
	assistantContent         []byte
	reasoningContent         []byte
	finalized                bool
	toolTurn                 bool
}

type providerToolCall struct {
	key             bridge.ToolCallKey
	callID          []byte // immutable downstream Responses call_id
	providerCallID  []byte // private provider ID/fragments for continuation
	name            []byte
	arguments       []byte
	kind            bridge.ToolKind
	registration    bridge.ToolRegistration
	started         bool
	customCandidate bool
	completed       bool
}

func NewBridgeStreamDecoder(reader io.Reader, options BridgeStreamDecoderOptions) *BridgeStreamDecoder {
	options.SSE = options.SSE.withDefaults()
	if options.MaxOutputBytes <= 0 {
		options.MaxOutputBytes = bridge.DefaultMaxOutputBytes
	}
	if options.MaxTextBytes <= 0 {
		options.MaxTextBytes = bridge.DefaultMaxTextBytes
	}
	if options.MaxReasoningBytes <= 0 {
		options.MaxReasoningBytes = bridge.DefaultMaxReasoningBytes
	}
	if options.MaxToolCalls <= 0 {
		options.MaxToolCalls = bridge.DefaultMaxStreamToolCalls
	}
	if options.MaxChoices <= 0 {
		options.MaxChoices = bridge.DefaultMaxStreamChoices
	}
	if options.MaxToolNameBytes <= 0 {
		options.MaxToolNameBytes = bridge.DefaultMaxToolNameBytes
	}
	if options.MaxProviderCallIDBytes <= 0 {
		options.MaxProviderCallIDBytes = bridge.DefaultMaxProviderCallIDBytes
	}
	allowedToolNames := make(map[string]struct{}, len(options.AllowedToolNames))
	for _, name := range options.AllowedToolNames {
		if name != "" {
			allowedToolNames[name] = struct{}{}
		}
	}
	if options.ToolRegistry != nil {
		for _, registration := range options.ToolRegistry.Registrations() {
			if registration.UpstreamName != "" {
				allowedToolNames[registration.UpstreamName] = struct{}{}
			}
		}
	}
	return &BridgeStreamDecoder{
		provider:                 NewChatCompletionStreamDecoder(reader, options.SSE),
		calls:                    make(map[bridge.ToolCallKey]*providerToolCall),
		choiceFinishReasons:      make(map[int]string),
		allowedToolNames:         allowedToolNames,
		maxAggregateBytes:        options.SSE.MaxAggregateBytes,
		maxToolCalls:             options.MaxToolCalls,
		maxChoices:               options.MaxChoices,
		maxToolNameBytes:         options.MaxToolNameBytes,
		maxProviderCallIDBytes:   options.MaxProviderCallIDBytes,
		maxToolCallArgumentBytes: options.SSE.MaxToolCallArgumentBytes,
		maxOutputBytes:           options.MaxOutputBytes,
		maxTextBytes:             options.MaxTextBytes,
		maxReasoningBytes:        options.MaxReasoningBytes,
		toolRegistry:             options.ToolRegistry,
	}
}

// ProviderCallID returns the private provider identifier reconstructed for a
// downstream call. It is intentionally separate from bridge events: the
// Responses call_id is immutable and safe to expose, while this value is
// retained for a future provider-specific continuation adapter.
func (decoder *BridgeStreamDecoder) ProviderCallID(callID string) (string, bool) {
	if decoder == nil || callID == "" {
		return "", false
	}
	for _, state := range decoder.calls {
		if string(state.callID) == callID && len(state.providerCallID) > 0 {
			return string(state.providerCallID), true
		}
	}
	return "", false
}

// Next returns semantic events until the first provider terminal marker has
// been translated. The bridge uses an explicit fail-closed, no-read-ahead
// policy: once that terminal event is queued, later provider bytes are not
// inspected and subsequent calls return io.EOF. Callers that need duplicate
// terminal detection must drain ChatCompletionStreamDecoder directly; the HTTP
// boundary must not block waiting for bytes that cannot change its response.
func (decoder *BridgeStreamDecoder) Next() (bridge.StreamEvent, error) {
	if decoder == nil || decoder.provider == nil {
		return nil, io.EOF
	}
	for {
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
			return decoder.bridgeFailure(err), nil
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
		if len(decoder.pending) > 0 {
			event := decoder.pending[0]
			decoder.pending = decoder.pending[1:]
			return event, nil
		}
		// A syntactically valid provider chunk can contain no choices, usage, or
		// bridge events. Charge that work against the same aggregate budget as
		// retained stream state so an endless no-op stream terminates
		// deterministically without relying on call-stack depth.
		if err := decoder.reserveAggregateBytes(bridgeStreamEventOverhead); err != nil {
			decoder.terminal = true
			return decoder.bridgeFailure(err), nil
		}
	}
}

func (decoder *BridgeStreamDecoder) bridgeFailure(err error) bridge.Failed {
	if errors.Is(err, ErrStreamAggregateLimit) {
		return bridge.Failed{Code: "stream_limit_exceeded", Message: "The upstream stream exceeded its limit."}
	}
	if errors.Is(err, ErrToolCallArgumentLimit) {
		return bridge.Failed{Code: "stream_limit_exceeded", Message: "The upstream stream exceeded its limit."}
	}
	if errors.Is(err, ErrToolCallLimit) || errors.Is(err, ErrToolCallMetadataLimit) {
		return bridge.Failed{Code: "stream_limit_exceeded", Message: "The upstream stream exceeded its limit."}
	}
	if errors.Is(err, ErrStreamCanceled) {
		return bridge.Failed{Code: "canceled", Message: "The upstream response stream was canceled."}
	}
	if errors.Is(err, ErrStreamTimeout) {
		return bridge.Failed{Code: "timeout", Message: "The upstream response stream timed out."}
	}
	if errors.Is(err, ErrStreamInterrupted) {
		return bridge.Failed{Code: "stream_interrupted", Message: "The upstream response stream was interrupted."}
	}
	if errors.Is(err, ErrUndeclaredToolCall) {
		return bridge.Failed{Code: "upstream_tool_not_declared", Message: "The upstream provider returned an undeclared function tool."}
	}
	if errors.Is(err, ErrMalformedCustomToolArguments) || errors.Is(err, ErrApplyPatchInputLimit) {
		return bridge.Failed{Code: customToolErrorCode(err), Message: customToolErrorMessage(err)}
	}
	return bridge.Failed{Code: "upstream_stream_error", Message: "upstream stream could not be decoded"}
}

func (decoder *BridgeStreamDecoder) consumeChunk(chunk ChatCompletionChunk) error {
	if err := decoder.validateChunkToolBounds(chunk); err != nil {
		return err
	}
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
		decoder.providerModel = chunk.Model
		decoder.createdAt = createdAt
	}
	if decoder.providerModel == "" && chunk.Model != "" {
		decoder.providerModel = chunk.Model
	}
	if decoder.createdAt.IsZero() && chunk.Created != 0 {
		decoder.createdAt = time.Unix(chunk.Created, 0).UTC()
	}
	seenChoiceIndexes := make(map[int]struct{}, minInt(len(chunk.Choices), decoder.maxChoices))
	for _, choice := range chunk.Choices {
		if choice.Index < 0 {
			return ErrMalformedStream
		}
		if choice.Index >= decoder.maxChoices {
			return ErrToolCallLimit
		}
		if _, exists := seenChoiceIndexes[choice.Index]; exists {
			return ErrMalformedStream
		}
		seenChoiceIndexes[choice.Index] = struct{}{}
		finishReason := ""
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
		previousFinishReason, choiceFinished := decoder.choiceFinishReasons[choice.Index]
		if choiceFinished {
			if finishReason != "" && finishReason != previousFinishReason {
				return ErrMalformedStream
			}
			if previousFinishReason != "tool_calls" {
				return ErrMalformedStream
			}
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				return ErrMalformedStream
			}
			if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
				return ErrMalformedStream
			}
			for _, tool := range choice.Delta.ToolCalls {
				if tool.Index == nil || *tool.Index < 0 {
					return ErrMalformedStream
				}
				key := bridge.ToolCallKey{ChoiceIndex: choice.Index, ToolIndex: *tool.Index}
				if _, exists := decoder.calls[key]; !exists {
					return ErrMalformedStream
				}
			}
		}
		if finishReason != "" {
			decoder.recordFinishReason(finishReason)
		}
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if len(*choice.Delta.Content) > decoder.maxTextBytes-decoder.textBytes || len(*choice.Delta.Content) > decoder.maxOutputBytes-decoder.outputBytes {
				return ErrStreamAggregateLimit
			}
			if err := decoder.queue(bridge.TextDelta{ChoiceIndex: choice.Index, Text: *choice.Delta.Content}, len(*choice.Delta.Content)); err != nil {
				return err
			}
			decoder.assistantContent = append(decoder.assistantContent, (*choice.Delta.Content)...)
			decoder.textBytes += len(*choice.Delta.Content)
			decoder.outputBytes += len(*choice.Delta.Content)
		}
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			if len(*choice.Delta.ReasoningContent) > decoder.maxReasoningBytes-decoder.reasoningBytes {
				return ErrStreamAggregateLimit
			}
			if err := decoder.queue(bridge.ReasoningDelta{ChoiceIndex: choice.Index, Text: *choice.Delta.ReasoningContent}, len(*choice.Delta.ReasoningContent)); err != nil {
				return err
			}
			decoder.reasoningContent = append(decoder.reasoningContent, (*choice.Delta.ReasoningContent)...)
			decoder.reasoningBytes += len(*choice.Delta.ReasoningContent)
		}
		seenToolIndexes := make(map[int]struct{}, minInt(len(choice.Delta.ToolCalls), decoder.maxToolCalls))
		for _, tool := range choice.Delta.ToolCalls {
			if tool.Index == nil || *tool.Index < 0 {
				return ErrMalformedStream
			}
			if _, exists := seenToolIndexes[*tool.Index]; exists {
				return ErrMalformedStream
			}
			seenToolIndexes[*tool.Index] = struct{}{}
			if err := decoder.consumeToolCall(choice.Index, tool); err != nil {
				return err
			}
		}
		if finishReason != "" {
			decoder.choiceFinishReasons[choice.Index] = finishReason
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

// validateChunkToolBounds performs the state-free part of provider tool
// reconstruction before consumeChunk can queue response state or append any
// call metadata. A provider can send many sparse/empty indexes or fragment a
// name over many chunks, so downstream event and queue limits are too late for
// this boundary.
func (decoder *BridgeStreamDecoder) validateChunkToolBounds(chunk ChatCompletionChunk) error {
	if len(chunk.Choices) > decoder.maxChoices {
		return ErrToolCallLimit
	}
	seenChoices := make(map[int]struct{}, len(chunk.Choices))
	newCalls := 0
	for _, choice := range chunk.Choices {
		if choice.Index < 0 {
			return ErrMalformedStream
		}
		if choice.Index >= decoder.maxChoices {
			return ErrToolCallLimit
		}
		if _, exists := seenChoices[choice.Index]; exists {
			return ErrMalformedStream
		}
		seenChoices[choice.Index] = struct{}{}
		if len(choice.Delta.ToolCalls) > decoder.maxToolCalls {
			return ErrToolCallLimit
		}
		seenTools := make(map[int]struct{}, len(choice.Delta.ToolCalls))
		for _, tool := range choice.Delta.ToolCalls {
			if tool.Index == nil || *tool.Index < 0 {
				return ErrMalformedStream
			}
			if tool.Type != "" && tool.Type != "function" {
				return ErrMalformedStream
			}
			toolIndex := *tool.Index
			if toolIndex >= decoder.maxToolCalls {
				return ErrToolCallLimit
			}
			if _, exists := seenTools[toolIndex]; exists {
				return ErrMalformedStream
			}
			seenTools[toolIndex] = struct{}{}
			key := bridge.ToolCallKey{ChoiceIndex: choice.Index, ToolIndex: toolIndex}
			state, exists := decoder.calls[key]
			if exists {
				if len(tool.Function.Name) > decoder.maxToolNameBytes-len(state.name) {
					return ErrToolCallMetadataLimit
				}
				growth := providerCallIDGrowth(state.providerCallID, tool.ID)
				if growth > decoder.maxProviderCallIDBytes-len(state.providerCallID) {
					return ErrToolCallMetadataLimit
				}
				continue
			}
			newCalls++
			callID := tool.ID
			if callID == "" {
				callID = syntheticToolCallID(key)
			}
			if len(callID) > decoder.maxProviderCallIDBytes || len(tool.ID) > decoder.maxProviderCallIDBytes || len(tool.Function.Name) > decoder.maxToolNameBytes {
				return ErrToolCallMetadataLimit
			}
		}
	}
	if newCalls > decoder.maxToolCalls-len(decoder.calls) {
		return ErrToolCallLimit
	}
	return nil
}

func (decoder *BridgeStreamDecoder) consumeToolCall(choiceIndex int, tool ToolCall) error {
	if tool.Index == nil || *tool.Index < 0 {
		return ErrMalformedStream
	}
	if tool.Type != "" && tool.Type != "function" {
		return ErrMalformedStream
	}
	toolIndex := *tool.Index
	if toolIndex >= decoder.maxToolCalls {
		return ErrToolCallLimit
	}
	key := bridge.ToolCallKey{ChoiceIndex: choiceIndex, ToolIndex: toolIndex}
	state, exists := decoder.calls[key]
	if !exists {
		if len(decoder.calls) >= decoder.maxToolCalls {
			return ErrToolCallLimit
		}
		if len(tool.ID) > decoder.maxProviderCallIDBytes || len(tool.Function.Name) > decoder.maxToolNameBytes {
			return ErrToolCallMetadataLimit
		}
		state = &providerToolCall{key: key}
		callID := tool.ID
		if callID == "" {
			callID = syntheticToolCallID(key)
		}
		state.callID = append(state.callID, callID...)
		decoder.calls[key] = state
	}
	if len(tool.Function.Name) > decoder.maxToolNameBytes-len(state.name) {
		return ErrToolCallMetadataLimit
	}
	nextName := string(state.name) + tool.Function.Name
	if err := decoder.validateToolNamePrefix(nextName); err != nil {
		return err
	}
	if tool.ID != "" {
		growth := providerCallIDGrowth(state.providerCallID, tool.ID)
		if growth > decoder.maxProviderCallIDBytes-len(state.providerCallID) {
			return ErrToolCallMetadataLimit
		}
		if err := decoder.reserveAggregateBytes(growth); err != nil {
			return err
		}
		state.providerCallID = appendProviderCallID(state.providerCallID, tool.ID)
	}
	state.name = append(state.name, tool.Function.Name...)
	if decoder.customNameCandidate(string(state.name)) {
		state.customCandidate = true
	}
	if len(tool.Function.Arguments) > decoder.maxToolCallArgumentBytes-len(state.arguments) {
		return ErrToolCallArgumentLimit
	}
	if len(tool.Function.Arguments) > decoder.maxOutputBytes-decoder.outputBytes {
		return ErrStreamAggregateLimit
	}
	if err := decoder.reserveAggregateBytes(len(tool.Function.Arguments)); err != nil {
		return err
	}
	state.arguments = append(state.arguments, tool.Function.Arguments...)
	decoder.outputBytes += len(tool.Function.Arguments)
	return nil
}

func (decoder *BridgeStreamDecoder) customNameCandidate(name string) bool {
	if name == "" || strings.HasPrefix(name, ReservedToolNamePrefix) {
		return name != ""
	}
	if decoder.toolRegistry == nil {
		return false
	}
	for _, registration := range decoder.toolRegistry.Registrations() {
		if registration.Kind == bridge.ToolCustom && strings.HasPrefix(registration.UpstreamName, name) {
			return true
		}
	}
	return false
}

func (decoder *BridgeStreamDecoder) validateToolNamePrefix(name string) error {
	if name == "" {
		return nil
	}
	if _, allowed := decoder.allowedToolNames[name]; allowed {
		return nil
	}
	for allowedName := range decoder.allowedToolNames {
		if strings.HasPrefix(allowedName, name) {
			return nil
		}
	}
	return ErrUndeclaredToolCall
}

func (decoder *BridgeStreamDecoder) ensureToolStarted(state *providerToolCall) error {
	if state.started {
		return nil
	}
	name := string(state.name)
	if name == "" {
		return nil
	}
	if _, allowed := decoder.allowedToolNames[name]; !allowed {
		return nil
	}
	kind := bridge.ToolFunction
	publicName := name
	if registration, ok := customToolRegistration(decoder.toolRegistry, name); ok {
		kind = registration.Kind
		publicName = registration.InboundName
		state.registration = registration
	}
	state.kind = kind
	if err := decoder.queue(bridge.ToolCallStarted{
		Key:    state.key,
		Kind:   kind,
		CallID: string(state.callID),
		Name:   publicName,
	}, len(state.callID)+len(publicName)); err != nil {
		return err
	}
	state.started = true
	if kind != bridge.ToolCustom && len(state.arguments) > 0 {
		// Argument fragments were charged when retained in state. Do not charge
		// their semantic replay payload a second time at the terminal boundary.
		if err := decoder.queue(bridge.ToolCallArgumentsDelta{Key: state.key, Arguments: string(state.arguments)}, 0); err != nil {
			return err
		}
	}
	return nil
}

func (decoder *BridgeStreamDecoder) reserveAggregateBytes(bytes int) error {
	if bytes < 0 || decoder.aggregateBytes > decoder.maxAggregateBytes-bytes {
		return ErrStreamAggregateLimit
	}
	decoder.aggregateBytes += bytes
	return nil
}

func appendProviderCallID(existing []byte, fragment string) []byte {
	if fragment == "" {
		return existing
	}
	current := string(existing)
	switch {
	case current == "":
		return append(existing, fragment...)
	case current == fragment:
		return existing
	case strings.HasPrefix(fragment, current):
		return append(existing[:0], fragment...)
	default:
		return append(existing, fragment...)
	}
}

func providerCallIDGrowth(existing []byte, fragment string) int {
	if fragment == "" {
		return 0
	}
	current := string(existing)
	switch {
	case current == "":
		return len(fragment)
	case current == fragment:
		return 0
	case strings.HasPrefix(fragment, current):
		return len(fragment) - len(current)
	default:
		return len(fragment)
	}
}

func syntheticToolCallID(key bridge.ToolCallKey) string {
	return fmt.Sprintf("call_%d_%d", key.ChoiceIndex, key.ToolIndex)
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
	switch decoder.finishReason {
	case "tool_calls":
		return decoder.finishToolCalls()
	case "stop":
		if len(decoder.calls) > 0 {
			return decoder.queue(bridge.Failed{Code: "upstream_terminal_error", Message: "upstream stream ended before tool calls were complete"}, 0)
		}
		decoder.finalized = true
		return decoder.queue(bridge.Completed{Reason: decoder.finishReason}, 0)
	case "length":
		// Tool-call state is deliberately left incomplete. A length-limited
		// provider turn must never be presented as an executable tool call.
		return decoder.queue(bridge.Incomplete{Reason: "max_output_tokens"}, 0)
	case "content_filter", "refusal":
		// Tool-call state is deliberately left incomplete. A filtered or
		// refused turn has no valid executable tool-call boundary.
		return decoder.queue(bridge.Incomplete{Reason: "other"}, 0)
	default:
		return decoder.queue(bridge.Failed{Code: "upstream_terminal_error", Message: "upstream stream reported an unsupported terminal reason"}, 0)
	}
}

func (decoder *BridgeStreamDecoder) finishToolCalls() error {
	for _, key := range sortedToolKeys(decoder.calls) {
		state := decoder.calls[key]
		if state.completed {
			continue
		}
		if err := decoder.ensureToolStarted(state); err != nil {
			return err
		}
		name := string(state.name)
		if name == "" {
			return ErrMalformedStream
		}
		if _, allowed := decoder.allowedToolNames[name]; !allowed {
			return ErrUndeclaredToolCall
		}
	}
	callIDs := make(map[string]bridge.ToolCallKey, len(decoder.calls))
	for _, key := range sortedToolKeys(decoder.calls) {
		state := decoder.calls[key]
		if state.completed {
			continue
		}
		callID := string(state.callID)
		if _, exists := callIDs[callID]; exists && callID != "" {
			return ErrMalformedStream
		}
		if callID != "" {
			callIDs[callID] = key
		}
		state.completed = true
		if state.kind == bridge.ToolCustom {
			input, err := unwrapApplyPatchArguments(string(state.arguments))
			if err != nil {
				return err
			}
			// The raw wrapper was charged as it was retained. The unwrapped
			// semantic payload is therefore intentionally not charged again.
			if err := decoder.queue(bridge.ToolCallArgumentsDelta{Key: state.key, Arguments: input}, 0); err != nil {
				return err
			}
			if err := decoder.queue(bridge.ToolCallCompleted{
				Key:       state.key,
				Kind:      bridge.ToolCustom,
				CallID:    string(state.callID),
				Name:      state.registration.InboundName,
				Arguments: input,
			}, len(state.callID)+len(state.registration.InboundName)); err != nil {
				return err
			}
			continue
		}
		if err := decoder.queue(bridge.ToolCallCompleted{
			Key:       state.key,
			Kind:      bridge.ToolFunction,
			CallID:    callID,
			Name:      string(state.name),
			Arguments: string(state.arguments),
		}, len(state.callID)+len(state.name)); err != nil {
			return err
		}
	}
	decoder.finalized = true
	decoder.toolTurn = len(decoder.calls) > 0
	return decoder.queue(bridge.Completed{Reason: decoder.finishReason}, 0)
}

// PendingTurn returns the finalized provider-side assistant turn when this
// stream ended with tool calls. It is intentionally unavailable for ordinary
// text turns and for streams that have not reached a valid terminal event.
// Private reasoning and provider identifiers stay inside the provider adapter
// and are never represented by bridge stream events.
func (decoder *BridgeStreamDecoder) PendingTurn() (*PendingTurn, bool) {
	if decoder == nil || !decoder.finalized || !decoder.toolTurn {
		return nil, false
	}
	keys := sortedToolKeys(decoder.calls)
	turn := PendingTurn{
		Provider:         ProviderName,
		Model:            decoder.providerModel,
		ReasoningContent: string(decoder.reasoningContent),
		AssistantContent: string(decoder.assistantContent),
		CreatedAt:        decoder.createdAt,
		ToolCalls:        make([]UpstreamToolCall, 0, len(keys)),
		CallIDs:          make([]string, 0, len(keys)),
	}
	registrations := make(map[string]bridge.ToolRegistration)
	for _, key := range keys {
		state := decoder.calls[key]
		if !state.completed || !state.started {
			return nil, false
		}
		providerCallID := string(state.providerCallID)
		if providerCallID == "" {
			providerCallID = string(state.callID)
		}
		name := string(state.name)
		arguments := string(state.arguments)
		if state.kind == bridge.ToolCustom {
			name = state.registration.UpstreamName
			if name == "" {
				return nil, false
			}
			registrations[state.registration.InboundName] = state.registration
		}
		turn.ToolCalls = append(turn.ToolCalls, UpstreamToolCall{
			CallID:         string(state.callID),
			ProviderCallID: providerCallID,
			Kind:           state.kind,
			Name:           name,
			Arguments:      arguments,
			Registration:   state.registration,
		})
		turn.CallIDs = append(turn.CallIDs, string(state.callID))
	}
	for _, registration := range registrations {
		turn.ToolRegistrations = append(turn.ToolRegistrations, registration)
	}
	sort.Slice(turn.ToolRegistrations, func(left, right int) bool {
		return turn.ToolRegistrations[left].InboundName < turn.ToolRegistrations[right].InboundName
	})
	return &turn, true
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
