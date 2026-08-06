package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

type streamTestNetError struct{}

func (streamTestNetError) Error() string   { return "network failure marker" }
func (streamTestNetError) Timeout() bool   { return true }
func (streamTestNetError) Temporary() bool { return true }

func (streamTestNetError) Unwrap() error { return net.ErrClosed }

type streamErrorReader struct {
	err error
}

func (reader streamErrorReader) Read([]byte) (int, error) { return 0, reader.err }

const providerStreamFixture = `data: {"id":"chat-1","object":"chat.completion.chunk","created":42,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"Olá, "},"finish_reason":null}]}

data: {"id":"chat-1","created":42,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"世界","reasoning_content":"private","tool_calls":[{"index":2,"id":"call-","type":"function","function":{"name":"exec","arguments":"{\"cmd\":\"true\""}}]},"finish_reason":null}]}

data: {"id":"chat-1","created":42,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"1","function":{"name":"_command","arguments":"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"completion_tokens_details":{"reasoning_tokens":1}}}

data: [DONE]

`

func TestChatCompletionStreamDecoderPreservesChunkFieldsAndConsumesDone(t *testing.T) {
	decoder := NewChatCompletionStreamDecoder(strings.NewReader(providerStreamFixture), SSEDecoderOptions{})

	first, err := decoder.Next()
	if err != nil || first.Chunk == nil {
		t.Fatalf("first = %#v, error = %v", first, err)
	}
	if first.Chunk.ID != "chat-1" || first.Chunk.Created != 42 || first.Chunk.Model != "deepseek-v4-flash" {
		t.Fatalf("chunk metadata = %#v", first.Chunk)
	}
	if first.Chunk.Choices[0].Delta.Content == nil || *first.Chunk.Choices[0].Delta.Content != "Olá, " {
		t.Fatalf("first content = %#v", first.Chunk.Choices[0].Delta.Content)
	}

	if _, err := decoder.Next(); err != nil {
		t.Fatal(err)
	}
	third, err := decoder.Next()
	if err != nil || third.Chunk == nil {
		t.Fatalf("third = %#v, error = %v", third, err)
	}
	choice := third.Chunk.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %#v", choice.FinishReason)
	}
	if third.Chunk.Usage == nil || third.Chunk.Usage.TotalTokens != 7 || third.Chunk.Usage.CompletionTokensDetails.ReasoningTokens != 1 {
		t.Fatalf("usage = %#v", third.Chunk.Usage)
	}
	tool := choice.Delta.ToolCalls[0]
	if tool.Index == nil || *tool.Index != 2 || tool.ID != "1" || tool.Function.Name != "_command" || tool.Function.Arguments != "}" {
		t.Fatalf("tool = %#v", tool)
	}

	done, err := decoder.Next()
	if err != nil || !done.Done {
		t.Fatalf("done = %#v, error = %v", done, err)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after done error = %v, want EOF", err)
	}
}

func TestChatCompletionStreamDecoderRejectsDuplicateDoneMarker(t *testing.T) {
	decoder := NewChatCompletionStreamDecoder(strings.NewReader("data: [DONE]\n\ndata: [DONE]\n\n"), SSEDecoderOptions{})
	if _, err := decoder.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Next(); !errors.Is(err, ErrDuplicateStreamTerminal) {
		t.Fatalf("error = %v, want duplicate terminal", err)
	}
}

func TestBridgeStreamDecoderUsesNoReadAheadAfterFirstTerminal(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chat-terminal","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(stream), BridgeStreamDecoderOptions{})
	var terminal bridge.StreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.StreamEventKind() == bridge.StreamCompleted || event.StreamEventKind() == bridge.StreamIncomplete || event.StreamEventKind() == bridge.StreamFailed {
			terminal = event
		}
	}
	completed, ok := terminal.(bridge.Completed)
	if !ok || completed.Reason != "stop" {
		t.Fatalf("terminal = %#v, want completed stop without read-ahead", terminal)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("post-terminal Next error = %v, want EOF", err)
	}
}

func TestChatCompletionStreamDecoderIsInvariantAcrossEveryChunkSplit(t *testing.T) {
	want := collectChatEvents(t, strings.NewReader(providerStreamFixture))
	for split := 1; split < len(providerStreamFixture); split++ {
		got := collectChatEvents(t, &chunkReader{data: []byte(providerStreamFixture), split: split})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: events differ\nwant %#v\ngot  %#v", split, want, got)
		}
	}
}

func TestChatCompletionStreamDecoderReturnsProviderErrorsWithoutRawJSONErrorText(t *testing.T) {
	decoder := NewChatCompletionStreamDecoder(strings.NewReader("data: {\"error\":{\"message\":\"secret prompt\",\"type\":\"server_error\",\"code\":\"bad\"}}\n\n"), SSEDecoderOptions{})
	event, err := decoder.Next()
	if err != nil || event.Error == nil {
		t.Fatalf("event = %#v, error = %v", event, err)
	}
	if event.Error.Message != "secret prompt" || event.Error.Code != "bad" {
		t.Fatalf("provider error = %#v", event.Error)
	}
}

func TestChatCompletionStreamDecoderClassifiesTypedMidstreamReadErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "canceled", err: context.Canceled, want: ErrStreamCanceled},
		{name: "deadline", err: context.DeadlineExceeded, want: ErrStreamTimeout},
		{name: "network timeout", err: streamTestNetError{}, want: ErrStreamTimeout},
		{name: "connection reset", err: net.ErrClosed, want: ErrStreamInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := NewChatCompletionStreamDecoder(streamErrorReader{err: test.err}, SSEDecoderOptions{})
			_, err := decoder.Next()
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, test.want)
			}
		})
	}
}

