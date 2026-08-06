package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func FuzzErrorSerialization(f *testing.F) {
	f.Add("invalid_request", "body", "The request could not be processed.")
	f.Add("provider-secret-code", "parameter", "Olá, 世界")
	f.Fuzz(func(t *testing.T, code, param, message string) {
		if len(code) > 512 || len(param) > 512 || len(message) > 4096 {
			return
		}
		recorder := httptest.NewRecorder()
		writer := &statusWriter{ResponseWriter: recorder, context: context.Background()}
		writeJSONErrorWithParam(writer, http.StatusBadRequest, code, param, message)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", recorder.Code)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("content type = %q", contentType)
		}
		if !json.Valid(recorder.Body.Bytes()) {
			t.Fatalf("serialized error is invalid JSON: %s", recorder.Body.Bytes())
		}
	})
}
