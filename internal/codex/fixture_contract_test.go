package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

// TestCheckedCodexRequestFixturesAreExecutableContracts keeps the checked-in
// capture metadata honest. The decoder tests assert bridge semantics, while
// this layer independently derives the field/type indexes from request.body
// and proves that every request fixture still loads through the public
// decoder boundary.
func TestCheckedCodexRequestFixturesAreExecutableContracts(t *testing.T) {
	requestDir := filepath.Join("..", "..", "testdata", "codex", "requests")
	policy := readFixtureFieldPolicy(t)
	entries, err := os.ReadDir(requestDir)
	if err != nil {
		t.Fatal(err)
	}
	decoder := mustDecoder(t, 1<<20)
	seen := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			fixtureBytes, err := os.ReadFile(filepath.Join(requestDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Request struct {
					Body           json.RawMessage `json:"body"`
					TopLevelFields []string        `json:"top_level_fields"`
					InputItemTypes []string        `json:"input_item_types"`
					ToolTypes      []string        `json:"tool_types"`
				} `json:"request"`
			}
			if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(fixture.Request.Body) {
				t.Fatal("request.body is not valid JSON")
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(fixture.Request.Body, &body); err != nil {
				t.Fatalf("request.body is not an object: %v", err)
			}

			fields := make([]string, 0, len(body))
			for field := range body {
				fields = append(fields, field)
				if _, ok := policy.TopLevel[field]; !ok {
					t.Fatalf("fixture field %q is not classified by field-policy.json", field)
				}
			}
			sort.Strings(fields)
			if !reflect.DeepEqual(fields, fixture.Request.TopLevelFields) {
				t.Fatalf("top-level field index = %v, derived = %v", fixture.Request.TopLevelFields, fields)
			}

			var input []struct {
				Type string `json:"type"`
			}
			if raw := body["input"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &input); err != nil {
					t.Fatalf("input is invalid: %v", err)
				}
			}
			inputTypes := make([]string, 0, len(input))
			for index, item := range input {
				if item.Type == "" {
					t.Fatalf("input[%d] has no type", index)
				}
				inputTypes = append(inputTypes, item.Type)
				if _, ok := policy.InputTypes[item.Type]; !ok {
					t.Fatalf("input type %q is not classified by field-policy.json", item.Type)
				}
			}
			inputTypes = uniqueSortedStrings(inputTypes)
			if !reflect.DeepEqual(inputTypes, fixture.Request.InputItemTypes) {
				t.Fatalf("input type index = %v, derived = %v", fixture.Request.InputItemTypes, inputTypes)
			}

			var tools []struct {
				Type string `json:"type"`
			}
			if raw := body["tools"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &tools); err != nil {
					t.Fatalf("tools is invalid: %v", err)
				}
			}
			toolTypes := make([]string, 0, len(tools))
			for index, tool := range tools {
				if tool.Type == "" {
					t.Fatalf("tools[%d] has no type", index)
				}
				toolTypes = append(toolTypes, tool.Type)
				if _, ok := policy.ToolTypes[tool.Type]; !ok {
					t.Fatalf("tool type %q is not classified by field-policy.json", tool.Type)
				}
			}
			toolTypes = uniqueSortedStrings(toolTypes)
			if !reflect.DeepEqual(toolTypes, fixture.Request.ToolTypes) {
				t.Fatalf("tool type index = %v, derived = %v", fixture.Request.ToolTypes, toolTypes)
			}

			if _, err := decoder.Decode(bytes.NewReader(fixture.Request.Body), "application/json"); err != nil {
				t.Fatalf("fixture does not decode through the request boundary: %v", err)
			}
		})
	}
	if seen == 0 {
		t.Fatal("no Codex request fixtures found")
	}
}

// TestCheckedCodexResponseFixturesAreExecutableContracts independently parses
// every checked-in Responses stream. It intentionally does not reuse the
// capture package's validator, so a regression in one test layer cannot make
// the entire contract suite tautological.
func TestCheckedCodexResponseFixturesAreExecutableContracts(t *testing.T) {
	responseDir := filepath.Join("..", "..", "testdata", "codex", "responses")
	entries, err := os.ReadDir(responseDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sse" {
			continue
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(responseDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			decoder := opencodego.NewSSEDecoder(bytes.NewReader(data), opencodego.SSEDecoderOptions{})
			previousSequence := -1
			terminalCount := 0
			lastType := ""
			for {
				event, err := decoder.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("invalid SSE: %v", err)
				}
				if event.Data == "[DONE]" {
					t.Fatal("Codex Responses fixture must not use [DONE]")
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
					t.Fatalf("invalid event JSON: %v", err)
				}
				eventType, ok := payload["type"].(string)
				if !ok || eventType == "" {
					t.Fatalf("event has no type: %#v", payload)
				}
				sequence, ok := payload["sequence_number"].(float64)
				if !ok || sequence != float64(int(sequence)) || int(sequence) <= previousSequence {
					t.Fatalf("sequence_number = %#v after %d", payload["sequence_number"], previousSequence)
				}
				previousSequence = int(sequence)
				lastType = eventType
				switch eventType {
				case "response.completed", "response.incomplete", "response.failed":
					terminalCount++
				}
			}
			if previousSequence < 0 {
				t.Fatal("fixture has no events")
			}
			if terminalCount != 1 {
				t.Fatalf("terminal event count = %d, want 1", terminalCount)
			}
			if lastType != "response.completed" && lastType != "response.incomplete" && lastType != "response.failed" {
				t.Fatalf("last event type = %q, not a Responses terminal", lastType)
			}
		})
	}
	if seen == 0 {
		t.Fatal("no Codex response fixtures found")
	}
}

type fixtureFieldPolicy struct {
	TopLevel   map[string]json.RawMessage `json:"top_level"`
	InputTypes map[string]string          `json:"item_types"`
	ToolTypes  map[string]string          `json:"tool_types"`
}

func readFixtureFieldPolicy(t *testing.T) fixtureFieldPolicy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", "field-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy fixtureFieldPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
