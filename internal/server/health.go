package server

import (
	"net/http"
)

func (s *Server) handleLive(w *statusWriter) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w *statusWriter) {
	if !s.ready.Load() {
		writeJSONError(w, http.StatusServiceUnavailable, "not_ready", "server is shutting down")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
