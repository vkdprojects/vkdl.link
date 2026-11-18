// Comando vkdl: servidor do vkdl.link, hospedagem de imagens de código aberto.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/cdn"
	"github.com/vkdprojects/vkdl.link/internal/config"
	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/server"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("encerrando com erro", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if cfg.SecretGenerated {
		log.Warn("VKDL_SECRET vazio: usando um segredo aleatório; os hashes de IP mudam a cada reinício")
	}
	if cfg.AdminToken == "" {
		log.Info("VKDL_ADMIN_TOKEN vazio: API de admin desativada")
	}

	filesDir := filepath.Join(cfg.DataDir, "files")
	spoolDir := filepath.Join(cfg.DataDir, "tmp")
	for _, d := range []string{cfg.DataDir, filesDir, spoolDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	cleanSpool(log, spoolDir)

	database, err := db.Open(filepath.Join(cfg.DataDir, "vkdl.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	st, err := store.NewFS(filesDir)
	if err != nil {
		return err
	}

	jan := &janitor.Janitor{
		DB:          database,
		Store:       st,
		MaxStorage:  cfg.MaxStorage,
		MinFreeDisk: cfg.MinFreeDisk,
		DiskFree:    janitor.DiskFree(cfg.DataDir),
		Log:         log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if n, err := jan.Reconcile(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("reconciliação: linhas sem arquivo (ou com arquivo truncado) removidas", "rows", n)
	}
	if err := jan.Sweep(ctx); err != nil {
		log.Error("despejo inicial falhou", "err", err)
	}
	go jan.Run(ctx, 60*time.Second)

	var purger cdn.Purger
	if cfg.CFZoneID != "" {
		purger = &cdn.Cloudflare{ZoneID: cfg.CFZoneID, Token: cfg.CFAPIToken}
		log.Info("purga de cache da Cloudflare ativada ao apagar arquivos")
	}

	srv := server.New(server.Options{
		BaseURL:            cfg.BaseURL,
		MaxUpload:          cfg.MaxUpload,
		AdminToken:         cfg.AdminToken,
		Secret:             cfg.Secret,
		TrustedProxyHeader: cfg.TrustedProxyHeader,
		TrustedProxies:     cfg.TrustedProxies,
		SpoolDir:           spoolDir,
		RateAnon:           cfg.RateAnon,
		RateKey:            cfg.RateKey,
		Purger:             purger,
		Log:                log,
	}, database, st, jan)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("ouvindo", "addr", cfg.Listen, "base_url", cfg.BaseURL, "data_dir", cfg.DataDir,
			"max_storage", cfg.MaxStorage, "max_upload", cfg.MaxUpload)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("sinal recebido, encerrando")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// cleanSpool apaga envios temporários deixados por um processo que morreu no meio.
func cleanSpool(log *slog.Logger, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warn("não foi possível limpar o diretório temporário", "err", err)
		return
	}
	for _, e := range entries {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
