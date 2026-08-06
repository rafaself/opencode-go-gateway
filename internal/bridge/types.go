// Package bridge contains the provider-neutral semantic request model.
//
// The package deliberately has no dependency on a wire protocol package. In
// particular, Responses item names and JSON object shapes belong in
// internal/codex; only validated meanings cross this boundary.
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Request is the protocol-neutral request understood by later upstream and
// streaming milestones.
type Request struct {
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Input              []InputItem       `json:"input"`
	Tools              []Tool            `json:"tools"`
	ToolChoice         ToolChoice        `json:"tool_choice"`
	Generation         GenerationOptions `json:"generation"`
	// ToolRegistry is request-scoped adapter state. It is deliberately not
	// serialized as part of the bridge request or shared between requests.
	// Provider adapters may attach it when a continuation chain needs stable
	// tool-name translations.
	ToolRegistry *ToolRegistry `json:"-"`
}

// GenerationOptions contains only options with a shared semantic meaning in
// the bridge. Compatibility-only wire fields are intentionally not present.
type GenerationOptions struct {
	Stream            bool             `json:"stream"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	Include           []string         `json:"include,omitempty"`
	Reasoning         ReasoningOptions `json:"reasoning"`
	Text              TextOptions      `json:"text"`
}

// ReasoningOptions contains generation controls that can be translated by an
// upstream client. Summary presentation preferences remain wire-level
// compatibility data until a shared semantic is defined.
type ReasoningOptions struct {
	Effort string `json:"effort,omitempty"`
}

// TextOptions contains the supported output-format semantics.
type TextOptions struct {
	Format TextFormat `json:"format"`
}

// TextFormatKind identifies the supported response text format.
type TextFormatKind string

const (
	TextFormatText       TextFormatKind = "text"
	TextFormatJSONSchema TextFormatKind = "json_schema"
	TextFormatJSONObject TextFormatKind = "json_object"
)

// TextFormat is an explicit union discriminator for output formatting. A
// JSON-schema format carries validated raw JSON because interpreting every
// schema keyword is outside the bridge's responsibility.
type TextFormat struct {
	Kind        TextFormatKind `json:"kind"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Schema      JSONSchema     `json:"schema,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// ToolChoiceKind identifies the request's tool-selection policy.
type ToolChoiceKind string

const (
	ToolChoiceUnset    ToolChoiceKind = ""
	ToolChoiceAuto     ToolChoiceKind = "auto"
	ToolChoiceNone     ToolChoiceKind = "none"
	ToolChoiceRequired ToolChoiceKind = "required"
	ToolChoiceFunction ToolChoiceKind = "function"
)

// ToolChoice is an explicit union for the string and function-object forms
// accepted by the Responses boundary.
type ToolChoice struct {
	Kind         ToolChoiceKind `json:"kind"`
	FunctionName string         `json:"function_name,omitempty"`
}

// Role is a semantic message role, independent of a provider's wire structs.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// InputKind identifies the explicit input union members.
type InputKind string

const (
	InputMessage              InputKind = "message"
	InputFunctionCall         InputKind = "function_call"
	InputFunctionCallOutput   InputKind = "function_call_output"
	InputCustomToolCall       InputKind = "custom_tool_call"
	InputCustomToolCallOutput InputKind = "custom_tool_call_output"
)

// InputItem is the validated input union consumed by later milestones.
type InputItem interface {
	Kind() InputKind
	isInputItem()
}

// Message is a user, developer, system, or assistant message.
type Message struct {
	ID      string        `json:"id,omitempty"`
	Role    Role          `json:"role"`
	Content []ContentPart `json:"content"`
}

func (Message) Kind() InputKind { return InputMessage }
func (Message) isInputItem()    {}

// FunctionCall is a function tool invocation from a prior Responses turn.
type FunctionCall struct {
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status,omitempty"`
}

func (FunctionCall) Kind() InputKind    { return InputFunctionCall }
func (FunctionCall) isInputItem()       {}
func (FunctionCall) ToolKind() ToolKind { return ToolFunction }

// FunctionCallOutput is the result correlated to a FunctionCall.
type FunctionCallOutput struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func (FunctionCallOutput) Kind() InputKind    { return InputFunctionCallOutput }
func (FunctionCallOutput) isInputItem()       {}
func (FunctionCallOutput) ToolKind() ToolKind { return ToolFunction }

