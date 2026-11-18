package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

const testAdmin = "admin-token-de-teste"

// syncBuffer é um bytes.Buffer seguro para goroutines (logs do servidor).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t     *testing.T
	ts    *httptest.Server
	db    *db.DB
	st    *store.FS
	jan   *janitor.Janitor
	logs  *syncBuffer
	now   *time.Time
	spool string
}

type envOpt func(*Options, *janitor.Janitor)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "vkdl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, _ := store.NewFS(filepath.Join(dir, "files"))
	spool := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	e := &env{t: t, db: d, st: st, logs: &syncBuffer{}, now: &now, spool: spool}
	clock := func() time.Time { return *e.now }
	log := slog.New(slog.NewJSONHandler(e.logs, nil))
	e.jan = &janitor.Janitor{DB: d, Store: st, MaxStorage: 1 << 30, Now: clock, Log: log}
	o := Options{
		BaseURL:            "https://vkdl.test",
		MaxUpload:          1 << 20,
		AdminToken:         testAdmin,
		Secret:             []byte("segredo"),
		TrustedProxyHeader: "CF-Connecting-IP",
		// O cliente de teste conecta por loopback: nos testes, ele faz o papel da borda da Cloudflare.
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
		SpoolDir:       spool,
		RateAnon:       1000,
		RateKey:        1000,
		Now:            clock,
		Log:            log,
	}
	for _, fn := range opts {
		fn(&o, e.jan)
	}
	e.ts = httptest.NewServer(New(o, d, st, e.jan).Handler())
	t.Cleanup(e.ts.Close)
	return e
}

// --- geração de imagens reais ---

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for x := range 4 {
		for y := range 3 {
			img.Set(x, y, color.RGBA{uint8(60 * x), uint8(80 * y), 200, 255})
		}
	}
	return img
}

func pngBytes(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, testImage()); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gifBytes(t *testing.T) []byte {
	var b bytes.Buffer
	if err := gif.Encode(&b, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// webpBytes é um WebP lossless 1x1 válido (a stdlib não tem codificador WebP).
func webpBytes() []byte {
	return []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")
}

// bigPNG devolve um PNG válido no começo, completado até n bytes.
func bigPNG(t *testing.T, n int) []byte {
	b := pngBytes(t)
	return append(b, bytes.Repeat([]byte{0}, n-len(b))...)
}

// --- requisições ---

type uploadResult struct {
	ID          string  `json:"id"`
	URL         string  `json:"url"`
	DeleteURL   string  `json:"delete_url"`
	DeleteToken string  `json:"delete_token"`
	Size        int64   `json:"size"`
	MIME        string  `json:"mime"`
	ExpiresAt   *string `json:"expires_at"`
	Error       string  `json:"error"`
	Message     string  `json:"message"`
}

func multipartBody(t *testing.T, field, filename string, data []byte) (io.Reader, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("descricao", "campo extra que deve ser ignorado")
	if field != "" {
		fw, err := mw.CreateFormFile(field, filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(data)
	}
	mw.Close()
	return &b, mw.FormDataContentType()
}

func (e *env) do(method, path string, body io.Reader, headers map[string]string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.ts.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// upload envia data em multipart (campo "file") e decodifica a resposta.
func (e *env) upload(data []byte, headers map[string]string) (*http.Response, uploadResult) {
	e.t.Helper()
	body, ct := multipartBody(e.t, "file", "imagem.png", data)
	h := map[string]string{"Content-Type": ct}
	for k, v := range headers {
		h[k] = v
	}
	resp := e.do("POST", "/v1/upload", body, h)
	var r uploadResult
	json.NewDecoder(resp.Body).Decode(&r)
	return resp, r
}

// path converte a URL pública (https://vkdl.test/...) no caminho local.
func path(publicURL string) string { return strings.TrimPrefix(publicURL, "https://vkdl.test") }

func readAll(t *testing.T, r io.Reader) []byte {
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeJSON(t *testing.T, r io.Reader) map[string]any {
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status %d, quero %d; corpo: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, b)
	}
}

// createKey cria uma chave de API pela API de admin e devolve o segredo.
func (e *env) createKey(t *testing.T, name string, days int) string {
	t.Helper()
	body := strings.NewReader(`{"name":"` + name + `","retention_days":` + strconv.Itoa(days) + `}`)
	resp := e.do("POST", "/v1/admin/keys", body, map[string]string{"Authorization": "Bearer " + testAdmin})
	expectStatus(t, resp, http.StatusCreated)
	m := decodeJSON(t, resp.Body)
	return m["key"].(string)
}
