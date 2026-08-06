package bridge

import (
	"fmt"
	"sort"
)

// ToolRegistration describes one explicit, request-scoped tool translation.
// It is metadata only: execution and provider-specific serialization remain
// in the owning adapter.
type ToolRegistration struct {
	Kind         ToolKind
	InboundName  string
	UpstreamName string
	WrapperField string
}

// ToolRegistry is an immutable lookup table for one request or continuation
// chain. It has no package-level state and returns copies of its registrations.
type ToolRegistry struct {
	byInbound  map[string]ToolRegistration
	byUpstream map[string]ToolRegistration
}

// NewToolRegistry validates and copies registrations into an immutable
// request-scoped registry.
func NewToolRegistry(registrations []ToolRegistration) (*ToolRegistry, error) {
	registry := &ToolRegistry{
		byInbound:  make(map[string]ToolRegistration, len(registrations)),
		byUpstream: make(map[string]ToolRegistration, len(registrations)),
	}
	for _, registration := range registrations {
		if registration.Kind == "" || registration.InboundName == "" || registration.UpstreamName == "" || registration.WrapperField == "" {
			return nil, fmt.Errorf("tool registration is incomplete")
		}
		if _, exists := registry.byInbound[registration.InboundName]; exists {
			return nil, fmt.Errorf("inbound tool registration is duplicated")
		}
		if _, exists := registry.byUpstream[registration.UpstreamName]; exists {
			return nil, fmt.Errorf("upstream tool registration is duplicated")
		}
		registry.byInbound[registration.InboundName] = registration
		registry.byUpstream[registration.UpstreamName] = registration
	}
	return registry, nil
}

// Inbound returns the registration for a Responses/Codex tool name.
func (registry *ToolRegistry) Inbound(name string) (ToolRegistration, bool) {
	if registry == nil {
		return ToolRegistration{}, false
	}
	registration, ok := registry.byInbound[name]
	return registration, ok
}

// Upstream returns the registration for a provider function name.
func (registry *ToolRegistry) Upstream(name string) (ToolRegistration, bool) {
	if registry == nil {
		return ToolRegistration{}, false
	}
	registration, ok := registry.byUpstream[name]
	return registration, ok
}

// Registrations returns deterministic copies for validation and request
// mapping. Callers cannot mutate registry-owned map state through the result.
func (registry *ToolRegistry) Registrations() []ToolRegistration {
	if registry == nil {
		return nil
	}
	result := make([]ToolRegistration, 0, len(registry.byInbound))
	for _, registration := range registry.byInbound {
		result = append(result, registration)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].InboundName < result[right].InboundName
	})
	return result
}
