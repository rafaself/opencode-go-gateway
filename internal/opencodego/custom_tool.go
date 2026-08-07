package opencodego

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

const (
	// ApplyPatchToolName is the Codex-facing custom tool name observed in the
	// #2 request/result fixtures and custom Responses events.
	ApplyPatchToolName = "apply_patch"
	// ReservedToolNamePrefix prevents a user function from impersonating an
	// adapter-owned function in the provider request.
	ReservedToolNamePrefix = "__ocg_"
	// ApplyPatchUpstreamName is never emitted in Codex-facing Responses events.
	ApplyPatchUpstreamName = ReservedToolNamePrefix + ApplyPatchToolName
	ApplyPatchWrapperField = "input"

	applyPatchWrapperSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The exact freeform apply_patch text, including all newlines and markers."}},"required":["input"],"additionalProperties":false}`

	// DefaultMaxApplyPatchInputBytes is an exclusive upper bound, independent
	// from generic function-call argument limits. Inputs of this size or larger
	// are rejected, so the largest accepted input is one byte smaller.
	DefaultMaxApplyPatchInputBytes = 512 << 10
)

var (
	ErrMalformedCustomToolArguments = errors.New("malformed custom tool arguments")
	ErrApplyPatchInputLimit         = errors.New("apply_patch input exceeds its limit")
)

// NewToolRegistry creates the adapter registry for one request or
// continuation chain. The custom apply_patch capability may be implicit (as
// in the #2 capture) or arrive as the Responses `type=custom` text
// declaration. Both forms use the same request-scoped registration.
func NewToolRegistry(request bridge.Request) (*bridge.ToolRegistry, error) {
	if err := validateToolNameCollisions(request.Tools); err != nil {
		return nil, err
	}
	if request.ToolRegistry != nil {
		return request.ToolRegistry, nil
	}
	return bridge.NewToolRegistry([]bridge.ToolRegistration{{
		Kind:         bridge.ToolCustom,
		InboundName:  ApplyPatchToolName,
		UpstreamName: ApplyPatchUpstreamName,
		WrapperField: ApplyPatchWrapperField,
	}})
}

func validateToolNameCollisions(tools []bridge.Tool) error {
	customDeclarationSeen := false
	for _, tool := range tools {
		switch tool := tool.(type) {
		case bridge.FunctionTool:
			if strings.HasPrefix(tool.Name, ReservedToolNamePrefix) || tool.Name == ApplyPatchToolName {
				return providerError(ErrorInvalidRequest, nil)
			}
		case bridge.CustomTool:
			if tool.Name != ApplyPatchToolName || (tool.Format.Kind != "" && tool.Format.Kind != bridge.CustomToolFormatText && tool.Format.Kind != bridge.CustomToolFormatGrammar) || customDeclarationSeen {
				return providerError(ErrorInvalidRequest, nil)
			}
			customDeclarationSeen = true
		}
	}
	return nil
}

func customToolDeclaration(tool bridge.Tool, registry *bridge.ToolRegistry) (bool, error) {
	custom, ok := tool.(bridge.CustomTool)
	if !ok {
		return false, nil
	}
	if registry == nil {
		return true, providerError(ErrorInvalidRequest, nil)
	}
	registration, registered := registry.Inbound(custom.Name)
	if !registered || validateCustomToolRegistration(registration) != nil {
		return true, providerError(ErrorInvalidRequest, nil)
	}
	return true, nil
}

func hasCustomToolDeclaration(tools []bridge.Tool) bool {
	for _, tool := range tools {
		if _, ok := tool.(bridge.CustomTool); ok {
			return true
		}
	}
	return false
}

func validateCustomToolRegistration(registration bridge.ToolRegistration) error {
	if registration.Kind != bridge.ToolCustom || registration.InboundName != ApplyPatchToolName || registration.UpstreamName != ApplyPatchUpstreamName || registration.WrapperField != ApplyPatchWrapperField {
		return providerError(ErrorInvalidRequest, nil)
	}
	return nil
}

