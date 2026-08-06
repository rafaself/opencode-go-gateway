package bridge

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestJSONSchemaCopiesAndPreservesRawJSON(t *testing.T) {
	raw := []byte(`{"type": "object", "properties": {"text": {"type": "string"}}}`)
	schema, err := NewJSONSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = '['
	if !bytes.Equal(schema.RawJSON(), []byte(`{"type": "object", "properties": {"text": {"type": "string"}}}`)) {
		t.Fatalf("schema changed after input mutation: %s", schema.RawJSON())
	}
	copy := schema.RawJSON()
	copy[0] = '['
	if bytes.Equal(copy, schema.RawJSON()) {
		t.Fatal("RawJSON exposed mutable schema storage")
	}
	serialized, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serialized, []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`)) {
		t.Fatalf("serialized schema = %s", serialized)
	}
}

func TestJSONSchemaRequiresObjectRoot(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`true`), []byte(`[]`), []byte(`"schema"`)} {
		if _, err := NewJSONSchema(raw); err == nil {
			t.Fatalf("NewJSONSchema(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestInputAndToolUnionsRemainExplicit(t *testing.T) {
	var item InputItem = FunctionCall{CallID: "call-1", Name: "exec", Arguments: "{}"}
	if item.Kind() != InputFunctionCall {
		t.Fatalf("input kind = %q", item.Kind())
	}
	call, ok := item.(FunctionCall)
	if !ok || call.ToolKind() != ToolFunction {
		t.Fatalf("function call union = %#v", item)
	}
	var tool Tool = DeferredTool{ToolKind: ToolNamespace, Name: "mcp"}
	if tool.Kind() != ToolNamespace {
		t.Fatalf("tool kind = %q", tool.Kind())
	}
	if _, ok := tool.(DeferredTool); !ok {
		t.Fatalf("tool union = %T", tool)
	}
}