func TestBridgeStreamDecoderKeepsMalformedSSEAsProtocolError(t *testing.T) {
	decoder := NewBridgeStreamDecoder(strings.NewReader("data: {\"id\":\n\n"), BridgeStreamDecoderOptions{})
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	failed, ok := event.(bridge.Failed)
	if !ok || failed.Code != "upstream_stream_error" {
		t.Fatalf("event = %#v, want provider protocol failure", event)
	}
}

func TestBridgeStreamDecoderMapsTransportFailuresToStableCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "canceled", err: context.Canceled, want: "canceled"},
		{name: "timeout", err: context.DeadlineExceeded, want: "timeout"},
		{name: "interrupted", err: net.ErrClosed, want: "stream_interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := NewBridgeStreamDecoder(streamErrorReader{err: test.err}, BridgeStreamDecoderOptions{})
			event, err := decoder.Next()
			if err != nil {
				t.Fatal(err)
			}
			failed, ok := event.(bridge.Failed)
			if !ok || failed.Code != test.want {
				t.Fatalf("event = %#v, want code %q", event, test.want)
			}
		})
	}
}

func TestBridgeStreamDecoderEmitsSemanticEventsAndMapsTerminalReason(t *testing.T) {
	decoder := NewBridgeStreamDecoder(strings.NewReader(providerStreamFixture), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"exec_command"},
	})
	var events []bridge.StreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}

	var kinds []bridge.StreamEventKind
	for _, event := range events {
		kinds = append(kinds, event.StreamEventKind())
	}
	wantKinds := []bridge.StreamEventKind{
		bridge.StreamResponseStarted,
		bridge.StreamTextDelta,
		bridge.StreamTextDelta,
		bridge.StreamReasoningDelta,
		bridge.StreamUsageUpdated,
		bridge.StreamToolCallStarted,
		bridge.StreamToolCallArgumentsDelta,
		bridge.StreamToolCallCompleted,
		bridge.StreamCompleted,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
	}
	completed, ok := events[len(events)-2].(bridge.ToolCallCompleted)
	if !ok || completed.CallID != "call-" || completed.Name != "exec_command" || completed.Arguments != "{\"cmd\":\"true\"}" {
		t.Fatalf("completed tool = %#v", events[len(events)-2])
	}
	var started bridge.ToolCallStarted
	for _, event := range events {
		if value, ok := event.(bridge.ToolCallStarted); ok {
			started = value
			break
		}
	}
	if started.CallID == "" || started.CallID != completed.CallID {
		t.Fatalf("tool call IDs were not stable: started %#v completed %#v", started, completed)
	}
	if providerCallID, ok := decoder.ProviderCallID(completed.CallID); !ok || providerCallID != "call-1" {
		t.Fatalf("private provider call ID = %q, %v; want call-1", providerCallID, ok)
	}
	if started := events[0].(bridge.ResponseStarted); !started.CreatedAt.Equal(time.Unix(42, 0).UTC()) {
		t.Fatalf("started timestamp = %v", started.CreatedAt)
	}
}

func TestBridgeStreamDecoderDoesNotLeakAnAllowlistedNameBeforeFinalization(t *testing.T) {
	index := 0
	first := ChatCompletionChunk{
		ID: "chat-extension", Created: 1, Model: "deepseek-v4-flash",
		Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{{
			Index: &index,
			ID:    "provider-call",
			Function: ToolCallFunction{
				Name:      "lookup",
				Arguments: "{}",
			},
		}}}}},
	}
	second := ChatCompletionChunk{
		ID: "chat-extension", Created: 1, Model: "deepseek-v4-flash",
		Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{{
			Index:    &index,
			Function: ToolCallFunction{Name: "Evil"},
		}}}}},
	}
	finishReason := "tool_calls"
	terminal := ChatCompletionChunk{
		ID: "chat-extension", Created: 1, Model: "deepseek-v4-flash",
		Choices: []ChatCompletionChunkChoice{{Index: 0, FinishReason: &finishReason}},
	}
	input := providerChunksInput(t, first, second, terminal)
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		AllowedToolNames: []string{"lookup"},
	})
	var failure bridge.Failed
	var leaked int
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case bridge.Failed:
			failure = value
		case bridge.ToolCallStarted, bridge.ToolCallMetadataDelta, bridge.ToolCallArgumentsDelta, bridge.ToolCallCompleted:
			leaked++
		}
	}
	if leaked != 0 || failure.Code != "upstream_tool_not_declared" {
		t.Fatalf("extension result leaked %d tool events or returned %#v", leaked, failure)
	}
}

func TestBridgeStreamDecoderMapsEOFAndProviderFailureToFailed(t *testing.T) {
	for name, input := range map[string]string{
		"eof":            "data: {\"id\":\"chat-1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		"truncated_done": "data: [DONE]\n",
		"provider":       "data: {\"error\":{\"type\":\"server_error\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{SSE: SSEDecoderOptions{}})
			var failed bridge.Failed
			for {
				event, err := decoder.Next()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					t.Fatal(err)
				}
				if value, ok := event.(bridge.Failed); ok {
					failed = value
				}
			}
			if failed.Code == "" {
				t.Fatalf("no failed event")
			}
		})
	}
}

func TestBridgeStreamDecoderMapsProviderTerminalReasons(t *testing.T) {
	for reason, want := range map[string]string{
		"length":         "max_output_tokens",
		"content_filter": "other",
		"refusal":        "other",
	} {
		t.Run(reason, func(t *testing.T) {
			input := `data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"` + reason + `"}]}

data: [DONE]

`
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{SSE: SSEDecoderOptions{}})
			var incomplete bridge.Incomplete
			for {
				event, err := decoder.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if value, ok := event.(bridge.Incomplete); ok {
					incomplete = value
				}
			}
			if incomplete.Reason != want {
				t.Fatalf("incomplete = %#v, want %q", incomplete, want)
			}
		})
	}
}

