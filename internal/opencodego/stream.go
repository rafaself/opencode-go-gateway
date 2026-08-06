package opencodego

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ErrUndeclaredToolCall      = errors.New("upstream tool call was not declared")
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

// BridgeStreamDecoderOptions configures provider-to-bridge stream translation.
// AllowedToolNames is copied when the decoder is constructed and is scoped to
// one upstream request. Provider tool names are checked only after their full
// fragmented name has been reconstructed; an empty allowlist rejects every
// provider function tool call.
type BridgeStreamDecoderOptions struct {
	SSE              SSEDecoderOptions
	AllowedToolNames []string
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
	maxToolCallArgumentBytes int
	toolRegistry             *bridge.ToolRegistry
	providerModel            string
	createdAt                time.Time
	assistantContent         []byte
	reasoningContent         []byte
	finalized                bool
	toolTurn                 bool
}

type providerToolCall struct {
	key              bridge.ToolCallKey
	callID           []byte // immutable downstream Responses call_id
	providerCallID   []byte // private provider ID/fragments for continuation
	name             []byte
	arguments        []byte
	argumentsEmitted int
	kind             bridge.ToolKind
	registration     bridge.ToolRegistration
	started          bool
	customCandidate  bool
	completed        bool
}

func NewBridgeStreamDecoder(reader io.Reader, options BridgeStreamDecoderOptions) *BridgeStreamDecoder {
	options.SSE = options.SSE.withDefaults()
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
		maxToolCallArgumentBytes: options.SSE.MaxToolCallArgumentBytes,
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
	if errors.Is(err, ErrToolCallArgumentLimit) {
		return bridge.Failed{Code: "stream_limit_exceeded", Message: "The upstream stream exceeded its limit."}
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
	for _, choice := range chunk.Choices {
		if choice.Index < 0 {
			return ErrMalformedStream
		}
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
			if err := decoder.queue(bridge.TextDelta{ChoiceIndex: choice.Index, Text: *choice.Delta.Content}, len(*choice.Delta.Content)); err != nil {
				return err
			}
			decoder.assistantContent = append(decoder.assistantContent, (*choice.Delta.Content)...)
		}
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			if err := decoder.queue(bridge.ReasoningDelta{ChoiceIndex: choice.Index, Text: *choice.Delta.ReasoningContent}, len(*choice.Delta.ReasoningContent)); err != nil {
				return err
			}
			decoder.reasoningContent = append(decoder.reasoningContent, (*choice.Delta.ReasoningContent)...)
		}
		seenToolIndexes := make(map[int]struct{}, len(choice.Delta.ToolCalls))
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

func (decoder *BridgeStreamDecoder) consumeToolCall(choiceIndex int, tool ToolCall) error {
	if tool.Index == nil || *tool.Index < 0 {
		return ErrMalformedStream
	}
	if tool.Type != "" && tool.Type != "function" {
		return ErrMalformedStream
	}
	toolIndex := *tool.Index
	key := bridge.ToolCallKey{ChoiceIndex: choiceIndex, ToolIndex: toolIndex}
	state, exists := decoder.calls[key]
	if !exists {
		state = &providerToolCall{key: key}
		callID := tool.ID
		if callID == "" {
			callID = syntheticToolCallID(key)
		}
		state.callID = append(state.callID, callID...)
		decoder.calls[key] = state
	}
	if tool.ID != "" {
		if err := decoder.reserveAggregateBytes(providerCallIDGrowth(state.providerCallID, tool.ID)); err != nil {
			return err
		}
		state.providerCallID = appendProviderCallID(state.providerCallID, tool.ID)
	}
	state.name = append(state.name, tool.Function.Name...)
	if decoder.customNameCandidate(string(state.name)) {
		state.customCandidate = true
	}
	if err := decoder.ensureToolStarted(state); err != nil {
		return err
	}
	if exists && tool.Function.Name != "" && state.kind != bridge.ToolCustom && state.started {
		if err := decoder.queue(bridge.ToolCallMetadataDelta{
			Key: key,
			// Provider IDs may be fragmented or delayed. The downstream call
			// ID was fixed in ToolCallStarted; later provider fragments remain
			// private continuation state and never mutate Responses identity.
			CallID: "",
			Name:   tool.Function.Name,
		}, len(tool.ID)+len(tool.Function.Name)); err != nil {
			return err
		}
	}
	if len(tool.Function.Arguments) > decoder.maxToolCallArgumentBytes-len(state.arguments) {
		return ErrToolCallArgumentLimit
	}
	state.arguments = append(state.arguments, tool.Function.Arguments...)
	if state.kind == bridge.ToolCustom || state.customCandidate {
		if err := decoder.reserveAggregateBytes(len(tool.Function.Arguments)); err != nil {
			return err
		}
		return nil
	}
	if state.started && state.argumentsEmitted < len(state.arguments) {
		delta := string(state.arguments[state.argumentsEmitted:])
		if err := decoder.queue(bridge.ToolCallArgumentsDelta{Key: key, Arguments: delta}, len(delta)); err != nil {
			return err
		}
		state.argumentsEmitted = len(state.arguments)
	}
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

func (decoder *BridgeStreamDecoder) ensureToolStarted(state *providerToolCall) error {
	if state.started {
		return nil
	}
	name := string(state.name)
	if name == "" || (state.customCandidate && name != ApplyPatchUpstreamName) {
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
	if kind != bridge.ToolCustom && state.argumentsEmitted < len(state.arguments) {
		delta := string(state.arguments[state.argumentsEmitted:])
		if err := decoder.queue(bridge.ToolCallArgumentsDelta{Key: state.key, Arguments: delta}, len(delta)); err != nil {
			return err
		}
		state.argumentsEmitted = len(state.arguments)
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
			if err := decoder.queue(bridge.ToolCallArgumentsDelta{Key: state.key, Arguments: input}, len(input)); err != nil {
				return err
			}
			if err := decoder.queue(bridge.ToolCallCompleted{
				Key:       state.key,
				Kind:      bridge.ToolCustom,
				CallID:    string(state.callID),
				Name:      state.registration.InboundName,
				Arguments: input,
			}, len(state.callID)+len(state.registration.InboundName)+len(input)); err != nil {
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
		}, len(state.callID)+len(state.name)+len(state.arguments)); err != nil {
			return err
		}
	}
	switch decoder.finishReason {
	case "stop", "tool_calls":
		decoder.finalized = true
		decoder.toolTurn = decoder.finishReason == "tool_calls" && len(decoder.calls) > 0
		return decoder.queue(bridge.Completed{Reason: decoder.finishReason}, 0)
	case "length":
		return decoder.queue(bridge.Incomplete{Reason: "max_output_tokens"}, 0)
	case "content_filter", "refusal":
		return decoder.queue(bridge.Incomplete{Reason: "other"}, 0)
	default:
		return decoder.queue(bridge.Failed{Code: "upstream_terminal_error", Message: "upstream stream reported an unsupported terminal reason"}, 0)
	}
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
