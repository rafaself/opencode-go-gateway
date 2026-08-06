package opencodego

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

// MapRequest translates a bridge request using the default explicit thinking
// policy. The bridge's model is intentionally not forwarded: the provider
// model is selected by the client configuration.
func MapRequest(request bridge.Request, model string) (ChatCompletionRequest, error) {
	return MapRequestWithThinking(request, model, ThinkingEnabled)
}

// MapRequestWithThinking is the pure mapping API used by the HTTP client and
// by tests. ThinkingDefault is normalized to the documented enabled MVP
// policy.
func MapRequestWithThinking(request bridge.Request, model string, mode ThinkingMode) (ChatCompletionRequest, error) {
	if err := validateProviderModel(model); err != nil {
		return ChatCompletionRequest{}, err
	}
	if err := validateToolNameCollisions(request.Tools); err != nil {
		return ChatCompletionRequest{}, err
	}
	if !request.Generation.Stream {
		return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
	}
	if mode == ThinkingDefault {
		mode = ThinkingEnabled
	}
	if mode != ThinkingEnabled && mode != ThinkingDisabled {
		return ChatCompletionRequest{}, providerError(ErrorInvalidConfiguration, nil)
	}
	if mode == ThinkingDisabled && request.Generation.Reasoning.Effort != "" {
		return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
	}

	result := ChatCompletionRequest{
		Model:             model,
		Messages:          make([]ChatMessage, 0, len(request.Input)+1),
		ParallelToolCalls: request.Generation.ParallelToolCalls,
		Stream:            true,
		Thinking:          &ThinkingOptions{Type: string(mode)},
	}
	if request.Instructions != "" {
		content := request.Instructions
		result.Messages = append(result.Messages, ChatMessage{Role: "system", Content: &content})
	}

	registry := request.ToolRegistry
	var err error
	if registry == nil && hasCustomToolDeclaration(request.Tools) {
		registry, err = NewToolRegistry(request)
		if err != nil {
			return ChatCompletionRequest{}, err
		}
	}
	messages, err := mapInputItems(request.Input, registry)
	if err != nil {
		return ChatCompletionRequest{}, err
	}
	result.Messages = append(result.Messages, messages...)
	if len(result.Messages) == 0 {
		return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
	}

	if len(request.Tools) > bridge.DefaultMaxFunctionTools {
		return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
	}
	toolNames := make(map[string]struct{}, len(request.Tools))
	schemaBytes := 0
	for _, tool := range request.Tools {
		if custom, err := customToolDeclaration(tool, registry); custom {
			if err != nil {
				return ChatCompletionRequest{}, err
			}
			continue
		}
		if deferred, ok := tool.(bridge.DeferredTool); ok {
			if err := ValidateCapturedDeferredTool(deferred); err != nil {
				return ChatCompletionRequest{}, err
			}
			if registry != nil {
				continue
			}
		}
		mapped, err := mapTool(tool)
		if err != nil {
			return ChatCompletionRequest{}, err
		}
		name := mapped.Function.Name
		if _, exists := toolNames[name]; exists {
			return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
		}
		toolNames[name] = struct{}{}
		if len(mapped.Function.Parameters) > bridge.DefaultMaxFunctionSchemaBytes-schemaBytes {
			return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
		}
		schemaBytes += len(mapped.Function.Parameters)
		result.Tools = append(result.Tools, mapped)
	}
	if registry != nil {
		for _, registration := range registry.Registrations() {
			if registration.Kind != bridge.ToolCustom {
				continue
			}
			if err := validateCustomToolRegistration(registration); err != nil {
				return ChatCompletionRequest{}, err
			}
			if _, exists := toolNames[registration.UpstreamName]; exists {
				return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
			}
			mapped := applyPatchTool()
			if len(mapped.Function.Parameters) > bridge.DefaultMaxFunctionSchemaBytes-schemaBytes {
				return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
			}
			toolNames[mapped.Function.Name] = struct{}{}
			result.Tools = append(result.Tools, mapped)
			schemaBytes += len(mapped.Function.Parameters)
		}
	}
	if len(result.Tools) > bridge.DefaultMaxFunctionTools {
		return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
	}

	if request.Generation.Reasoning.Effort != "" {
		mapped, err := mapReasoningEffort(request.Generation.Reasoning.Effort)
		if err != nil {
			return ChatCompletionRequest{}, err
		}
		result.ReasoningEffort = mapped
	}

	choice, err := mapToolChoice(request.ToolChoice, result.Tools, mode)
	if err != nil {
		return ChatCompletionRequest{}, err
	}
	result.ToolChoice = choice

	switch request.Generation.Text.Format.Kind {
	case "", bridge.TextFormatText:
	case bridge.TextFormatJSONObject:
		result.ResponseFormat = &ResponseFormat{Type: "json_object"}
	case bridge.TextFormatJSONSchema:
		return ChatCompletionRequest{}, providerError(ErrorUnsupportedResponseFormat, nil)
	default:
		return ChatCompletionRequest{}, providerError(ErrorUnsupportedResponseFormat, nil)
	}
	return result, nil
}