func TestBridgeStreamDecoderDoesNotCompleteTruncatedToolCalls(t *testing.T) {
	for _, test := range []struct {
		name         string
		toolName     string
		args         string
		finishReason string
		wantReason   string
		registry     bool
	}{
		{name: "function length", toolName: "lookup", args: "{\"query\":\"partial", finishReason: "length", wantReason: "max_output_tokens"},
		{name: "function content filter", toolName: "lookup", args: "{\"query\":\"partial", finishReason: "content_filter", wantReason: "other"},
		{name: "function refusal", toolName: "lookup", args: "{\"query\":\"partial", finishReason: "refusal", wantReason: "other"},
		{name: "custom length", toolName: ApplyPatchUpstreamName, args: `{"input":"*** Begin Patch\n*** Update File: partial`, finishReason: "length", wantReason: "max_output_tokens", registry: true},
		{name: "custom content filter", toolName: ApplyPatchUpstreamName, args: `{"input":"*** Begin Patch\n*** Update File: partial`, finishReason: "content_filter", wantReason: "other", registry: true},
		{name: "custom refusal", toolName: ApplyPatchUpstreamName, args: `{"input":"*** Begin Patch\n*** Update File: partial`, finishReason: "refusal", wantReason: "other", registry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			index := 0
			finishReason := test.finishReason
			first := ChatCompletionChunk{
				ID: "chat-truncated", Created: 1, Model: "m",
				Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{{
					Index: &index,
					ID:    "call-truncated",
					Type:  "function",
					Function: ToolCallFunction{
						Name:      test.toolName,
						Arguments: test.args,
					},
				}}}}},
			}
			terminal := ChatCompletionChunk{
				ID: "chat-truncated", Created: 1, Model: "m",
				Choices: []ChatCompletionChunkChoice{{Index: 0, FinishReason: &finishReason}},
			}
			options := BridgeStreamDecoderOptions{}
			if test.registry {
				registry, err := NewToolRegistry(minimalRequest())
				if err != nil {
					t.Fatal(err)
				}
				options.ToolRegistry = registry
			} else {
				options.AllowedToolNames = []string{test.toolName}
			}
			decoder := NewBridgeStreamDecoder(strings.NewReader(providerChunksInput(t, first, terminal)), options)
			var completedCalls int
			var incomplete *bridge.Incomplete
			for {
				event, err := decoder.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				switch value := event.(type) {
				case bridge.ToolCallCompleted:
					completedCalls++
				case bridge.Incomplete:
					copy := value
					incomplete = &copy
				}
			}
			if completedCalls != 0 || incomplete == nil || incomplete.Reason != test.wantReason {
				t.Fatalf("truncated %s calls = %d incomplete = %#v", test.name, completedCalls, incomplete)
			}
		})
	}
}

func TestBridgeStreamDecoderCapturesPrivatePendingTurnAfterFinalizedCustomCall(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"provider-turn","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"private reasoning"},"finish_reason":null}]}`,
		`data: {"id":"provider-turn","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"provider-custom","type":"function","function":{"name":"__ocg_apply_patch","arguments":"{\"input\":\"patch"}}]},"finish_reason":null}]}`,
		`data: {"id":"provider-turn","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	registry, err := NewToolRegistry(bridge.Request{})
	if err != nil {
		t.Fatal(err)
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(stream), BridgeStreamDecoderOptions{ToolRegistry: registry})
	for {
		_, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	turn, ok := decoder.PendingTurn()
	if !ok {
		t.Fatal("finalized tool turn was not captured")
	}
	if turn.Model != "deepseek-v4-flash" || turn.ReasoningContent != "private reasoning" || turn.AssistantContent != "" || len(turn.ToolCalls) != 1 {
		t.Fatalf("pending turn = %#v", turn)
	}
	call := turn.ToolCalls[0]
	if call.CallID != "provider-custom" || call.ProviderCallID != "provider-custom" || call.Kind != bridge.ToolCustom || call.Name != ApplyPatchUpstreamName || call.Arguments != `{"input":"patch"}` {
		t.Fatalf("pending custom call = %#v", call)
	}
}

func TestBridgeStreamDecoderRejectsMissingOrNegativeToolIndexes(t *testing.T) {
	for name, tool := range map[string]string{
		"missing":  `{"id":"call","function":{"name":"first","arguments":"{}"}}`,
		"negative": `{"index":-1,"id":"call","function":{"name":"first","arguments":"{}"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := `data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[` + tool + `]},"finish_reason":null}]}

data: [DONE]

`
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{SSE: SSEDecoderOptions{}})
			for {
				event, err := decoder.Next()
				if err != nil {
					t.Fatal(err)
				}
				failed, ok := event.(bridge.Failed)
				if !ok {
					continue
				}
				if failed.Code != "upstream_stream_error" {
					t.Fatalf("failure = %#v, want upstream_stream_error", failed)
				}
				return
			}
		})
	}
}

func TestBridgeStreamDecoderRejectsDeltasAfterChoiceFinish(t *testing.T) {
	input := `data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":"stop"}]}

data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"late"},"finish_reason":null}]}

data: [DONE]

`
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{SSE: SSEDecoderOptions{}})
	for {
		event, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		failed, ok := event.(bridge.Failed)
		if !ok {
			continue
		}
		if failed.Code != "upstream_stream_error" {
			t.Fatalf("failure = %#v, want upstream_stream_error", failed)
		}
		return
	}
}