// CustomToolCall is a custom tool invocation such as apply_patch.
type CustomToolCall struct {
	ID     string `json:"id,omitempty"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
	Status string `json:"status,omitempty"`
}

func (CustomToolCall) Kind() InputKind    { return InputCustomToolCall }
func (CustomToolCall) isInputItem()       {}
func (CustomToolCall) ToolKind() ToolKind { return ToolCustom }

// CustomToolCallOutput is the result correlated to a CustomToolCall.
type CustomToolCallOutput struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func (CustomToolCallOutput) Kind() InputKind    { return InputCustomToolCallOutput }
func (CustomToolCallOutput) isInputItem()       {}
func (CustomToolCallOutput) ToolKind() ToolKind { return ToolCustom }

// ContentKind identifies an explicit message content union member.
type ContentKind string

const (
	ContentInputText ContentKind = "input_text"
)

// ContentPart is the validated message content union.
type ContentPart interface {
	Kind() ContentKind
	isContentPart()
}

// TextContent is the currently supported input_text content part.
type TextContent struct {
	Text string `json:"text"`
}

func (TextContent) Kind() ContentKind { return ContentInputText }
func (TextContent) isContentPart()    {}

// ToolKind distinguishes supported and explicitly deferred tools.
type ToolKind string

const (
	ToolFunction  ToolKind = "function"
	ToolNamespace ToolKind = "namespace"
	ToolWebSearch ToolKind = "web_search"
	ToolCustom    ToolKind = "custom"
)

// Tool is the explicit tool declaration union. DeferredTool is retained as an
// explicit marker for observed declarations that the first bridge milestone
// cannot translate; it never carries an untyped provider map.
type Tool interface {
	Kind() ToolKind
	isTool()
}

// FunctionTool is a function declaration with a validated JSON Schema.
type FunctionTool struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  JSONSchema `json:"parameters"`
	Strict      *bool      `json:"strict,omitempty"`
}

func (FunctionTool) Kind() ToolKind { return ToolFunction }
func (FunctionTool) isTool()        {}

// CustomTool is the Responses custom-tool declaration. The current gateway
// translates only the text/freeform apply_patch declaration at the provider
// boundary; retaining the explicit union here prevents it from being
// mistaken for a generic function schema.
type CustomTool struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Format      CustomToolFormat `json:"format"`
}

func (CustomTool) Kind() ToolKind { return ToolCustom }
func (CustomTool) isTool()        {}

type CustomToolFormatKind string

const (
	CustomToolFormatText CustomToolFormatKind = "text"
)

type CustomToolFormat struct {
	Kind CustomToolFormatKind `json:"kind"`
}

// DeferredTool identifies a known provider tool that is intentionally held
// out of translation until a later milestone.
type DeferredTool struct {
	ToolKind ToolKind `json:"tool_kind"`
	Name     string   `json:"name,omitempty"`
}

func (tool DeferredTool) Kind() ToolKind { return tool.ToolKind }
func (DeferredTool) isTool()             {}

// JSONSchema preserves a validated schema without exposing mutable backing
// storage or forcing the bridge to understand every JSON Schema keyword.
type JSONSchema struct {
	raw []byte
}

// NewJSONSchema makes an immutable copy of a valid JSON value. Schema
// semantics are validated by internal/codex before this constructor is used.
func NewJSONSchema(raw []byte) (JSONSchema, error) {
	if !json.Valid(raw) {
		return JSONSchema{}, fmt.Errorf("invalid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return JSONSchema{}, fmt.Errorf("JSON Schema root must be an object")
	}
	return JSONSchema{raw: bytes.Clone(raw)}, nil
}

// RawJSON returns a copy of the exact schema JSON supplied at the boundary.
func (schema JSONSchema) RawJSON() []byte {
	return bytes.Clone(schema.raw)
}

// MarshalJSON keeps schema values as JSON objects rather than exposing the
// bridge's private storage representation in deterministic test output.
func (schema JSONSchema) MarshalJSON() ([]byte, error) {
	if len(schema.raw) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(schema.raw) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return bytes.Clone(schema.raw), nil
}
