package opencodego

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSSEDecoderHandlesFieldsCommentsCRLFAndDataJoining(t *testing.T) {
	input := ": keepalive\r\n" +
		"event: message\r\n" +
		"id: 7\r\n" +
		"retry: 1500\r\n" +
		"data: first\r\n" +
		"data: second\r\n\r\n"
	decoder := NewSSEDecoder(strings.NewReader(input), SSEDecoderOptions{})

	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "message" || event.ID != "7" || event.Data != "first\nsecond" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.Retry == nil || *event.Retry != 1500*time.Millisecond {
		t.Fatalf("retry = %#v", event.Retry)
	}
}

func TestSSEDecoderHandlesCRLFWhenTheBoundaryIsSplitAcrossReads(t *testing.T) {
	const input = "data: one\r\ndata: two\r\n\r\ndata: three\r\n\r\n"
	for split := 1; split < len(input); split++ {
		decoder := NewSSEDecoder(&chunkReader{data: []byte(input), split: split}, SSEDecoderOptions{})
		first, err := decoder.Next()
		if err != nil || first.Data != "one\ntwo" {
			t.Fatalf("split %d first = %#v, error = %v", split, first, err)
		}
		second, err := decoder.Next()
		if err != nil || second.Data != "three" {
			t.Fatalf("split %d second = %#v, error = %v", split, second, err)
		}
	}
}

func TestSSEDecoderIsIndependentOfReaderChunkBoundariesAndUTF8(t *testing.T) {
	const input = "data: Olá, 世界\n\ndata: [DONE]\n\n"
	for split := 1; split < len(input); split++ {
		decoder := NewSSEDecoder(&chunkReader{data: []byte(input), split: split}, SSEDecoderOptions{})
		first, err := decoder.Next()
		if err != nil {
			t.Fatalf("split %d: first event: %v", split, err)
		}
		if first.Data != "Olá, 世界" {
			t.Fatalf("split %d: data = %q", split, first.Data)
		}
		second, err := decoder.Next()
		if err != nil {
			t.Fatalf("split %d: second event: %v", split, err)
		}
		if second.Data != "[DONE]" {
			t.Fatalf("split %d: second data = %q", split, second.Data)
		}
	}
}

func TestSSEDecoderEnforcesLineEventAndBufferedLimits(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
		opts  SSEDecoderOptions
	}{
		{name: "line", input: "data: 123456\n\n", opts: SSEDecoderOptions{MaxLineBytes: 5}},
		{name: "event", input: "data: 12345\ndata: 67890\n\n", opts: SSEDecoderOptions{MaxEventBytes: 12}},
		{name: "buffer", input: "data: 12345\n\n", opts: SSEDecoderOptions{MaxBufferedBytes: 4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := NewSSEDecoder(strings.NewReader(test.input), test.opts)
			_, err := decoder.Next()
			if err == nil || !errors.Is(err, ErrSSELimitExceeded) {
				t.Fatalf("error = %v, want ErrSSELimitExceeded", err)
			}
		})
	}
}

func TestSSEDecoderRejectsEOFInAnUnterminatedLine(t *testing.T) {
	decoder := NewSSEDecoder(strings.NewReader("data: incomplete"), SSEDecoderOptions{})
	_, err := decoder.Next()
	if !errors.Is(err, ErrSSEUnexpectedEOF) {
		t.Fatalf("error = %v, want ErrSSEUnexpectedEOF", err)
	}
	_, err = decoder.Next()
	if !errors.Is(err, ErrSSEUnexpectedEOF) {
		t.Fatalf("second error = %v, want ErrSSEUnexpectedEOF", err)
	}
}

func TestSSEDecoderRejectsAnEventWithoutABlankTerminator(t *testing.T) {
	for name, input := range map[string]string{
		"data":    "data: final\n",
		"done":    "data: [DONE]\n",
		"partial": "data: partial",
	} {
		t.Run(name, func(t *testing.T) {
			decoder := NewSSEDecoder(strings.NewReader(input), SSEDecoderOptions{})
			_, err := decoder.Next()
			if !errors.Is(err, ErrSSEUnexpectedEOF) {
				t.Fatalf("error = %v, want ErrSSEUnexpectedEOF", err)
			}
			_, err = decoder.Next()
			if !errors.Is(err, ErrSSEUnexpectedEOF) {
				t.Fatalf("second error = %v, want ErrSSEUnexpectedEOF", err)
			}
		})
	}
}

func TestSSEDecoderStripsExactlyOneLeadingBOM(t *testing.T) {
	const input = "\ufeffdata: first\n\ndata: \ufeffsecond\n\n"
	for split := 1; split < len(input); split++ {
		decoder := NewSSEDecoder(&chunkReader{data: []byte(input), split: split}, SSEDecoderOptions{})
		first, err := decoder.Next()
		if err != nil || first.Data != "first" {
			t.Fatalf("split %d first = %#v, error = %v", split, first, err)
		}
		second, err := decoder.Next()
		if err != nil || second.Data != "\ufeffsecond" {
			t.Fatalf("split %d second = %#v, error = %v", split, second, err)
		}
	}
}

func TestSSEDecoderHandlesAReaderThatReturnsOneSlowByteAtATime(t *testing.T) {
	decoder := NewSSEDecoder(&slowByteReader{data: []byte("data: slow\n\n")}, SSEDecoderOptions{})
	event, err := decoder.Next()
	if err != nil || event.Data != "slow" {
		t.Fatalf("event = %#v, error = %v", event, err)
	}
}

type chunkReader struct {
	data  []byte
	index int
	split int
}

type slowByteReader struct {
	data  []byte
	index int
}

func (reader *slowByteReader) Read(p []byte) (int, error) {
	if reader.index == len(reader.data) {
		return 0, io.EOF
	}
	time.Sleep(time.Microsecond)
	p[0] = reader.data[reader.index]
	reader.index++
	return 1, nil
}

func (reader *chunkReader) Read(p []byte) (int, error) {
	if reader.index == len(reader.data) {
		return 0, io.EOF
	}
	end := reader.index + reader.split
	if end > len(reader.data) {
		end = len(reader.data)
	}
	if end-reader.index > len(p) {
		end = reader.index + len(p)
	}
	n := copy(p, reader.data[reader.index:end])
	reader.index += n
	return n, nil
}
