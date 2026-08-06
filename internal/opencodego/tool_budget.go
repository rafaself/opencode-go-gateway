package opencodego

import "github.com/rafaself/opencode-go-gateway/internal/bridge"

// ValidateProviderToolBudget applies one provider-facing budget after
// request-scoped registrations have been resolved. Deferred Codex metadata is
// not sent upstream; every provider-visible function, including the implicit
// apply_patch wrapper, consumes one tool slot and its raw schema consumes the
// aggregate schema budget exactly once.
func ValidateProviderToolBudget(request bridge.Request, maxProviderTools int, maxSchemaBytes int64) error {
	if maxProviderTools <= 0 || maxSchemaBytes <= 0 {
		return providerError(ErrorInvalidConfiguration, nil)
	}

	registry := request.ToolRegistry
	providerTools := 0
	var schemaBytes int64
	providerNames := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		switch tool := tool.(type) {
		case bridge.FunctionTool:
			providerTools++
			if _, exists := providerNames[tool.Name]; exists {
				return providerError(ErrorInvalidRequest, nil)
			}
			providerNames[tool.Name] = struct{}{}
			schemaBytes += int64(len(tool.Parameters.RawJSON()))
		case bridge.CustomTool:
			if registry == nil {
				return providerError(ErrorInvalidRequest, nil)
			}
		case bridge.DeferredTool:
			if err := ValidateCapturedDeferredTool(tool); err != nil {
				return err
			}
			if registry == nil {
				return providerError(ErrorUnsupportedTool, nil)
			}
		default:
			return providerError(ErrorUnsupportedTool, nil)
		}
		if providerTools > maxProviderTools || schemaBytes > maxSchemaBytes {
			return providerError(ErrorRequestTooLarge, nil)
		}
	}

	for _, registration := range registry.Registrations() {
		if registration.Kind != bridge.ToolCustom {
			continue
		}
		if err := validateCustomToolRegistration(registration); err != nil {
			return err
		}
		providerTools++
		schemaBytes += ApplyPatchWrapperSchemaBytes()
		if providerTools > maxProviderTools || schemaBytes > maxSchemaBytes {
			return providerError(ErrorRequestTooLarge, nil)
		}
	}
	return nil
}
