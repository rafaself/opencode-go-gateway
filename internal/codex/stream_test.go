package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

func TestStreamSessionWritesTextResponsesContractAndFlushesMeaningfulEvents(t *testing.T) {
	recorder := httptest.NewRecorder()
	session := newTestStreamSession(t, recorder)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	for _, event := range []bridge.StreamEvent{
		bridge.TextDelta{ChoiceIndex: 0, Text: "Olá, "},
		bridge.TextDelta{ChoiceIndex: 0, Text: "mundo"},
		bridge.UsageUpdated{Usage: bridge.Usage{
			PromptTokens:         2,
			CompletionTokens:     3,
			TotalTokens:          5,
			PromptCacheHitTokens: 1,
			ReasoningTokens:      1,
		}},
		bridge.Completed{Reason: "stop"},
	} {
		if err := session.Handle(event); err != nil {
			t.Fatal(err)
		}
	}

	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get("Cache-Control") != "no-cache" || recorder.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream headers = %#v", recorder.Header())
	}
	events := decodeResponseSSE(t, recorder.Body.Bytes())
	wantTypes := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if got := responseEventTypes(events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	assertIncreasingSequences(t, events)
	if terminalCount(events) != 1 || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("terminal events = %d, last = %v", terminalCount(events), events[len(events)-1]["type"])
	}
	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatal("downstream Responses stream forwarded upstream [DONE]")
	}
	if got := events[2]["output_index"]; got != float64(0) {
		t.Fatalf("added output index = %v", got)
	}
	if got := events[3]["content_index"]; got != float64(0) {
		t.Fatalf("content index = %v", got)
	}
	if got := events[4]["item_id"]; got != "msg_test_0" || events[5]["item_id"] != got {
		t.Fatalf("text IDs are not stable: %v and %v", got, events[5]["item_id"])
	}
	if got := events[6]["text"]; got != "Olá, mundo" {
		t.Fatalf("text done = %v", got)
	}
	response := events[len(events)-1]["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	wantUsage := map[string]any{
		"input_tokens":  float64(2),
		"output_tokens": float64(3),
		"total_tokens":  float64(5),
		"input_tokens_details": map[string]any{
			"cached_tokens": float64(1),
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": float64(1),
		},
	}
	if !reflect.DeepEqual(usage, wantUsage) {
		t.Fatalf("terminal usage = %#v, want %#v", usage, wantUsage)
	}
	for _, legacyField := range []string{"prompt_tokens", "completion_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "reasoning_tokens"} {
		if _, ok := usage[legacyField]; ok {
			t.Fatalf("usage contains provider field %q: %#v", legacyField, usage)
		}
	}
}

