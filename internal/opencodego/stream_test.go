package opencodego

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

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

func TestBridgeStreamDecoderEmitsSemanticEventsAndMapsTerminalReason(t *testing.T) {
	decoder := NewBridgeStreamDecoder(strings.NewReader(providerStreamFixture), SSEDecoderOptions{})
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
		bridge.StreamToolCallStarted,
		bridge.StreamToolCallArgumentsDelta,
		bridge.StreamToolCallMetadataDelta,
		bridge.StreamToolCallArgumentsDelta,
		bridge.StreamUsageUpdated,
		bridge.StreamToolCallCompleted,
		bridge.StreamCompleted,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
	}
	completed, ok := events[len(events)-2].(bridge.ToolCallCompleted)
	if !ok || completed.CallID != "call-1" || completed.Name != "exec_command" || completed.Arguments != "{\"cmd\":\"true\"}" {
		t.Fatalf("completed tool = %#v", events[len(events)-2])
	}
	if started := events[0].(bridge.ResponseStarted); !started.CreatedAt.Equal(time.Unix(42, 0).UTC()) {
		t.Fatalf("started timestamp = %v", started.CreatedAt)
	}
}

func TestBridgeStreamDecoderMapsEOFAndProviderFailureToFailed(t *testing.T) {
	for name, input := range map[string]string{
		"eof":            "data: {\"id\":\"chat-1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		"truncated_done": "data: [DONE]\n",
		"provider":       "data: {\"error\":{\"type\":\"server_error\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), SSEDecoderOptions{})
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
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), SSEDecoderOptions{})
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

func TestBridgeStreamDecoderRejectsMissingOrNegativeToolIndexes(t *testing.T) {
	for name, tool := range map[string]string{
		"missing":  `{"id":"call","function":{"name":"first","arguments":"{}"}}`,
		"negative": `{"index":-1,"id":"call","function":{"name":"first","arguments":"{}"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := `data: {"id":"chat-1","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[` + tool + `]},"finish_reason":null}]}

data: [DONE]

`
			decoder := NewBridgeStreamDecoder(strings.NewReader(input), SSEDecoderOptions{})
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
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), SSEDecoderOptions{})
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
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), SSEDecoderOptions{MaxAggregateBytes: 256})
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
	decoder := NewBridgeStreamDecoder(strings.NewReader(input), SSEDecoderOptions{})
	var completed []bridge.ToolCallCompleted
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(bridge.ToolCallCompleted); ok {
			completed = append(completed, value)
		}
	}
	if len(completed) != 2 || completed[0].Key.ToolIndex != 0 || completed[1].Key.ToolIndex != 1 || completed[0].CallID != "a" || completed[1].CallID != "b" {
		t.Fatalf("completed calls = %#v", completed)
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
