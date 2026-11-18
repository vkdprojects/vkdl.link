package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/ident"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// fileCSP impede que qualquer coisa servida aqui execute script, mesmo que
// alguém consiga enganar o navegador sobre o tipo.
const fileCSP = "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'"

// handleFile serve GET/HEAD /{id}.{ext} e /{id}. Extensão errada → 404.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id, ext, hasExt := strings.Cut(name, ".")
	if !ident.ValidFileID(id) {
		fileNotFound(w)
		return
	}
	f, err := s.db.GetFile(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) || (err == nil && hasExt && ext != f.Ext) {
		fileNotFound(w)
		return
	}
	if err != nil {
		s.internal(w, "buscando arquivo", err)
		return
	}
	rc, err := s.store.Open(r.Context(), f.Key())
	if errors.Is(err, store.ErrNotFound) {
		fileNotFound(w)
		return
	}
	if err != nil {
		s.internal(w, "abrindo arquivo", err)
		return
	}
	defer rc.Close()

	h := w.Header()
	h.Set("Content-Type", f.MIME)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+f.SHA256+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", fileCSP)
	h.Set("Content-Disposition", `inline; filename="`+f.Key()+`"`)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Access-Control-Allow-Origin", "*")
	// ServeContent cuida de Range, If-None-Match, If-Modified-Since e HEAD.
	http.ServeContent(w, r, "", f.CreatedAt, rc)
}

// fileNotFound responde 404 em texto simples: quem cai aqui costuma ser um
// navegador abrindo um link antigo, não um cliente da API.
func fileNotFound(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("404 - Arquivo não encontrado (pode ter sido removido). / File not found (it may have been removed).\n"))
}

// handleDelete apaga um arquivo com X-Delete-Token ou com o bearer de admin.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ident.ValidFileID(id) {
		writeError(w, http.StatusNotFound, "not_found", "Arquivo não encontrado.")
		return
	}
	f, err := s.db.GetFile(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Arquivo não encontrado.")
		return
	}
	if err != nil {
		s.internal(w, "buscando arquivo", err)
		return
	}
	allowed := s.isAdmin(r)
	if !allowed {
		if tok := strings.TrimSpace(r.Header.Get("X-Delete-Token")); tok != "" {
			allowed = ident.EqualHash(ident.HashSecret(tok), f.DeleteTokenHash)
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden", "Token de exclusão ausente ou inválido.")
		return
	}
	if err := s.janitor.Remove(r.Context(), f); err != nil {
		s.internal(w, "apagando arquivo", err)
		return
	}
	s.log.Info("arquivo apagado", "id", f.ID, "by_admin", s.isAdmin(r))
	if s.opt.Purger != nil {
		go s.purgeCDN(f)
	}
	w.WriteHeader(http.StatusNoContent)
}

// purgeCDN tira o arquivo do cache da CDN (melhor esforço). Sem isso, o
// "Cache-Control: immutable" de um ano deixaria a cópia da borda no ar
// depois de apagado.
func (s *Server) purgeCDN(f db.File) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	urls := []string{s.opt.BaseURL + "/" + f.Key(), s.opt.BaseURL + "/" + f.ID}
	if err := s.opt.Purger.Purge(ctx, urls); err != nil {
		s.log.Warn("falha ao limpar o cache da CDN", "id", f.ID, "err", err)
		return
	}
	s.log.Info("cache da CDN limpo", "id", f.ID)
}
