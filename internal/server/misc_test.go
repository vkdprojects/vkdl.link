package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/vkdprojects/vkdl.link/internal/janitor"
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
	body := readAll(t, resp.Body)
	page := string(body)
	for _, s := range []string{
		`<html lang="pt-BR">`, "Compartilhe imagens com", "Share images with",
		"https://vkdl.test/v1/upload", "AGPL-3.0", "https://github.com/vkdprojects/vkdl.link",
		"https://github.com/FabioHAraujo/Recortei", "prefers-color-scheme",
		"não têm prazo garantido", "no guaranteed retention", "mais antigas são apagadas primeiro",
		"Não guardamos o seu IP", "Para desenvolvedores", "X-Delete-Token", `type="file"`,
	} {
		if !strings.Contains(page, s) {
			t.Errorf("página sem %q", s)
		}
	}
	// o limite vem da configuração (1 MB no ambiente de teste), legível e em bytes para o script
	if !strings.Contains(page, "até 1 MB") || !strings.Contains(page, `data-max-bytes="1048576"`) {
		t.Error("página sem o tamanho máximo da configuração")
	}
	// autocontida: nada carregado de fora e nada de HTML montado com dados
	for _, s := range []string{"<script src", "<link rel=\"stylesheet\"", "@import", "url(http", "innerHTML", "eval(", "new Function"} {
		if strings.Contains(page, s) {
			t.Errorf("página contém %q", s)
		}
	}
	// pt-BR vem antes do inglês
	if strings.Index(page, "Compartilhe imagens") > strings.Index(page, "Share images") {
		t.Error("pt-BR devia vir primeiro")
	}

	// CSP: só os blocos inline desta página, pelo hash, sem 'unsafe-inline'
	csp := resp.Header.Get("Content-Security-Policy")
	for _, s := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, s) {
			t.Errorf("CSP sem %q: %s", s, csp)
		}
	}
	if strings.Contains(csp, "unsafe-") {
		t.Errorf("CSP afrouxada: %s", csp)
	}
	for _, tag := range []string{"script", "style"} {
		blocks := regexp.MustCompile(`(?s)<`+tag+`>(.*?)</`+tag+`>`).FindAllSubmatch(body, -1)
		if len(blocks) == 0 {
			t.Fatalf("página sem <%s>", tag)
		}
		var want []string
		for _, b := range blocks {
			sum := sha256.Sum256(b[1])
			want = append(want, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		if d := tag + "-src " + strings.Join(want, " ") + ";"; !strings.Contains(csp, d) {
			t.Errorf("CSP sem %q: %s", d, csp)
		}
	}
	// todo bloco com atributos ficaria fora da política
	if regexp.MustCompile(`<(script|style)\s`).Match(body) {
		t.Error("<script>/<style> com atributos não entram no hash da CSP")
	}
	if regexp.MustCompile(`\son[a-z]+=|\sstyle=`).Match(body) {
		t.Error("atributos on*/style= são bloqueados pela CSP")
	}

	head := e.do("HEAD", "/", nil, nil)
	expectStatus(t, head, http.StatusOK)
	if head.Header.Get("Content-Security-Policy") != csp {
		t.Error("HEAD com CSP diferente do GET")
	}
}

func TestIndexPageMaxUploadFromConfig(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *janitor.Janitor) { o.MaxUpload = 20 << 20 })
	page := string(readAll(t, e.do("GET", "/", nil, nil).Body))
	for _, s := range []string{"até 20 MB", "up to 20 MB", `data-max-bytes="20971520"`} {
		if !strings.Contains(page, s) {
			t.Errorf("página sem %q", s)
		}
	}
}

// Regras de entrada do script da página (o comportamento em si é conferido no navegador):
// o <input type="file"> é limpo a cada escolha, para que repetir o mesmo arquivo depois de
// um erro dispare "change" de novo, e colar/soltar só vale no estado inicial, para que um
// segundo envio não substitua o resultado (e o único link de remoção) sem aviso.
func TestPageScriptInputRules(t *testing.T) {
	change := regexp.MustCompile(`(?s)input\.addEventListener\("change", \(\) => \{(.*?)\n  \}\);`).FindStringSubmatch(pageHTML)
	if change == nil {
		t.Fatal("página sem o tratador de change do input")
	}
	clear, call := strings.Index(change[1], `input.value = "";`), strings.Index(change[1], "upload(")
	if clear < 0 || call < 0 || clear > call {
		t.Errorf("o input devia ser limpo antes do envio: %s", change[1])
	}
	if !strings.Contains(pageHTML, "const canTakeFile = () => !busy && !drop.hidden;") {
		t.Error("colar/soltar devia valer só com o quadro inicial (#drop) visível")
	}
	for _, s := range []string{"canTakeFile()) upload(e.dataTransfer.files[0])", "if (!canTakeFile() || !e.clipboardData) return;"} {
		if !strings.Contains(pageHTML, s) {
			t.Errorf("soltar/colar sem passar por canTakeFile: falta %q", s)
		}
	}
}

func TestInlineHashes(t *testing.T) {
	sum := sha256.Sum256([]byte("a()"))
	h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if got := inlineHashes([]byte("x<script>a()</script>y<script>a()</script>"), "script"); len(got) != 2 || got[0] != h || got[1] != h {
		t.Errorf("inlineHashes = %v", got)
	}
	if got := inlineHashes([]byte("<p>nada</p>"), "style"); len(got) != 1 || got[0] != "'none'" {
		t.Errorf("sem blocos = %v", got)
	}
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
