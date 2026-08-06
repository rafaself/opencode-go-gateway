package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

// DefaultMaxBodyBytes bounds a request before the decoder allocates a JSON
// representation for it.
const DefaultMaxBodyBytes int64 = 16 << 20

const (
	// DefaultMaxCollectionItems bounds object members and array elements while
	// the request structure is being scanned. It applies before any decoded
	// collection is materialized.
	DefaultMaxCollectionItems = bridge.DefaultMaxCollectionItems
	// DefaultMaxJSONDepth and DefaultMaxJSONTokens keep the structural scan
	// bounded independently of the body byte limit.
	DefaultMaxJSONDepth  = bridge.DefaultMaxJSONDepth
	DefaultMaxJSONTokens = bridge.DefaultMaxJSONTokens
)

// DecoderLimits bounds every request-controlled collection that is expanded
// after the body has been read. The body bound alone is not sufficient because
// a small JSON document can still contain many input items or schemas.
type DecoderLimits struct {
	MaxBodyBytes       int64
	MaxInputItems      int
	MaxCollectionItems int
	MaxTools           int
	MaxSchemaBytes     int64
	MaxJSONDepth       int
	MaxJSONTokens      int
}

func defaultDecoderLimits(maxBodyBytes int64) DecoderLimits {
	return DecoderLimits{
		MaxBodyBytes:       maxBodyBytes,
		MaxInputItems:      bridge.DefaultMaxInputItems,
		MaxCollectionItems: DefaultMaxCollectionItems,
		MaxTools:           bridge.DefaultMaxFunctionTools,
		MaxSchemaBytes:     bridge.DefaultMaxFunctionSchemaBytes,
		MaxJSONDepth:       DefaultMaxJSONDepth,
		MaxJSONTokens:      DefaultMaxJSONTokens,
	}
}

// Decoder validates the supported Codex Responses request subset and returns
// only protocol-neutral bridge values.
type Decoder struct {
	limits DecoderLimits
}

// NewDecoder constructs a bounded request decoder. A non-positive limit is a
// configuration error; silently treating it as unlimited would defeat the
// request boundary.
func NewDecoder(maxBodyBytes int64) (*Decoder, error) {
	if maxBodyBytes <= 0 {
		return nil, fmt.Errorf("max body bytes must be positive")
	}
	return NewDecoderWithLimits(defaultDecoderLimits(maxBodyBytes))
}

// NewDecoderWithLimits constructs a decoder with explicit resource limits.
// Zero values select the documented safe defaults; negative values are
// configuration errors rather than an accidental unlimited mode.
func NewDecoderWithLimits(limits DecoderLimits) (*Decoder, error) {
	defaults := defaultDecoderLimits(DefaultMaxBodyBytes)
	if limits.MaxBodyBytes == 0 {
		limits.MaxBodyBytes = defaults.MaxBodyBytes
	}
	if limits.MaxInputItems == 0 {
		limits.MaxInputItems = defaults.MaxInputItems
	}
	if limits.MaxCollectionItems == 0 {
		limits.MaxCollectionItems = defaults.MaxCollectionItems
	}
	if limits.MaxTools == 0 {
		limits.MaxTools = defaults.MaxTools
	}
	if limits.MaxSchemaBytes == 0 {
		limits.MaxSchemaBytes = defaults.MaxSchemaBytes
	}
	if limits.MaxJSONDepth == 0 {
		limits.MaxJSONDepth = defaults.MaxJSONDepth
	}
	if limits.MaxJSONTokens == 0 {
		limits.MaxJSONTokens = defaults.MaxJSONTokens
	}
	if limits.MaxBodyBytes <= 0 || limits.MaxInputItems <= 0 || limits.MaxCollectionItems <= 0 || limits.MaxTools <= 0 || limits.MaxSchemaBytes <= 0 || limits.MaxJSONDepth <= 0 || limits.MaxJSONTokens <= 0 {
		return nil, fmt.Errorf("decoder limits must be positive")
	}
	return &Decoder{limits: limits}, nil
}

// Decode validates contentType, reads at most the configured body limit, and
// translates one JSON Responses request into the bridge model.
func (decoder *Decoder) Decode(body io.Reader, contentType string) (bridge.Request, error) {
	if decoder == nil || decoder.limits.MaxBodyBytes <= 0 {
		return bridge.Request{}, fmt.Errorf("decoder is not configured")
	}
	if err := validateContentType(contentType); err != nil {
		return bridge.Request{}, err
	}
	data, err := readBounded(body, decoder.limits.MaxBodyBytes)
	if err != nil {
		return bridge.Request{}, err
	}
	if !utf8.Valid(data) {
		return bridge.Request{}, newError(ErrorMalformedJSON, "body", "request body is not valid UTF-8")
	}
	fields, err := decodeJSONObject(data, decoder.limits)
	if err != nil {
		return bridge.Request{}, err
	}
	wire, err := decodeRequestWire(fields, decoder.limits)
	if err != nil {
		return bridge.Request{}, err
	}
	request, err := translateRequest(wire)
	if err != nil {
		return bridge.Request{}, err
	}
	return request, nil
}

// DecodeRequest applies the same boundary to an HTTP request without tying
// the bridge model to net/http. The caller retains ownership of r.Body.
func (decoder *Decoder) DecodeRequest(r *http.Request) (bridge.Request, error) {
	if r == nil {
		return bridge.Request{}, invalidRequest("request", "request is required")
	}
	if r.Body == nil {
		return bridge.Request{}, invalidRequest("body", "request body is required")
	}
	if decoder == nil || decoder.limits.MaxBodyBytes <= 0 {
		return bridge.Request{}, fmt.Errorf("decoder is not configured")
	}
	if r.ContentLength > decoder.limits.MaxBodyBytes {
		return bridge.Request{}, newError(ErrorRequestTooLarge, "body", "request body exceeds the configured limit")
	}
	return decoder.Decode(r.Body, r.Header.Get("Content-Type"))
}

