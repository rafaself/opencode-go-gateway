package server

import (
	"errors"
	"io"
	"net/http"
)

func (s *Server) handleResponses(w *statusWriter, r *http.Request) {
	defer r.Body.Close()
	if r.ContentLength > s.config.MaxBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_entity_too_large", "request body exceeds the configured limit")
		return
	}

	limited := http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes)
	_, err := io.Copy(io.Discard, limited)
	_ = limited.Close()
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_entity_too_large", "request body exceeds the configured limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "request body could not be read")
		return
	}

	writeJSONError(w, http.StatusNotImplemented, "not_implemented", "Responses translation is not implemented yet")
}