func applyPatchTool() ChatCompletionTool {
	strict := true
	return ChatCompletionTool{
		Type: "function",
		Function: FunctionDefinition{
			Name:        ApplyPatchUpstreamName,
			Description: "Apply the exact patch text supplied by Codex to the current workspace.",
			Parameters:  json.RawMessage(applyPatchWrapperSchema),
			Strict:      &strict,
		},
	}
}

// ApplyPatchWrapperSchemaBytes returns the exact provider schema size reserved
// by the synthetic apply_patch function. The provider-tool budget counts this
// registration once, whether the Codex declaration is implicit or explicit.
func ApplyPatchWrapperSchemaBytes() int64 {
	return int64(len(applyPatchWrapperSchema))
}

func wrapApplyPatchInput(input string) (string, error) {
	if !utf8.ValidString(input) {
		return "", providerError(ErrorInvalidRequest, nil)
	}
	if err := validateApplyPatchInputLength(input); err != nil {
		return "", providerError(ErrorInvalidRequest, ErrApplyPatchInputLimit)
	}
	// A struct, rather than a map, keeps the wrapper's one-field shape
	// deterministic. JSON escaping changes only the provider wire encoding;
	// decoding the field returns the exact original Go string.
	encoded, err := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: input})
	if err != nil {
		return "", providerError(ErrorInvalidRequest, nil)
	}
	return string(encoded), nil
}

func unwrapApplyPatchArguments(arguments string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	fields, err := decodeApplyPatchObject(decoder)
	if err != nil {
		return "", ErrMalformedCustomToolArguments
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", ErrMalformedCustomToolArguments
	}
	if len(fields) != 1 {
		return "", ErrMalformedCustomToolArguments
	}
	raw, ok := fields[ApplyPatchWrapperField]
	if !ok || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", ErrMalformedCustomToolArguments
	}
	var input string
	if err := json.Unmarshal(raw, &input); err != nil || !utf8.ValidString(input) {
		return "", ErrMalformedCustomToolArguments
	}
	if err := validateApplyPatchInputLength(input); err != nil {
		return "", err
	}
	return input, nil
}

func validateApplyPatchInputLength(input string) error {
	if len(input) >= DefaultMaxApplyPatchInputBytes {
		return ErrApplyPatchInputLimit
	}
	return nil
}

func decodeApplyPatchObject(decoder *json.Decoder) (map[string]json.RawMessage, error) {
	first, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := first.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, ErrMalformedCustomToolArguments
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrMalformedCustomToolArguments
		}
		if _, exists := fields[key]; exists {
			return nil, ErrMalformedCustomToolArguments
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	last, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := last.(json.Delim); !ok || delimiter != '}' {
		return nil, ErrMalformedCustomToolArguments
	}
	return fields, nil
}

func customToolRegistration(registry *bridge.ToolRegistry, upstreamName string) (bridge.ToolRegistration, bool) {
	if registry == nil {
		return bridge.ToolRegistration{}, false
	}
	registration, ok := registry.Upstream(upstreamName)
	if !ok || validateCustomToolRegistration(registration) != nil {
		return bridge.ToolRegistration{}, false
	}
	return registration, true
}

func customToolErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrApplyPatchInputLimit):
		return "stream_limit_exceeded"
	case errors.Is(err, ErrMalformedCustomToolArguments):
		return "upstream_custom_tool_invalid"
	default:
		return "upstream_stream_error"
	}
}

func customToolErrorMessage(err error) string {
	switch {
	case errors.Is(err, ErrApplyPatchInputLimit):
		return "The apply_patch input exceeded its configured limit."
	case errors.Is(err, ErrMalformedCustomToolArguments):
		return "The upstream apply_patch wrapper was invalid."
	default:
		return "The upstream custom tool could not be decoded."
	}
}
