package opencodego

import "github.com/rafaself/opencode-go-gateway/internal/bridge"

const capturedNamespaceToolName = "mcp"

// ValidateCapturedDeferredTool applies the request-scoped policy for the
// deferred tool declarations observed in the #2 capture. The accepted values
// are metadata-only and are intentionally omitted from the provider request;
// no arbitrary namespace or web-search declaration is treated as equivalent.
//
// The policy is kept at the provider adapter boundary and is immutable: it is
// a function over one bridge value, not mutable package-level registration
// state.
func ValidateCapturedDeferredTool(tool bridge.DeferredTool) error {
	switch tool.ToolKind {
	case bridge.ToolNamespace:
		if tool.Name == capturedNamespaceToolName {
			return nil
		}
	case bridge.ToolWebSearch:
		if tool.Name == "" {
			return nil
		}
	}
	return providerError(ErrorUnsupportedTool, nil)
}
