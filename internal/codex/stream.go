package codex

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

var (
	ErrStreamWrite      = errors.New("downstream stream write failed")
	ErrStreamTransition = errors.New("invalid stream transition")
	ErrStreamTerminal   = errors.New("stream is already terminal")
	ErrStreamLimit      = errors.New("stream aggregate limit exceeded")
)

type StreamErrorCode string

const (
	StreamErrorWrite      StreamErrorCode = "stream_write"
	StreamErrorTransition StreamErrorCode = "stream_transition"
	StreamErrorTerminal   StreamErrorCode = "stream_terminal"
	StreamErrorLimit      StreamErrorCode = "stream_limit"
)

const DefaultStreamMaxAggregateBytes = 16 << 20

type StreamError struct {
	Code  StreamErrorCode
	cause error
}

func (err *StreamError) Error() string {
	if err == nil {
		return "<nil>"
	}
	switch err.Code {
	case StreamErrorWrite:
		return ErrStreamWrite.Error()
	case StreamErrorTransition:
		return ErrStreamTransition.Error()
	case StreamErrorTerminal:
		return ErrStreamTerminal.Error()
	case StreamErrorLimit:
		return ErrStreamLimit.Error()
	default:
		return "stream error"
	}
}

func (err *StreamError) Unwrap() error {
	if err == nil {
		return nil
	}
	switch err.Code {
	case StreamErrorWrite:
		return errors.Join(ErrStreamWrite, err.cause)
	case StreamErrorTransition:
		return errors.Join(ErrStreamTransition, err.cause)
	case StreamErrorTerminal:
		return errors.Join(ErrStreamTerminal, err.cause)
	case StreamErrorLimit:
		return errors.Join(ErrStreamLimit, err.cause)
	default:
		return err.cause
	}
}

// IDGenerator is scoped to one StreamSession. The index is an output index,
// not a provider index, and the generator never receives prompt/tool data.
type IDGenerator func(prefix string, index int) string

type CustomToolHook struct {
	Kind      bridge.ToolKind
	ItemType  string
	DeltaType string
	DoneType  string
	InputName string
}

type StreamSessionOptions struct {
	ResponseID string
	CreatedAt  time.Time
	Model      string
	Clock      func() time.Time
	// MaxAggregateBytes bounds retained text, reasoning, tool metadata, tool
	// arguments, and per-session state. A stream crossing the cap fails with a
	// stable stream-limit error.
	MaxAggregateBytes int
	IDGenerator       IDGenerator
	CustomTools       map[bridge.ToolKind]CustomToolHook
}

// StreamSession owns all Responses-specific stream state for one request. It
// is safe for concurrent Handle calls; writes are serialized so event order
// remains deterministic within the session.
type StreamSession struct {
	ResponseID      string
	CreatedAt       time.Time
	Model           string
	SequenceNumber  int64
	NextOutputIndex int
	TerminalEmitted bool

	writer      http.ResponseWriter
	controller  *http.ResponseController
	idGenerator IDGenerator
	customTools map[bridge.ToolKind]CustomToolHook

	mu                sync.Mutex
	started           bool
	headersSent       bool
	writeFailure      error
	downstreamDone    chan struct{}
	clock             func() time.Time
	terminalAt        time.Time
	aggregateBytes    int
	maxAggregateBytes int
	reasoning         []byte
	items             map[streamItemKey]*streamItem
	itemOrder         []*streamItem
	usage             *bridge.Usage
}

type streamItemKey struct {
	kind        string
	choiceIndex int
	toolIndex   int
}

const (
	streamTextKind = "text"
	streamToolKind = "tool"
)

type streamItem struct {
	key         streamItemKey
	id          string
	outputIndex int
	itemType    string
	toolKind    bridge.ToolKind
	hook        CustomToolHook
	callID      []byte
	callIDSynth bool
	name        []byte
	text        []byte
	arguments   []byte
	finalStatus string
	contentDone bool
	itemDone    bool
}

