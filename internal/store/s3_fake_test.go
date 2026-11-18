package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 é um S3 mínimo em memória (estilo caminho, bucket "vkdl") com o
// suficiente para o minio-go: HEAD, GET com Range e ListObjectsV2. Registra o
// Range de cada GET de objeto. Com hang, toda requisição fica sem resposta.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    []string
	hang    chan struct{}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.hang != nil {
		select {
		case <-f.hang:
		case <-r.Context().Done():
		}
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := strings.CutPrefix(r.URL.Path, "/vkdl/")
	if !ok || name == "" {
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r.URL.Query().Get("prefix"))
			return
		}
		http.Error(w, "inesperado: "+r.Method+" "+r.URL.String(), 400)
		return
	}
	data, ok := f.objects[name]
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(404)
		if r.Method != "HEAD" {
			fmt.Fprintf(w, `<Error><Code>NoSuchKey</Code><Message>nao existe</Message><Key>%s</Key></Error>`, name)
		}
		return
	}
	h := w.Header()
	h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	h.Set("ETag", `"0123456789abcdef"`)
	h.Set("Content-Type", "image/png")
	switch r.Method {
	case "HEAD":
		h.Set("Content-Length", strconv.Itoa(len(data)))
	case "GET":
		rng := r.Header.Get("Range")
		f.gets = append(f.gets, rng)
		if rng == "" {
			h.Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data)
			return
		}
		var a, b int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err != nil {
			// "bytes=a-" (aberto até o fim)
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &a); err != nil {
				http.Error(w, "range inesperado "+rng, 400)
				return
			}
			b = len(data) - 1
		}
		b = min(b, len(data)-1)
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		h.Set("Content-Length", strconv.Itoa(b-a+1))
		w.WriteHeader(206)
		w.Write(data[a : b+1])
	default:
		http.Error(w, "método inesperado", 405)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, prefix string) {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>vkdl</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`, prefix, len(keys))
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-01-02T03:04:05.000Z</LastModified><ETag>"x"</ETag><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents>`, k, len(f.objects[k]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(b.String()))
}

func (f *fakeS3) takeGets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.gets
	f.gets = nil
	return g
}

// newFakeS3 sobe o fake e devolve um *S3 apontado para ele. Ajustes nas
// variáveis de prazo precisam vir antes, porque NewS3 as lê.
func newFakeS3(t *testing.T, f *fakeS3) *S3 {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	if f.hang != nil {
		t.Cleanup(func() { close(f.hang) }) // roda antes de srv.Close (LIFO)
	}
	s, err := NewS3(S3Config{
		Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "vkdl",
		KeyID: "id", KeySecret: "segredo", PathStyle: PathStyleTrue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func setVar[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// Cada GET pede no máximo s3ReadWindow bytes: um Range de 1 byte não abre um
// GET até o fim do objeto, e ler tudo anda de janela em janela.
func TestS3ReadsInBoundedWindows(t *testing.T) {
	setVar(t, &s3ReadWindow, 1000)
	data := randBytes(2500)
	fk := &fakeS3{objects: map[string][]byte{"aB/aB3dE9xQ.png": data}}
	s := newFakeS3(t, fk)
	ctx := context.Background()

	f, err := s.Open(ctx, "aB3dE9xQ.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if g := fk.takeGets(); !slices.Equal(g, []string{"bytes=0-999"}) {
		t.Fatalf("GETs = %q", g)
	}

	f, _ = s.Open(ctx, "aB3dE9xQ.png")
	all, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("leitura inteira: %d bytes, %v", len(all), err)
	}
	if g := fk.takeGets(); !slices.Equal(g, []string{"bytes=0-999", "bytes=1000-1999", "bytes=2000-2499"}) {
		t.Fatalf("GETs = %q", g)
	}

	f.Seek(1500, io.SeekStart)
	part := make([]byte, 600)
	if _, err := io.ReadFull(f, part); err != nil || !bytes.Equal(part, data[1500:2100]) {
		t.Fatalf("leitura depois do Seek: %v", err)
	}
	f.Close()
	if g := fk.takeGets(); !slices.Equal(g, []string{"bytes=1500-2499"}) {
		t.Fatalf("GETs = %q", g)
	}
}

// Um GET de faixa única pelo http.ServeContent (como o handler de arquivos)
// faz um GET só, limitado à janela.
func TestS3ServeContentSingleRange(t *testing.T) {
	setVar(t, &s3ReadWindow, 1000)
	data := randBytes(5000)
	fk := &fakeS3{objects: map[string][]byte{"aB/aB3dE9xQ.png": data}}
	s := newFakeS3(t, fk)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := s.Open(r.Context(), "aB3dE9xQ.png")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "image/png") // como o handler de arquivos (sem isso o ServeContent lê 512 bytes para adivinhar)
		http.ServeContent(w, r, "", time.Time{}, f)
	}))
	defer srv.Close()
	for rng, want := range map[string]string{"bytes=0-0": "bytes=0-999", "bytes=4990-": "bytes=4990-4999"} {
		req, _ := http.NewRequest("GET", srv.URL, nil)
		req.Header.Set("Range", rng)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 206 {
			t.Fatalf("%s: status %d", rng, resp.StatusCode)
		}
		if g := fk.takeGets(); !slices.Equal(g, []string{want}) {
			t.Fatalf("%s: GETs = %q, quero %q", rng, g, want)
		}
	}
}

