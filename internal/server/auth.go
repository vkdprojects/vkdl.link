package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/ident"
)

// bearer extrai o token de "Authorization: Bearer <token>".
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// isAdmin compara o bearer com VKDL_ADMIN_TOKEN em tempo constante.
func (s *Server) isAdmin(r *http.Request) bool {
	if s.opt.AdminToken == "" {
		return false
	}
	tok, ok := bearer(r)
	return ok && ident.EqualSecret(tok, s.opt.AdminToken)
}

var errBadKey = errors.New("chave de API inválida ou revogada")

// apiKey resolve a chave de API do envio. Sem Authorization → (nil, nil).
// Com Authorization inválido → errBadKey (nunca cai para anônimo em silêncio).
func (s *Server) apiKey(r *http.Request) (*db.APIKey, error) {
	if r.Header.Get("Authorization") == "" {
		return nil, nil
	}
	tok, ok := bearer(r)
	if !ok || !strings.HasPrefix(tok, ident.APIKeyPrefix) {
		return nil, errBadKey
	}
	k, err := s.db.KeyByHash(r.Context(), ident.HashSecret(tok))
	if errors.Is(err, db.ErrNotFound) || (err == nil && !k.Active()) {
		return nil, errBadKey
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// admin protege as rotas /v1/admin/*: desativado → 404; token errado → 401.
func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opt.AdminToken == "" {
			writeError(w, http.StatusNotFound, "not_found", "Rota não encontrada.")
			return
		}
		if !s.isAdmin(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vkdl-admin"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "Token de administrador ausente ou inválido.")
			return
		}
		next(w, r)
	})
}
