package opencodego

import (
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func TestSSEDecoderExhaustivelyHandlesEverySingleBoundary(t *testing.T) {
	input := ": comment\r\n" +
		"event: response\r\n" +
		"id: fixture-1\r\n" +
		"retry: 25\r\n" +
		"data: {\"message\":\r\n" +
		"data: \"Olá, 世界\"}\r\n\r\n" +
		"data: plain\n\n"
	want := []SSEEvent{
		{Event: "response", ID: "fixture-1", Retry: durationPointer(25), Data: "{\"message\":\n\"Olá, 世界\"}"},
		{Data: "plain"},
	}
	for boundary := 1; boundary < len(input); boundary++ {
		t.Run(boundaryName(boundary), func(t *testing.T) {
			got := collectSSEEvents(t, &singleBoundaryReader{data: []byte(input), boundary: boundary})
			if len(got) != len(want) {
				t.Fatalf("events = %#v, want %#v", got, want)
			}
			if got[0].Event != want[0].Event || got[0].ID != want[0].ID || got[0].Data != want[0].Data {
				t.Fatalf("first event = %#v, want %#v", got[0], want[0])
			}
			if got[0].Retry == nil || want[0].Retry == nil || *got[0].Retry != *want[0].Retry || got[1].Data != want[1].Data {
				t.Fatalf("event metadata/data = %#v, want %#v", got, want)
			}
		})
	}
}

func TestSSEDecoderHandlesDeterministicRandomChunkPartitions(t *testing.T) {
	input := strings.Join([]string{
		": keepalive",
		"event: response",
		"data: {\"type\":",
		"data: \"response.output_text.delta\",\"delta\":\"Olá, 世界\"}",
		"",
		"data: {\"type\":\"response.completed\",\"sequence_number\":1}",
		"",
		"",
	}, "\r\n")
	for seed := int64(1); seed <= 32; seed++ {
		t.Run("seed-"+itoa(seed), func(t *testing.T) {
			random := rand.New(rand.NewSource(seed))
			reader := &partitionReader{data: []byte(input), random: random}
			got := collectSSEEvents(t, reader)
			if len(got) != 2 || !strings.Contains(got[0].Data, "response.output_text.delta") || !strings.Contains(got[1].Data, "response.completed") {
				t.Fatalf("seed %d events = %#v", seed, got)
			}
		})
	}
}

func TestSSEDecoderAcceptsEventsWithoutDoneAndKeepsDownstreamParsingIndependent(t *testing.T) {
	input := "data: {\"id\":\"chunk\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Olá\"},\"finish_reason\":\"stop\"}]}\n\n"
	first := collectSSEEvents(t, strings.NewReader(input))
	second := collectSSEEvents(t, strings.NewReader(input))
	if len(first) != 1 || len(second) != 1 || first[0].Data != second[0].Data {
		t.Fatalf("independent parses diverged: first=%#v second=%#v", first, second)
	}
	decoder := NewChatCompletionStreamDecoder(strings.NewReader(input), SSEDecoderOptions{})
	if _, err := decoder.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("provider decoder error = %v, want missing terminal marker", err)
	}
	bridgeEvents := collectBridgeEvents(t, strings.NewReader(input), SSEDecoderOptions{}, "lookup")
	if len(bridgeEvents) == 0 {
		t.Fatal("bridge decoder emitted no semantic events for the truncated provider stream")
	}
	failure, ok := bridgeEvents[len(bridgeEvents)-1].(bridge.Failed)
	if !ok || failure.Code != "upstream_eof" {
		t.Fatalf("bridge terminal = %#v, want upstream_eof failure", bridgeEvents[len(bridgeEvents)-1])
	}
}

func TestBridgeStreamDecoderHandlesRandomPartitionsOfCheckedFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "opencodego", "parallel-fragmented.sse"))
	if err != nil {
		t.Fatal(err)
	}
	for seed := int64(101); seed <= 132; seed++ {
		t.Run("seed-"+itoa(seed), func(t *testing.T) {
			random := rand.New(rand.NewSource(seed))
			events := collectBridgeEvents(t, &partitionReader{data: fixture, random: random}, SSEDecoderOptions{}, "look_up", "other_tool")
			if len(events) == 0 || events[0].StreamEventKind() != bridge.StreamResponseStarted {
				t.Fatalf("events did not start with response metadata: %#v", events)
			}
			if events[len(events)-1].StreamEventKind() != bridge.StreamCompleted {
				t.Fatalf("last semantic event = %s, want completed", events[len(events)-1].StreamEventKind())
			}
			completedCalls := 0
			for _, event := range events {
				if event.StreamEventKind() == bridge.StreamToolCallCompleted {
					completedCalls++
				}
			}
			if completedCalls != 2 {
				t.Fatalf("completed tool calls = %d, want 2", completedCalls)
			}
		})
	}
}

func collectSSEEvents(t *testing.T, reader io.Reader) []SSEEvent {
	t.Helper()
	decoder := NewSSEDecoder(reader, SSEDecoderOptions{})
	var events []SSEEvent
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

type singleBoundaryReader struct {
	data     []byte
	position int
	boundary int
}

func (reader *singleBoundaryReader) Read(target []byte) (int, error) {
	if reader.position == len(reader.data) {
		return 0, io.EOF
	}
	end := len(reader.data)
	if reader.position < reader.boundary {
		end = reader.boundary
	}
	if end-reader.position > len(target) {
		end = reader.position + len(target)
	}
	n := copy(target, reader.data[reader.position:end])
	reader.position += n
	return n, nil
}

type partitionReader struct {
	data     []byte
	position int
	random   *rand.Rand
}

func (reader *partitionReader) Read(target []byte) (int, error) {
	if reader.position == len(reader.data) {
		return 0, io.EOF
	}
	size := reader.random.Intn(11) + 1
	if size > len(target) {
		size = len(target)
	}
	if remaining := len(reader.data) - reader.position; size > remaining {
		size = remaining
	}
	n := copy(target[:size], reader.data[reader.position:reader.position+size])
	reader.position += n
	return n, nil
}

func durationPointer(milliseconds int) *time.Duration {
	duration := time.Duration(milliseconds) * time.Millisecond
	return &duration
}

func boundaryName(boundary int) string {
	return "byte-" + itoa(int64(boundary))
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}
