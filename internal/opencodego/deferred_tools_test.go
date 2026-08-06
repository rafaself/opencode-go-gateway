package opencodego

import (
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestMapRequestAcceptsOnlyCapturedDeferredToolIdentities(t *testing.T) {
	for _, tool := range []bridge.DeferredTool{
		{ToolKind: bridge.ToolNamespace, Name: "mcp"},
		{ToolKind: bridge.ToolWebSearch},
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
				t.Fatalf("captured deferred tool was rejected: %v", err)
			}
		})
	}
}

func TestMapRequestRejectsArbitraryDeferredToolIdentities(t *testing.T) {
	for _, tool := range []bridge.DeferredTool{
		{ToolKind: bridge.ToolNamespace, Name: "secret_namespace"},
		{ToolKind: bridge.ToolWebSearch, Name: "secret_search"},
		{ToolKind: bridge.ToolCustom, Name: "mcp"},
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