func mapInputItems(items []bridge.InputItem, registry *bridge.ToolRegistry) ([]ChatMessage, error) {
	messages := make([]ChatMessage, 0, len(items))
	for index := 0; index < len(items); {
		if isToolCallInput(items[index]) {
			calls := make([]ToolCall, 0, 1)
			for index < len(items) && isToolCallInput(items[index]) {
				call, err := mapToolCallInput(items[index], registry)
				if err != nil {
					return nil, err
				}
				calls = append(calls, call)
				index++
			}
			messages = append(messages, ChatMessage{Role: "assistant", ToolCalls: calls})
			continue
		}
		message, err := mapInputItem(items[index])
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
		index++
	}
	return messages, nil
}

func isToolCallInput(item bridge.InputItem) bool {
	switch item.(type) {
	case bridge.FunctionCall, bridge.CustomToolCall:
		return true
	default:
		return false
	}
}

func mapInputItem(item bridge.InputItem) (ChatMessage, error) {
	switch item := item.(type) {
	case bridge.Message:
		role, err := mapRole(item.Role)
		if err != nil {
			return ChatMessage{}, err
		}
		content, err := messageContent(item.Content)
		if err != nil {
			return ChatMessage{}, err
		}
		return ChatMessage{Role: role, Content: &content}, nil
	case bridge.FunctionCallOutput:
		return mapToolOutput(item.CallID, item.Output)
	case bridge.CustomToolCallOutput:
		return mapToolOutput(item.CallID, item.Output)
	default:
		return ChatMessage{}, providerError(ErrorInvalidRequest, nil)
	}
}

func mapToolCallInput(item bridge.InputItem, registry *bridge.ToolRegistry) (ToolCall, error) {
	var callID, name, arguments string
	switch item := item.(type) {
	case bridge.FunctionCall:
		callID, name, arguments = item.CallID, item.Name, item.Arguments
	case bridge.CustomToolCall:
		registration, ok := registry.Inbound(item.Name)
		if !ok || validateCustomToolRegistration(registration) != nil {
			return ToolCall{}, providerError(ErrorInvalidRequest, nil)
		}
		wrapped, err := wrapApplyPatchInput(item.Input)
		if err != nil {
			return ToolCall{}, err
		}
		callID, name, arguments = item.CallID, registration.UpstreamName, wrapped
	default:
		return ToolCall{}, providerError(ErrorInvalidRequest, nil)
	}
	if strings.TrimSpace(callID) == "" || !validFunctionName(name) {
		return ToolCall{}, providerError(ErrorInvalidRequest, nil)
	}
	return ToolCall{
		ID:   callID,
		Type: "function",
		Function: ToolCallFunction{
			Name:      name,
			Arguments: arguments,
		},
	}, nil
}

