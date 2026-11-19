package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// gateRemote é um FS cujo Put pode ficar preso (gate) ou falhar (fails
// primeiras vezes), para simular um bucket lento ou fora do ar.
type gateRemote struct {
	*FS
	mu    sync.Mutex
	gate  chan struct{} // nil = não segura
	fails int
	puts  int
}

func (g *gateRemote) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	g.mu.Lock()
	g.puts++
	gate := g.gate
	fail := g.fails > 0
	if fail {
		g.fails--
	}
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if fail {
		return 0, errors.New("bucket fora do ar")
	}
	return g.FS.Put(ctx, key, r)
}

func (g *gateRemote) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gate != nil {
		close(g.gate)
		g.gate = nil
	}
}

func (g *gateRemote) has(t *testing.T, key string) bool {
	t.Helper()
	_, err := g.FS.Stat(context.Background(), key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

type wbEnv struct {
	t      *testing.T
	remote *gateRemote
	dir    string // fila local
	spool  string
}

func newWBEnv(t *testing.T) *wbEnv {
	t.Helper()
	oldMin, oldMax := writeBackRetryMin, writeBackRetryMax
	writeBackRetryMin, writeBackRetryMax = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { writeBackRetryMin, writeBackRetryMax = oldMin, oldMax })
	root := t.TempDir()
	fs, err := NewFS(filepath.Join(root, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(root, "tmp")
	if err := os.MkdirAll(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	return &wbEnv{t: t, remote: &gateRemote{FS: fs}, dir: filepath.Join(root, "pending"), spool: spool}
}

func (e *wbEnv) open(opt WriteBackOptions) *WriteBack {
	e.t.Helper()
	opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := NewWriteBack(e.remote, e.dir, opt)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// spoolFile grava data num temporário do spool, como o handler de envio faz.
func (e *wbEnv) spoolFile(data []byte) string {
	e.t.Helper()
	f, err := os.CreateTemp(e.spool, "up-*")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		e.t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func waitDrained(t *testing.T, w *WriteBack) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, _ := w.Pending(); n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a fila de envio não esvaziou")
		}
		time.Sleep(time.Millisecond)
	}
}

func readFile(t *testing.T, w *WriteBack, key string) []byte {
	t.Helper()
	f, err := w.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const wbKey = "aB3dE9xQ.png"

func TestWriteBackServesLocalUntilUploaded(t *testing.T) {
	e := newWBEnv(t)
	e.remote.gate = make(chan struct{})
	w := e.open(WriteBackOptions{})
	w.Start(t.Context())

	data := []byte("imagem de verdade")
	src := e.spoolFile(data)
	if err := w.Adopt(context.Background(), wbKey, src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("o temporário do spool devia ter sido movido para a fila")
	}
	// o remoto ainda não tem o arquivo, mas Open e Stat já respondem
	if e.remote.has(t, wbKey) {
		t.Fatal("o envio não devia ter terminado com o remoto travado")
	}
	if got := readFile(t, w, wbKey); !bytes.Equal(got, data) {
		t.Fatalf("Open devolveu %q", got)
	}
	if info, err := w.Stat(context.Background(), wbKey); err != nil || info.Size != int64(len(data)) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if n, b := w.Pending(); n != 1 || b != int64(len(data)) {
		t.Fatalf("Pending = %d, %d", n, b)
	}

	e.remote.release()
	waitDrained(t, w)
	if !e.remote.has(t, wbKey) {
		t.Fatal("o arquivo não chegou ao remoto")
	}
	if _, err := os.Stat(filepath.Join(e.dir, wbKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cópia local devia sumir depois do envio")
	}
	if got := readFile(t, w, wbKey); !bytes.Equal(got, data) {
		t.Fatalf("depois do envio, Open devolveu %q", got)
	}
}

func TestWriteBackRetriesUntilSuccess(t *testing.T) {
	e := newWBEnv(t)
	e.remote.fails = 3
	w := e.open(WriteBackOptions{})
	w.Start(t.Context())
	if err := w.Adopt(context.Background(), wbKey, e.spoolFile([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	waitDrained(t, w)
	if !e.remote.has(t, wbKey) {
		t.Fatal("o arquivo não chegou ao remoto")
	}
	if e.remote.puts != 4 {
		t.Fatalf("Put chamado %d vezes, esperado 4", e.remote.puts)
	}
}

func TestWriteBackDeleteCancelsUpload(t *testing.T) {
	e := newWBEnv(t)
	e.remote.gate = make(chan struct{})
	w := e.open(WriteBackOptions{})
	w.Start(t.Context())
	if err := w.Adopt(context.Background(), wbKey, e.spoolFile([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := w.Delete(context.Background(), wbKey); err != nil {
		t.Fatalf("Delete de um arquivo pendente: %v", err)
	}
	e.remote.release()
	if n, _ := w.Pending(); n != 0 {
		t.Fatal("o item continuou na fila")
	}
	if _, err := os.Stat(filepath.Join(e.dir, wbKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cópia local devia ter sido apagada")
	}
	if e.remote.has(t, wbKey) {
		t.Fatal("o envio cancelado chegou ao remoto")
	}
	if _, err := w.Open(context.Background(), wbKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open depois do Delete: %v", err)
	}
}

func TestWriteBackResumesAfterRestart(t *testing.T) {
	e := newWBEnv(t)
	e.remote.gate = make(chan struct{})
	ctx, stop := context.WithCancel(context.Background())
	w := e.open(WriteBackOptions{})
	w.Start(ctx)
	if err := w.Adopt(context.Background(), wbKey, e.spoolFile([]byte("sobrevive"))); err != nil {
		t.Fatal(err)
	}
	stop() // "o processo caiu" com o envio preso
	e.remote.release()

	w2 := e.open(WriteBackOptions{})
	if n, b := w2.Pending(); n != 1 || b != int64(len("sobrevive")) {
		t.Fatalf("depois de reabrir, Pending = %d, %d", n, b)
	}
	// antes de Start, já serve a cópia local
	if got := readFile(t, w2, wbKey); string(got) != "sobrevive" {
		t.Fatalf("Open devolveu %q", got)
	}
	w2.Start(t.Context())
	waitDrained(t, w2)
	if !e.remote.has(t, wbKey) {
		t.Fatal("o envio não foi retomado")
	}
}

func TestWriteBackFullQueueUploadsNow(t *testing.T) {
	e := newWBEnv(t)
	e.remote.gate = make(chan struct{})
	w := e.open(WriteBackOptions{MaxBytes: 10})
	w.Start(t.Context())
	if err := w.Adopt(context.Background(), "aaaaaaaa.png", e.spoolFile([]byte("12345678"))); err != nil {
		t.Fatal(err)
	}
	// o segundo não cabe na fila: vai na hora, e com o remoto travado o
	// prazo de quem chama é que manda
	src := e.spoolFile([]byte("12345"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.Adopt(ctx, "bbbbbbbb.png", src); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Adopt com a fila cheia: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("no envio na hora, o temporário fica com quem chama")
	}
	if n, _ := w.Pending(); n != 1 {
		t.Fatalf("Pending = %d", n)
	}
	e.remote.release()
	if err := w.Adopt(context.Background(), "cccccccc.png", e.spoolFile([]byte("12345"))); err != nil {
		t.Fatal(err)
	}
	waitDrained(t, w)
}

func TestWriteBackListMergesPending(t *testing.T) {
	e := newWBEnv(t)
	for _, k := range []string{"aaaaaaaa.png", "cccccccc.png"} {
		if _, err := e.remote.FS.Put(context.Background(), k, bytes.NewReader([]byte("remoto"))); err != nil {
			t.Fatal(err)
		}
	}
	e.remote.gate = make(chan struct{})
	w := e.open(WriteBackOptions{})
	w.Start(t.Context())
	for _, k := range []string{"bbbbbbbb.png", "dddddddd.png"} {
		if err := w.Adopt(context.Background(), k, e.spoolFile([]byte("pendente"))); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err := w.List(context.Background(), func(key string, info Info) error {
		got = append(got, key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaaaaaaa.png", "bbbbbbbb.png", "cccccccc.png", "dddddddd.png"}
	if len(got) != len(want) {
		t.Fatalf("List = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List = %v, esperado %v", got, want)
		}
	}
}