func NewStreamSession(writer http.ResponseWriter, options StreamSessionOptions) (*StreamSession, error) {
	if writer == nil {
		return nil, &StreamError{Code: StreamErrorTransition, cause: errors.New("nil response writer")}
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = defaultStreamID
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	responseID := options.ResponseID
	if responseID == "" {
		responseID = idGenerator("resp", 0)
	}
	if !validStreamValue(responseID, 128) || !validOptionalStreamValue(options.Model, 256) {
		return nil, &StreamError{Code: StreamErrorTransition, cause: errors.New("invalid stream identity")}
	}
	customTools := defaultCustomTools()
	for kind, hook := range options.CustomTools {
		if err := validateCustomToolHook(kind, hook); err != nil {
			return nil, err
		}
		customTools[kind] = hook
	}
	createdAt := options.CreatedAt
	if createdAt.IsZero() {
		createdAt = clock().UTC()
	} else {
		createdAt = createdAt.UTC()
	}
	maxAggregateBytes := options.MaxAggregateBytes
	if maxAggregateBytes <= 0 {
		maxAggregateBytes = DefaultStreamMaxAggregateBytes
	}
	return &StreamSession{
		ResponseID:        responseID,
		CreatedAt:         createdAt,
		Model:             options.Model,
		writer:            writer,
		controller:        http.NewResponseController(writer),
		idGenerator:       idGenerator,
		customTools:       customTools,
		clock:             clock,
		maxAggregateBytes: maxAggregateBytes,
		downstreamDone:    make(chan struct{}),
		items:             make(map[streamItemKey]*streamItem),
	}, nil
}

func defaultCustomTools() map[bridge.ToolKind]CustomToolHook {
	return map[bridge.ToolKind]CustomToolHook{
		bridge.ToolCustom: {
			Kind:      bridge.ToolCustom,
			ItemType:  "custom_tool_call",
			DeltaType: "response.custom_tool_call_input.delta",
			DoneType:  "response.custom_tool_call_input.done",
			InputName: "input",
		},
	}
}

func validateCustomToolHook(kind bridge.ToolKind, hook CustomToolHook) error {
	if kind == bridge.ToolFunction {
		return &StreamError{Code: StreamErrorTransition, cause: errors.New("function tools use the built-in Responses codec")}
	}
	if hook.Kind == "" {
		hook.Kind = kind
	}
	if hook.Kind != kind || !validStreamValue(hook.ItemType, 128) || !validStreamValue(hook.DeltaType, 128) || !validStreamValue(hook.DoneType, 128) || !validStreamValue(hook.InputName, 64) {
		return &StreamError{Code: StreamErrorTransition, cause: errors.New("invalid custom tool hook")}
	}
	return nil
}

func defaultStreamID(prefix string, index int) string {
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err == nil {
		return prefix + "_" + hex.EncodeToString(randomBytes)
	}
	return prefix + "_" + strconv.Itoa(index)
}

func validStreamValue(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, runeValue := range value {
		if runeValue == '\r' || runeValue == '\n' || runeValue == 0 || runeValue < 0x20 {
			return false
		}
	}
	return true
}

func validOptionalStreamValue(value string, maxBytes int) bool {
	return value == "" || validStreamValue(value, maxBytes)
}

func (session *StreamSession) Start() error {
	if session == nil {
		return &StreamError{Code: StreamErrorTransition}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.writeFailure != nil {
		return session.writeFailure
	}
	if session.TerminalEmitted {
		return &StreamError{Code: StreamErrorTerminal}
	}
	if session.started {
		return session.invalidTransitionLocked("response already started")
	}
	return session.startLocked()
}

func (session *StreamSession) Handle(event bridge.StreamEvent) error {
	if session == nil {
		return &StreamError{Code: StreamErrorTransition}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.writeFailure != nil {
		return session.writeFailure
	}
	if session.TerminalEmitted {
		return &StreamError{Code: StreamErrorTerminal}
	}
	if event == nil {
		return session.invalidTransitionLocked("nil stream event")
	}
	switch value := event.(type) {
	case bridge.ResponseStarted:
		return session.responseStartedLocked(value)
	case *bridge.ResponseStarted:
		if value == nil {
			return session.invalidTransitionLocked("nil response start")
		}
		return session.responseStartedLocked(*value)
	case bridge.TextDelta:
		return session.textDeltaLocked(value)
	case *bridge.TextDelta:
		if value == nil {
			return session.invalidTransitionLocked("nil text delta")
		}
		return session.textDeltaLocked(*value)
	case bridge.ToolCallStarted:
		return session.toolCallStartedLocked(value)
	case *bridge.ToolCallStarted:
		if value == nil {
			return session.invalidTransitionLocked("nil tool start")
		}
		return session.toolCallStartedLocked(*value)
	case bridge.ToolCallMetadataDelta:
		return session.toolCallMetadataLocked(value)
	case *bridge.ToolCallMetadataDelta:
		if value == nil {
			return session.invalidTransitionLocked("nil tool metadata")
		}
		return session.toolCallMetadataLocked(*value)
	case bridge.ToolCallArgumentsDelta:
		return session.toolCallArgumentsLocked(value)
	case *bridge.ToolCallArgumentsDelta:
		if value == nil {
			return session.invalidTransitionLocked("nil tool arguments")
		}
		return session.toolCallArgumentsLocked(*value)
	case bridge.ToolCallCompleted:
		return session.toolCallCompletedLocked(value)
	case *bridge.ToolCallCompleted:
		if value == nil {
			return session.invalidTransitionLocked("nil tool completion")
		}
		return session.toolCallCompletedLocked(*value)
	case bridge.ReasoningDelta:
		return session.reasoningDeltaLocked(value)
	case *bridge.ReasoningDelta:
		if value == nil {
			return session.invalidTransitionLocked("nil reasoning delta")
		}
		return session.reasoningDeltaLocked(*value)
	case bridge.UsageUpdated:
		return session.usageUpdatedLocked(value)
	case *bridge.UsageUpdated:
		if value == nil {
			return session.invalidTransitionLocked("nil usage update")
		}
		return session.usageUpdatedLocked(*value)
	case bridge.Completed:
		return session.completedLocked(value.Reason)
	case *bridge.Completed:
		if value == nil {
			return session.invalidTransitionLocked("nil completion")
		}
		return session.completedLocked(value.Reason)
	case bridge.Incomplete:
		return session.incompleteLocked(value.Reason)
	case *bridge.Incomplete:
		if value == nil {
			return session.invalidTransitionLocked("nil incomplete event")
		}
		return session.incompleteLocked(value.Reason)
	case bridge.Failed:
		return session.failedLocked(value)
	case *bridge.Failed:
		if value == nil {
			return session.invalidTransitionLocked("nil failure")
		}
		return session.failedLocked(*value)
	default:
		return session.invalidTransitionLocked("unsupported stream event")
	}
}

func (session *StreamSession) Complete() error {
	return session.Handle(bridge.Completed{Reason: "stop"})
}

func (session *StreamSession) Incomplete(reason string) error {
	return session.Handle(bridge.Incomplete{Reason: reason})
}

func (session *StreamSession) Fail(code, message string) error {
	return session.Handle(bridge.Failed{Code: code, Message: message})
}

// Done is closed when a downstream write or flush fails. The stream
// orchestrator owns the upstream response body and context; issue #7 must
// observe this signal and cancel/close that upstream resource. StreamSession
// never takes ownership of the provider body.
func (session *StreamSession) Done() <-chan struct{} {
	if session == nil {
		return nil
	}
	return session.downstreamDone
}

// WriteFailure returns the stable downstream failure, if one has occurred.
// It is intended for the issue #7 orchestrator to pair with Done and its own
// upstream cancellation/response-body ownership.
func (session *StreamSession) WriteFailure() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.writeFailure
}

func (session *StreamSession) startLocked() error {
	if !session.headersSent {
		header := session.writer.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		header.Set("X-Accel-Buffering", "no")
		header.Del("Content-Length")
		session.writer.WriteHeader(http.StatusOK)
		session.headersSent = true
	}
	session.started = true
	if err := session.writeEventLocked("response.created", map[string]any{
		"response": session.responseObjectLocked("in_progress", nil, ""),
	}); err != nil {
		return err
	}
	return session.writeEventLocked("response.in_progress", map[string]any{
		"response": session.responseObjectLocked("in_progress", nil, ""),
	})
}

func (session *StreamSession) responseStartedLocked(event bridge.ResponseStarted) error {
	if session.started {
		return nil
	}
	if !event.CreatedAt.IsZero() {
		session.CreatedAt = event.CreatedAt.UTC()
	}
	if event.Model != "" {
		if !validStreamValue(event.Model, 256) {
			return session.invalidTransitionLocked("invalid response model")
		}
		session.Model = event.Model
	}
	return session.startLocked()
}

func (session *StreamSession) ensureStartedLocked() error {
	if session.started {
		return nil
	}
	return session.startLocked()
}

func (session *StreamSession) textDeltaLocked(event bridge.TextDelta) error {
	if event.ChoiceIndex < 0 {
		return session.invalidTransitionLocked("negative text choice index")
	}
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	if !utf8.ValidString(event.Text) {
		return session.invalidTransitionLocked("invalid text encoding")
	}
	key := streamItemKey{kind: streamTextKind, choiceIndex: event.ChoiceIndex}
	item := session.items[key]
	if item == nil {
		if event.Text == "" {
			return nil
		}
		var err error
		item, err = session.newTextItemLocked(key)
		if err != nil {
			return err
		}
	}
	if item.itemDone {
		return session.invalidTransitionLocked("text arrived after item completion")
	}
	if event.Text == "" {
		return nil
	}
	if err := session.reserveAggregateLocked(len(event.Text)); err != nil {
		return session.limitLocked()
	}
	item.text = append(item.text, event.Text...)
	return session.writeEventLocked("response.output_text.delta", map[string]any{
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"delta":         event.Text,
		"logprobs":      []any{},
	})
}

func (session *StreamSession) newTextItemLocked(key streamItemKey) (*streamItem, error) {
	item, err := session.newItemLocked(key, "msg", "message", bridge.ToolFunction)
	if err != nil {
		return nil, err
	}
	item.itemType = "message"
	if err := session.writeEventLocked("response.output_item.added", map[string]any{
		"output_index": item.outputIndex,
		"item":         item.snapshot("in_progress"),
	}); err != nil {
		return nil, err
	}
	if err := session.writeEventLocked("response.content_part.added", map[string]any{
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	}); err != nil {
		return nil, err
	}
	return item, nil
}

func (session *StreamSession) toolCallStartedLocked(event bridge.ToolCallStarted) error {
	if event.Key.ChoiceIndex < 0 || event.Key.ToolIndex < 0 {
		return session.invalidTransitionLocked("negative tool index")
	}
	if event.Kind == "" {
		event.Kind = bridge.ToolFunction
	}
	if !validOptionalStreamValue(event.CallID, 256) || !validOptionalStreamValue(event.Name, 256) {
		return session.invalidTransitionLocked("invalid tool metadata")
	}
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	key := streamItemKey{kind: streamToolKind, choiceIndex: event.Key.ChoiceIndex, toolIndex: event.Key.ToolIndex}
	if _, exists := session.items[key]; exists {
		return session.invalidTransitionLocked("tool call started twice")
	}
	hook, itemType, prefix, err := session.toolDefinition(event.Kind)
	if err != nil {
		return session.invalidTransitionLocked(err.Error())
	}
	item, err := session.newItemLocked(key, prefix, itemType, event.Kind)
	if err != nil {
		return err
	}
	item.hook = hook
	item.callID = []byte(event.CallID)
	if len(item.callID) == 0 {
		item.callID = []byte(session.idGenerator("call", item.outputIndex))
		item.callIDSynth = true
	}
	if !validStreamValue(string(item.callID), 256) {
		return session.invalidTransitionLocked("invalid generated call ID")
	}
	if err := session.reserveAggregateLocked(len(item.callID) + len(event.Name)); err != nil {
		return session.limitLocked()
	}
	item.name = append(item.name, event.Name...)
	if err := session.writeEventLocked("response.output_item.added", map[string]any{
		"output_index": item.outputIndex,
		"item":         item.snapshot("in_progress"),
	}); err != nil {
		return err
	}
	return nil
}

func (session *StreamSession) toolCallMetadataLocked(event bridge.ToolCallMetadataDelta) error {
	item := session.toolItemLocked(event.Key)
	if item == nil || item.itemDone {
		return session.invalidTransitionLocked("tool metadata has no active item")
	}
	if !validOptionalStreamValue(event.CallID, 256) || !validOptionalStreamValue(event.Name, 256) {
		return session.invalidTransitionLocked("invalid tool metadata")
	}
	if err := session.reserveAggregateLocked(len(event.CallID) + len(event.Name)); err != nil {
		return session.limitLocked()
	}
	if event.CallID != "" {
		if item.callIDSynth {
			item.callID = append(item.callID[:0], event.CallID...)
			item.callIDSynth = false
		} else {
			item.callID = append(item.callID, event.CallID...)
		}
	}
	item.name = append(item.name, event.Name...)
	return nil
}

func (session *StreamSession) toolCallArgumentsLocked(event bridge.ToolCallArgumentsDelta) error {
	item := session.toolItemLocked(event.Key)
	if item == nil || item.itemDone {
		return session.invalidTransitionLocked("tool arguments have no active item")
	}
	if !utf8.ValidString(event.Arguments) {
		return session.invalidTransitionLocked("invalid tool argument encoding")
	}
	if event.Arguments == "" {
		return nil
	}
	if err := session.reserveAggregateLocked(len(event.Arguments)); err != nil {
		return session.limitLocked()
	}
	item.arguments = append(item.arguments, event.Arguments...)
	eventType := "response.function_call_arguments.delta"
	if item.toolKind != bridge.ToolFunction {
		eventType = item.hook.DeltaType
	}
	return session.writeEventLocked(eventType, map[string]any{
		"item_id":      item.id,
		"output_index": item.outputIndex,
		"delta":        event.Arguments,
	})
}

func (session *StreamSession) toolCallCompletedLocked(event bridge.ToolCallCompleted) error {
	item := session.toolItemLocked(event.Key)
	if item == nil || item.itemDone {
		return session.invalidTransitionLocked("tool call completed without an active item")
	}
	if event.Kind != "" && event.Kind != item.toolKind {
		return session.invalidTransitionLocked("tool kind changed during completion")
	}
	if !validOptionalStreamValue(event.CallID, 256) || !validOptionalStreamValue(event.Name, 256) || !utf8.ValidString(event.Arguments) {
		return session.invalidTransitionLocked("invalid tool completion")
	}
	if err := session.reserveAggregateLocked(len(event.CallID) + len(event.Name) + len(event.Arguments)); err != nil {
		return session.limitLocked()
	}
	if event.CallID != "" {
		item.callID = append(item.callID[:0], event.CallID...)
		item.callIDSynth = false
	}
	if event.Name != "" {
		item.name = append(item.name[:0], event.Name...)
	}
	if event.Arguments != "" || len(item.arguments) == 0 {
		item.arguments = append(item.arguments[:0], event.Arguments...)
	}
	if len(item.callID) == 0 || len(item.name) == 0 {
		return session.invalidTransitionLocked("tool call is missing identity")
	}
	return session.finishToolItemLocked(item, "completed")
}

func (session *StreamSession) reasoningDeltaLocked(event bridge.ReasoningDelta) error {
	if event.ChoiceIndex < 0 || !utf8.ValidString(event.Text) {
		return session.invalidTransitionLocked("invalid reasoning delta")
	}
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	if event.Text == "" {
		return nil
	}
	if err := session.reserveAggregateLocked(len(event.Text)); err != nil {
		return session.limitLocked()
	}
	session.reasoning = append(session.reasoning, event.Text...)
	return nil
}

func (session *StreamSession) usageUpdatedLocked(event bridge.UsageUpdated) error {
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	usage := event.Usage
	session.usage = &usage
	return nil
}

func (session *StreamSession) completedLocked(_ string) error {
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	if err := session.finishItemsLocked("completed", true); err != nil {
		return err
	}
	session.TerminalEmitted = true
	return session.writeTerminalLocked("completed", nil, "")
}

func (session *StreamSession) incompleteLocked(reason string) error {
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	reason = normalizeIncompleteReason(reason)
	if err := session.finishItemsLocked("incomplete", false); err != nil {
		return err
	}
	session.TerminalEmitted = true
	return session.writeTerminalLocked("incomplete", nil, reason)
}

func (session *StreamSession) failedLocked(event bridge.Failed) error {
	if err := session.ensureStartedLocked(); err != nil {
		return err
	}
	if err := session.finishItemsLocked("incomplete", false); err != nil {
		return err
	}
	session.TerminalEmitted = true
	code := safeFailureCode(event.Code)
	message := safeFailureMessage(event.Message)
	return session.writeTerminalLocked("failed", map[string]any{"code": code, "message": message}, "")
}

func (session *StreamSession) finishItemsLocked(status string, requireIdentity bool) error {
	for _, item := range session.itemOrder {
		if item.itemDone {
			continue
		}
		if item.key.kind == streamTextKind {
			if err := session.finishTextItemLocked(item, status); err != nil {
				return err
			}
			continue
		}
		if requireIdentity && (len(item.callID) == 0 || len(item.name) == 0) {
			return session.invalidTransitionLocked("tool call is missing identity")
		}
		if err := session.finishToolItemLocked(item, status); err != nil {
			return err
		}
	}
	return nil
}

func (session *StreamSession) finishTextItemLocked(item *streamItem, status string) error {
	if item.contentDone || item.itemDone {
		return nil
	}
	if err := session.writeEventLocked("response.output_text.done", map[string]any{
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"text":          string(item.text),
	}); err != nil {
		return err
	}
	item.contentDone = true
	if err := session.writeEventLocked("response.content_part.done", map[string]any{
		"item_id":       item.id,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": string(item.text), "annotations": []any{}},
	}); err != nil {
		return err
	}
	item.finalStatus = status
	item.itemDone = true
	return session.writeEventLocked("response.output_item.done", map[string]any{
		"output_index": item.outputIndex,
		"item":         item.snapshot(status),
	})
}

func (session *StreamSession) finishToolItemLocked(item *streamItem, status string) error {
	if item.itemDone {
		return nil
	}
	doneType := "response.function_call_arguments.done"
	if item.toolKind != bridge.ToolFunction {
		doneType = item.hook.DoneType
	}
	body := map[string]any{
		"item_id":      item.id,
		"output_index": item.outputIndex,
	}
	if item.toolKind == bridge.ToolFunction {
		body["name"] = string(item.name)
		body["arguments"] = string(item.arguments)
	} else {
		body[item.hook.InputName] = string(item.arguments)
	}
	if err := session.writeEventLocked(doneType, body); err != nil {
		return err
	}
	item.finalStatus = status
	item.itemDone = true
	return session.writeEventLocked("response.output_item.done", map[string]any{
		"output_index": item.outputIndex,
		"item":         item.snapshot(status),
	})
}

func (session *StreamSession) writeTerminalLocked(status string, responseError map[string]any, incompleteReason string) error {
	if status == "completed" && session.terminalAt.IsZero() {
		if session.clock != nil {
			session.terminalAt = session.clock().UTC()
		}
		if session.terminalAt.IsZero() {
			session.terminalAt = session.CreatedAt
		}
		if session.terminalAt.Before(session.CreatedAt) {
			session.terminalAt = session.CreatedAt
		}
	}
	return session.writeEventLocked("response."+status, map[string]any{
		"response": session.responseObjectLocked(status, responseError, incompleteReason),
	})
}

func (session *StreamSession) responseObjectLocked(status string, responseError map[string]any, incompleteReason string) map[string]any {
	response := map[string]any{
		"id":         session.ResponseID,
		"object":     "response",
		"created_at": session.CreatedAt.Unix(),
		"status":     status,
		"output":     session.outputLocked(),
	}
	if session.Model != "" {
		response["model"] = session.Model
	}
	if status != "in_progress" {
		if status == "completed" {
			terminalAt := session.terminalAt
			if terminalAt.IsZero() {
				terminalAt = session.CreatedAt
			}
			response["completed_at"] = terminalAt.Unix()
		} else {
			response["completed_at"] = nil
		}
	}
	if responseError != nil {
		response["error"] = responseError
	} else if status == "completed" {
		response["error"] = nil
	}
	if incompleteReason != "" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	if session.usage != nil {
		usage := map[string]any{
			"input_tokens":  session.usage.PromptTokens,
			"output_tokens": session.usage.CompletionTokens,
			"total_tokens":  session.usage.TotalTokens,
		}
		if session.usage.PromptCacheHitTokens > 0 {
			usage["input_tokens_details"] = map[string]any{
				"cached_tokens": session.usage.PromptCacheHitTokens,
			}
		}
		if session.usage.ReasoningTokens > 0 {
			usage["output_tokens_details"] = map[string]any{
				"reasoning_tokens": session.usage.ReasoningTokens,
			}
		}
		response["usage"] = usage
	}
	return response
}

func (session *StreamSession) outputLocked() []any {
	output := make([]any, 0, len(session.itemOrder))
	for _, item := range session.itemOrder {
		output = append(output, item.snapshot(""))
	}
	return output
}

func (item *streamItem) snapshot(status string) map[string]any {
	if status == "" {
		status = item.finalStatus
		if status == "" {
			status = "in_progress"
		}
	}
	result := map[string]any{
		"id":     item.id,
		"status": status,
		"type":   item.itemType,
	}
	if item.key.kind == streamTextKind {
		result["role"] = "assistant"
		content := []any{}
		if item.contentDone || item.itemDone {
			content = append(content, map[string]any{"type": "output_text", "text": string(item.text), "annotations": []any{}})
		}
		result["content"] = content
		return result
	}
	result["call_id"] = string(item.callID)
	result["name"] = string(item.name)
	if item.toolKind == bridge.ToolFunction {
		result["arguments"] = string(item.arguments)
	} else {
		result[item.hook.InputName] = string(item.arguments)
	}
	return result
}

func (session *StreamSession) newItemLocked(key streamItemKey, prefix, itemType string, toolKind bridge.ToolKind) (*streamItem, error) {
	itemID := session.idGenerator(prefix, session.NextOutputIndex)
	if !validStreamValue(itemID, 128) {
		return nil, &StreamError{Code: StreamErrorTransition, cause: errors.New("invalid generated item ID")}
	}
	if err := session.reserveAggregateLocked(len(itemID) + streamStateOverhead); err != nil {
		return nil, session.limitLocked()
	}
	item := &streamItem{
		key:         key,
		id:          itemID,
		outputIndex: session.NextOutputIndex,
		itemType:    itemType,
		toolKind:    toolKind,
	}
	session.NextOutputIndex++
	session.items[key] = item
	session.itemOrder = append(session.itemOrder, item)
	return item, nil
}

func (session *StreamSession) toolItemLocked(key bridge.ToolCallKey) *streamItem {
	return session.items[streamItemKey{kind: streamToolKind, choiceIndex: key.ChoiceIndex, toolIndex: key.ToolIndex}]
}

func (session *StreamSession) toolDefinition(kind bridge.ToolKind) (CustomToolHook, string, string, error) {
	if kind == bridge.ToolFunction {
		return CustomToolHook{Kind: kind}, "function_call", "fc", nil
	}
	hook, ok := session.customTools[kind]
	if !ok {
		return CustomToolHook{}, "", "", errors.New("unsupported custom tool kind")
	}
	prefix := "tool"
	if kind == bridge.ToolCustom {
		prefix = "ctc"
	}
	return hook, hook.ItemType, prefix, nil
}

const streamStateOverhead = 32

func (session *StreamSession) reserveAggregateLocked(bytes int) error {
	if bytes < 0 || bytes > session.maxAggregateBytes || session.aggregateBytes > session.maxAggregateBytes-bytes {
		return ErrStreamLimit
	}
	session.aggregateBytes += bytes
	return nil
}

func (session *StreamSession) limitLocked() error {
	if session.TerminalEmitted {
		return &StreamError{Code: StreamErrorTerminal}
	}
	if session.writeFailure != nil {
		return session.writeFailure
	}
	if !session.started {
		if err := session.startLocked(); err != nil {
			return err
		}
	}
	if err := session.finishItemsLocked("incomplete", false); err != nil {
		return err
	}
	session.TerminalEmitted = true
	if err := session.writeTerminalLocked("failed", map[string]any{
		"code":    "stream_limit_exceeded",
		"message": "The response stream exceeded its limit.",
	}, ""); err != nil {
		return err
	}
	return &StreamError{Code: StreamErrorLimit, cause: ErrStreamLimit}
}

func (session *StreamSession) invalidTransitionLocked(_ string) error {
	if session.TerminalEmitted {
		return &StreamError{Code: StreamErrorTerminal}
	}
	if session.writeFailure != nil {
		return session.writeFailure
	}
	if !session.started {
		if err := session.startLocked(); err != nil {
			return err
		}
	}
	session.TerminalEmitted = true
	if err := session.writeTerminalLocked("failed", map[string]any{
		"code":    "stream_invalid_transition",
		"message": "The response stream entered an invalid state.",
	}, ""); err != nil {
		return err
	}
	return &StreamError{Code: StreamErrorTransition}
}

func (session *StreamSession) writeEventLocked(eventType string, fields map[string]any) error {
	if session.writeFailure != nil {
		return session.writeFailure
	}
	payload := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		payload[key] = value
	}
	payload["type"] = eventType
	payload["sequence_number"] = session.SequenceNumber
	data, err := json.Marshal(payload)
	if err != nil {
		return session.recordWriteFailureLocked(err)
	}
	frame := make([]byte, 0, len(data)+9)
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	written, writeErr := session.writer.Write(frame)
	if writeErr != nil || written != len(frame) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return session.recordWriteFailureLocked(writeErr)
	}
	if flushErr := session.controller.Flush(); flushErr != nil {
		return session.recordWriteFailureLocked(flushErr)
	}
	session.SequenceNumber++
	return nil
}

func (session *StreamSession) recordWriteFailureLocked(cause error) error {
	if session.writeFailure == nil {
		session.writeFailure = &StreamError{Code: StreamErrorWrite, cause: cause}
		close(session.downstreamDone)
	}
	return session.writeFailure
}

func normalizeIncompleteReason(reason string) string {
	switch reason {
	case "max_output_tokens", "max_tokens", "length":
		return "max_output_tokens"
	case "other":
		return "other"
	default:
		return "other"
	}
}

func safeFailureCode(code string) string {
	if !validStreamValue(code, 64) {
		return "stream_error"
	}
	if code == "" {
		return "stream_error"
	}
	return code
}

func safeFailureMessage(message string) string {
	if !validStreamValue(message, 256) {
		return "The response stream failed."
	}
	if message == "" {
		return "The response stream failed."
	}
	return message
}
