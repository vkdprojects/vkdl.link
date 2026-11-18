package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	resp := e.do("GET", "/healthz", nil, nil)
	expectStatus(t, resp, http.StatusOK)
	if m := decodeJSON(t, resp.Body); m["status"] != "ok" {
		t.Fatalf("healthz = %v", m)
	}
	e.db.Close()
	expectStatus(t, e.do("GET", "/healthz", nil, nil), http.StatusServiceUnavailable)
}

func TestIndexPage(t *testing.T) {
	e := newEnv(t)
	resp := e.do("GET", "/", nil, nil)
	expectStatus(t, resp, http.StatusOK)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	body := string(readAll(t, resp.Body))
	for _, s := range []string{
		"https://vkdl.test/v1/upload", "1 MB", "AGPL-3.0", "https://github.com/vkdprojects/vkdl.link",
		"prefers-color-scheme", "não têm prazo garantido", "no guaranteed retention", "remoção",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("página sem %q", s)
		}
	}
	// autocontida: nada de recursos externos
	for _, s := range []string{"<script", "<link", "src=\"http"} {
		if strings.Contains(body, s) {
			t.Errorf("página carrega recurso externo: %q", s)
		}
	}
	// pt-BR vem antes do inglês
	if strings.Index(body, "Como enviar") > strings.Index(body, "In English") {
		t.Error("pt-BR devia vir primeiro")
	}
	head := e.do("HEAD", "/", nil, nil)
	expectStatus(t, head, http.StatusOK)
}

func TestUnknownRoutesAreJSON404(t *testing.T) {
	e := newEnv(t)
	for _, rt := range [][2]string{{"GET", "/v1/upload"}, {"GET", "/v1/nada"}, {"POST", "/qualquer/coisa"}, {"PUT", "/aaaaaaaa.png"}, {"GET", "/a/b"}} {
		resp := e.do(rt[0], rt[1], nil, nil)
		expectStatus(t, resp, http.StatusNotFound)
		if m := decodeJSON(t, resp.Body); m["error"] != "not_found" || m["message"] == "" {
			t.Errorf("%s %s: corpo = %v", rt[0], rt[1], m)
		}
	}
}

func TestClientIP(t *testing.T) {
	cf := []netip.Prefix{netip.MustParsePrefix("173.245.48.0/20"), netip.MustParsePrefix("2606:4700::/32")}
	cases := []struct {
		name    string
		headers map[string]string
		remote  string
		want    string
	}{
		// Produção: Cloudflare -> Traefik (rede privada) -> nós. Traefik acrescenta a borda no fim do XFF.
		{"via cloudflare e traefik", map[string]string{"CF-Connecting-IP": "203.0.113.5", "X-Forwarded-For": "203.0.113.5, 173.245.48.10"}, "10.0.0.1:1234", "203.0.113.5"},
		{"cloudflare em ipv6", map[string]string{"CF-Connecting-IP": "2001:db8::7", "X-Forwarded-For": "2606:4700::1"}, "172.18.0.5:80", "2001:db8::7"},
		// Ataque: fala direto com a origem e forja os cabeçalhos. A borda real (último XFF) vence.
		{"cabeçalho forjado direto na origem", map[string]string{"CF-Connecting-IP": "203.0.113.5", "X-Forwarded-For": "173.245.48.10, 198.51.100.66"}, "10.0.0.1:1234", "198.51.100.66"},
		{"forja sem xff", map[string]string{"CF-Connecting-IP": "203.0.113.5"}, "198.51.100.66:4444", "198.51.100.66"},
		{"cabeçalho inválido vindo da cloudflare", map[string]string{"CF-Connecting-IP": "lixo", "X-Forwarded-For": "173.245.48.10"}, "10.0.0.1:1234", "173.245.48.10"},
		{"conexão pública direta", nil, "192.0.2.4:5555", "192.0.2.4"},
		{"ipv6 direto", nil, "[2001:db8::2]:443", "2001:db8::2"},
		{"proxy interno sem xff", nil, "10.0.0.1:1234", "10.0.0.1"},
		{"remote inválido", nil, "nonsense", "unknown"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := clientIP(r, "CF-Connecting-IP", cf); got != c.want {
			t.Errorf("%s: clientIP = %q, quero %q", c.name, got, c.want)
		}
	}
	// Sem faixas confiáveis, o cabeçalho nunca é usado.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "173.245.48.10:1"
	r.Header.Set("CF-Connecting-IP", "203.0.113.5")
	if got := clientIP(r, "CF-Connecting-IP", nil); got != "173.245.48.10" {
		t.Errorf("sem faixas confiáveis: %q", got)
	}
}

func TestRecoverReturnsJSON500(t *testing.T) {
	e := newEnv(t)
	s := &Server{log: e.jan.Log}
	h := s.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Fatalf("recover = %d %s", rec.Code, rec.Body)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{20 << 20: "20 MB", 1 << 30: "1 GB", 4096: "4 KB", 1000: "1000 bytes"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, quero %q", in, got, want)
		}
	}
}