func mapRole(role bridge.Role) (string, error) {
	switch role {
	case bridge.RoleSystem, bridge.RoleDeveloper:
		// DeepSeek's Chat Completions contract has system but not developer.
		// Both bridge instruction semantics therefore remain system messages,
		// with their original ordering and boundaries intact.
		return "system", nil
	case bridge.RoleUser:
		return "user", nil
	case bridge.RoleAssistant:
		return "assistant", nil
	default:
		return "", providerError(ErrorInvalidRequest, nil)
	}
}

func messageContent(parts []bridge.ContentPart) (string, error) {
	if len(parts) == 0 {
		return "", nil
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		text, ok := part.(bridge.TextContent)
		if !ok {
			return "", providerError(ErrorInvalidRequest, nil)
		}
		values = append(values, text.Text)
	}
	return strings.Join(values, "\n"), nil
}

func mapToolOutput(callID, output string) (ChatMessage, error) {
	if strings.TrimSpace(callID) == "" {
		return ChatMessage{}, providerError(ErrorInvalidRequest, nil)
	}
	return ChatMessage{Role: "tool", Content: &output, ToolCallID: callID}, nil
}

func mapTool(tool bridge.Tool) (ChatCompletionTool, error) {
	function, ok := tool.(bridge.FunctionTool)
	if !ok {
		return ChatCompletionTool{}, providerError(ErrorUnsupportedTool, nil)
	}
	if !validFunctionName(function.Name) {
		return ChatCompletionTool{}, providerError(ErrorInvalidRequest, nil)
	}
	parameters := function.Parameters.RawJSON()
	if len(parameters) == 0 || !json.Valid(parameters) || !isJSONObject(parameters) {
		return ChatCompletionTool{}, providerError(ErrorInvalidRequest, nil)
	}
	return ChatCompletionTool{
		Type: "function",
		Function: FunctionDefinition{
			Name:        function.Name,
			Description: function.Description,
			Parameters:  bytes.Clone(parameters),
			Strict:      cloneBool(function.Strict),
		},
	}, nil
}

func mapToolChoice(choice bridge.ToolChoice, tools []ChatCompletionTool, mode ThinkingMode) (*ToolChoice, error) {
	switch choice.Kind {
	case bridge.ToolChoiceUnset:
		return nil, nil
	case bridge.ToolChoiceAuto:
		if mode == ThinkingEnabled {
			// The thinking-mode policy omits auto because the provider already
			// defaults to auto when function tools are present.
			return nil, nil
		}
		if len(tools) == 0 {
			return nil, nil
		}
		return &ToolChoice{String: "auto"}, nil
	case bridge.ToolChoiceNone:
		return &ToolChoice{String: "none"}, nil
	case bridge.ToolChoiceRequired:
		return nil, providerError(ErrorUnsupportedToolChoice, nil)
	case bridge.ToolChoiceFunction:
		return nil, providerError(ErrorUnsupportedToolChoice, nil)
	default:
		return nil, providerError(ErrorUnsupportedToolChoice, nil)
	}
}

func mapReasoningEffort(effort string) (string, error) {
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return "", providerError(ErrorInvalidRequest, nil)
	}
	switch effort {
	case "low", "medium", "high":
		return "high", nil
	case "xhigh", "max":
		return "max", nil
	default:
		return "", providerError(ErrorInvalidRequest, nil)
	}
}

func validateProviderModel(model string) error {
	if strings.TrimSpace(model) == "" || len(model) > 256 || !utf8.ValidString(model) {
		return providerError(ErrorInvalidConfiguration, nil)
	}
	for _, runeValue := range model {
		if unicode.IsSpace(runeValue) || unicode.IsControl(runeValue) {
			return providerError(ErrorInvalidConfiguration, nil)
		}
	}
	if !isSupportedProviderModel(model) {
		return providerError(ErrorInvalidConfiguration, nil)
	}
	return nil
}

func validFunctionName(name string) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for index := 0; index < len(name); index++ {
		value := name[index]
		if !((value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '_' || value == '-') {
			return false
		}
	}
	return true
}

func isSupportedProviderModel(model string) bool {
	switch model {
	case DefaultModel, DeepSeekV4ProModel:
		return true
	default:
		return false
	}
}

func isJSONObject(raw []byte) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
