package opencodego

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

const (
	DefaultSSEMaxLineBytes          = 256 << 10
	DefaultSSEMaxEventBytes         = 4 << 20
	DefaultSSEMaxBufferedBytes      = 8 << 20
	DefaultStreamMaxAggregateBytes  = 16 << 20
	DefaultMaxToolCallArgumentBytes = bridge.DefaultMaxToolCallArgumentBytes
	defaultSSEReadBufferBytes       = 32 << 10
)

var (
	ErrSSELimitExceeded = errors.New("sse limit exceeded")
	ErrSSEUnexpectedEOF = errors.New("unexpected end of sse event")
	ErrSSEInvalidUTF8   = errors.New("sse stream is not valid UTF-8")
)

// SSEDecoderOptions bounds allocations controlled by an SSE peer at the SSE
// layer. The limits apply to bytes, not runes, so they remain valid when UTF-8
// is split across reader calls.
type SSEDecoderOptions struct {
	MaxLineBytes     int
	MaxEventBytes    int
	MaxBufferedBytes int
	// MaxAggregateBytes is consumed by the bridge decoder to bound retained
	// state across a complete provider stream. SSEDecoder itself applies the
	// per-line, per-event, and buffered-byte limits above.
	MaxAggregateBytes int
	// MaxToolCallArgumentBytes bounds the accumulated argument string for one
	// provider tool-call index. It is separate from MaxAggregateBytes so one
	// model call cannot consume the entire response budget.
	MaxToolCallArgumentBytes int
	ReadBufferBytes          int
}

func (options SSEDecoderOptions) withDefaults() SSEDecoderOptions {
	if options.MaxLineBytes <= 0 {
		options.MaxLineBytes = DefaultSSEMaxLineBytes
	}
	if options.MaxEventBytes <= 0 {
		options.MaxEventBytes = DefaultSSEMaxEventBytes
	}
	if options.MaxBufferedBytes <= 0 {
		options.MaxBufferedBytes = DefaultSSEMaxBufferedBytes
	}
	if options.MaxAggregateBytes <= 0 {
		options.MaxAggregateBytes = DefaultStreamMaxAggregateBytes
	}
	if options.MaxToolCallArgumentBytes <= 0 {
		options.MaxToolCallArgumentBytes = DefaultMaxToolCallArgumentBytes
	}
	if options.ReadBufferBytes <= 0 {
		options.ReadBufferBytes = defaultSSEReadBufferBytes
	}
	return options
}

// SSEEvent is one dispatched Server-Sent Event. Data lines are joined with a
// newline according to the SSE algorithm. Retry is expressed as a duration;
// malformed retry fields are ignored as required by the wire format.
type SSEEvent struct {
	Event string
	ID    string
	Retry *time.Duration
	Data  string
}

// SSEDecoder incrementally parses SSE without Scanner's implicit token limit.
// It reads through a buffered reader one byte at a time so CR, LF, and CRLF
// boundaries are handled even when the underlying reader returns arbitrary
// chunks. The buffered reader keeps the per-byte parser independent from
// network read boundaries.
type SSEDecoder struct {
	reader  *bufio.Reader
	options SSEDecoderOptions

	data        []byte
	event       string
	id          string
	retry       *time.Duration
	eventBytes  int
	bomHandled  bool
	terminalErr error
}

func NewSSEDecoder(reader io.Reader, options SSEDecoderOptions) *SSEDecoder {
	if reader == nil {
		reader = strings.NewReader("")
	}
	options = options.withDefaults()
	return &SSEDecoder{
		reader:  bufio.NewReaderSize(reader, options.ReadBufferBytes),
		options: options,
	}
}

// Next returns the next dispatched event. An event is dispatched only after a
// blank line terminates it; a stream ending with pending fields is rejected as
// an incomplete event, including a truncated [DONE] marker.
func (decoder *SSEDecoder) Next() (SSEEvent, error) {
	if decoder == nil || decoder.reader == nil {
		return SSEEvent{}, io.EOF
	}
	if decoder.terminalErr != nil {
		return SSEEvent{}, decoder.terminalErr
	}
	for {
		line, terminated, err := decoder.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if decoder.hasPendingEvent() {
					return SSEEvent{}, decoder.rememberError(ErrSSEUnexpectedEOF)
				}
				return SSEEvent{}, io.EOF
			}
			return SSEEvent{}, decoder.rememberError(err)
		}
		if !decoder.bomHandled {
			line = stripLeadingBOM(line)
			decoder.bomHandled = true
		}
		if !utf8.Valid(line) {
			return SSEEvent{}, decoder.rememberError(ErrSSEInvalidUTF8)
		}
		if !terminated {
			if err := decoder.addEventBytes(len(line) + 1); err != nil {
				return SSEEvent{}, decoder.rememberError(err)
			}
			if len(line) != 0 {
				if err := decoder.processLine(line); err != nil {
					return SSEEvent{}, decoder.rememberError(err)
				}
			}
			return SSEEvent{}, decoder.rememberError(ErrSSEUnexpectedEOF)
		}
		if err := decoder.addEventBytes(len(line) + 1); err != nil {
			return SSEEvent{}, decoder.rememberError(err)
		}
		if len(line) == 0 {
			if !decoder.hasPendingEvent() {
				decoder.eventBytes = 0
				continue
			}
			event, err := decoder.dispatch()
			if err != nil {
				return SSEEvent{}, decoder.rememberError(err)
			}
			if event.Data == "" {
				continue
			}
			return event, nil
		}
		if err := decoder.processLine(line); err != nil {
			return SSEEvent{}, decoder.rememberError(err)
		}
	}
}

