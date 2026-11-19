// Comando vkdl: servidor do vkdl.link, hospedagem de imagens de código aberto.
package main

import (
	"context"
	"errors"
	"fmt"
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

	spoolDir := filepath.Join(cfg.DataDir, "tmp")
	for _, d := range []string{cfg.DataDir, spoolDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	cleanSpool(log, spoolDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	database, err := db.Open(filepath.Join(cfg.DataDir, "vkdl.db"))
	if err != nil {
		return err
	}
	defer database.Close()

	jan := &janitor.Janitor{
		DB:         database,
		MaxStorage: cfg.MaxStorage,
		Log:        log,
	}
	var st store.Store
	var wb *store.WriteBack
	switch cfg.Storage {
	case config.StorageS3:
		s3, err := openS3(ctx, cfg.S3)
		if err != nil {
			return err
		}
		st = s3
		log.Info("armazenamento: s3", "endpoint", s3.Endpoint(), "bucket", s3.Bucket(), "prefix", s3.Prefix(),
			"ssl", cfg.S3.UseSSL, "path_style", cfg.S3.PathStyle)
		if cfg.MaxStorage <= 0 {
			log.Warn("VKDL_MAX_STORAGE=0 com s3: nada limita o tamanho do bucket")
		}
		if cfg.PendingMax > 0 {
			// o link sai assim que o arquivo está no disco; o envio ao bucket
			// segue em segundo plano (veja store.WriteBack)
			wb, err = store.NewWriteBack(s3, filepath.Join(cfg.DataDir, "pending"), store.WriteBackOptions{
				MaxBytes: cfg.PendingMax,
				Log:      log,
			})
			if err != nil {
				return err
			}
			st = wb
			n, bytes := wb.Pending()
			log.Info("envio ao bucket em segundo plano", "pending_max", cfg.PendingMax, "pending", n, "pending_bytes", bytes)
		} else {
			log.Info("VKDL_PENDING_MAX=0: o envio só responde depois de gravar no bucket")
		}
	default:
		filesDir := filepath.Join(cfg.DataDir, "files")
		fs, err := store.NewFS(filesDir)
		if err != nil {
			return err
		}
		st = fs
		// o disco local só importa quando os arquivos moram nele
		jan.MinFreeDisk = cfg.MinFreeDisk
		jan.DiskFree = janitor.DiskFree(cfg.DataDir)
		log.Info("armazenamento: disco", "dir", filesDir)
	}
	jan.Store = st

	if n, err := jan.Reconcile(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("reconciliação: linhas sem arquivo (ou com arquivo truncado) removidas", "rows", n)
	}
	// depois da reconciliação: os órfãos que ela apagou não precisam ser
	// enviados. Os envios seguem durante o desligamento gracioso do HTTP (o
	// defer roda depois do Shutdown); o que sobrar fica em disco para o
	// próximo boot.
	if wb != nil {
		wbCtx, wbStop := context.WithCancel(context.Background())
		defer wbStop()
		wb.Start(wbCtx)
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
			"storage", cfg.Storage, "max_storage", cfg.MaxStorage, "max_upload", cfg.MaxUpload)
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

// openS3 cria o armazenamento S3 e confere, com prazo curto, se o bucket
// responde. Falhar aqui derruba o boot: melhor que aceitar envios que não
// têm para onde ir. As mensagens nunca incluem as credenciais.
func openS3(ctx context.Context, c config.S3) (*store.S3, error) {
	s3, err := store.NewS3(store.S3Config{
		Endpoint:  c.Endpoint,
		Region:    c.Region,
		Bucket:    c.Bucket,
		KeyID:     c.KeyID,
		KeySecret: c.KeySecret,
		Prefix:    c.Prefix,
		UseSSL:    c.UseSSL,
		PathStyle: store.PathStyle(c.PathStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("configuração do S3 inválida: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3.Check(cctx); err != nil {
		return nil, fmt.Errorf("bucket S3 inacessível (confira S3_ENDPOINT, S3_REGION, S3_BUCKET e as credenciais): %w", err)
	}
	return s3, nil
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
