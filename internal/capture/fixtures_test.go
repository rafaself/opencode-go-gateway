package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type fieldPolicyFile struct {
	TopLevel map[string]struct {
		Policy   string `json:"policy"`
		Observed bool   `json:"observed"`
	} `json:"top_level"`
	ItemTypes map[string]string `json:"item_types"`
	ToolTypes map[string]string `json:"tool_types"`
}

func TestCodexRequestFixturesAreRedactedAndClassified(t *testing.T) {
	requestDir := filepath.Join("..", "..", "testdata", "codex", "requests")
	policyBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", "field-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy fieldPolicyFile
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(requestDir)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{
		"apply-patch-request.json":            true,
		"cancellation-request.json":           true,
		"continuation-request.json":           true,
		"custom-tool-result-request.json":     true,
		"developer-instructions-request.json": true,
		"empty-tool-result-request.json":      true,
		"function-tools-request.json":         true,
		"parallel-tools-request.json":         true,
		"shell-command-request.json":          true,
		"simple-request.json":                 true,
		"tool-error-request.json":             true,
		"tool-results-request.json":           true,
		"workspace-file-read-request.json":    true,
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		seen[entry.Name()] = true
		fixtureBytes, err := os.ReadFile(filepath.Join(requestDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var fixture Fixture
		if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
			t.Fatalf("%s: invalid JSON: %v", entry.Name(), err)
		}
		if fixture.SchemaVersion != fixtureSchemaVersion {
			t.Fatalf("%s: schema version = %d", entry.Name(), fixture.SchemaVersion)
		}
		if fixture.CodexVersion == "" || fixture.CodexVersion == "unknown" {
			t.Fatalf("%s: missing Codex version", entry.Name())
		}
		if fixture.Request.Method != "POST" || fixture.Request.Path != "/v1/responses" {
			t.Fatalf("%s: request endpoint = %s %s", entry.Name(), fixture.Request.Method, fixture.Request.Path)
		}
		body, ok := fixture.Request.Body.(map[string]any)
		if !ok {
			t.Fatalf("%s: request body is not an object", entry.Name())
		}
		fields := make([]string, 0, len(body))
		for field := range body {
			fields = append(fields, field)
			if _, ok := policy.TopLevel[field]; !ok {
				t.Fatalf("%s: unclassified top-level request field %q", entry.Name(), field)
			}
		}
		for _, itemType := range fixture.Request.InputItemTypes {
			if _, ok := policy.ItemTypes[itemType]; !ok {
				t.Fatalf("%s: unclassified input item type %q", entry.Name(), itemType)
			}
		}
		for _, toolType := range fixture.Request.ToolTypes {
			if _, ok := policy.ToolTypes[toolType]; !ok {
				t.Fatalf("%s: unclassified tool type %q", entry.Name(), toolType)
			}
		}
		sort.Strings(fields)
		if !reflect.DeepEqual(fields, fixture.Request.TopLevelFields) {
			t.Fatalf("%s: top-level field index does not match body: %v vs %v", entry.Name(), fixture.Request.TopLevelFields, fields)
		}
		if !sort.StringsAreSorted(fixture.Request.InputItemTypes) || !sort.StringsAreSorted(fixture.Request.ToolTypes) {
			t.Fatalf("%s: item/type indexes are not sorted", entry.Name())
		}
		for _, forbidden := range []string{"sk-", "Bearer ", "/home/", "/Users/", "private prompt", "private instructions", "source.go"} {
			if strings.Contains(string(fixtureBytes), forbidden) {
				t.Fatalf("%s: redaction marker leaked %q", entry.Name(), forbidden)
			}
		}
	}
	for name := range expected {
		if !seen[name] {
			t.Errorf("missing required request fixture %s", name)
		}
	}
}

func TestCodexResponseFixturesAreValidSSE(t *testing.T) {
	responseDir := filepath.Join("..", "..", "testdata", "codex", "responses")
	entries, err := os.ReadDir(responseDir)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{
		"completed.sse":          true,
		"custom-tool-call.sse":   true,
		"failed.sse":             true,
		"function-tool-call.sse": true,
		"incomplete.sse":         true,
		"text.sse":               true,
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sse" {
			continue
		}
		seen[entry.Name()] = true
		file, err := os.Open(filepath.Join(responseDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		events, doneMarker, err := readSSE(file)
		closeErr := file.Close()
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if doneMarker {
			t.Fatalf("%s: [DONE] marker is not part of the Codex contract", entry.Name())
		}
		if len(events) == 0 {
			t.Fatalf("%s: no SSE events", entry.Name())
		}
		previous := -1
		for _, event := range events {
			sequence, ok := sequenceNumber(event["sequence_number"])
			if !ok || sequence <= previous {
				t.Fatalf("%s: non-increasing sequence number %v after %d", entry.Name(), event["sequence_number"], previous)
			}
			previous = sequence
			if _, ok := event["type"].(string); !ok {
				t.Fatalf("%s: event has no type", entry.Name())
			}
			if err := validateResponseEvent(event); err != nil {
				t.Fatalf("%s: %v", entry.Name(), err)
			}
		}
		terminal := events[len(events)-1]["type"]
		if terminal != "response.completed" && terminal != "response.incomplete" && terminal != "response.failed" {
			t.Fatalf("%s: terminal event = %v", entry.Name(), terminal)
		}
		if entry.Name() == "custom-tool-call.sse" && !containsEvent(events, "response.custom_tool_call_input.delta") {
			t.Fatalf("%s: missing custom tool input delta", entry.Name())
		}
		if entry.Name() == "function-tool-call.sse" && !containsEvent(events, "response.function_call_arguments.delta") {
			t.Fatalf("%s: missing function argument delta", entry.Name())
		}
	}
	for name := range expected {
		if !seen[name] {
			t.Errorf("missing required response fixture %s", name)
		}
	}
}

func readSSE(file *os.File) ([]map[string]any, bool, error) {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var events []map[string]any
	doneMarker := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			doneMarker = true
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, doneMarker, fmt.Errorf("invalid data payload: %w", err)
		}
		events = append(events, event)
	}
	return events, doneMarker, scanner.Err()
}