func TestStreamSessionDoesNotExposeProviderResponseIDs(t *testing.T) {
	recorder := httptest.NewRecorder()
	session, err := NewStreamSession(recorder, StreamSessionOptions{
		ResponseID: "resp_public",
		CreatedAt:  time.Unix(0, 0).UTC(),
		Clock:      func() time.Time { return time.Unix(0, 0).UTC() },
		IDGenerator: func(prefix string, index int) string {
			return prefix + "_public_" + strconvItoa(index)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []bridge.StreamEvent{
		bridge.ResponseStarted{ID: "chatcmpl-provider-secret", CreatedAt: time.Unix(42, 0), Model: "provider-model"},
		bridge.TextDelta{ChoiceIndex: 0, Text: "ok"},
		bridge.Completed{Reason: "stop"},
	} {
		if err := session.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(recorder.Body.String(), "chatcmpl-provider-secret") {
		t.Fatalf("provider response ID leaked into Responses stream: %s", recorder.Body.String())
	}
	for _, event := range decodeResponseSSE(t, recorder.Body.Bytes()) {
		if response, ok := event["response"].(map[string]any); ok && response["id"] != "resp_public" {
			t.Fatalf("response ID = %v, want resp_public", response["id"])
		}
	}
}

func TestStreamSessionEnforcesAggregateLimitAcrossManySmallChunks(t *testing.T) {
	recorder := httptest.NewRecorder()
	session, err := NewStreamSession(recorder, StreamSessionOptions{
		ResponseID:        "resp_limit",
		CreatedAt:         time.Unix(0, 0).UTC(),
		Clock:             func() time.Time { return time.Unix(0, 0).UTC() },
		MaxAggregateBytes: 256,
		IDGenerator: func(prefix string, index int) string {
			return prefix + "_limit_" + strconvItoa(index)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 512; index++ {
		err = session.Handle(bridge.TextDelta{ChoiceIndex: 0, Text: "x"})
		if errors.Is(err, ErrStreamLimit) {
			break
		}
		if err != nil {
			t.Fatalf("chunk %d: %v", index, err)
		}
	}
	if !errors.Is(err, ErrStreamLimit) {
		t.Fatal("aggregate limit was not enforced")
	}
	events := decodeResponseSSE(t, recorder.Body.Bytes())
	if events[len(events)-1]["type"] != "response.failed" {
		t.Fatalf("last event = %#v, want response.failed", events[len(events)-1])
	}
	response := events[len(events)-1]["response"].(map[string]any)
	if response["error"].(map[string]any)["code"] != "stream_limit_exceeded" {
		t.Fatalf("limit error = %#v", response["error"])
	}
	before := recorder.Body.Len()
	if err := session.Handle(bridge.TextDelta{ChoiceIndex: 0, Text: "late"}); !errors.Is(err, ErrStreamTerminal) {
		t.Fatalf("post-limit error = %v, want ErrStreamTerminal", err)
	}
	if recorder.Body.Len() != before {
		t.Fatal("writer emitted bytes after aggregate limit terminal")
	}
}

func TestStreamSessionAggregateLimitCoversReasoningAndToolState(t *testing.T) {
	tests := map[string]func(*StreamSession) error{
		"reasoning": func(session *StreamSession) error {
			return session.Handle(bridge.ReasoningDelta{ChoiceIndex: 0, Text: "r"})
		},
		"tool arguments": func(session *StreamSession) error {
			return session.Handle(bridge.ToolCallArgumentsDelta{
				Key:       bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0},
				Arguments: "a",
			})
		},
		"tool metadata": func(session *StreamSession) error {
			return session.Handle(bridge.ToolCallMetadataDelta{
				Key:  bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0},
				Name: "n",
			})
		},
	}
	for name, next := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			session, err := NewStreamSession(recorder, StreamSessionOptions{
				ResponseID:        "resp_state_limit",
				CreatedAt:         time.Unix(0, 0).UTC(),
				Clock:             func() time.Time { return time.Unix(0, 0).UTC() },
				MaxAggregateBytes: 256,
			})
			if err != nil {
				t.Fatal(err)
			}
			if name != "reasoning" {
				if err := session.Handle(bridge.ToolCallStarted{
					Key:    bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0},
					Kind:   bridge.ToolFunction,
					CallID: "call",
					Name:   "tool",
				}); err != nil {
					t.Fatal(err)
				}
			}
			for index := 0; index < 512; index++ {
				if err := next(session); errors.Is(err, ErrStreamLimit) {
					return
				} else if err != nil {
					t.Fatalf("chunk %d: %v", index, err)
				}
			}
			t.Fatal("aggregate limit was not enforced")
		})
	}
}

func TestStreamSessionUsesTerminalClockOnlyForCompletedResponses(t *testing.T) {
	statuses := []struct {
		name             string
		event            bridge.StreamEvent
		type_            string
		wantCompletedAt  any
		wantTimestampKey bool
	}{
		{name: "completed", event: bridge.Completed{Reason: "stop"}, type_: "response.completed", wantCompletedAt: float64(11), wantTimestampKey: true},
		{name: "incomplete", event: bridge.Incomplete{Reason: "max_output_tokens"}, type_: "response.incomplete", wantCompletedAt: nil, wantTimestampKey: true},
		{name: "failed", event: bridge.Failed{Code: "upstream", Message: "The response stream failed."}, type_: "response.failed", wantCompletedAt: nil, wantTimestampKey: true},
	}
	for _, test := range statuses {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			session, err := NewStreamSession(recorder, StreamSessionOptions{
				ResponseID: "resp_clock",
				CreatedAt:  time.Unix(10, 0).UTC(),
				Clock:      func() time.Time { return time.Unix(11, 0).UTC() },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Handle(test.event); err != nil {
				t.Fatal(err)
			}
			events := decodeResponseSSE(t, recorder.Body.Bytes())
			terminal := events[len(events)-1]
			if terminal["type"] != test.type_ {
				t.Fatalf("terminal type = %v", terminal["type"])
			}
			response := terminal["response"].(map[string]any)
			if response["created_at"] != float64(10) || response["completed_at"] != test.wantCompletedAt {
				t.Fatalf("terminal timestamps = created %v completed %v", response["created_at"], response["completed_at"])
			}
			if _, ok := response["completed_at"]; ok != test.wantTimestampKey {
				t.Fatalf("completed_at presence = %v, want %v", ok, test.wantTimestampKey)
			}
		})
	}
}

func TestStreamSessionWritesFragmentedFunctionAndCustomToolItems(t *testing.T) {
	recorder := httptest.NewRecorder()
	session := newTestStreamSession(t, recorder)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	key := bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 2}
	for _, event := range []bridge.StreamEvent{
		bridge.ToolCallStarted{Key: key, Kind: bridge.ToolFunction, CallID: "call-", Name: "exec"},
		bridge.ToolCallMetadataDelta{Key: key, CallID: "1", Name: "_command"},
		bridge.ToolCallArgumentsDelta{Key: key, Arguments: `{"cmd":`},
		bridge.ToolCallArgumentsDelta{Key: key, Arguments: `"true"}`},
		bridge.ToolCallCompleted{Key: key, Kind: bridge.ToolFunction, CallID: "call-1", Name: "exec_command", Arguments: `{"cmd":"true"}`},
		bridge.Completed{Reason: "tool_calls"},
	} {
		if err := session.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	events := decodeResponseSSE(t, recorder.Body.Bytes())
	if got := responseEventTypes(events); !reflect.DeepEqual(got, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}) {
		t.Fatalf("event types = %v", got)
	}
	added := events[2]["item"].(map[string]any)
	done := events[5]
	if added["id"] != "fc_test_0" || done["item_id"] != "fc_test_0" || done["name"] != "exec_command" || done["arguments"] != `{"cmd":"true"}` {
		t.Fatalf("function identity = added %#v done %#v", added, done)
	}
	if events[2]["output_index"] != float64(0) || events[3]["output_index"] != float64(0) || events[5]["output_index"] != float64(0) {
		t.Fatalf("function output index changed")
	}

	customRecorder := httptest.NewRecorder()
	custom := newTestStreamSession(t, customRecorder)
	if err := custom.Start(); err != nil {
		t.Fatal(err)
	}
	customKey := bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}
	for _, event := range []bridge.StreamEvent{
		bridge.ToolCallStarted{Key: customKey, Kind: bridge.ToolCustom, CallID: "call-custom", Name: "apply_patch"},
		bridge.ToolCallArgumentsDelta{Key: customKey, Arguments: "*** Begin Patch\n*** End Patch"},
		bridge.ToolCallCompleted{Key: customKey, Kind: bridge.ToolCustom, CallID: "call-custom", Name: "apply_patch", Arguments: "*** Begin Patch\n*** End Patch"},
		bridge.Completed{Reason: "tool_calls"},
	} {
		if err := custom.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	customEvents := decodeResponseSSE(t, customRecorder.Body.Bytes())
	if !containsType(customEvents, "response.custom_tool_call_input.delta") || !containsType(customEvents, "response.custom_tool_call_input.done") {
		t.Fatalf("custom event types = %v", responseEventTypes(customEvents))
	}
}

