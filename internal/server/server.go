// Package server contém o roteamento e os handlers HTTP do vkdl.link.
package server

import (
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/cdn"
	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/ratelimit"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// Options é a configuração dos handlers.
type Options struct {
	BaseURL            string // origem pública, sem barra no fim
	MaxUpload          int64
	AdminToken         string // vazio = API de admin desativada
	Secret             []byte // segredo do HMAC de IPs
	TrustedProxyHeader string
	TrustedProxies     []netip.Prefix // só bordas nessas faixas podem informar o IP do cliente pelo cabeçalho
	SpoolDir           string         // diretório temporário para receber envios (mesmo disco dos arquivos)
	RateAnon           int
	RateKey            int
	Purger             cdn.Purger // limpa o cache da CDN ao apagar (nil = desligado)
	Now                func() time.Time
	Log                *slog.Logger
}

// Server reúne as dependências dos handlers.
type Server struct {
	opt     Options
	db      *db.DB
	store   store.Store
	janitor *janitor.Janitor
	anonRL  *ratelimit.Limiter
	keyRL   *ratelimit.Limiter
	log     *slog.Logger

	pageOnce sync.Once
	page     []byte // HTML da página inicial, renderizado uma vez

	idMu       sync.Mutex
	pendingIDs map[string]struct{} // ids de envios em andamento (veja claimID)
}

// New cria o Server.
func New(opt Options, d *db.DB, st store.Store, j *janitor.Janitor) *Server {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Server{
		opt:     opt,
		db:      d,
		store:   st,
		janitor: j,
		anonRL:  ratelimit.NewWithClock(opt.RateAnon, opt.Now),
		keyRL:   ratelimit.NewWithClock(opt.RateKey, opt.Now),
		log:     opt.Log,

		pendingIDs: map[string]struct{}{},
	}
}

// Handler devolve o http.Handler com todas as rotas e middlewares.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("POST /v1/upload", s.handleUpload)
	mux.HandleFunc("OPTIONS /v1/upload", s.handleUploadPreflight)
	mux.HandleFunc("DELETE /v1/files/{id}", s.handleDelete)

	mux.Handle("POST /v1/admin/keys", s.admin(s.handleCreateKey))
	mux.Handle("GET /v1/admin/keys", s.admin(s.handleListKeys))
	mux.Handle("DELETE /v1/admin/keys/{id}", s.admin(s.handleRevokeKey))
	mux.Handle("GET /v1/admin/stats", s.admin(s.handleStats))

	// GET também atende HEAD. "/{name}" cobre "/<id>" e "/<id>.<ext>".
	mux.HandleFunc("GET /{name}", s.handleFile)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Rota não encontrada.")
	})
	return s.withRecover(s.withLogging(mux))
}
