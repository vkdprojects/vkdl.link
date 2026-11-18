package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/ident"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
)

func TestUploadEachFormat(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, ext, mime string
		data            []byte
	}{
		{"png", "png", "image/png", pngBytes(t)},
		{"jpeg", "jpg", "image/jpeg", jpegBytes(t)},
		{"gif", "gif", "image/gif", gifBytes(t)},
		{"webp", "webp", "image/webp", webpBytes()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, r := e.upload(c.data, nil)
			expectStatus(t, resp, http.StatusCreated)
			if !ident.ValidFileID(r.ID) {
				t.Fatalf("id inválido %q", r.ID)
			}
			if r.URL != "https://vkdl.test/"+r.ID+"."+c.ext || r.DeleteURL != "https://vkdl.test/v1/files/"+r.ID {
				t.Fatalf("urls erradas: %+v", r)
			}
			if r.MIME != c.mime || r.Size != int64(len(c.data)) || r.DeleteToken == "" || r.ExpiresAt != nil {
				t.Fatalf("resposta errada: %+v", r)
			}
			if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
				t.Error("upload sem CORS")
			}
			got := e.do("GET", path(r.URL), nil, nil)
			expectStatus(t, got, http.StatusOK)
			if !bytes.Equal(readAll(t, got.Body), c.data) {
				t.Fatal("conteúdo servido difere do enviado")
			}
			if got.Header.Get("Content-Type") != c.mime {
				t.Errorf("Content-Type = %q", got.Header.Get("Content-Type"))
			}
		})
	}
}

func TestUploadRawBody(t *testing.T) {
	e := newEnv(t)
	data := pngBytes(t)
	// Content-Type mentiroso: o formato vem dos bytes mágicos
	resp := e.do("POST", "/v1/upload", bytes.NewReader(data), map[string]string{"Content-Type": "text/plain"})
	expectStatus(t, resp, http.StatusCreated)
	m := decodeJSON(t, resp.Body)
	if m["mime"] != "image/png" || !strings.HasSuffix(m["url"].(string), ".png") {
		t.Fatalf("resposta = %v", m)
	}
	if resp.Header.Get("Location") != m["url"] {
		t.Error("Location devia apontar para a url")
	}
}

func TestUploadRejectsNonImages(t *testing.T) {
	e := newEnv(t)
	bodies := map[string][]byte{
		"svg":      []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect/></svg>`),
		"svg-xml":  []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`),
		"text":     []byte("só um texto qualquer"),
		"html":     []byte("<!doctype html><script>alert(1)</script>"),
		"pdf":      []byte("%PDF-1.7\n"),
		"riffwave": []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
	}
	for name, data := range bodies {
		t.Run(name, func(t *testing.T) {
			// nome .png e Content-Type image/png não enganam o servidor
			body, ct := multipartBody(t, "file", "foto.png", data)
			resp := e.do("POST", "/v1/upload", body, map[string]string{"Content-Type": ct})
			expectStatus(t, resp, http.StatusUnsupportedMediaType)
			if m := decodeJSON(t, resp.Body); m["error"] != "unsupported_type" || m["message"] == "" {
				t.Fatalf("erro = %v", m)
			}
			raw := e.do("POST", "/v1/upload", bytes.NewReader(data), map[string]string{"Content-Type": "image/png"})
			expectStatus(t, raw, http.StatusUnsupportedMediaType)
		})
	}
	if n, _ := e.db.TotalBytes(t.Context()); n != 0 {
		t.Fatalf("nada devia ter sido gravado; total=%d", n)
	}
	assertSpoolEmpty(t, e)
}

func TestUploadNoFile(t *testing.T) {
	e := newEnv(t)
	body, ct := multipartBody(t, "", "", nil) // só o campo extra
	resp := e.do("POST", "/v1/upload", body, map[string]string{"Content-Type": ct})
	expectStatus(t, resp, http.StatusBadRequest)
	if m := decodeJSON(t, resp.Body); m["error"] != "no_file" {
		t.Fatalf("erro = %v", m)
	}
	body, ct = multipartBody(t, "outro", "x.png", pngBytes(t)) // campo com outro nome
	expectStatus(t, e.do("POST", "/v1/upload", body, map[string]string{"Content-Type": ct}), http.StatusBadRequest)
	expectStatus(t, e.do("POST", "/v1/upload", nil, nil), http.StatusBadRequest) // corpo vazio
	body, ct = multipartBody(t, "file", "vazio.png", nil)                        // arquivo vazio
	expectStatus(t, e.do("POST", "/v1/upload", body, map[string]string{"Content-Type": ct}), http.StatusBadRequest)
	broken := strings.NewReader("--xyz\r\nsem fim")
	expectStatus(t, e.do("POST", "/v1/upload", broken, map[string]string{"Content-Type": "multipart/form-data; boundary=xyz"}), http.StatusBadRequest)
}