// Decode is the package-level convenience API for callers that do not need
// to retain a decoder instance.
func Decode(body io.Reader, contentType string, maxBodyBytes int64) (bridge.Request, error) {
	decoder, err := NewDecoder(maxBodyBytes)
	if err != nil {
		return bridge.Request{}, err
	}
	return decoder.Decode(body, contentType)
}

// DecodeRequest is the package-level HTTP convenience API for the initial
// Responses boundary.
func DecodeRequest(r *http.Request, maxBodyBytes int64) (bridge.Request, error) {
	decoder, err := NewDecoder(maxBodyBytes)
	if err != nil {
		return bridge.Request{}, err
	}
	return decoder.DecodeRequest(r)
}

func validateContentType(contentType string) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return invalidRequest("content-type", "Content-Type must be application/json")
	}
	return nil
}

func readBounded(body io.Reader, maxBodyBytes int64) ([]byte, error) {
	if body == nil {
		return nil, invalidRequest("body", "request body is required")
	}
	limit := maxBodyBytes
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		readError := invalidRequest("body", "request body could not be read")
		readError.cause = err
		return nil, readError
	}
	if int64(len(data)) > maxBodyBytes {
		return nil, newError(ErrorRequestTooLarge, "body", "request body exceeds the configured limit")
	}
	return data, nil
}

func decodeJSONObject(data []byte, limits DecoderLimits) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, newError(ErrorMalformedJSON, "body", "request body contains malformed JSON")
	}
	if err := validateJSONDocument(data, limits.MaxJSONDepth, limits.MaxJSONTokens, limits.MaxCollectionItems); err != nil {
		if _, limited := err.(jsonStructureLimitError); limited {
			return nil, newError(ErrorRequestTooLarge, "body", "request JSON structure exceeds the configured limit")
		}
		if _, duplicate := err.(jsonDuplicateKeyError); duplicate {
			return nil, newError(ErrorMalformedJSON, "body", "request body contains duplicate JSON object keys")
		}
		if _, trailing := err.(jsonTrailingValueError); trailing {
			return nil, newError(ErrorMalformedJSON, "body", "request body must contain one JSON object without trailing values")
		}
		return nil, newError(ErrorMalformedJSON, "body", "request body contains malformed JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, newError(ErrorMalformedJSON, "body", "request body contains malformed JSON")
	}
	return fields, nil
}

type jsonStructureLimitError struct {
	detail string
}

func (err jsonStructureLimitError) Error() string { return err.detail }

type jsonDuplicateKeyError struct{}

func (jsonDuplicateKeyError) Error() string { return "duplicate object key" }

type jsonTrailingValueError struct{}

func (jsonTrailingValueError) Error() string { return "trailing JSON value" }

