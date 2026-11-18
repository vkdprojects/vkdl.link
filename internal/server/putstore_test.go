package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// putOnlyStore esconde o Adopt do FS: o envio passa por Put, como no S3.
// Com fail, Put grava o objeto e mesmo assim devolve erro (o caso de um PUT
// que chegou ao bucket mas cuja resposta se perdeu).
type putOnlyStore struct {
	store.Store
	fail error
	puts int
}

func (p *putOnlyStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	p.puts++
	if _, ok := r.(*os.File); !ok {
		return 0, errors.New("Put devia receber o *os.File do spool (tamanho conhecido, PUT único)")
	}
	n, err := p.Store.Put(ctx, key, r)
	if p.fail != nil {
		return 0, p.fail
	}
	return n, err
}

func spoolEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("o temporário do spool ficou para trás: %v", entries)
	}
}

func TestUploadThroughPutRemovesSpool(t *testing.T) {
	var ps *putOnlyStore
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) {
		ps = &putOnlyStore{Store: j.Store}
		j.Store = ps
	})
	data := pngBytes(t)
	resp, up := e.upload(data, nil)
	expectStatus(t, resp, 201)
	if ps.puts != 1 {
		t.Fatalf("Put chamado %d vezes", ps.puts)
	}
	spoolEmpty(t, e.spool)
	got := e.do("GET", path(up.URL), nil, nil)
	expectStatus(t, got, 200)
	if b := readAll(t, got.Body); !bytes.Equal(b, data) {
		t.Fatal("conteúdo servido difere do enviado")
	}
}

func TestUploadPutFailureRollsBack(t *testing.T) {
	var ps *putOnlyStore
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) {
		ps = &putOnlyStore{Store: j.Store, fail: errors.New("bucket fora do ar")}
		j.Store = ps
	})
	resp, _ := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, 500)
	spoolEmpty(t, e.spool)
	st, err := e.db.Stats(context.Background(), *e.now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 0 || st.TotalBytes != 0 {
		t.Fatalf("a linha do envio que falhou continuou no banco: %+v", st)
	}
	// o objeto que chegou a ser gravado também foi apagado
	n := 0
	e.st.List(context.Background(), func(string, store.Info) error { n++; return nil })
	if n != 0 {
		t.Fatalf("sobrou %d objeto(s) do envio que falhou", n)
	}
}