func TestBridgeStreamDecoderEnforcesAggregateLimitAcrossSmallChunks(t *testing.T) {
	input := strings.Repeat(`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}

`, 32)
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{SSE: SSEDecoderOptions{MaxAggregateBytes: 256}})
	for {
		event, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		failed, ok := event.(bridge.Failed)
		if !ok {
			continue
		}
		if failed.Code != "stream_limit_exceeded" {
			t.Fatalf("failure = %#v, want stream_limit_exceeded", failed)
		}
		return
	}
}

func TestBridgeStreamDecoderChargesFunctionArgumentsBeforeMutation(t *testing.T) {
	index := 0
	makeChunk := func(arguments string) ChatCompletionChunk {
		return ChatCompletionChunk{
			ID: "chat-function-limit", Model: "m",
			Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{{
				Index:    &index,
				Function: ToolCallFunction{Arguments: arguments},
			}}}}},
		}
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(""), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{MaxAggregateBytes: 1 << 20},
		AllowedToolNames: []string{"lookup"},
	})
	first := makeChunk("")
	first.Choices[0].Delta.ToolCalls[0].ID = "call-function-limit"
	first.Choices[0].Delta.ToolCalls[0].Function.Name = "lookup"
	if err := decoder.consumeChunk(first); err != nil {
		t.Fatal(err)
	}
	state := decoder.calls[bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}]
	if state == nil {
		t.Fatal("function call state was not retained")
	}
	before := decoder.aggregateBytes
	fragment := `{"query":"ok"}`
	decoder.maxAggregateBytes = before + len(fragment)
	if err := decoder.consumeChunk(makeChunk(fragment)); err != nil {
		t.Fatalf("argument fragment at exact aggregate boundary = %v", err)
	}
	if decoder.aggregateBytes != before+len(fragment) || string(state.arguments) != fragment {
		t.Fatalf("argument accounting = aggregate %d arguments %q, want %d/%q", decoder.aggregateBytes, state.arguments, before+len(fragment), fragment)
	}

	decoder.maxAggregateBytes = decoder.aggregateBytes
	if err := decoder.consumeChunk(makeChunk("!")); !errors.Is(err, ErrStreamAggregateLimit) {
		t.Fatalf("argument fragment over aggregate boundary = %v, want %v", err, ErrStreamAggregateLimit)
	}
	if string(state.arguments) != fragment {
		t.Fatalf("argument state mutated after rejected fragment = %q", state.arguments)
	}
}

func TestBridgeStreamDecoderBoundsNoOpChunksWithoutRecursion(t *testing.T) {
	const noOpChunk = `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[]}` + "\n\n"
	input := strings.Repeat(noOpChunk, 128) + "data: [DONE]\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE: SSEDecoderOptions{MaxAggregateBytes: 256},
	})

	for events := 0; events < 128; events++ {
		event, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		failed, ok := event.(bridge.Failed)
		if !ok {
			continue
		}
		if failed.Code != "stream_limit_exceeded" {
			t.Fatalf("no-op stream failure = %#v, want stream_limit_exceeded", failed)
		}
		return
	}
	t.Fatal("no-op stream was not bounded by the aggregate stream limit")
}

func TestBridgeStreamDecoderChargesRetainedProviderCallIDGrowth(t *testing.T) {
	makeChunk := func(id, name string) ChatCompletionChunk {
		index := 0
		return ChatCompletionChunk{
			ID:    "chat-1",
			Model: "m",
			Choices: []ChatCompletionChunkChoice{{
				Index: 0,
				Delta: ChatMessage{ToolCalls: []ToolCall{{
					Index: &index,
					ID:    id,
					Function: ToolCallFunction{
						Name: name,
					},
				}}},
			}},
		}
	}
	first := makeChunk("seed", "lookup")
	decoder := NewBridgeStreamDecoder(strings.NewReader(""), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{MaxAggregateBytes: 1 << 20},
		AllowedToolNames: []string{"lookup"},
	})
	if err := decoder.consumeChunk(first); err != nil {
		t.Fatal(err)
	}
	beforeRepeat := decoder.aggregateBytes
	repeated := makeChunk("seed", "")
	if err := decoder.consumeChunk(repeated); err != nil {
		t.Fatal(err)
	}
	if decoder.aggregateBytes != beforeRepeat {
		t.Fatalf("repeated provider ID changed aggregate budget: before %d after %d", beforeRepeat, decoder.aggregateBytes)
	}

	fragment := "unique-provider-fragment"
	beforeUnique := decoder.aggregateBytes
	decoder.maxAggregateBytes = beforeUnique + len(fragment) - 1
	unique := makeChunk(fragment, "")
	err := decoder.consumeChunk(unique)
	if !errors.Is(err, ErrStreamAggregateLimit) {
		t.Fatalf("unique provider ID fragment error = %v, want %v", err, ErrStreamAggregateLimit)
	}
	if strings.Contains(err.Error(), fragment) {
		t.Fatalf("stream limit error exposed provider ID fragment: %v", err)
	}
	if providerID, ok := decoder.ProviderCallID("seed"); !ok || providerID != "seed" {
		t.Fatalf("failed provider ID fragment mutated retained state: %q, %v", providerID, ok)
	}
	decoder.maxAggregateBytes = beforeUnique + len(fragment)
	if err := decoder.consumeChunk(unique); err != nil {
		t.Fatalf("provider ID fragment at aggregate boundary = %v", err)
	}
}

