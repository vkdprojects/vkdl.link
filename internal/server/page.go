package server

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// pageCSP: a página é autocontida (sem assets externos), só CSS inline.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) renderPage() []byte {
	var buf bytes.Buffer
	err := pageTmpl.Execute(&buf, map[string]string{
		"BaseURL":   s.opt.BaseURL,
		"MaxUpload": humanBytes(s.opt.MaxUpload),
	})
	if err != nil {
		panic(err) // template embutido: só falha por erro de programação
	}
	return buf.Bytes()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.pageOnce.Do(func() { s.page = s.renderPage() })
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "public, max-age=300")
	w.Write(s.page)
}
