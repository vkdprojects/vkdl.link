package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// orderStore registra se a linha já existia no banco e se a trava do janitor
// estava livre quando o arquivo foi adotado, e pode simular falha na adoção.
type orderStore struct {
	*store.FS
	db         *db.DB
	jan        *janitor.Janitor
	rowAtAdopt bool
	lockFree   bool
	fail       error
}

func (o *orderStore) Adopt(ctx context.Context, key, src string) error {
	id, _, _ := strings.Cut(key, ".")
	o.rowAtAdopt, _ = o.db.FileExists(ctx, id)
	sctx, cancel := context.WithTimeout(ctx, time.Second)
	o.lockFree = o.jan.Sweep(sctx) == nil // com a trava ocupada, Sweep desiste com o prazo
	cancel()
	if o.fail != nil {
		return o.fail
	}
	return o.FS.Adopt(ctx, key, src)
}

func spoolFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "up-teste")
	if err := os.WriteFile(p, pngBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// O arquivo vai ao armazenamento antes da linha e sem a trava do janitor: no
// S3 o PUT leva o tempo da rede, e com a trava cada envio esperaria todos os
// da fila (e DELETE e despejo também). Uma queda no meio deixa um objeto sem
// linha, que a reconciliação do boot apaga.
func TestCommitStoresFileBeforeRowWithoutLock(t *testing.T) {
	e := newEnv(t)
	ost := &orderStore{FS: e.st, db: e.db, jan: e.jan}
	s := New(Options{SpoolDir: e.spool}, e.db, ost, e.jan)
	f := db.File{Ext: "png", MIME: "image/png", Size: int64(len(pngBytes(t))), SHA256: "s", DeleteTokenHash: "d", CreatedAt: time.Now()}
	if err := s.commit(context.Background(), &f, spoolFile(t, e.spool)); err != nil {
		t.Fatal(err)
	}
	if ost.rowAtAdopt {
		t.Fatal("a linha devia ser gravada só depois do arquivo")
	}
	if !ost.lockFree {
		t.Fatal("o arquivo foi enviado com a trava do janitor ocupada")
	}
	if ok, _ := e.db.FileExists(context.Background(), f.ID); !ok {
		t.Fatal("a linha não foi gravada")
	}
	if _, err := e.st.Stat(context.Background(), f.Key()); err != nil {
		t.Fatalf("arquivo não foi adotado: %v", err)
	}
}

func TestCommitRollsBackRowWhenFileFails(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("disco falhou")
	ost := &orderStore{FS: e.st, db: e.db, jan: e.jan, fail: boom}
	s := New(Options{SpoolDir: e.spool}, e.db, ost, e.jan)
	f := db.File{Ext: "png", MIME: "image/png", Size: 10, SHA256: "s", DeleteTokenHash: "d", CreatedAt: time.Now()}
	if err := s.commit(context.Background(), &f, spoolFile(t, e.spool)); !errors.Is(err, boom) {
		t.Fatalf("commit = %v", err)
	}
	if ok, _ := e.db.FileExists(context.Background(), f.ID); ok {
		t.Fatal("linha devia ter sido desfeita")
	}
}

type fakePurger struct{ got chan []string }

func (p *fakePurger) Purge(ctx context.Context, urls []string) error {
	p.got <- urls
	return nil
}

// Apagar precisa tirar o arquivo do cache da CDN, senão a cópia da borda
// (Cache-Control imutável de um ano) continua no ar.
func TestDeletePurgesCDN(t *testing.T) {
	p := &fakePurger{got: make(chan []string, 4)}
	e := newEnv(t, func(o *Options, _ *janitor.Janitor) { o.Purger = p })
	_, up := e.upload(pngBytes(t), nil)
	// token errado: nada de purga
	resp := e.do("DELETE", "/v1/files/"+up.ID, nil, map[string]string{"X-Delete-Token": "errado"})
	expectStatus(t, resp, 403)
	resp = e.do("DELETE", "/v1/files/"+up.ID, nil, map[string]string{"X-Delete-Token": up.DeleteToken})
	expectStatus(t, resp, 204)
	select {
	case urls := <-p.got:
		want := []string{"https://vkdl.test/" + up.ID + ".png", "https://vkdl.test/" + up.ID}
		if !reflect.DeepEqual(urls, want) {
			t.Fatalf("purga = %v, quero %v", urls, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("o cache da CDN não foi limpo")
	}
	select {
	case urls := <-p.got:
		t.Fatalf("purga extra: %v", urls)
	case <-time.After(50 * time.Millisecond):
	}
}
