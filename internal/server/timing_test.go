package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/ident"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// gatedStore esconde o Adopt (o envio passa por Put, como no S3) e deixa o
// teste controlar quanto o Put demora.
type gatedStore struct {
	store.Store
	delay   time.Duration // espera antes de gravar
	hang    bool          // só volta quando o contexto acabar (endpoint mudo)
	entered chan struct{} // recebe um sinal quando Put começa (opcional, com buffer)
	release chan struct{} // Put espera isto fechar antes de gravar (opcional)
}

func (g *gatedStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if g.hang {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if g.delay > 0 {
		select {
		case <-time.After(g.delay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return g.Store.Put(ctx, key, r)
}

// shortBudgets encurta os prazos do envio durante o teste. Precisa ser
// chamada antes de newEnv, para a restauração rodar depois de o servidor
// de teste fechar (t.Cleanup é LIFO).
func shortBudgets(t *testing.T, put, reserve time.Duration) {
	oldPut, oldReserve := putBudget, reserveBudget
	putBudget = func(int64) time.Duration { return put }
	reserveBudget = reserve
	t.Cleanup(func() { putBudget, reserveBudget = oldPut, oldReserve })
}

// holdJanitor ocupa a trava do janitor até o retorno ser chamado.
func holdJanitor(t *testing.T, j *janitor.Janitor) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	go j.Reserve(context.Background(), 0, func(context.Context) error {
		close(held)
		<-done
		return nil
	})
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("não consegui ocupar a trava do janitor")
	}
	var once bool
	release = func() {
		if !once {
			once = true
			close(done)
		}
	}
	t.Cleanup(release)
	return release
}

func (e *env) countObjects() int {
	n := 0
	e.st.List(context.Background(), func(string, store.Info) error { n++; return nil })
	return n
}

func (e *env) countRows() int64 {
	st, err := e.db.Stats(context.Background(), *e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	return st.Files
}

// O WriteTimeout do servidor conta desde o cabeçalho e não cancela o handler:
// um envio cujo PUT passa dele ficava gravado e o cliente recebia uma
// resposta vazia. O handler estende o prazo de escrita para cobrir o envio.
func TestUploadSlowerThanWriteTimeoutStillAnswers(t *testing.T) {
	e := newEnvWith(t, func(s *http.Server) { s.WriteTimeout = 300 * time.Millisecond },
		func(_ *Options, j *janitor.Janitor) { j.Store = &gatedStore{Store: j.Store, delay: time.Second} })
	data := pngBytes(t)
	resp, up := e.upload(data, nil) // sem o ajuste, o cliente recebe EOF aqui
	expectStatus(t, resp, 201)
	if up.ID == "" || up.DeleteToken == "" {
		t.Fatalf("resposta sem id/token: %+v", up)
	}
	got := e.do("GET", path(up.URL), nil, nil)
	expectStatus(t, got, 200)
}

// Um PUT demorado não pode segurar a trava do janitor: DELETE e despejo de
// outros arquivos seguem enquanto ele roda.
func TestSlowPutDoesNotBlockDeleteOrSweep(t *testing.T) {
	gs := &gatedStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) { gs.Store = j.Store; j.Store = gs })
	t.Cleanup(func() {
		select {
		case <-gs.release:
		default:
			close(gs.release)
		}
	})
	ctx := context.Background()
	old := db.File{ID: ident.NewFileID(), Ext: "png", MIME: "image/png", Size: 1, SHA256: "s",
		DeleteTokenHash: ident.HashSecret("token-antigo"), CreatedAt: *e.now}
	if _, err := e.st.Put(ctx, old.Key(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := e.db.InsertFile(ctx, old); err != nil {
		t.Fatal(err)
	}

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		body, ct := multipartBody(t, "file", "imagem.png", pngBytes(t))
		req, _ := http.NewRequest("POST", e.ts.URL+"/v1/upload", body)
		req.Header.Set("Content-Type", ct)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()
	select {
	case <-gs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("o envio não chegou ao Put")
	}

	// com o PUT parado, DELETE e Sweep precisam andar
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(dctx, "DELETE", e.ts.URL+"/v1/files/"+old.ID, nil)
	req.Header.Set("X-Delete-Token", "token-antigo")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE ficou preso atrás do PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	sctx, scancel := context.WithTimeout(ctx, time.Second)
	defer scancel()
	if err := e.jan.Sweep(sctx); err != nil {
		t.Fatalf("Sweep ficou preso atrás do PUT: %v", err)
	}

	close(gs.release)
	select {
	case r := <-done:
		if r.err != nil || r.status != 201 {
			t.Fatalf("envio = %d, %v", r.status, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("o envio não terminou")
	}
}

// Armazenamento mudo: o envio desiste no prazo e o cliente recebe um 503 em
// JSON (antes de o prazo de escrita vencer), sem linha nem objeto sobrando.
func TestStuckStorageAnswers503(t *testing.T) {
	shortBudgets(t, 300*time.Millisecond, time.Second)
	e := newEnvWith(t, func(s *http.Server) { s.WriteTimeout = 200 * time.Millisecond },
		func(_ *Options, j *janitor.Janitor) { j.Store = &gatedStore{Store: j.Store, hang: true} })
	start := time.Now()
	resp, up := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, 503)
	if up.Error != "storage_timeout" {
		t.Fatalf("erro = %q", up.Error)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("demorou %v", d)
	}
	if n := e.countRows(); n != 0 {
		t.Fatalf("%d linha(s) sobraram", n)
	}
	if n := e.countObjects(); n != 0 {
		t.Fatalf("%d objeto(s) sobraram", n)
	}
	spoolEmpty(t, e.spool)
}

// Trava do janitor ocupada além do prazo: 503, e o objeto já enviado é apagado.
func TestJanitorBusyAnswers503(t *testing.T) {
	shortBudgets(t, 5*time.Second, 300*time.Millisecond)
	var gs *gatedStore
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) { gs = &gatedStore{Store: j.Store}; j.Store = gs })
	holdJanitor(t, e.jan)
	resp, up := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, 503)
	if up.Error != "storage_timeout" {
		t.Fatalf("erro = %q", up.Error)
	}
	if n := e.countObjects(); n != 0 {
		t.Fatalf("%d objeto(s) sobraram", n)
	}
}

// Um cliente que desiste enquanto espera a trava não pode ter o envio
// gravado depois: ninguém tem o token de exclusão e o arquivo ocuparia o
// orçamento (e uma nova tentativa viraria duplicata).
func TestUploadAbandonedWhileWaitingIsNotStored(t *testing.T) {
	e := newEnv(t)
	release := holdJanitor(t, e.jan)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	body, ct := multipartBody(t, "file", "imagem.png", pngBytes(t))
	req, _ := http.NewRequestWithContext(ctx, "POST", e.ts.URL+"/v1/upload", body)
	req.Header.Set("Content-Type", ct)
	if resp, err := e.ts.Client().Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("o envio devia esperar a trava, veio %d", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.logs.String(), "envio abandonado pelo cliente") {
		if time.Now().After(deadline) {
			release()
			t.Fatalf("o handler não desistiu ao perder o cliente; logs: %s", e.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	release()
	// dá tempo de um handler (errado) que ainda estivesse na fila gravar
	time.Sleep(100 * time.Millisecond)
	if n := e.countRows(); n != 0 {
		t.Fatalf("o envio abandonado virou %d linha(s)", n)
	}
	if n := e.countObjects(); n != 0 {
		t.Fatalf("o envio abandonado deixou %d objeto(s)", n)
	}
	if strings.Contains(e.logs.String(), `"level":"ERROR"`) {
		t.Fatalf("cliente que desistiu não é erro do servidor: %s", e.logs.String())
	}
}

// Multi-range vira 200 com o corpo inteiro (no S3 seria um GET por faixa);
// uma faixa só continua funcionando.
func TestMultiRangeServesWholeFile(t *testing.T) {
	e := newEnv(t)
	data := pngBytes(t)
	_, up := e.upload(data, nil)
	// poucas faixas: se a soma passasse do tamanho, o próprio ServeContent já ignoraria o Range
	resp := e.do("GET", path(up.URL), nil, map[string]string{"Range": "bytes=0-0,2-2,4-4"})
	expectStatus(t, resp, 200)
	if b := readAll(t, resp.Body); len(b) != len(data) {
		t.Fatalf("corpo com %d bytes, quero %d", len(b), len(data))
	}
	resp = e.do("GET", path(up.URL), nil, map[string]string{"Range": "bytes=1-3"})
	expectStatus(t, resp, 206)
	if b := readAll(t, resp.Body); string(b) != string(data[1:4]) {
		t.Fatalf("faixa única = %q", b)
	}
}