func TestStreamSessionKeepsParallelToolOutputIndexesStable(t *testing.T) {
	recorder := httptest.NewRecorder()
	session := newTestStreamSession(t, recorder)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	first := bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}
	second := bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 1}
	for _, event := range []bridge.StreamEvent{
		bridge.ToolCallStarted{Key: first, Kind: bridge.ToolFunction, CallID: "call-0", Name: "first"},
		bridge.ToolCallStarted{Key: second, Kind: bridge.ToolFunction, CallID: "call-1", Name: "second"},
		bridge.ToolCallArgumentsDelta{Key: second, Arguments: "2"},
		bridge.ToolCallArgumentsDelta{Key: first, Arguments: "1"},
		bridge.ToolCallCompleted{Key: second, Kind: bridge.ToolFunction, CallID: "call-1", Name: "second", Arguments: "2"},
		bridge.ToolCallCompleted{Key: first, Kind: bridge.ToolFunction, CallID: "call-0", Name: "first", Arguments: "1"},
		bridge.Completed{Reason: "tool_calls"},
	} {
		if err := session.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	events := decodeResponseSSE(t, recorder.Body.Bytes())
	if events[2]["output_index"] != float64(0) || events[3]["output_index"] != float64(1) {
		t.Fatalf("added indexes = %v, %v", events[2]["output_index"], events[3]["output_index"])
	}
	for _, event := range events {
		if event["type"] == "response.function_call_arguments.delta" || event["type"] == "response.function_call_arguments.done" || event["type"] == "response.output_item.done" {
			if event["output_index"] != float64(0) && event["output_index"] != float64(1) {
				t.Fatalf("invalid parallel index in %#v", event)
			}
		}
	}
}