func TestBridgeStreamDecoderBoundsToolIndexesBeforeStateMutation(t *testing.T) {
	makeChunk := func(index int) ChatCompletionChunk {
		return ChatCompletionChunk{
			ID:    "chat-1",
			Model: "m",
			Choices: []ChatCompletionChunkChoice{{
				Index: 0,
				Delta: ChatMessage{ToolCalls: []ToolCall{{
					Index: &index,
				}}},
			}},
		}
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(""), BridgeStreamDecoderOptions{
		SSE:          SSEDecoderOptions{MaxAggregateBytes: 1 << 20},
		MaxToolCalls: 2,
	})
	for _, index := range []int{0, 1} {
		if err := decoder.consumeChunk(makeChunk(index)); err != nil {
			t.Fatalf("tool index %d: %v", index, err)
		}
	}
	if len(decoder.calls) != 2 {
		t.Fatalf("retained calls = %d, want 2", len(decoder.calls))
	}
	if err := decoder.consumeChunk(makeChunk(2)); !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("third empty tool index error = %v, want %v", err, ErrToolCallLimit)
	}
	if len(decoder.calls) != 2 {
		t.Fatalf("call count after boundary rejection = %d, want 2", len(decoder.calls))
	}

	sparse := NewBridgeStreamDecoder(strings.NewReader(""), BridgeStreamDecoderOptions{MaxToolCalls: 2})
	if err := sparse.consumeChunk(makeChunk(1000000)); !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("sparse empty tool index error = %v, want %v", err, ErrToolCallLimit)
	}
	if len(sparse.calls) != 0 || sparse.started || len(sparse.pending) != 0 {
		t.Fatalf("sparse rejection mutated decoder state: calls=%d started=%t pending=%d", len(sparse.calls), sparse.started, len(sparse.pending))
	}
}

func TestBridgeStreamDecoderBoundsFragmentedProviderToolNamesBeforeAppend(t *testing.T) {
	index := 0
	chunk := func(name string) ChatCompletionChunk {
		return ChatCompletionChunk{Choices: []ChatCompletionChunkChoice{{
			Index: 0,
			Delta: ChatMessage{ToolCalls: []ToolCall{{
				Index:    &index,
				Function: ToolCallFunction{Name: name},
			}}},
		}}}
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(""), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{MaxAggregateBytes: 1 << 20},
		MaxToolNameBytes: 4,
		AllowedToolNames: []string{"long"},
	})
	if err := decoder.consumeChunk(chunk("lo")); err != nil {
		t.Fatal(err)
	}
	if err := decoder.consumeChunk(chunk("ng")); err != nil {
		t.Fatal(err)
	}
	state := decoder.calls[bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}]
	if state == nil || string(state.name) != "long" {
		t.Fatalf("name before overflow = %#v", state)
	}
	if err := decoder.consumeChunk(chunk("x")); !errors.Is(err, ErrToolCallMetadataLimit) {
		t.Fatalf("fragmented name overflow error = %v, want %v", err, ErrToolCallMetadataLimit)
	}
	if got := string(state.name); got != "long" {
		t.Fatalf("name mutated after overflow = %q, want long", got)
	}
}

