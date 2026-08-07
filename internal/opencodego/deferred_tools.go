package opencodego

import "github.com/rafaself/opencode-go-gateway/internal/bridge"

// ValidateCapturedDeferredTool applies the request-scoped policy for deferred
// namespace and web-search tool declarations. Both kinds are classified
// "defer" in the field policy: they are metadata-only declarations that the
// adapter never forwards upstream, so any declaration identity is accepted as
// a no-op. An unknown deferred kind is rejected rather than silently dropped.
//
// The policy is kept at the provider adapter boundary and is immutable: it is
// a function over one bridge value, not mutable package-level registration
// state.
func ValidateCapturedDeferredTool(tool bridge.DeferredTool) error {
	switch tool.ToolKind {
	case bridge.ToolNamespace, bridge.ToolWebSearch:
		return nil
	}
	return providerError(ErrorUnsupportedTool, nil)
}