// validateJSONDocument performs the request-wide structural pass before any
// map or array is materialized. It rejects duplicate object keys, bounds
// nesting and tokens, and counts each collection while scanning it.
func validateJSONDocument(data []byte, maxDepth, maxTokens, maxCollectionItems int) error {
	type frame struct {
		kind         json.Delim
		expectingKey bool
		memberCount  int
		seenKeys     map[string]struct{}
	}
	if maxDepth <= 0 || maxTokens <= 0 || maxCollectionItems <= 0 {
		return jsonStructureLimitError{detail: "JSON structure limits are not positive"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	stack := make([]frame, 0, 8)
	rootComplete := false
	tokens := 0
	completeValue := func() {
		if len(stack) == 0 {
			rootComplete = true
			return
		}
		parent := &stack[len(stack)-1]
		if parent.kind == '{' {
			parent.expectingKey = true
		}
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		tokens++
		if tokens > maxTokens {
			return jsonStructureLimitError{detail: "JSON token count exceeds the configured limit"}
		}

		if len(stack) == 0 {
			if rootComplete {
				return jsonTrailingValueError{}
			}
			delimiter, isDelimiter := token.(json.Delim)
			if !isDelimiter {
				rootComplete = true
				continue
			}
			if delimiter != '{' && delimiter != '[' {
				return fmt.Errorf("unexpected JSON delimiter")
			}
			if 1 > maxDepth {
				return jsonStructureLimitError{detail: "JSON nesting exceeds the configured limit"}
			}
			stack = append(stack, frame{
				kind:         delimiter,
				expectingKey: delimiter == '{',
				seenKeys:     map[string]struct{}{},
			})
			continue
		}

		current := &stack[len(stack)-1]
		if current.kind == '{' && current.expectingKey {
			if delimiter, isDelimiter := token.(json.Delim); isDelimiter && delimiter == '}' {
				stack = stack[:len(stack)-1]
				completeValue()
				continue
			}
			key, isString := token.(string)
			if !isString {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := current.seenKeys[key]; duplicate {
				return jsonDuplicateKeyError{}
			}
			current.seenKeys[key] = struct{}{}
			current.memberCount++
			if current.memberCount > maxCollectionItems {
				return jsonStructureLimitError{detail: "JSON object member count exceeds the configured limit"}
			}
			current.expectingKey = false
			continue
		}
		if current.kind == '[' {
			if delimiter, isDelimiter := token.(json.Delim); isDelimiter && delimiter == ']' {
				stack = stack[:len(stack)-1]
				completeValue()
				continue
			}
			current.memberCount++
			if current.memberCount > maxCollectionItems {
				return jsonStructureLimitError{detail: "JSON array element count exceeds the configured limit"}
			}
		} else if delimiter, isDelimiter := token.(json.Delim); isDelimiter && delimiter == '}' {
			return fmt.Errorf("object value is missing")
		}

		if delimiter, isDelimiter := token.(json.Delim); isDelimiter {
			if delimiter != '{' && delimiter != '[' {
				return fmt.Errorf("unexpected JSON delimiter")
			}
			if len(stack)+1 > maxDepth {
				return jsonStructureLimitError{detail: "JSON nesting exceeds the configured limit"}
			}
			stack = append(stack, frame{
				kind:         delimiter,
				expectingKey: delimiter == '{',
				seenKeys:     map[string]struct{}{},
			})
			continue
		}
		completeValue()
	}
	if !rootComplete || len(stack) != 0 {
		return fmt.Errorf("incomplete JSON value")
	}
	return nil
}

type fieldPolicy string

const (
	policyTranslate fieldPolicy = "translate"
	policyNoOp      fieldPolicy = "accept as no-op"
	policyDefer     fieldPolicy = "defer"
)

// topLevelPolicy is the executable copy of testdata/codex/field-policy.json.
// Keep this list deliberately explicit so a newly observed field cannot
// disappear during decoding.
var topLevelPolicy = map[string]fieldPolicy{
	"model":                policyTranslate,
	"instructions":         policyTranslate,
	"input":                policyTranslate,
	"tools":                policyTranslate,
	"tool_choice":          policyTranslate,
	"parallel_tool_calls":  policyTranslate,
	"reasoning":            policyTranslate,
	"text":                 policyTranslate,
	"stream":               policyTranslate,
	"include":              policyTranslate,
	"previous_response_id": policyTranslate,

	"stream_options":   policyNoOp,
	"store":            policyNoOp,
	"service_tier":     policyNoOp,
	"prompt_cache_key": policyNoOp,
	"metadata":         policyNoOp,
	"client_metadata":  policyNoOp,

	"background": policyDefer,
}

type responsesRequestWire struct {
	Model              string
	Instructions       string
	PreviousResponseID string
	Input              []inputWire
	Tools              []toolWire
	ToolChoice         toolChoiceWire
	Generation         generationWire
}

type generationWire struct {
	Stream            bool
	ParallelToolCalls bool
	Include           []string
	Reasoning         reasoningWire
	Text              textWire
}

type reasoningWire struct {
	Effort string
}

type textWire struct {
	Format textFormatWire
}

type textFormatWire struct {
	Kind        bridge.TextFormatKind
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

type toolChoiceWire struct {
	Kind         bridge.ToolChoiceKind
	FunctionName string
}

type inputWire interface {
	isInputWire()
}

type messageWire struct {
	ID      string
	Role    bridge.Role
	Content []textContentWire
}

func (messageWire) isInputWire() {}

type textContentWire struct {
	Text string
}

type functionCallWire struct {
	ID        string
	CallID    string
	Name      string
	Arguments string
	Status    string
}

func (functionCallWire) isInputWire() {}

type functionCallOutputWire struct {
	CallID string
	Output string
	Status string
	Error  bool
}

func (functionCallOutputWire) isInputWire() {}

type customToolCallWire struct {
	ID     string
	CallID string
	Name   string
	Input  string
	Status string
}

func (customToolCallWire) isInputWire() {}

type customToolCallOutputWire struct {
	CallID string
	Output string
	Status string
	Error  bool
}

func (customToolCallOutputWire) isInputWire() {}

type toolWire interface {
	isToolWire()
	toolName() string
}

type functionToolWire struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      *bool
}

func (tool functionToolWire) isToolWire()      {}
func (tool functionToolWire) toolName() string { return tool.Name }

type customToolWire struct {
	Name        string
	Description string
	Format      bridge.CustomToolFormatKind
}

func (tool customToolWire) isToolWire()      {}
func (tool customToolWire) toolName() string { return tool.Name }

type deferredToolWire struct {
	Kind bridge.ToolKind
	Name string
}

func (tool deferredToolWire) isToolWire()      {}
func (tool deferredToolWire) toolName() string { return tool.Name }

func decodeRequestWire(fields map[string]json.RawMessage, limits DecoderLimits) (responsesRequestWire, error) {
	for _, field := range sortedKeys(fields) {
		policy, ok := topLevelPolicy[field]
		if !ok {
			return responsesRequestWire{}, unsupportedField(unknownFieldParam("request"), "field is not classified by the Codex contract")
		}
		if policy == policyDefer {
			return responsesRequestWire{}, unsupportedField(deferredFieldParam(field), "field is deferred until a later milestone")
		}
	}

	wire := responsesRequestWire{
		Input:      make([]inputWire, 0),
		Tools:      make([]toolWire, 0),
		ToolChoice: toolChoiceWire{Kind: bridge.ToolChoiceAuto},
		Generation: generationWire{
			Include: make([]string, 0),
			Text:    textWire{Format: textFormatWire{Kind: bridge.TextFormatText}},
		},
	}

	model, ok := fields["model"]
	if !ok {
		return responsesRequestWire{}, invalidRequest("model", "model is required")
	}
	var err error
	wire.Model, err = requiredString(model, "model")
	if err != nil {
		return responsesRequestWire{}, err
	}
	if raw, ok := fields["instructions"]; ok {
		wire.Instructions, err = stringValue(raw, "instructions")
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["previous_response_id"]; ok {
		wire.PreviousResponseID, err = requiredString(raw, "previous_response_id")
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["input"]; ok {
		items, decodeErr := rawArray(raw, "input", limits.MaxInputItems)
		if decodeErr != nil {
			return responsesRequestWire{}, decodeErr
		}
		wire.Input, err = decodeInputItems(items, limits.MaxInputItems, limits.MaxCollectionItems)
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["tools"]; ok {
		tools, decodeErr := rawArray(raw, "tools", limits.MaxTools)
		if decodeErr != nil {
			return responsesRequestWire{}, decodeErr
		}
		wire.Tools, err = decodeTools(tools, limits.MaxTools, limits.MaxSchemaBytes, limits)
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["tool_choice"]; ok {
		wire.ToolChoice, err = decodeToolChoice(raw)
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		wire.Generation.ParallelToolCalls, err = boolValue(raw, "parallel_tool_calls")
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["reasoning"]; ok {
		wire.Generation.Reasoning, err = decodeReasoning(raw)
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["text"]; ok {
		remainingSchemaBytes := limits.MaxSchemaBytes
		for _, tool := range wire.Tools {
			function, ok := tool.(functionToolWire)
			if !ok {
				continue
			}
			remainingSchemaBytes -= int64(len(function.Parameters))
		}
		wire.Generation.Text, err = decodeText(raw, remainingSchemaBytes, limits)
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	stream, ok := fields["stream"]
	if !ok {
		return responsesRequestWire{}, unsupportedField("stream", "stream must be present and true for the streaming milestone")
	}
	wire.Generation.Stream, err = boolValue(stream, "stream")
	if err != nil {
		return responsesRequestWire{}, err
	}
	if !wire.Generation.Stream {
		return responsesRequestWire{}, unsupportedField("stream", "stream must be true for the streaming milestone")
	}
	if raw, ok := fields["include"]; ok {
		wire.Generation.Include, err = stringArray(raw, "include", limits.MaxCollectionItems)
		if err != nil {
			return responsesRequestWire{}, err
		}
	}
	if raw, ok := fields["store"]; ok {
		store, decodeErr := boolValue(raw, "store")
		if decodeErr != nil {
			return responsesRequestWire{}, decodeErr
		}
		if store {
			return responsesRequestWire{}, unsupportedField("store", "store=true is not supported; only false is a compatibility no-op")
		}
	}
	if raw, ok := fields["stream_options"]; ok {
		if _, decodeErr := rawObject(raw, "stream_options"); decodeErr != nil {
			return responsesRequestWire{}, decodeErr
		}
	}
	if raw, ok := fields["service_tier"]; ok {
		if _, decodeErr := stringValue(raw, "service_tier"); decodeErr != nil {
			return responsesRequestWire{}, decodeErr
		}
	}
	if raw, ok := fields["prompt_cache_key"]; ok {
		if _, decodeErr := stringValue(raw, "prompt_cache_key"); decodeErr != nil {
			return responsesRequestWire{}, decodeErr
		}
	}
	for _, field := range []string{"metadata", "client_metadata"} {
		if raw, ok := fields[field]; ok {
			if _, decodeErr := rawObject(raw, field); decodeErr != nil {
				return responsesRequestWire{}, decodeErr
			}
		}
	}
	if err := validateToolChoice(wire.ToolChoice, wire.Tools); err != nil {
		return responsesRequestWire{}, err
	}
	return wire, nil
}

func decodeInputItems(items []json.RawMessage, maxItems, maxCollectionItems int) ([]inputWire, error) {
	if maxItems <= 0 || len(items) > maxItems {
		return nil, newError(ErrorRequestTooLarge, "input", "input item count exceeds the configured limit")
	}
	// Validate item shapes here, but defer result correlation to the
	// continuation owner. A standard continuation may contain only output
	// items, so an output call_id is not required to have a local declaration.
	// The provider adapter owns retained state and can therefore return stable
	// continuation taxonomy instead of collapsing these cases into
	// invalid_request.
	result := make([]inputWire, 0, len(items))
	callKinds := make(map[string]bridge.InputKind)
	for index, raw := range items {
		path := fmt.Sprintf("input[%d]", index)
		item, err := decodeInputItem(raw, path, maxCollectionItems)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
		switch item := item.(type) {
		case functionCallWire:
			if _, exists := callKinds[item.CallID]; exists {
				return nil, invalidRequest(path+".call_id", "call_id must be unique within the request")
			}
			callKinds[item.CallID] = bridge.InputFunctionCall
		case customToolCallWire:
			if _, exists := callKinds[item.CallID]; exists {
				return nil, invalidRequest(path+".call_id", "call_id must be unique within the request")
			}
			callKinds[item.CallID] = bridge.InputCustomToolCall
		case functionCallOutputWire, customToolCallOutputWire:
			// Syntactically valid output-only items are intentionally accepted.
			// ContinuationStore.Begin validates unknown, mixed, duplicate, and
			// stored-kind correlations after the request reaches the adapter.
		}
	}
	return result, nil
}

func decodeInputItem(raw json.RawMessage, path string, maxCollectionItems int) (inputWire, error) {
	fields, err := rawObject(raw, path)
	if err != nil {
		return nil, err
	}
	typeValue, ok := fields["type"]
	if !ok {
		return nil, invalidRequest(path+".type", "type is required")
	}
	itemType, err := requiredString(typeValue, path+".type")
	if err != nil {
		return nil, err
	}
	switch itemType {
	case string(bridge.InputMessage):
		return decodeMessage(fields, path, maxCollectionItems)
	case string(bridge.InputFunctionCall):
		return decodeFunctionCall(fields, path)
	case string(bridge.InputFunctionCallOutput):
		return decodeFunctionCallOutput(fields, path)
	case string(bridge.InputCustomToolCall):
		return decodeCustomToolCall(fields, path)
	case string(bridge.InputCustomToolCallOutput):
		return decodeCustomToolCallOutput(fields, path)
	default:
		return nil, newError(ErrorUnsupportedItemType, path+".type", "input item type is not supported")
	}
}

func decodeMessage(fields map[string]json.RawMessage, path string, maxCollectionItems int) (messageWire, error) {
	if err := rejectUnknown(fields, path, "type", "id", "role", "content"); err != nil {
		return messageWire{}, err
	}
	roleRaw, ok := fields["role"]
	if !ok {
		return messageWire{}, invalidRequest(path+".role", "role is required")
	}
	roleValue, err := requiredString(roleRaw, path+".role")
	if err != nil {
		return messageWire{}, err
	}
	role, err := parseRole(roleValue, path+".role")
	if err != nil {
		return messageWire{}, err
	}
	id, err := optionalString(fields, "id", path+".id")
	if err != nil {
		return messageWire{}, err
	}
	contentRaw, ok := fields["content"]
	if !ok {
		return messageWire{}, invalidRequest(path+".content", "content is required")
	}
	content, err := decodeContent(contentRaw, path+".content", maxCollectionItems)
	if err != nil {
		return messageWire{}, err
	}
	return messageWire{ID: id, Role: role, Content: content}, nil
}

func decodeContent(raw json.RawMessage, path string, maxItems int) ([]textContentWire, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		text, err := stringValue(raw, path)
		if err != nil {
			return nil, err
		}
		return []textContentWire{{Text: text}}, nil
	}
	parts, err := rawArray(raw, path, maxItems)
	if err != nil {
		return nil, err
	}
	result := make([]textContentWire, 0, len(parts))
	for index, part := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, index)
		fields, decodeErr := rawObject(part, partPath)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if decodeErr := rejectUnknown(fields, partPath, "type", "text"); decodeErr != nil {
			return nil, decodeErr
		}
		typeValue, exists := fields["type"]
		if !exists {
			return nil, invalidRequest(partPath+".type", "type is required")
		}
		contentType, decodeErr := requiredString(typeValue, partPath+".type")
		if decodeErr != nil {
			return nil, decodeErr
		}
		if contentType != string(bridge.ContentInputText) {
			return nil, newError(ErrorUnsupportedItemType, partPath+".type", "content type is not supported")
		}
		textRaw, exists := fields["text"]
		if !exists {
			return nil, invalidRequest(partPath+".text", "text is required")
		}
		text, decodeErr := stringValue(textRaw, partPath+".text")
		if decodeErr != nil {
			return nil, decodeErr
		}
		result = append(result, textContentWire{Text: text})
	}
	return result, nil
}

func decodeFunctionCall(fields map[string]json.RawMessage, path string) (functionCallWire, error) {
	if err := rejectUnknown(fields, path, "type", "id", "call_id", "name", "arguments", "status"); err != nil {
		return functionCallWire{}, err
	}
	callID, err := requiredStringField(fields, "call_id", path)
	if err != nil {
		return functionCallWire{}, err
	}
	name, err := requiredStringField(fields, "name", path)
	if err != nil {
		return functionCallWire{}, err
	}
	arguments, err := requiredStringFieldAllowEmpty(fields, "arguments", path)
	if err != nil {
		return functionCallWire{}, err
	}
	id, err := optionalString(fields, "id", path+".id")
	if err != nil {
		return functionCallWire{}, err
	}
	status, err := optionalString(fields, "status", path+".status")
	if err != nil {
		return functionCallWire{}, err
	}
	return functionCallWire{ID: id, CallID: callID, Name: name, Arguments: arguments, Status: status}, nil
}

func decodeFunctionCallOutput(fields map[string]json.RawMessage, path string) (functionCallOutputWire, error) {
	if err := rejectUnknown(fields, path, "type", "call_id", "output", "status", "error"); err != nil {
		return functionCallOutputWire{}, err
	}
	callID, err := requiredStringField(fields, "call_id", path)
	if err != nil {
		return functionCallOutputWire{}, err
	}
	output, err := requiredStringFieldAllowEmpty(fields, "output", path)
	if err != nil {
		return functionCallOutputWire{}, err
	}
	status, err := optionalString(fields, "status", path+".status")
	if err != nil {
		return functionCallOutputWire{}, err
	}
	errorMarker, err := optionalErrorMarker(fields, path+".error")
	if err != nil {
		return functionCallOutputWire{}, err
	}
	return functionCallOutputWire{CallID: callID, Output: output, Status: status, Error: errorMarker || resultStatusIsError(status)}, nil
}

func decodeCustomToolCall(fields map[string]json.RawMessage, path string) (customToolCallWire, error) {
	if err := rejectUnknown(fields, path, "type", "id", "call_id", "name", "input", "status"); err != nil {
		return customToolCallWire{}, err
	}
	callID, err := requiredStringField(fields, "call_id", path)
	if err != nil {
		return customToolCallWire{}, err
	}
	name, err := requiredStringField(fields, "name", path)
	if err != nil {
		return customToolCallWire{}, err
	}
	input, err := requiredStringFieldAllowEmpty(fields, "input", path)
	if err != nil {
		return customToolCallWire{}, err
	}
	id, err := optionalString(fields, "id", path+".id")
	if err != nil {
		return customToolCallWire{}, err
	}
	status, err := optionalString(fields, "status", path+".status")
	if err != nil {
		return customToolCallWire{}, err
	}
	return customToolCallWire{ID: id, CallID: callID, Name: name, Input: input, Status: status}, nil
}

func decodeCustomToolCallOutput(fields map[string]json.RawMessage, path string) (customToolCallOutputWire, error) {
	if err := rejectUnknown(fields, path, "type", "call_id", "output", "status", "error"); err != nil {
		return customToolCallOutputWire{}, err
	}
	callID, err := requiredStringField(fields, "call_id", path)
	if err != nil {
		return customToolCallOutputWire{}, err
	}
	output, err := requiredStringFieldAllowEmpty(fields, "output", path)
	if err != nil {
		return customToolCallOutputWire{}, err
	}
	status, err := optionalString(fields, "status", path+".status")
	if err != nil {
		return customToolCallOutputWire{}, err
	}
	errorMarker, err := optionalErrorMarker(fields, path+".error")
	if err != nil {
		return customToolCallOutputWire{}, err
	}
	return customToolCallOutputWire{CallID: callID, Output: output, Status: status, Error: errorMarker || resultStatusIsError(status)}, nil
}

func decodeTools(tools []json.RawMessage, maxTools int, maxSchemaBytes int64, limits DecoderLimits) ([]toolWire, error) {
	if maxTools <= 0 || len(tools) > maxTools {
		return nil, newError(ErrorRequestTooLarge, "tools", "tool count exceeds the configured limit")
	}
	result := make([]toolWire, 0, len(tools))
	names := make(map[string]struct{})
	schemaBytes := int64(0)
	for index, raw := range tools {
		path := fmt.Sprintf("tools[%d]", index)
		fields, err := rawObject(raw, path)
		if err != nil {
			return nil, err
		}
		typeRaw, ok := fields["type"]
		if !ok {
			return nil, invalidRequest(path+".type", "type is required")
		}
		toolType, err := requiredString(typeRaw, path+".type")
		if err != nil {
			return nil, err
		}
		var tool toolWire
		switch toolType {
		case string(bridge.ToolFunction):
			parsed, parseErr := decodeFunctionTool(fields, path, maxSchemaBytes-schemaBytes, limits)
			if parseErr != nil {
				return nil, parseErr
			}
			schemaBytes += int64(len(parsed.Parameters))
			tool = parsed
		case string(bridge.ToolCustom):
			parsed, parseErr := decodeCustomTool(fields, path)
			if parseErr != nil {
				return nil, parseErr
			}
			tool = parsed
		case string(bridge.ToolNamespace):
			parsed, parseErr := decodeNamespaceTool(fields, path, limits.MaxCollectionItems)
			if parseErr != nil {
				return nil, parseErr
			}
			tool = parsed
		case string(bridge.ToolWebSearch):
			parsed, parseErr := decodeWebSearchTool(fields, path)
			if parseErr != nil {
				return nil, parseErr
			}
			tool = parsed
		default:
			return nil, newError(ErrorUnsupportedToolType, path+".type", "tool type is not supported")
		}
		if name := tool.toolName(); name != "" {
			if _, exists := names[name]; exists {
				return nil, invalidRequest(path+".name", "tool name is duplicate")
			}
			names[name] = struct{}{}
		}
		result = append(result, tool)
	}
	return result, nil
}

func decodeCustomTool(fields map[string]json.RawMessage, path string) (customToolWire, error) {
	if err := rejectUnknown(fields, path, "type", "name", "description", "format"); err != nil {
		return customToolWire{}, err
	}
	name, err := requiredStringField(fields, "name", path)
	if err != nil {
		return customToolWire{}, err
	}
	description, err := optionalString(fields, "description", path+".description")
	if err != nil {
		return customToolWire{}, err
	}
	formatKind := bridge.CustomToolFormatText
	if raw, exists := fields["format"]; exists {
		formatFields, decodeErr := rawObject(raw, path+".format")
		if decodeErr != nil {
			return customToolWire{}, decodeErr
		}
		if decodeErr := rejectUnknown(formatFields, path+".format", "type"); decodeErr != nil {
			return customToolWire{}, decodeErr
		}
		formatType, decodeErr := requiredStringField(formatFields, "type", path+".format")
		if decodeErr != nil {
			return customToolWire{}, decodeErr
		}
		if bridge.CustomToolFormatKind(formatType) != bridge.CustomToolFormatText {
			return customToolWire{}, newError(ErrorInvalidRequest, path+".format.type", "only text custom-tool input is supported")
		}
	}
	return customToolWire{Name: name, Description: description, Format: formatKind}, nil
}

func decodeFunctionTool(fields map[string]json.RawMessage, path string, remainingSchemaBytes int64, limits DecoderLimits) (functionToolWire, error) {
	if err := rejectUnknown(fields, path, "type", "name", "description", "parameters", "strict"); err != nil {
		return functionToolWire{}, err
	}
	name, err := requiredStringField(fields, "name", path)
	if err != nil {
		return functionToolWire{}, err
	}
	description, err := optionalString(fields, "description", path+".description")
	if err != nil {
		return functionToolWire{}, err
	}
	parameters, ok := fields["parameters"]
	if !ok {
		return functionToolWire{}, invalidRequest(path+".parameters", "parameters JSON Schema is required")
	}
	if int64(len(parameters)) > remainingSchemaBytes {
		return functionToolWire{}, newError(ErrorRequestTooLarge, "tools", "aggregate function schema bytes exceed the configured limit")
	}
	if err := validateJSONSchema(parameters, limits); err != nil {
		return functionToolWire{}, invalidRequest(path+".parameters", "parameters must be a valid JSON Schema object")
	}
	strict, err := optionalBoolPointer(fields, "strict", path+".strict")
	if err != nil {
		return functionToolWire{}, err
	}
	return functionToolWire{Name: name, Description: description, Parameters: bytes.Clone(parameters), Strict: strict}, nil
}

func decodeNamespaceTool(fields map[string]json.RawMessage, path string, maxCollectionItems int) (deferredToolWire, error) {
	if err := rejectUnknown(fields, path, "type", "name", "description", "tools"); err != nil {
		return deferredToolWire{}, err
	}
	name, err := optionalString(fields, "name", path+".name")
	if err != nil {
		return deferredToolWire{}, err
	}
	if description, exists := fields["description"]; exists {
		if _, err := stringValue(description, path+".description"); err != nil {
			return deferredToolWire{}, err
		}
	}
	if raw, exists := fields["tools"]; exists {
		if _, err := rawArray(raw, path+".tools", maxCollectionItems); err != nil {
			return deferredToolWire{}, err
		}
	}
	return deferredToolWire{Kind: bridge.ToolNamespace, Name: name}, nil
}

func decodeWebSearchTool(fields map[string]json.RawMessage, path string) (deferredToolWire, error) {
	if err := rejectUnknown(fields, path, "type", "external_web_access"); err != nil {
		return deferredToolWire{}, err
	}
	if raw, exists := fields["external_web_access"]; exists {
		if _, err := boolValue(raw, path+".external_web_access"); err != nil {
			return deferredToolWire{}, err
		}
	}
	return deferredToolWire{Kind: bridge.ToolWebSearch}, nil
}

func decodeToolChoice(raw json.RawMessage) (toolChoiceWire, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		choice, err := stringValue(raw, "tool_choice")
		if err != nil {
			return toolChoiceWire{}, err
		}
		switch choice {
		case string(bridge.ToolChoiceAuto):
			return toolChoiceWire{Kind: bridge.ToolChoiceAuto}, nil
		case string(bridge.ToolChoiceNone):
			return toolChoiceWire{Kind: bridge.ToolChoiceNone}, nil
		case string(bridge.ToolChoiceRequired):
			return toolChoiceWire{Kind: bridge.ToolChoiceRequired}, nil
		default:
			return toolChoiceWire{}, invalidRequest("tool_choice", "tool choice is not supported")
		}
	}
	fields, err := rawObject(raw, "tool_choice")
	if err != nil {
		return toolChoiceWire{}, err
	}
	if err := rejectUnknown(fields, "tool_choice", "type", "name"); err != nil {
		return toolChoiceWire{}, err
	}
	typeRaw, ok := fields["type"]
	if !ok {
		return toolChoiceWire{}, invalidRequest("tool_choice.type", "type is required")
	}
	choiceType, err := requiredString(typeRaw, "tool_choice.type")
	if err != nil {
		return toolChoiceWire{}, err
	}
	if choiceType != "function" {
		return toolChoiceWire{}, invalidRequest("tool_choice.type", "tool choice type is not supported")
	}
	name, err := requiredStringField(fields, "name", "tool_choice")
	if err != nil {
		return toolChoiceWire{}, err
	}
	return toolChoiceWire{Kind: bridge.ToolChoiceFunction, FunctionName: name}, nil
}

func validateToolChoice(choice toolChoiceWire, tools []toolWire) error {
	if choice.Kind != bridge.ToolChoiceFunction {
		return nil
	}
	for _, tool := range tools {
		if function, ok := tool.(functionToolWire); ok && function.Name == choice.FunctionName {
			return nil
		}
	}
	return invalidRequest("tool_choice.name", "tool choice references an undeclared function tool")
}

func decodeReasoning(raw json.RawMessage) (reasoningWire, error) {
	fields, err := rawObject(raw, "reasoning")
	if err != nil {
		return reasoningWire{}, err
	}
	if err := rejectUnknown(fields, "reasoning", "effort", "summary"); err != nil {
		return reasoningWire{}, err
	}
	effort, err := optionalString(fields, "effort", "reasoning.effort")
	if err != nil {
		return reasoningWire{}, err
	}
	if summary, exists := fields["summary"]; exists {
		if _, err := stringValue(summary, "reasoning.summary"); err != nil {
			return reasoningWire{}, err
		}
	}
	return reasoningWire{Effort: effort}, nil
}

func decodeText(raw json.RawMessage, remainingSchemaBytes int64, limits DecoderLimits) (textWire, error) {
	fields, err := rawObject(raw, "text")
	if err != nil {
		return textWire{}, err
	}
	if err := rejectUnknown(fields, "text", "format", "verbosity"); err != nil {
		return textWire{}, err
	}
	if verbosity, exists := fields["verbosity"]; exists {
		if _, err := stringValue(verbosity, "text.verbosity"); err != nil {
			return textWire{}, err
		}
	}
	formatRaw, exists := fields["format"]
	if !exists {
		return textWire{Format: textFormatWire{Kind: bridge.TextFormatText}}, nil
	}
	formatFields, err := rawObject(formatRaw, "text.format")
	if err != nil {
		return textWire{}, err
	}
	formatTypeRaw, exists := formatFields["type"]
	if !exists {
		return textWire{}, invalidRequest("text.format.type", "type is required")
	}
	formatType, err := requiredString(formatTypeRaw, "text.format.type")
	if err != nil {
		return textWire{}, err
	}
	format := textFormatWire{}
	switch formatType {
	case string(bridge.TextFormatText):
		if err := rejectUnknown(formatFields, "text.format", "type"); err != nil {
			return textWire{}, err
		}
		format.Kind = bridge.TextFormatText
	case string(bridge.TextFormatJSONObject):
		if err := rejectUnknown(formatFields, "text.format", "type"); err != nil {
			return textWire{}, err
		}
		format.Kind = bridge.TextFormatJSONObject
	case string(bridge.TextFormatJSONSchema):
		if err := rejectUnknown(formatFields, "text.format", "type", "name", "description", "schema", "strict"); err != nil {
			return textWire{}, err
		}
		format.Kind = bridge.TextFormatJSONSchema
		format.Name, err = requiredStringField(formatFields, "name", "text.format")
		if err != nil {
			return textWire{}, err
		}
		format.Description, err = optionalString(formatFields, "description", "text.format.description")
		if err != nil {
			return textWire{}, err
		}
		format.Schema, exists = formatFields["schema"]
		if !exists {
			return textWire{}, invalidRequest("text.format.schema", "schema is required for json_schema format")
		}
		if int64(len(format.Schema)) > remainingSchemaBytes {
			return textWire{}, newError(ErrorRequestTooLarge, "text.format.schema", "aggregate schema bytes exceed the configured limit")
		}
		if err := validateJSONSchema(format.Schema, limits); err != nil {
			return textWire{}, invalidRequest("text.format.schema", "schema must be a valid JSON Schema object")
		}
		format.Strict, err = optionalBoolPointer(formatFields, "strict", "text.format.strict")
		if err != nil {
			return textWire{}, err
		}
	default:
		return textWire{}, invalidRequest("text.format.type", "text format is not supported")
	}
	return textWire{Format: format}, nil
}

func translateRequest(wire responsesRequestWire) (bridge.Request, error) {
	request := bridge.Request{
		Model:              wire.Model,
		Instructions:       wire.Instructions,
		PreviousResponseID: wire.PreviousResponseID,
		Input:              make([]bridge.InputItem, 0, len(wire.Input)),
		Tools:              make([]bridge.Tool, 0, len(wire.Tools)),
		ToolChoice:         bridge.ToolChoice{Kind: wire.ToolChoice.Kind, FunctionName: wire.ToolChoice.FunctionName},
		Generation: bridge.GenerationOptions{
			Stream:            wire.Generation.Stream,
			ParallelToolCalls: wire.Generation.ParallelToolCalls,
			Include:           append([]string(nil), wire.Generation.Include...),
			Reasoning:         bridge.ReasoningOptions{Effort: wire.Generation.Reasoning.Effort},
			Text: bridge.TextOptions{Format: bridge.TextFormat{
				Kind:        wire.Generation.Text.Format.Kind,
				Name:        wire.Generation.Text.Format.Name,
				Description: wire.Generation.Text.Format.Description,
				Strict:      cloneBoolPointer(wire.Generation.Text.Format.Strict),
			}},
		},
	}
	for _, item := range wire.Input {
		switch item := item.(type) {
		case messageWire:
			message := bridge.Message{ID: item.ID, Role: item.Role, Content: make([]bridge.ContentPart, 0, len(item.Content))}
			for _, content := range item.Content {
				message.Content = append(message.Content, bridge.TextContent{Text: content.Text})
			}
			request.Input = append(request.Input, message)
		case functionCallWire:
			request.Input = append(request.Input, bridge.FunctionCall{ID: item.ID, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Status: item.Status})
		case functionCallOutputWire:
			request.Input = append(request.Input, bridge.FunctionCallOutput{CallID: item.CallID, Output: item.Output, Status: item.Status, Error: item.Error})
		case customToolCallWire:
			request.Input = append(request.Input, bridge.CustomToolCall{ID: item.ID, CallID: item.CallID, Name: item.Name, Input: item.Input, Status: item.Status})
		case customToolCallOutputWire:
			request.Input = append(request.Input, bridge.CustomToolCallOutput{CallID: item.CallID, Output: item.Output, Status: item.Status, Error: item.Error})
		default:
			return bridge.Request{}, fmt.Errorf("internal error: unsupported decoded input %T", item)
		}
	}
	for _, tool := range wire.Tools {
		switch tool := tool.(type) {
		case functionToolWire:
			parameters, err := bridge.NewJSONSchema(tool.Parameters)
			if err != nil {
				return bridge.Request{}, fmt.Errorf("internal error: function schema was not validated: %w", err)
			}
			request.Tools = append(request.Tools, bridge.FunctionTool{Name: tool.Name, Description: tool.Description, Parameters: parameters, Strict: cloneBoolPointer(tool.Strict)})
		case customToolWire:
			request.Tools = append(request.Tools, bridge.CustomTool{Name: tool.Name, Description: tool.Description, Format: bridge.CustomToolFormat{Kind: tool.Format}})
		case deferredToolWire:
			request.Tools = append(request.Tools, bridge.DeferredTool{ToolKind: tool.Kind, Name: tool.Name})
		default:
			return bridge.Request{}, fmt.Errorf("internal error: unsupported decoded tool %T", tool)
		}
	}
	if len(wire.Generation.Text.Format.Schema) > 0 {
		schema, err := bridge.NewJSONSchema(wire.Generation.Text.Format.Schema)
		if err != nil {
			return bridge.Request{}, fmt.Errorf("internal error: text schema was not validated: %w", err)
		}
		request.Generation.Text.Format.Schema = schema
	}
	return request, nil
}

func sortedKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func rejectUnknown(fields map[string]json.RawMessage, path string, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		known[field] = struct{}{}
	}
	for _, field := range sortedKeys(fields) {
		if _, ok := known[field]; !ok {
			return unsupportedField(unknownFieldParam(path), "field is not classified for this Responses object")
		}
	}
	return nil
}

func rawObject(raw json.RawMessage, param string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, invalidRequest(param, "value must be a JSON object")
	}
	return fields, nil
}

func unknownFieldParam(path string) string {
	if path == "" {
		return "request.<unknown_field>"
	}
	return path + ".<unknown_field>"
}

func deferredFieldParam(field string) string {
	if field == "background" {
		return field
	}
	return "request.<deferred_field>"
}

func rawArray(raw json.RawMessage, param string, maxItems int) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, invalidRequest(param, "value must be a JSON array")
	}
	if maxItems <= 0 {
		return nil, newError(ErrorRequestTooLarge, param, "array item count exceeds the configured limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return nil, invalidRequest(param, "value must be a JSON array")
	}
	values := make([]json.RawMessage, 0, minInt(maxItems, 8))
	for decoder.More() {
		if len(values) >= maxItems {
			return nil, newError(ErrorRequestTooLarge, param, "array item count exceeds the configured limit")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalidRequest(param, "value must be a JSON array")
		}
		values = append(values, value)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, invalidRequest(param, "value must be a JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, invalidRequest(param, "value must be a JSON array")
	}
	return values, nil
}

func stringArray(raw json.RawMessage, param string, maxItems int) ([]string, error) {
	values, err := rawArray(raw, param, maxItems)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for index, value := range values {
		item, decodeErr := stringValue(value, fmt.Sprintf("%s[%d]", param, index))
		if decodeErr != nil {
			return nil, decodeErr
		}
		result = append(result, item)
	}
	return result, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func stringValue(raw json.RawMessage, param string) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalidRequest(param, "value must be a string")
	}
	return value, nil
}

func requiredString(raw json.RawMessage, param string) (string, error) {
	value, err := stringValue(raw, param)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", invalidRequest(param, "value is required")
	}
	return value, nil
}

func requiredStringField(fields map[string]json.RawMessage, field, path string) (string, error) {
	raw, ok := fields[field]
	if !ok {
		return "", invalidRequest(path+"."+field, "value is required")
	}
	return requiredString(raw, path+"."+field)
}

func requiredStringFieldAllowEmpty(fields map[string]json.RawMessage, field, path string) (string, error) {
	raw, ok := fields[field]
	if !ok {
		return "", invalidRequest(path+"."+field, "value is required")
	}
	return stringValue(raw, path+"."+field)
}

func optionalString(fields map[string]json.RawMessage, field, param string) (string, error) {
	raw, ok := fields[field]
	if !ok {
		return "", nil
	}
	return stringValue(raw, param)
}

// optionalErrorMarker records only whether a result carries an error marker.
// Error payloads can contain tool output or other sensitive text, so they are
// deliberately not copied into the bridge model or any client-facing error.
// Codex has emitted boolean, string, and object-shaped markers across result
// forms; every non-null marker is semantically an error unless it is false.
func optionalErrorMarker(fields map[string]json.RawMessage, param string) (bool, error) {
	raw, ok := fields["error"]
	if !ok {
		return false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(trimmed, &value); err == nil {
		return value, nil
	}
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return false, invalidRequest(param, "error marker is malformed")
	}
	return true, nil
}

func resultStatusIsError(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "failure", "cancelled", "canceled", "incomplete":
		return true
	default:
		return false
	}
}

func boolValue(raw json.RawMessage, param string) (bool, error) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, invalidRequest(param, "value must be a boolean")
	}
	return value, nil
}

func optionalBoolPointer(fields map[string]json.RawMessage, field, param string) (*bool, error) {
	raw, ok := fields[field]
	if !ok {
		return nil, nil
	}
	value, err := boolValue(raw, param)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func parseRole(value, param string) (bridge.Role, error) {
	role := bridge.Role(value)
	switch role {
	case bridge.RoleSystem, bridge.RoleDeveloper, bridge.RoleUser, bridge.RoleAssistant:
		return role, nil
	default:
		return "", invalidRequest(param, "message role is not supported")
	}
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