func TestBridgeStreamDecoderBoundsFragmentedProviderCallIDsBeforeAppend(t *testing.T) {
	index := 0
	chunk := func(id string) ChatCompletionChunk {
		return ChatCompletionChunk{Choices: []ChatCompletionChunkChoice{{
			Index: 0,
			Delta: ChatMessage{ToolCalls: []ToolCall{{
				Index: &index,
				ID:    id,
			}}},
		}}}
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(""), BridgeStreamDecoderOptions{
		SSE:                    SSEDecoderOptions{MaxAggregateBytes: 1 << 20},
		MaxProviderCallIDBytes: 8,
	})
	if err := decoder.consumeChunk(chunk("ab")); err != nil {
		t.Fatal(err)
	}
	if err := decoder.consumeChunk(chunk("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	state := decoder.calls[bridge.ToolCallKey{ChoiceIndex: 0, ToolIndex: 0}]
	if state == nil || string(state.providerCallID) != "abcdefgh" {
		t.Fatalf("provider ID before overflow = %#v", state)
	}
	if err := decoder.consumeChunk(chunk("abcdefghi")); !errors.Is(err, ErrToolCallMetadataLimit) {
		t.Fatalf("provider ID overflow error = %v, want %v", err, ErrToolCallMetadataLimit)
	}
	if got := string(state.providerCallID); got != "abcdefgh" {
		t.Fatalf("provider ID mutated after overflow = %q, want abcdefgh", got)
	}
}

func TestChatCompletionStreamDecoderRejectsMalformedJSONWithoutEchoingPayload(t *testing.T) {
	decoder := NewChatCompletionStreamDecoder(strings.NewReader("data: {\"secret\":\"private prompt marker\"\n\n"), SSEDecoderOptions{})
	_, err := decoder.Next()
	if !errors.Is(err, ErrMalformedStream) {
		t.Fatalf("error = %v, want malformed stream", err)
	}
	if strings.Contains(err.Error(), "private prompt marker") {
		t.Fatalf("malformed stream error leaked payload: %v", err)
	}
}

func TestBridgeStreamDecoderPreservesParallelToolIndexesAndOrder(t *testing.T) {
	input := `data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"second","arguments":"2"}},{"index":0,"id":"a","function":{"name":"first","arguments":"1"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"first", "second"},
	})
	started := make(map[int]bridge.ToolCallStarted)
	var completed []bridge.ToolCallCompleted
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case bridge.ToolCallStarted:
			started[value.Key.ToolIndex] = value
		case bridge.ToolCallCompleted:
			completed = append(completed, value)
		}
	}
	if len(completed) != 2 || completed[0].Key.ToolIndex != 0 || completed[1].Key.ToolIndex != 1 || completed[0].CallID != "a" || completed[1].CallID != "b" {
		t.Fatalf("completed calls = %#v", completed)
	}
	if started[0].CallID != completed[0].CallID || started[1].CallID != completed[1].CallID {
		t.Fatalf("call IDs changed between start and completion: started=%#v completed=%#v", started, completed)
	}
}

func TestBridgeStreamDecoderReconstructsInterleavedFragmentedCallsAndText(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat-1","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"before","tool_calls":[{"index":0,"id":"call-","type":"function","function":{"name":"get","arguments":"[1"}}]},"finish_reason":null}]}`,
		`data: {"id":"chat-1","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"type":"function","function":{"name":"get","arguments":"not"}}]},"finish_reason":null}]}`,
		`data: {"id":"chat-1","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"after","tool_calls":[{"index":0,"id":"0","function":{"name":"_one","arguments":"]"}},{"index":1,"function":{"name":"_two","arguments":"-json"}}]},"finish_reason":null}]}`,
		`data: {"id":"chat-1","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"get_one", "get_two"},
	})
	var events []bridge.StreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}

	var text string
	completed := make(map[int]bridge.ToolCallCompleted)
	for _, event := range events {
		switch value := event.(type) {
		case bridge.TextDelta:
			text += value.Text
		case bridge.ToolCallCompleted:
			completed[value.Key.ToolIndex] = value
		}
	}
	if text != "beforeafter" {
		t.Fatalf("text = %q, want beforeafter", text)
	}
	if got := completed[0]; got.CallID != "call-" || got.Name != "get_one" || got.Arguments != `[1]` {
		t.Fatalf("first call = %#v", got)
	}
	if got := completed[1]; got.CallID != "call_0_1" || got.Name != "get_two" || got.Arguments != "not-json" {
		t.Fatalf("second call = %#v", got)
	}
	if _, ok := events[len(events)-1].(bridge.Completed); !ok {
		t.Fatalf("last event = %#v, want completed", events[len(events)-1])
	}
}

func TestBridgeStreamDecoderUnwrapsFragmentedApplyPatchWrapper(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: café.txt\n+Olá, 世界\n*** End Patch"
	toolIndex := 0
	finishReason := "tool_calls"
	wrapped, err := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: patch})
	if err != nil {
		t.Fatal(err)
	}
	firstArgument := string(wrapped[:4])
	secondArgument := string(wrapped[4:])
	firstCall := ToolCall{
		Index: &toolIndex,
		ID:    "provider-call",
		Type:  "function",
		Function: ToolCallFunction{
			Name:      ReservedToolNamePrefix,
			Arguments: firstArgument,
		},
	}
	secondCall := ToolCall{
		Index: &toolIndex,
		Function: ToolCallFunction{
			Name:      ApplyPatchToolName,
			Arguments: secondArgument,
		},
	}
	chunks := []ChatCompletionChunk{
		{
			ID: "chat-apply", Created: 1, Model: "deepseek-v4-flash",
			Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{firstCall}}}},
		},
		{
			ID: "chat-apply", Created: 1, Model: "deepseek-v4-flash",
			Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{secondCall}}}},
		},
		{
			ID: "chat-apply", Created: 1, Model: "deepseek-v4-flash",
			Choices: []ChatCompletionChunkChoice{{Index: 0, FinishReason: &finishReason}},
		},
	}
	input := providerChunksInput(t, chunks...)
	request := minimalRequest()
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{ToolRegistry: registry})
	var events []bridge.StreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	var started bridge.ToolCallStarted
	var argumentDeltas []bridge.ToolCallArgumentsDelta
	var completed bridge.ToolCallCompleted
	for _, event := range events {
		switch value := event.(type) {
		case bridge.ToolCallStarted:
			started = value
		case bridge.ToolCallArgumentsDelta:
			argumentDeltas = append(argumentDeltas, value)
		case bridge.ToolCallCompleted:
			completed = value
		}
		if strings.Contains(fmt.Sprintf("%#v", event), ApplyPatchUpstreamName) {
			t.Fatalf("synthetic function name leaked through bridge event: %#v", event)
		}
	}
	if started.Kind != bridge.ToolCustom || started.Name != ApplyPatchToolName || started.CallID != "provider-call" {
		t.Fatalf("custom start = %#v", started)
	}
	if len(argumentDeltas) != 1 || argumentDeltas[0].Arguments != patch {
		t.Fatalf("custom argument deltas = %#v, want one exact patch", argumentDeltas)
	}
	if completed.Kind != bridge.ToolCustom || completed.CallID != started.CallID || completed.Name != ApplyPatchToolName || completed.Arguments != patch {
		t.Fatalf("custom completion = %#v", completed)
	}
	if _, ok := events[len(events)-1].(bridge.Completed); !ok {
		t.Fatalf("last event = %#v", events[len(events)-1])
	}
}

func providerChunksInput(t *testing.T, chunks ...ChatCompletionChunk) string {
	t.Helper()
	var lines []string
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "data: "+string(encoded))
	}
	lines = append(lines, "data: [DONE]")
	return strings.Join(lines, "\n\n") + "\n\n"
}

func TestBridgeStreamDecoderRejectsMalformedApplyPatchWrappers(t *testing.T) {
	for name, arguments := range map[string]string{
		"missing":    `{"other":"patch"}`,
		"non-string": `{"input":42}`,
		"duplicate":  `{"input":"one","input":"two"}`,
		"trailing":   `{"input":"patch"} {}`,
		"malformed":  `{"input":`,
	} {
		t.Run(name, func(t *testing.T) {
			input := `data: {"id":"chat-apply","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"__ocg_apply_patch","arguments":"` + escapeJSON(arguments) + `"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n"
			request := minimalRequest()
			registry, err := NewToolRegistry(request)
			if err != nil {
				t.Fatal(err)
			}
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{ToolRegistry: registry})
			failed := collectLastFailed(t, decoder)
			if failed.Code != "upstream_custom_tool_invalid" {
				t.Fatalf("failure = %#v, want upstream_custom_tool_invalid", failed)
			}
		})
	}
}

