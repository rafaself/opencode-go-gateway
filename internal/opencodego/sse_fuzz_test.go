package opencodego

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func FuzzSSEDecoderChunkBoundaries(f *testing.F) {
	f.Add([]byte("hello"), uint8(1))
	f.Add([]byte("Olá, 世界"), uint8(3))
	f.Add([]byte("{\"type\":\"response.completed\"}"), uint8(7))
	f.Fuzz(func(t *testing.T, payload []byte, split uint8) {
		if len(payload) > 1<<20 {
			t.Skip()
		}
		if split == 0 {
			split = 1
		}
		stream := make([]byte, 0, len(payload)+16)
		stream = append(stream, "data: "...)
		stream = append(stream, payload...)
		stream = append(stream, '\n', '\n')
		decoder := NewSSEDecoder(&chunkReader{data: stream, split: int(split)}, SSEDecoderOptions{})
		for {
			_, err := decoder.Next()
			if errors.Is(err, io.EOF) || errors.Is(err, ErrSSELimitExceeded) || errors.Is(err, ErrSSEInvalidUTF8) || errors.Is(err, ErrSSEUnexpectedEOF) {
				return
			}
			if err != nil {
				t.Fatalf("unexpected decoder error: %v", err)
			}
		}
	})
}

func FuzzProviderChunkJSON(f *testing.F) {
	f.Add([]byte(`{"id":"chunk","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[]}`))
	f.Add([]byte(`{"error":{"message":"provider failure","type":"server_error","code":"temporary"}}`))
	f.Add([]byte(`{"id":"chunk","choices":[{"index":0,"delta":{"content":"Olá, 世界"},"finish_reason":"stop"}]}`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 64<<10 {
			return
		}
		stream := make([]byte, 0, len(payload)+8)
		stream = append(stream, "data: "...)
		stream = append(stream, payload...)
		stream = append(stream, '\n', '\n')
		decoder := NewChatCompletionStreamDecoder(&chunkReader{data: stream, split: 3}, SSEDecoderOptions{})
		for count := 0; count < 4; count++ {
			_, err := decoder.Next()
			if errors.Is(err, io.EOF) || errors.Is(err, ErrSSELimitExceeded) || errors.Is(err, ErrSSEInvalidUTF8) || errors.Is(err, ErrSSEUnexpectedEOF) || errors.Is(err, ErrMalformedStream) || errors.Is(err, ErrDuplicateStreamTerminal) {
				return
			}
			if err != nil {
				t.Fatalf("unexpected provider decoder error: %v", err)
			}
		}
	})
}

func FuzzFragmentedToolArguments(f *testing.F) {
	f.Add([]byte(`{"query":"one"}`), []byte(`{"query":"two"}`))
	f.Add([]byte("Olá, "), []byte("世界"))
	f.Fuzz(func(t *testing.T, first, second []byte) {
		if len(first) > 16<<10 || len(second) > 16<<10 {
			return
		}
		chunks := []ChatCompletionChunk{
			providerToolChunk("call-fuzz", "exec_command", string(first), nil),
			providerToolChunk("", "", string(second), nil),
		}
		finish := "tool_calls"
		chunks = append(chunks, ChatCompletionChunk{
			ID:      "fuzz-stream",
			Object:  "chat.completion.chunk",
			Created: 1,
			Model:   "deepseek-v4-flash",
			Choices: []ChatCompletionChunkChoice{{Index: 0, FinishReason: &finish}},
		})
		var stream strings.Builder
		for _, chunk := range chunks {
			encoded, err := json.Marshal(chunk)
			if err != nil {
				t.Fatal(err)
			}
			stream.WriteString("data: ")
			stream.Write(encoded)
			stream.WriteString("\n\n")
		}
		stream.WriteString("data: [DONE]\n\n")
		decoder := NewBridgeStreamDecoder(bytes.NewReader([]byte(stream.String())), BridgeStreamDecoderOptions{AllowedToolNames: []string{"exec_command"}})
		for count := 0; count < 32; count++ {
			_, err := decoder.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				t.Fatalf("unexpected bridge decoder error: %v", err)
			}
		}
	})
}

func providerToolChunk(callID, name, arguments string, finishReason *string) ChatCompletionChunk {
	index := 0
	return ChatCompletionChunk{
		ID:      "fuzz-stream",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "deepseek-v4-flash",
		Choices: []ChatCompletionChunkChoice{{
			Index:        0,
			FinishReason: finishReason,
			Delta: ChatMessage{ToolCalls: []ToolCall{{
				Index:    &index,
				ID:       callID,
				Type:     "function",
				Function: ToolCallFunction{Name: name, Arguments: arguments},
			}}},
		}},
	}
}
