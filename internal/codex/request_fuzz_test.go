package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func FuzzResponsesRequestDecoder(f *testing.F) {
	f.Add([]byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":"hello"}],"stream":true}`))
	f.Add([]byte(`{"model":"gpt-5.3-codex","input":[{"type":"function_call_output","call_id":"call","output":""}],"stream":true}`))
	f.Add([]byte(`{"model":"gpt-5.3-codex","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 128<<10 {
			return
		}
		decoder, err := NewDecoder(128 << 10)
		if err != nil {
			t.Fatal(err)
		}
		request, decodeErr := decoder.Decode(bytes.NewReader(body), "application/json")
		if decodeErr == nil {
			if request.Model == "" || !request.Generation.Stream {
				t.Fatalf("successful decode lost required request semantics: %#v", request)
			}
			return
		}
		var boundaryErr *Error
		if !errors.As(decodeErr, &boundaryErr) {
			return
		}
		encoded, marshalErr := json.Marshal(boundaryErr)
		if marshalErr != nil || !json.Valid(encoded) {
			t.Fatalf("boundary error is not serializable: err=%v payload=%s", marshalErr, encoded)
		}
	})
}