func TestBridgeStreamDecoderEnforcesDedicatedApplyPatchInputLimit(t *testing.T) {
	const marker = "sensitive-patch-marker"
	for _, test := range []struct {
		name string
		size int
		ok   bool
	}{
		{name: "one byte below", size: DefaultMaxApplyPatchInputBytes - 1, ok: true},
		{name: "exact boundary", size: DefaultMaxApplyPatchInputBytes},
		{name: "one byte above", size: DefaultMaxApplyPatchInputBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.Repeat("x", test.size-len(marker)) + marker
			wrapper, err := json.Marshal(struct {
				Input string `json:"input"`
			}{Input: input})
			if err != nil {
				t.Fatal(err)
			}
			toolIndex := 0
			chunks := make([]ChatCompletionChunk, 0, (len(wrapper)+32767)/32768+2)
			for offset := 0; offset < len(wrapper); {
				end := offset + 32768
				if end > len(wrapper) {
					end = len(wrapper)
				}
				call := ToolCall{
					Index: &toolIndex,
					Function: ToolCallFunction{
						Arguments: string(wrapper[offset:end]),
					},
				}
				if offset == 0 {
					call.ID = "limit-call"
					call.Type = "function"
					call.Function.Name = ApplyPatchUpstreamName
				}
				chunks = append(chunks, ChatCompletionChunk{
					ID:      "chat-limit",
					Created: 1,
					Model:   "deepseek-v4-flash",
					Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: ChatMessage{ToolCalls: []ToolCall{call}}}},
				})
				offset = end
			}
			finishReason := "tool_calls"
			chunks = append(chunks, ChatCompletionChunk{
				ID: "chat-limit", Created: 1, Model: "deepseek-v4-flash",
				Choices: []ChatCompletionChunkChoice{{Index: 0, FinishReason: &finishReason}},
			})
			registry, err := NewToolRegistry(minimalRequest())
			if err != nil {
				t.Fatal(err)
			}
			decoder := NewBridgeStreamDecoder(strings.NewReader(providerChunksInput(t, chunks...)), BridgeStreamDecoderOptions{ToolRegistry: registry})
			var events []bridge.StreamEvent
			for {
				event, err := decoder.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, event)
			}
			if test.ok {
				completed, ok := events[len(events)-1].(bridge.Completed)
				if !ok || completed.Reason != "tool_calls" {
					t.Fatalf("terminal event = %#v", events[len(events)-1])
				}
				for _, event := range events {
					if call, ok := event.(bridge.ToolCallCompleted); ok && call.Name == ApplyPatchToolName {
						if len(call.Arguments) != test.size || !strings.HasSuffix(call.Arguments, marker) {
							t.Fatalf("completed input length/content = %d/%q", len(call.Arguments), call.Arguments[len(call.Arguments)-len(marker):])
						}
					}
				}
				return
			}
			failed, ok := events[len(events)-1].(bridge.Failed)
			if !ok || failed.Code != "stream_limit_exceeded" {
				t.Fatalf("limit terminal event = %#v", events[len(events)-1])
			}
			if strings.Contains(fmt.Sprintf("%#v", failed), marker) {
				t.Fatal("input marker leaked through the stream failure")
			}
		})
	}
}

func escapeJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded[1 : len(encoded)-1])
}

func collectLastFailed(t *testing.T, decoder *BridgeStreamDecoder) bridge.Failed {
	t.Helper()
	var failed bridge.Failed
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(bridge.Failed); ok {
			failed = value
		}
	}
	return failed
}