func TestStreamSessionUsesCustomToolHooksWithoutProviderBranches(t *testing.T) {
	writer := httptest.NewRecorder()
	kind := bridge.ToolKind("sandbox_tool")
	session, err := NewStreamSession(writer, StreamSessionOptions{
		ResponseID:  "resp_hook",
		IDGenerator: func(prefix string, index int) string { return prefix + "_hook_" + strconvItoa(index) },
		CustomTools: map[bridge.ToolKind]CustomToolHook{
			kind: {
				Kind:      kind,
				ItemType:  "sandbox_call",
				DeltaType: "response.sandbox_input.delta",
				DoneType:  "response.sandbox_input.done",
				InputName: "payload",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}
	for _, event := range []bridge.StreamEvent{
		bridge.ToolCallStarted{Key: key, Kind: kind, CallID: "call-hook", Name: "sandbox"},
		bridge.ToolCallArgumentsDelta{Key: key, Arguments: "payload"},
		bridge.ToolCallCompleted{Key: key, Kind: kind, CallID: "call-hook", Name: "sandbox", Arguments: "payload"},
		bridge.Completed{Reason: "tool_calls"},
	} {
		if err := session.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	events := decodeResponseSSE(t, writer.Body.Bytes())
	if !containsType(events, "response.sandbox_input.delta") || !containsType(events, "response.sandbox_input.done") {
		t.Fatalf("custom hook events = %v", responseEventTypes(events))
	}
	added := events[2]["item"].(map[string]any)
	if added["type"] != "sandbox_call" || added["payload"] != "" {
		t.Fatalf("custom hook item = %#v", added)
	}
}

func TestStreamSessionInvalidTransitionFailsExactlyOnceAndDoesNotWriteAfterTerminal(t *testing.T) {
	recorder := httptest.NewRecorder()
	session := newTestStreamSession(t, recorder)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	err := session.Handle(bridge.ToolCallArgumentsDelta{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Arguments: "{}"})
	if err == nil {
		t.Fatal("invalid transition returned nil")
	}
	events := decodeResponseSSE(t, recorder.Body.Bytes())
	if terminalCount(events) != 1 || events[len(events)-1]["type"] != "response.failed" {
		t.Fatalf("events = %v", responseEventTypes(events))
	}
	before := recorder.Body.Len()
	if err := session.Handle(bridge.Completed{Reason: "stop"}); err == nil {
		t.Fatal("post-terminal event returned nil")
	}
	if recorder.Body.Len() != before {
		t.Fatal("writer emitted bytes after terminal failure")
	}
}

func TestStreamSessionMapsIncompleteAndDownstreamWriteFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	session := newTestStreamSession(t, recorder)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	if err := session.Handle(bridge.Incomplete{Reason: "length"}); err != nil {
		t.Fatal(err)
	}
	events := decodeResponseSSE(t, recorder.Body.Bytes())
	if events[len(events)-1]["type"] != "response.incomplete" {
		t.Fatalf("terminal = %v", events[len(events)-1]["type"])
	}
	if events[len(events)-1]["response"].(map[string]any)["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete details = %#v", events[len(events)-1]["response"])
	}

	failing := &flushErrorWriter{}
	failingSession := newTestStreamSession(t, failing)
	err := failingSession.Start()
	if err == nil || !errors.Is(err, ErrStreamWrite) {
		t.Fatalf("start error = %v, want ErrStreamWrite", err)
	}
	if failing.Contains("response.failed") {
		t.Fatal("attempted a second response after flush failure")
	}
	before := failing.body.Len()
	if err := failingSession.Handle(bridge.TextDelta{ChoiceIndex: 0, Text: "late"}); !errors.Is(err, ErrStreamWrite) {
		t.Fatalf("post-write-failure error = %v, want ErrStreamWrite", err)
	}
	if failing.body.Len() != before {
		t.Fatal("writer emitted bytes after downstream failure")
	}
	select {
	case <-failingSession.Done():
	default:
		t.Fatal("downstream failure did not signal session cancellation")
	}
	if !errors.Is(failingSession.WriteFailure(), ErrStreamWrite) {
		t.Fatalf("session write failure = %v", failingSession.WriteFailure())
	}

	writeFailing := &writeErrorWriter{}
	writeFailingSession := newTestStreamSession(t, writeFailing)
	if err := writeFailingSession.Start(); !errors.Is(err, ErrStreamWrite) {
		t.Fatalf("write error = %v, want ErrStreamWrite", err)
	}
	select {
	case <-writeFailingSession.Done():
	default:
		t.Fatal("write failure did not signal session cancellation")
	}
}

func TestStreamSessionFlushesAfterEveryMeaningfulWireEvent(t *testing.T) {
	writer := &flushCountingWriter{}
	session := newTestStreamSession(t, writer)
	for _, event := range []bridge.StreamEvent{
		bridge.ResponseStarted{ID: "resp_test", CreatedAt: time.Unix(0, 0), Model: "gpt-5.3-codex"},
		bridge.TextDelta{ChoiceIndex: 0, Text: "x"},
		bridge.Completed{Reason: "stop"},
	} {
		if err := session.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	events := decodeResponseSSE(t, writer.body.Bytes())
	if writer.flushes != len(events) {
		t.Fatalf("flushes = %d, events = %d", writer.flushes, len(events))
	}
}

func TestStreamSessionToleratesSlowDownstreamWriter(t *testing.T) {
	writer := &slowFlushWriter{}
	session := newTestStreamSession(t, writer)
	if err := session.Handle(bridge.TextDelta{ChoiceIndex: 0, Text: "slow"}); err != nil {
		t.Fatal(err)
	}
	if err := session.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(decodeResponseSSE(t, writer.body.Bytes())) == 0 || writer.flushes == 0 {
		t.Fatal("slow writer received no flushed events")
	}
}

func TestStreamSessionInstancesAreIndependentUnderConcurrentUse(t *testing.T) {
	const workers = 12
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			recorder := httptest.NewRecorder()
			session := newTestStreamSession(t, recorder)
			if err := session.Start(); err != nil {
				errs <- err
				return
			}
			if err := session.Handle(bridge.TextDelta{ChoiceIndex: index, Text: "ok"}); err != nil {
				errs <- err
				return
			}
			errs <- session.Handle(bridge.Completed{Reason: "stop"})
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestResponseFixturesDecodeUnderAllSmallChunkSplits(t *testing.T) {
	responseDir := filepath.Join("..", "..", "testdata", "codex", "responses")
	entries, err := os.ReadDir(responseDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sse" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(responseDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for split := 1; split < len(data); split++ {
			decoder := opencodego.NewSSEDecoder(&fixtureChunkReader{data: data, chunk: split}, opencodego.SSEDecoderOptions{})
			count := 0
			for {
				event, err := decoder.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("%s split %d: %v", entry.Name(), split, err)
				}
				if event.Data != "" {
					var payload map[string]any
					if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
						t.Fatalf("%s split %d: %v", entry.Name(), split, err)
					}
					count++
				}
			}
			if count == 0 {
				t.Fatalf("%s split %d: no events", entry.Name(), split)
			}
		}
	}
}

func TestStreamSessionPayloadsMatchCheckedResponseFixtures(t *testing.T) {
	fixtures := map[string][]bridge.StreamEvent{
		"completed.sse": {
			bridge.Completed{Reason: "stop"},
		},
		"incomplete.sse": {
			bridge.Incomplete{Reason: "max_output_tokens"},
		},
		"failed.sse": {
			bridge.Failed{Code: "server_error", Message: "The model failed to generate a response."},
		},
		"text.sse": {
			bridge.TextDelta{ChoiceIndex: 0, Text: "capture acknowledged"},
			bridge.Completed{Reason: "stop"},
		},
		"function-tool-call.sse": {
			bridge.ToolCallStarted{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Kind: bridge.ToolFunction, CallID: "call_0", Name: "exec_command"},
			bridge.ToolCallArgumentsDelta{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Arguments: `{"cmd":"true"}`},
			bridge.ToolCallCompleted{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Kind: bridge.ToolFunction, CallID: "call_0", Name: "exec_command", Arguments: `{"cmd":"true"}`},
			bridge.Completed{Reason: "tool_calls"},
		},
		"custom-tool-call.sse": {
			bridge.ToolCallStarted{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Kind: bridge.ToolCustom, CallID: "call_0", Name: "apply_patch"},
			bridge.ToolCallArgumentsDelta{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Arguments: "*** Begin Patch\n*** End Patch"},
			bridge.ToolCallCompleted{Key: bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}, Kind: bridge.ToolCustom, CallID: "call_0", Name: "apply_patch", Arguments: "*** Begin Patch\n*** End Patch"},
			bridge.Completed{Reason: "tool_calls"},
		},
	}
	responseDir := filepath.Join("..", "..", "testdata", "codex", "responses")
	for fixtureName, semanticEvents := range fixtures {
		t.Run(fixtureName, func(t *testing.T) {
			writer := httptest.NewRecorder()
			session := newFixtureStreamSession(t, writer)
			if err := session.Start(); err != nil {
				t.Fatal(err)
			}
			for _, event := range semanticEvents {
				if err := session.Handle(event); err != nil {
					t.Fatal(err)
				}
			}
			fixtureBytes, err := os.ReadFile(filepath.Join(responseDir, fixtureName))
			if err != nil {
				t.Fatal(err)
			}
			fixtureEvents := decodeResponseSSE(t, fixtureBytes)
			generatedEvents := decodeResponseSSE(t, writer.Body.Bytes())
			canonicalFixture := canonicalizeResponseEvents(fixtureEvents)
			canonicalGenerated := canonicalizeResponseEvents(generatedEvents)
			if !reflect.DeepEqual(canonicalGenerated, canonicalFixture) {
				t.Fatalf("generated payloads = %#v, fixture payloads = %#v", canonicalGenerated, canonicalFixture)
			}
		})
	}
}

