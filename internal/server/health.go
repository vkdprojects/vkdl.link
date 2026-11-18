package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.log.Error("healthz: banco indisponível", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Banco de dados indisponível.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