func TestBridgeStreamDecoderKeepsSyntheticIDWhenProviderIDArrivesLater(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"look","arguments":"["}}]},"finish_reason":null}]}`,
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"provider-","function":{"name":"up","arguments":"1"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"id","function":{"name":"","arguments":"]"}}]},"finish_reason":null}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"lookup"},
	})
	var started bridge.ToolCallStarted
	var completed bridge.ToolCallCompleted
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case bridge.ToolCallStarted:
			started = value
		case bridge.ToolCallCompleted:
			completed = value
		}
	}
	if started.CallID != "call_0_0" || completed.CallID != started.CallID {
		t.Fatalf("stable synthetic IDs = started %q completed %q", started.CallID, completed.CallID)
	}
	if providerCallID, ok := decoder.ProviderCallID(started.CallID); !ok || providerCallID != "provider-id" {
		t.Fatalf("private delayed provider ID = %q, %v; want provider-id", providerCallID, ok)
	}
}

func TestBridgeStreamDecoderAllowsFinishBeforeParallelToolFragments(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"look","arguments":"["}},{"index":1,"id":"call-b","type":"function","function":{"name":"other","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"up","arguments":"]"}},{"index":1,"function":{"name":"_tool","arguments":"}"}}]},"finish_reason":null}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"lookup", "other_tool"},
	})
	var completed []bridge.ToolCallCompleted
	var failed *bridge.Failed
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case bridge.ToolCallCompleted:
			completed = append(completed, value)
		case bridge.Failed:
			failed = &value
		}
	}
	if failed != nil || len(completed) != 2 || completed[0].Name != "lookup" || completed[1].Name != "other_tool" {
		t.Fatalf("early-finish parallel calls = completed %#v failed %#v", completed, failed)
	}
}

func TestBridgeStreamDecoderRejectsUndeclaredCompletedToolName(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"look","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"_evil","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"lookup"},
	})
	var completed, terminals int
	var leakedToolEvents int
	var failure bridge.Failed
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case bridge.ToolCallCompleted:
			completed++
		case bridge.Completed:
			terminals++
		case bridge.Failed:
			failure = value
		}
		switch event.StreamEventKind() {
		case bridge.StreamToolCallStarted, bridge.StreamToolCallMetadataDelta, bridge.StreamToolCallArgumentsDelta, bridge.StreamToolCallCompleted:
			leakedToolEvents++
		}
	}
	if completed != 0 || terminals != 0 || leakedToolEvents != 0 || failure.Code != "upstream_tool_not_declared" {
		t.Fatalf("undeclared tool result = completed %d terminals %d leaked %d failure %#v", completed, terminals, leakedToolEvents, failure)
	}
}

func TestBridgeStreamDecoderMatchesCheckedFragmentedFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "opencodego", "parallel-fragmented.sse"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectBridgeEvents(t, strings.NewReader(string(fixture)), SSEDecoderOptions{}, "look_up", "other_tool")
	var kinds []bridge.StreamEventKind
	completed := make(map[int]bridge.ToolCallCompleted)
	var text string
	for _, event := range events {
		kinds = append(kinds, event.StreamEventKind())
		switch value := event.(type) {
		case bridge.TextDelta:
			text += value.Text
		case bridge.ToolCallCompleted:
			completed[value.Key.ToolIndex] = value
		}
	}
	wantKinds := []bridge.StreamEventKind{
		bridge.StreamResponseStarted,
		bridge.StreamTextDelta,
		bridge.StreamTextDelta,
		bridge.StreamToolCallStarted,
		bridge.StreamToolCallArgumentsDelta,
		bridge.StreamToolCallStarted,
		bridge.StreamToolCallArgumentsDelta,
		bridge.StreamToolCallCompleted,
		bridge.StreamToolCallCompleted,
		bridge.StreamCompleted,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("fixture event kinds = %v, want %v", kinds, wantKinds)
	}
	if text != "prefixsuffix" || completed[0].CallID != "call-" || completed[0].Name != "look_up" || completed[0].Arguments != "[1]" {
		t.Fatalf("fixture first call/text = %q %#v", text, completed[0])
	}
	if completed[1].CallID != "call_0_1" || completed[1].Name != "other_tool" || completed[1].Arguments != "not-json" {
		t.Fatalf("fixture second call = %#v", completed[1])
	}
}

func TestBridgeStreamDecoderMatchesCheckedApplyPatchFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "opencodego", "apply-patch-fragmented.sse"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewToolRegistry(minimalRequest())
	if err != nil {
		t.Fatal(err)
	}
	decoder := NewBridgeStreamDecoder(strings.NewReader(string(fixture)), BridgeStreamDecoderOptions{ToolRegistry: registry})
	var events []bridge.StreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	var completed bridge.ToolCallCompleted
	var kinds []bridge.StreamEventKind
	for _, event := range events {
		kinds = append(kinds, event.StreamEventKind())
		if value, ok := event.(bridge.ToolCallCompleted); ok {
			completed = value
		}
		if strings.Contains(fmt.Sprintf("%#v", event), ApplyPatchUpstreamName) {
			t.Fatalf("synthetic name leaked through bridge event: %#v", event)
		}
	}
	wantKinds := []bridge.StreamEventKind{
		bridge.StreamResponseStarted,
		bridge.StreamToolCallStarted,
		bridge.StreamToolCallArgumentsDelta,
		bridge.StreamToolCallCompleted,
		bridge.StreamCompleted,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("apply_patch fixture event kinds = %v, want %v", kinds, wantKinds)
	}
	if completed.Kind != bridge.ToolCustom || completed.CallID != "provider-custom" || completed.Name != ApplyPatchToolName || completed.Arguments != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("apply_patch fixture completion = %#v", completed)
	}
}

func TestBridgeStreamDecoderRejectsDuplicateToolIndexWithinOneChunk(t *testing.T) {
	input := "data: {\"id\":\"chat-1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"one\",\"arguments\":\"1\"}},{\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"two\",\"arguments\":\"2\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{SSE: SSEDecoderOptions{}, AllowedToolNames: []string{"one", "two"}})
	for {
		event, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if failed, ok := event.(bridge.Failed); ok {
			if failed.Code != "upstream_stream_error" {
				t.Fatalf("failure = %#v", failed)
			}
			return
		}
	}
}

func TestBridgeStreamDecoderEnforcesPerCallArgumentLimit(t *testing.T) {
	input := "data: {\"id\":\"chat-1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"one\",\"arguments\":\"12345\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{MaxToolCallArgumentBytes: 4},
		AllowedToolNames: []string{"one"},
	})
	for {
		event, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if failed, ok := event.(bridge.Failed); ok {
			if failed.Code != "stream_limit_exceeded" {
				t.Fatalf("failure = %#v, want stream_limit_exceeded", failed)
			}
			return
		}
	}
}

func TestBridgeStreamDecoderRejectsDuplicateCompletedCallIDs(t *testing.T) {
	input := "data: {\"id\":\"chat-1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"same\",\"type\":\"function\",\"function\":{\"name\":\"one\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"same\",\"type\":\"function\",\"function\":{\"name\":\"two\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), BridgeStreamDecoderOptions{
		SSE:              SSEDecoderOptions{},
		AllowedToolNames: []string{"one", "two"},
	})
	for {
		event, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if failed, ok := event.(bridge.Failed); ok {
			if failed.Code != "upstream_stream_error" {
				t.Fatalf("failure = %#v, want upstream_stream_error", failed)
			}
			return
		}
	}
}

func collectChatEvents(t *testing.T, reader io.Reader) []ChatCompletionStreamEvent {
	t.Helper()
	decoder := NewChatCompletionStreamDecoder(reader, SSEDecoderOptions{})
	var events []ChatCompletionStreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func collectBridgeEvents(t *testing.T, reader io.Reader, options SSEDecoderOptions, allowedToolNames ...string) []bridge.StreamEvent {
	t.Helper()
	decoder := NewBridgeStreamDecoder(reader, BridgeStreamDecoderOptions{SSE: options, AllowedToolNames: allowedToolNames})
	var events []bridge.StreamEvent
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}
