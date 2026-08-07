package opencodego

import (
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestMapRequestAcceptsDeferredNamespaceAndWebSearchDeclarations(t *testing.T) {
	for _, tool := range []bridge.DeferredTool{
		{ToolKind: bridge.ToolNamespace, Name: "mcp"},
		{ToolKind: bridge.ToolNamespace, Name: "collaboration"},
		{ToolKind: bridge.ToolNamespace, Name: "followup_task"},
		{ToolKind: bridge.ToolNamespace, Name: "mcp__codex_apps__github"},
		{ToolKind: bridge.ToolNamespace, Name: "secret_namespace"},
		{ToolKind: bridge.ToolWebSearch},
		{ToolKind: bridge.ToolWebSearch, Name: "secret_search"},
	} {
		t.Run(string(tool.ToolKind)+"/"+tool.Name, func(t *testing.T) {
			request := minimalRequest()
			request.Tools = []bridge.Tool{tool}
			registry, err := NewToolRegistry(request)
			if err != nil {
				t.Fatal(err)
			}
			request.ToolRegistry = registry

			if _, err := MapRequest(request, DefaultModel); err != nil {
				t.Fatalf("deferred namespace or web-search declaration was rejected: %v", err)
			}
		})
	}
}

func TestMapRequestRejectsUnknownDeferredToolKinds(t *testing.T) {
	for _, tool := range []bridge.DeferredTool{
		{ToolKind: bridge.ToolCustom, Name: "mcp"},
		{ToolKind: bridge.ToolFunction, Name: "apply_patch"},
	} {
		t.Run(string(tool.ToolKind)+"/"+tool.Name, func(t *testing.T) {
			request := minimalRequest()
			request.Tools = []bridge.Tool{tool}
			registry, err := NewToolRegistry(request)
			if err != nil {
				t.Fatal(err)
			}
			request.ToolRegistry = registry

			_, err = MapRequest(request, DefaultModel)
			assertProviderCode(t, err, ErrorUnsupportedTool)
		})
	}
}