func TestUploadSizeLimit(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *janitor.Janitor) { o.MaxUpload = 4096 })
	// exatamente no limite passa
	resp, _ := e.upload(bigPNG(t, 4096), nil)
	expectStatus(t, resp, http.StatusCreated)
	// um byte a mais: 413 (multipart)
	resp, r := e.upload(bigPNG(t, 4097), nil)
	expectStatus(t, resp, http.StatusRequestEntityTooLarge)
	if r.Error != "too_large" {
		t.Fatalf("erro = %+v", r)
	}
	// corpo cru com Content-Length
	expectStatus(t, e.do("POST", "/v1/upload", bytes.NewReader(bigPNG(t, 5000)), nil), http.StatusRequestEntityTooLarge)
	// corpo cru sem Content-Length (chunked) e muito maior que a folga
	pr, pw := io.Pipe()
	go func() {
		pw.Write(pngBytes(t))
		pw.Write(bytes.Repeat([]byte{0}, 3<<20))
		pw.Close()
	}()
	expectStatus(t, e.do("POST", "/v1/upload", pr, nil), http.StatusRequestEntityTooLarge)
	// Content-Length declarado enorme: rejeitado antes de ler
	req, _ := http.NewRequest("POST", e.ts.URL+"/v1/upload", bytes.NewReader(pngBytes(t)))
	req.ContentLength = 50 << 20
	if resp, err := e.ts.Client().Do(req); err == nil {
		expectStatus(t, resp, http.StatusRequestEntityTooLarge)
		resp.Body.Close()
	}
	if n, _ := e.db.TotalBytes(t.Context()); n != 4096 {
		t.Fatalf("só o primeiro envio devia existir; total=%d", n)
	}
	assertSpoolEmpty(t, e)
}

func assertSpoolEmpty(t *testing.T, e *env) {
	t.Helper()
	entries, err := os.ReadDir(e.spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporários esquecidos no spool: %d", len(entries))
	}
}

func TestUploadStorageFull(t *testing.T) {
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) { j.MaxStorage = 100 })
	resp, r := e.upload(bigPNG(t, 200), nil)
	expectStatus(t, resp, http.StatusInsufficientStorage)
	if r.Error != "storage_full" {
		t.Fatalf("erro = %+v", r)
	}
	assertSpoolEmpty(t, e)
}

func TestUploadEvictsOldestAnonFirst(t *testing.T) {
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) { j.MaxStorage = 3000 })
	key := e.createKey(t, "cliente", 30)
	auth := map[string]string{"Authorization": "Bearer " + key}

	step := func() { *e.now = e.now.Add(time.Second) }
	resp, keyed := e.upload(bigPNG(t, 1000), auth) // o mais antigo, mas protegido
	expectStatus(t, resp, http.StatusCreated)
	step()
	_, anon1 := e.upload(bigPNG(t, 1000), nil)
	step()
	_, anon2 := e.upload(bigPNG(t, 1000), nil)
	step()
	// 3000 usados; mais 1000 → precisa despejar 1 arquivo: o anônimo mais antigo
	resp, anon3 := e.upload(bigPNG(t, 1000), nil)
	expectStatus(t, resp, http.StatusCreated)

	for _, c := range []struct {
		r    uploadResult
		want int
	}{{keyed, 200}, {anon1, 404}, {anon2, 200}, {anon3, 200}} {
		if got := e.do("GET", path(c.r.URL), nil, nil).StatusCode; got != c.want {
			t.Errorf("%s: status %d, quero %d", c.r.ID, got, c.want)
		}
	}
	// quando a retenção da chave vence, ela vira despejável como as outras
	*e.now = e.now.Add(31 * 24 * time.Hour)
	_, anon4 := e.upload(bigPNG(t, 1000), nil)
	if got := e.do("GET", path(keyed.URL), nil, nil).StatusCode; got != 404 {
		t.Errorf("arquivo com retenção vencida devia ser o primeiro despejado; status %d", got)
	}
	if got := e.do("GET", path(anon4.URL), nil, nil).StatusCode; got != 200 {
		t.Errorf("novo envio devia existir; status %d", got)
	}
}

