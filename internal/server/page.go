package server

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
	"net/http"
	"strings"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// pagePolicy é a CSP da página inicial, sem as fontes de script e estilo. A
// página é autocontida: o único script e o único estilo são os blocos inline,
// liberados pelo hash exato do conteúdo (veja pageCSP). Nada de eval, de
// recursos externos nem de atributos style/on*. O envio e a exclusão falam só
// com a própria origem; a prévia da imagem é um blob: local. Os arquivos
// servidos continuam com a fileCSP, bem mais restrita.
const pagePolicy = "default-src 'none'; img-src 'self' data: blob:; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// renderedPage é a página pronta e a CSP que libera exatamente os seus blocos inline.
type renderedPage struct {
	body []byte
	csp  string
}

func (s *Server) renderPage() renderedPage {
	var buf bytes.Buffer
	err := pageTmpl.Execute(&buf, map[string]any{
		"BaseURL":   s.opt.BaseURL,
		"MaxUpload": humanBytes(s.opt.MaxUpload),
		"MaxBytes":  s.opt.MaxUpload,
	})
	if err != nil {
		panic(err) // template embutido: só falha por erro de programação
	}
	body := buf.Bytes()
	return renderedPage{body: body, csp: pageCSP(body)}
}

// pageCSP monta a CSP com o hash SHA-256 de cada <script> e <style> inline da
// página. Os hashes saem do HTML já renderizado, então qualquer mudança no
// template entra sozinha na política. A página é igual em toda resposta e
// pode ir para cache, o que um nonce por resposta não permitiria.
func pageCSP(page []byte) string {
	return "script-src " + strings.Join(inlineHashes(page, "script"), " ") +
		"; style-src " + strings.Join(inlineHashes(page, "style"), " ") +
		"; " + pagePolicy
}

// inlineHashes devolve 'sha256-…' do conteúdo de cada <tag>…</tag> (sem
// atributos) de page. Sem nenhum bloco, devolve só 'none'.
func inlineHashes(page []byte, tag string) []string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	var out []string
	for {
		i := bytes.Index(page, open)
		if i < 0 {
			break
		}
		page = page[i+len(open):]
		j := bytes.Index(page, end)
		if j < 0 {
			panic("page.html: <" + tag + "> sem fechamento")
		}
		sum := sha256.Sum256(page[:j])
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		page = page[j+len(end):]
	}
	if len(out) == 0 {
		return []string{"'none'"}
	}
	return out
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.pageOnce.Do(func() { s.page = s.renderPage() })
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", s.page.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "public, max-age=300")
	w.Write(s.page.body)
}