// Endpoint que aceita a conexão e nunca responde: as operações falham em
// segundos (prazo de cabeçalho + poucas tentativas), não em ~10 minutos.
func TestS3UnresponsiveEndpointFailsFast(t *testing.T) {
	setVar(t, &s3HeaderTimeout, 200*time.Millisecond)
	s := newFakeS3(t, &fakeS3{hang: make(chan struct{})})
	ctx := context.Background()
	for name, op := range map[string]func() error{
		"stat":   func() error { _, err := s.Stat(ctx, "aB3dE9xQ.png"); return err },
		"open":   func() error { _, err := s.Open(ctx, "aB3dE9xQ.png"); return err },
		"delete": func() error { return s.Delete(ctx, "aB3dE9xQ.png") },
		"put":    func() error { _, err := s.Put(ctx, "aB3dE9xQ.png", bytes.NewReader([]byte("x"))); return err },
	} {
		errc := make(chan error, 1)
		start := time.Now()
		go func() { errc <- op() }()
		select {
		case err := <-errc:
			if err == nil {
				t.Errorf("%s: devia falhar", name)
			}
			if strings.Contains(err.Error(), "segredo") {
				t.Errorf("%s: erro vazou credencial: %v", name, err)
			}
			t.Logf("%s falhou em %v", name, time.Since(start).Round(time.Millisecond))
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: endpoint mudo segurou a operação por mais de 10 s", name)
		}
	}
}

// O HEAD do Open tem prazo próprio, mesmo com o prazo de cabeçalho padrão.
func TestS3OpenHasDeadline(t *testing.T) {
	setVar(t, &s3OpenTimeout, 300*time.Millisecond)
	s := newFakeS3(t, &fakeS3{hang: make(chan struct{})})
	errc := make(chan error, 1)
	go func() { _, err := s.Open(context.Background(), "aB3dE9xQ.png"); errc <- err }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Open devia falhar")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open sem prazo com o endpoint mudo")
	}
}

func listGoroutines() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*Client).ListObjects")
}

// Parar a listagem no meio não pode deixar goroutine do minio-go presa.
func TestS3ListStopsWithoutLeak(t *testing.T) {
	objs := map[string][]byte{}
	var want []string
	for i := range 6 {
		id := fmt.Sprintf("aB%06d", i)
		objs["aB/"+id+".png"] = []byte("x")
		want = append(want, id+".png")
	}
	objs["lixo.txt"] = []byte("x") // fora do formato: ignorado
	s := newFakeS3(t, &fakeS3{objects: objs})
	ctx := context.Background()

	var got []string
	if err := s.List(ctx, func(k string, _ Info) error { got = append(got, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("List = %v, quero %v", got, want)
	}

	before := listGoroutines()
	stop := errors.New("pare")
	for range 5 {
		if err := s.List(ctx, func(string, Info) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("List = %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for listGoroutines() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines de listagem presas: %d", listGoroutines()-before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
