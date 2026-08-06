package opencodego

import (
	"errors"
	"io"
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