func stripLeadingBOM(line []byte) []byte {
	if len(line) >= len([]byte{0xef, 0xbb, 0xbf}) && line[0] == 0xef && line[1] == 0xbb && line[2] == 0xbf {
		return line[3:]
	}
	return line
}

func (decoder *SSEDecoder) rememberError(err error) error {
	if err != nil {
		decoder.terminalErr = err
	}
	return err
}

func (decoder *SSEDecoder) readLine() ([]byte, bool, error) {
	lineLimit, limitKind := decoder.lineLimit()
	line := make([]byte, 0, minInt(lineLimit, 1024))
	for {
		value, err := decoder.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) == 0 {
					return nil, false, io.EOF
				}
				return line, false, nil
			}
			return nil, false, err
		}
		switch value {
		case '\n':
			return line, true, nil
		case '\r':
			next, nextErr := decoder.reader.ReadByte()
			if nextErr == nil && next != '\n' {
				if unreadErr := decoder.reader.UnreadByte(); unreadErr != nil {
					return nil, false, unreadErr
				}
			} else if nextErr != nil && !errors.Is(nextErr, io.EOF) {
				return nil, false, nextErr
			}
			return line, true, nil
		default:
			if len(line) >= lineLimit {
				return nil, false, newSSELimitError(limitKind, lineLimit)
			}
			line = append(line, value)
		}
	}
}

func (decoder *SSEDecoder) lineLimit() (int, string) {
	limit := decoder.options.MaxLineBytes
	kind := "line"
	if decoder.options.MaxBufferedBytes < limit {
		limit = decoder.options.MaxBufferedBytes
		kind = "buffer"
	}
	if decoder.options.MaxEventBytes < limit {
		limit = decoder.options.MaxEventBytes
		kind = "event"
	}
	return limit, kind
}

func (decoder *SSEDecoder) addEventBytes(bytes int) error {
	if bytes < 0 || decoder.eventBytes > decoder.options.MaxEventBytes-bytes {
		return newSSELimitError("event", decoder.options.MaxEventBytes)
	}
	decoder.eventBytes += bytes
	if decoder.eventBytes > decoder.options.MaxBufferedBytes {
		return newSSELimitError("buffer", decoder.options.MaxBufferedBytes)
	}
	return nil
}

func (decoder *SSEDecoder) processLine(line []byte) error {
	if len(line) > decoder.options.MaxLineBytes {
		return newSSELimitError("line", decoder.options.MaxLineBytes)
	}
	if len(line) > decoder.options.MaxBufferedBytes {
		return newSSELimitError("buffer", decoder.options.MaxBufferedBytes)
	}
	if line[0] == ':' {
		return nil
	}
	colon := bytes.IndexByte(line, ':')
	field := line
	value := []byte(nil)
	if colon >= 0 {
		field = line[:colon]
		value = line[colon+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	}
	switch string(field) {
	case "data":
		if len(decoder.data) > decoder.options.MaxEventBytes-len(value)-1 {
			return newSSELimitError("event", decoder.options.MaxEventBytes)
		}
		decoder.data = append(decoder.data, value...)
		decoder.data = append(decoder.data, '\n')
	case "event":
		decoder.event = string(value)
	case "id":
		if !containsNUL(value) {
			decoder.id = string(value)
		}
	case "retry":
		if milliseconds, ok := parseRetry(value); ok {
			duration := time.Duration(milliseconds) * time.Millisecond
			decoder.retry = &duration
		}
	}
	return nil
}

func (decoder *SSEDecoder) hasPendingEvent() bool {
	return len(decoder.data) > 0 || decoder.event != "" || decoder.retry != nil
}

func (decoder *SSEDecoder) dispatch() (SSEEvent, error) {
	data := decoder.data
	if len(data) > 0 {
		data = data[:len(data)-1]
	}
	event := SSEEvent{
		Event: decoder.event,
		ID:    decoder.id,
		Retry: cloneDuration(decoder.retry),
		Data:  string(data),
	}
	decoder.data = decoder.data[:0]
	decoder.event = ""
	decoder.retry = nil
	decoder.eventBytes = 0
	return event, nil
}

func parseRetry(value []byte) (int64, bool) {
	if len(value) == 0 {
		return 0, false
	}
	parsed, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	maxDurationMilliseconds := int64((time.Duration(1<<63 - 1)) / time.Millisecond)
	return parsed, parsed <= maxDurationMilliseconds
}

func containsNUL(value []byte) bool {
	for _, byteValue := range value {
		if byteValue == 0 {
			return true
		}
	}
	return false
}

func cloneDuration(value *time.Duration) *time.Duration {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

type sseLimitError struct {
	kind  string
	limit int
}

func newSSELimitError(kind string, limit int) error {
	return &sseLimitError{kind: kind, limit: limit}
}

func (err *sseLimitError) Error() string {
	return fmt.Sprintf("sse %s exceeds configured limit", err.kind)
}

func (err *sseLimitError) Unwrap() error { return ErrSSELimitExceeded }

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