func newFixtureStreamSession(t *testing.T, writer http.ResponseWriter) *StreamSession {
	t.Helper()
	session, err := NewStreamSession(writer, StreamSessionOptions{
		ResponseID: "resp_fixture",
		CreatedAt:  time.Unix(0, 0).UTC(),
		Clock:      func() time.Time { return time.Unix(0, 0).UTC() },
		IDGenerator: func(prefix string, index int) string {
			return prefix + "_fixture_" + strconvItoa(index)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func canonicalizeResponseEvents(events []map[string]any) []map[string]any {
	result := make([]map[string]any, len(events))
	for index, event := range events {
		result[index] = canonicalizeResponseValue(event, "").(map[string]any)
	}
	return result
}

func canonicalizeResponseValue(value any, key string) any {
	if key == "id" || key == "item_id" {
		return "<dynamic-id>"
	}
	if key == "created_at" || key == "completed_at" {
		return float64(0)
	}
	switch typed := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for field, nested := range typed {
			copy[field] = canonicalizeResponseValue(nested, field)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for index, nested := range typed {
			copy[index] = canonicalizeResponseValue(nested, "")
		}
		return copy
	default:
		return value
	}
}

func newTestStreamSession(t *testing.T, writer http.ResponseWriter) *StreamSession {
	t.Helper()
	session, err := NewStreamSession(writer, StreamSessionOptions{
		ResponseID: "resp_test",
		CreatedAt:  time.Unix(0, 0).UTC(),
		Model:      "gpt-5.3-codex",
		IDGenerator: func(prefix string, index int) string {
			return prefix + "_test_" + strconvItoa(index)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func decodeResponseSSE(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := bufio.NewScanner(bytes.NewReader(data))
	decoder.Buffer(make([]byte, 128), 1<<20)
	var events []map[string]any
	for decoder.Scan() {
		line := decoder.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := decoder.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func responseEventTypes(events []map[string]any) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event["type"].(string))
	}
	return result
}

func assertIncreasingSequences(t *testing.T, events []map[string]any) {
	t.Helper()
	previous := -1.0
	for _, event := range events {
		sequence, ok := event["sequence_number"].(float64)
		if !ok || sequence <= previous {
			t.Fatalf("non-increasing sequence in %#v after %v", event, previous)
		}
		previous = sequence
	}
}

func terminalCount(events []map[string]any) int {
	count := 0
	for _, event := range events {
		switch event["type"] {
		case "response.completed", "response.incomplete", "response.failed":
			count++
		}
	}
	return count
}

func containsType(events []map[string]any, wanted string) bool {
	for _, event := range events {
		if event["type"] == wanted {
			return true
		}
	}
	return false
}

type flushErrorWriter struct {
	body bytes.Buffer
}

func (writer *flushErrorWriter) Header() http.Header { return make(http.Header) }

func (writer *flushErrorWriter) Write(data []byte) (int, error) { return writer.body.Write(data) }

func (writer *flushErrorWriter) WriteHeader(int) {}

func (writer *flushErrorWriter) FlushError() error { return errors.New("client went away") }

func (writer *flushErrorWriter) Contains(text string) bool {
	return strings.Contains(writer.body.String(), text)
}

type writeErrorWriter struct{}

func (writer *writeErrorWriter) Header() http.Header { return make(http.Header) }

func (writer *writeErrorWriter) Write([]byte) (int, error) {
	return 0, errors.New("client write failed")
}

func (writer *writeErrorWriter) WriteHeader(int) {}

type flushCountingWriter struct {
	body    bytes.Buffer
	header  http.Header
	flushes int
}

func (writer *flushCountingWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *flushCountingWriter) Write(data []byte) (int, error) { return writer.body.Write(data) }

func (writer *flushCountingWriter) WriteHeader(int) {}

func (writer *flushCountingWriter) Flush() { writer.flushes++ }

type slowFlushWriter struct {
	body    bytes.Buffer
	header  http.Header
	flushes int
}

func (writer *slowFlushWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *slowFlushWriter) Write(data []byte) (int, error) {
	time.Sleep(time.Microsecond)
	return writer.body.Write(data)
}

func (writer *slowFlushWriter) WriteHeader(int) {}

func (writer *slowFlushWriter) Flush() {
	time.Sleep(time.Microsecond)
	writer.flushes++
}

type fixtureChunkReader struct {
	data  []byte
	index int
	chunk int
}

func (reader *fixtureChunkReader) Read(target []byte) (int, error) {
	if reader.index == len(reader.data) {
		return 0, io.EOF
	}
	end := reader.index + reader.chunk
	if end > len(reader.data) {
		end = len(reader.data)
	}
	if end-reader.index > len(target) {
		end = reader.index + len(target)
	}
	n := copy(target, reader.data[reader.index:end])
	reader.index += n
	return n, nil
}

func strconvItoa(value int) string {
	return string([]byte{'0' + byte(value)})
}