func TestUploadRateLimit(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *janitor.Janitor) { o.RateAnon = 2; o.RateKey = 3 })
	ip := map[string]string{"CF-Connecting-IP": "203.0.113.7"}
	for range 2 {
		resp, _ := e.upload(pngBytes(t), ip)
		expectStatus(t, resp, http.StatusCreated)
	}
	resp, r := e.upload(pngBytes(t), ip)
	expectStatus(t, resp, http.StatusTooManyRequests)
	if r.Error != "rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("429 sem Retry-After ou código: %+v %v", r, resp.Header)
	}
	if resp.Header.Get("Access-Control-Expose-Headers") != "Retry-After" {
		t.Error("Retry-After devia ser exposto via CORS")
	}
	// outro IP tem a própria cota
	resp, _ = e.upload(pngBytes(t), map[string]string{"CF-Connecting-IP": "198.51.100.1"})
	expectStatus(t, resp, http.StatusCreated)
	// Quem não vem da Cloudflare é limitado pela borda real (último salto do X-Forwarded-For,
	// escrito pelo proxy), mesmo trocando o CF-Connecting-IP e o início do XFF a cada envio.
	for i := range 2 {
		resp, _ = e.upload(pngBytes(t), map[string]string{
			"CF-Connecting-IP": fmt.Sprintf("203.0.113.%d", 100+i),
			"X-Forwarded-For":  fmt.Sprintf("192.0.2.%d, 198.51.100.9", 10+i),
		})
		expectStatus(t, resp, http.StatusCreated)
	}
	resp, _ = e.upload(pngBytes(t), map[string]string{"CF-Connecting-IP": "203.0.113.200", "X-Forwarded-For": "192.0.2.99, 198.51.100.9"})
	expectStatus(t, resp, http.StatusTooManyRequests)
	// chave de API tem limite próprio, mais alto
	auth := map[string]string{"Authorization": "Bearer " + e.createKey(t, "k", 0), "CF-Connecting-IP": "203.0.113.7"}
	for range 3 {
		resp, _ = e.upload(pngBytes(t), auth)
		expectStatus(t, resp, http.StatusCreated)
	}
	resp, _ = e.upload(pngBytes(t), auth)
	expectStatus(t, resp, http.StatusTooManyRequests)
	// a cota volta com o tempo
	*e.now = e.now.Add(time.Minute)
	resp, _ = e.upload(pngBytes(t), ip)
	expectStatus(t, resp, http.StatusCreated)
}

func TestUploadPreflight(t *testing.T) {
	e := newEnv(t)
	resp := e.do("OPTIONS", "/v1/upload", nil, map[string]string{
		"Origin": "https://exemplo.com", "Access-Control-Request-Method": "POST"})
	expectStatus(t, resp, http.StatusNoContent)
	h := resp.Header
	if h.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(h.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("cabeçalhos CORS = %v", h)
	}
}

func TestLogsHaveNoIPsOrTokens(t *testing.T) {
	e := newEnv(t)
	ip := "203.0.113.99"
	resp, r := e.upload(pngBytes(t), map[string]string{"CF-Connecting-IP": ip})
	expectStatus(t, resp, http.StatusCreated)
	f, err := e.db.GetFile(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.IPHash != ident.HashIP(ip, []byte("segredo")) || strings.Contains(f.IPHash, ip) {
		t.Fatalf("banco devia ter só o HMAC do IP; tem %q", f.IPHash)
	}
	if f.DeleteTokenHash == r.DeleteToken {
		t.Fatal("token de exclusão gravado em claro")
	}
	expectStatus(t, e.do("DELETE", "/v1/files/"+r.ID, nil, map[string]string{"X-Delete-Token": r.DeleteToken}), http.StatusNoContent)
	logs := e.logs.String()
	if !strings.Contains(logs, `"path":"/v1/upload"`) || !strings.Contains(logs, `"ip_hash":"`+f.IPHash+`"`) ||
		!strings.Contains(logs, `"status":201`) || !strings.Contains(logs, `"duration_ms":`) || !strings.Contains(logs, `"bytes":`) {
		t.Fatalf("linha de log incompleta: %s", logs)
	}
	for _, secret := range []string{ip, r.DeleteToken} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log vazou %q", secret)
		}
	}
}

func TestConcurrentUploadsRespectBudget(t *testing.T) {
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) { j.MaxStorage = 5000 })
	data := bigPNG(t, 1000)
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for range 20 {
		wg.Go(func() {
			body, ct := multipartBody(t, "file", "x.png", data)
			req, _ := http.NewRequest("POST", e.ts.URL+"/v1/upload", body)
			req.Header.Set("Content-Type", ct)
			resp, err := e.ts.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		})
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusCreated {
			t.Errorf("status %d, quero 201 (sempre há anônimos antigos para despejar)", c)
		}
	}
	if n, _ := e.db.TotalBytes(t.Context()); n > 5000 {
		t.Fatalf("orçamento estourado: %d", n)
	}
	assertSpoolEmpty(t, e)
}
