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
	request.ToolRegistry = registry
	if err := ValidateProviderToolBudget(request, bridge.DefaultMaxProviderTools, bridge.DefaultMaxFunctionSchemaBytes); err != nil {
		return ChatCompletionRequest{}, err
	}
	var messages []ChatMessage
	if request.Continuation != nil {
		continuation, ok := request.Continuation.(*ContinuationRequest)
		if !ok || continuation == nil || continuation.lease == nil {
			return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
		}
		messages, err = mapContinuationInputItems(request.Input, continuation.lease, model)
	} else {
		messages, err = mapInputItems(request.Input, registry)
	}
	if err != nil {
		return ChatCompletionRequest{}, err
	}
	result.Messages = append(result.Messages, messages...)
	if len(result.Messages) == 0 {
		return ChatCompletionRequest{}, providerError(ErrorInvalidRequest, nil)
	}

	toolNames := make(map[string]struct{}, len(request.Tools))
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
			toolNames[mapped.Function.Name] = struct{}{}
			result.Tools = append(result.Tools, mapped)
		}
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

func mapContinuationInputItems(items []bridge.InputItem, lease *ContinuationLease, model string) ([]ChatMessage, error) {
	if lease == nil {
		return nil, providerError(ErrorInvalidRequest, nil)
	}
	turn := lease.Turn()
	if turn.Provider != ProviderName || (turn.Model != "" && turn.Model != model) || len(turn.ToolCalls) == 0 {
		return nil, providerError(ErrorInvalidRequest, nil)
	}
	results := lease.Results()
	if len(results) != len(turn.ToolCalls) {
		return nil, providerError(ErrorInvalidRequest, nil)
	}
	callKinds := make(map[string]bridge.ToolKind, len(turn.ToolCalls))
	callNames := make(map[string]string, len(turn.ToolCalls))
	callArguments := make(map[string]string, len(turn.ToolCalls))
	providerIDs := make(map[string]string, len(turn.ToolCalls))
	for _, call := range turn.ToolCalls {
		callKinds[call.CallID] = call.Kind
		providerIDs[call.CallID] = call.ProviderCallID
		if call.Kind == bridge.ToolCustom {
			callNames[call.CallID] = ApplyPatchToolName
			callArguments[call.CallID] = call.Arguments
		} else {
			callNames[call.CallID] = call.Name
			callArguments[call.CallID] = call.Arguments
		}
	}
	seenCalls := make(map[string]struct{}, len(turn.ToolCalls))
	seenResults := make(map[string]struct{}, len(results))
	messages := make([]ChatMessage, 0, len(items)+1+len(results))
	replayed := false
	localCallsPresent := false
	for _, item := range items {
		switch value := item.(type) {
		case bridge.FunctionCall:
			localCallsPresent = true
			if err := validateContinuationCall(value.CallID, bridge.ToolFunction, value.Name, value.Arguments, callKinds, callNames, callArguments, seenCalls); err != nil {
				return nil, err
			}
			if !replayed {
				messages = append(messages, continuationMessages(turn, results, providerIDs)...)
				replayed = true
			}
		case bridge.CustomToolCall:
			localCallsPresent = true
			wrapped, err := wrapApplyPatchInput(value.Input)
			if err != nil {
				return nil, providerError(ErrorInvalidRequest, err)
			}
			if err := validateContinuationCall(value.CallID, bridge.ToolCustom, value.Name, wrapped, callKinds, callNames, callArguments, seenCalls); err != nil {
				return nil, err
			}
			if !replayed {
				messages = append(messages, continuationMessages(turn, results, providerIDs)...)
				replayed = true
			}
		case bridge.FunctionCallOutput:
			if err := validateContinuationResult(value.CallID, bridge.ToolFunction, callKinds, seenResults); err != nil {
				return nil, err
			}
			if !replayed {
				messages = append(messages, continuationMessages(turn, results, providerIDs)...)
				replayed = true
			}
		case bridge.CustomToolCallOutput:
			if err := validateContinuationResult(value.CallID, bridge.ToolCustom, callKinds, seenResults); err != nil {
				return nil, err
			}
			if !replayed {
				messages = append(messages, continuationMessages(turn, results, providerIDs)...)
				replayed = true
			}
		default:
			message, err := mapInputItem(item)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
		}
	}
	if !replayed || (localCallsPresent && len(seenCalls) != len(turn.ToolCalls)) || len(seenResults) != len(results) {
		return nil, providerError(ErrorInvalidRequest, nil)
	}
	return messages, nil
}

func validateContinuationCall(callID string, kind bridge.ToolKind, name, arguments string, callKinds map[string]bridge.ToolKind, callNames, callArguments map[string]string, seen map[string]struct{}) error {
	wantKind, ok := callKinds[callID]
	if !ok {
		return providerError(ErrorInvalidRequest, nil)
	}
	if wantKind != kind || callNames[callID] != name {
		return providerError(ErrorInvalidRequest, nil)
	}
	if kind == bridge.ToolCustom {
		storedInput, storedErr := unwrapApplyPatchArguments(callArguments[callID])
		incomingInput, incomingErr := unwrapApplyPatchArguments(arguments)
		if storedErr != nil || incomingErr != nil || storedInput != incomingInput {
			return providerError(ErrorInvalidRequest, nil)
		}
	} else if callArguments[callID] != arguments {
		return providerError(ErrorInvalidRequest, nil)
	}
	if _, exists := seen[callID]; exists {
		return providerError(ErrorInvalidRequest, nil)
	}
	seen[callID] = struct{}{}
	return nil
}

func validateContinuationResult(callID string, kind bridge.ToolKind, callKinds map[string]bridge.ToolKind, seen map[string]struct{}) error {
	wantKind, ok := callKinds[callID]
	if !ok || wantKind != kind {
		return providerError(ErrorInvalidRequest, nil)
	}
	if _, exists := seen[callID]; exists {
		return providerError(ErrorInvalidRequest, nil)
	}
	seen[callID] = struct{}{}
	return nil
}

func continuationMessages(turn PendingTurn, results []bridge.ToolResult, providerIDs map[string]string) []ChatMessage {
	content := turn.AssistantContent
	reasoning := turn.ReasoningContent
	calls := make([]ToolCall, 0, len(turn.ToolCalls))
	for _, call := range turn.ToolCalls {
		providerID := call.ProviderCallID
		if mapped := providerIDs[call.CallID]; mapped != "" {
			providerID = mapped
		}
		calls = append(calls, ToolCall{
			ID:   providerID,
			Type: "function",
			Function: ToolCallFunction{
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	messages := []ChatMessage{{Role: "assistant", Content: &content, ReasoningContent: &reasoning, ToolCalls: calls}}
	for _, result := range results {
		providerID := providerIDs[result.CallID]
		output := result.Output
		messages = append(messages, ChatMessage{Role: "tool", Content: &output, ToolCallID: providerID})
	}
	return messages
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

// ValidateModel is the exported validation boundary used by the runtime
// configuration before a client is constructed.
func ValidateModel(model string) error {
	return validateProviderModel(model)
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
	case DefaultModel, DeepSeekV4ProModel, DeepSeekV4FlashFreeModel:
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
