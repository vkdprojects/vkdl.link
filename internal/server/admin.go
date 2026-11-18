package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/ident"
)

const maxRetentionDays = 36500 // 100 anos

type keyView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	RetentionDays int     `json:"retention_days"`
	CreatedAt     string  `json:"created_at"`
	RevokedAt     *string `json:"revoked_at"`
	Active        bool    `json:"active"`
}

func viewKey(k db.APIKey) keyView {
	v := keyView{ID: k.ID, Name: k.Name, RetentionDays: k.RetentionDays,
		CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339), Active: k.Active()}
	if k.RevokedAt != nil {
		t := k.RevokedAt.UTC().Format(time.RFC3339)
		v.RevokedAt = &t
	}
	return v
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		RetentionDays *int   `json:"retention_days"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "JSON inválido. Esperado {\"name\":\"...\",\"retention_days\":30}.")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 100 {
		writeError(w, http.StatusBadRequest, "bad_request", "\"name\" é obrigatório (até 100 caracteres).")
		return
	}
	days := 0
	if req.RetentionDays != nil {
		days = *req.RetentionDays
	}
	if days < 0 || days > maxRetentionDays {
		writeError(w, http.StatusBadRequest, "bad_request", "\"retention_days\" precisa estar entre 0 (para sempre) e 36500.")
		return
	}
	secret := ident.NewAPIKey()
	k := db.APIKey{ID: ident.NewKeyID(), Name: req.Name, KeyHash: ident.HashSecret(secret),
		RetentionDays: days, CreatedAt: s.opt.Now().UTC()}
	if err := s.db.CreateKey(r.Context(), k); err != nil {
		s.internal(w, "criando chave", err)
		return
	}
	s.log.Info("chave criada", "key_id", k.ID, "retention_days", days)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             k.ID,
		"name":           k.Name,
		"key":            secret, // mostrada só agora; guardamos apenas o hash
		"retention_days": k.RetentionDays,
		"created_at":     k.CreatedAt.Format(time.RFC3339),
	})
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.db.ListKeys(r.Context())
	if err != nil {
		s.internal(w, "listando chaves", err)
		return
	}
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, viewKey(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	err := s.db.RevokeKey(r.Context(), r.PathValue("id"), s.opt.Now().UTC())
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Chave não encontrada.")
		return
	}
	if err != nil {
		s.internal(w, "revogando chave", err)
		return
	}
	s.log.Info("chave revogada", "key_id", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.db.Stats(r.Context(), s.opt.Now())
	if err != nil {
		s.internal(w, "calculando estatísticas", err)
		return
	}
	var diskFree *int64
	if s.janitor.DiskFree != nil {
		if n, err := s.janitor.DiskFree(); err == nil {
			diskFree = &n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files":           st.Files,
		"anon_files":      st.AnonFiles,
		"keyed_files":     st.KeyedFiles,
		"total_bytes":     st.TotalBytes,
		"max_storage":     s.janitor.MaxStorage,
		"min_free_disk":   s.janitor.MinFreeDisk,
		"disk_free":       diskFree,
		"evictable_files": st.EvictableFiles,
		"evictable_bytes": st.EvictableBytes,
		"active_keys":     st.ActiveKeys,
		"revoked_keys":    st.RevokedKeys,
		"max_upload":      s.opt.MaxUpload,
	})
}