func containsEvent(events []map[string]any, wanted string) bool {
	for _, event := range events {
		if event["type"] == wanted {
			return true
		}
	}
	return false
}

func validateResponseEvent(event map[string]any) error {
	eventType, _ := event["type"].(string)
	switch eventType {
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		response, ok := event["response"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s has no response object", eventType)
		}
		if id, ok := response["id"].(string); !ok || id == "" {
			return fmt.Errorf("%s has no response ID", eventType)
		}
	case "response.output_item.added", "response.output_item.done":
		if !hasNumber(event["output_index"]) {
			return fmt.Errorf("%s has no output_index", eventType)
		}
		item, ok := event["item"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s has no item", eventType)
		}
		if id, ok := item["id"].(string); !ok || id == "" {
			return fmt.Errorf("%s has no item ID", eventType)
		}
		if itemType, ok := item["type"].(string); !ok || itemType == "" {
			return fmt.Errorf("%s has no item type", eventType)
		}
	case "response.content_part.added", "response.content_part.done":
		if !hasString(event["item_id"]) || !hasNumber(event["output_index"]) || !hasNumber(event["content_index"]) {
			return fmt.Errorf("%s has incomplete content indexes", eventType)
		}
	case "response.output_text.delta", "response.output_text.done":
		if !hasString(event["item_id"]) || !hasNumber(event["output_index"]) || !hasNumber(event["content_index"]) || !hasString(event["delta"]) && !hasString(event["text"]) {
			return fmt.Errorf("%s has incomplete text fields", eventType)
		}
	case "response.function_call_arguments.delta":
		if !hasString(event["item_id"]) || !hasNumber(event["output_index"]) || !hasString(event["delta"]) {
			return fmt.Errorf("%s has incomplete function delta fields", eventType)
		}
	case "response.function_call_arguments.done":
		if !hasString(event["item_id"]) || !hasNumber(event["output_index"]) || !hasString(event["name"]) || !hasString(event["arguments"]) {
			return fmt.Errorf("%s has incomplete function completion fields", eventType)
		}
	case "response.custom_tool_call_input.delta":
		if !hasString(event["item_id"]) || !hasNumber(event["output_index"]) || !hasString(event["delta"]) {
			return fmt.Errorf("%s has incomplete custom-tool delta fields", eventType)
		}
	case "response.custom_tool_call_input.done":
		if !hasString(event["item_id"]) || !hasNumber(event["output_index"]) || !hasString(event["input"]) {
			return fmt.Errorf("%s has incomplete custom-tool completion fields", eventType)
		}
	}
	return nil
}

func hasString(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

func hasNumber(value any) bool {
	_, ok := sequenceNumber(value)
	return ok
}
