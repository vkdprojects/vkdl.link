package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/vkdprojects/vkdl.link/internal/janitor"
)

func TestServeHeadersHeadAndRange(t *testing.T) {
	e := newEnv(t)
	data := pngBytes(t)
	resp, r := e.upload(data, nil)
	expectStatus(t, resp, http.StatusCreated)
	p := path(r.URL)

	get := e.do("GET", p, nil, nil)
	expectStatus(t, get, http.StatusOK)
	want := map[string]string{
		"Content-Type":                "image/png",
		"Cache-Control":               "public, max-age=31536000, immutable",
		"X-Content-Type-Options":      "nosniff",
		"Content-Security-Policy":     "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'",
		"Content-Disposition":         `inline; filename="` + r.ID + `.png"`,
		"Referrer-Policy":             "no-referrer",
		"Access-Control-Allow-Origin": "*",
		"Accept-Ranges":               "bytes",
	}
	for k, v := range want {
		if got := get.Header.Get(k); got != v {
			t.Errorf("%s = %q, quero %q", k, got, v)
		}
	}
	etag := get.Header.Get("ETag")
	f, _ := e.db.GetFile(t.Context(), r.ID)
	if etag != `"`+f.SHA256+`"` || len(f.SHA256) != 64 {
		t.Errorf("ETag = %q, sha256 = %q", etag, f.SHA256)
	}
	if !bytes.Equal(readAll(t, get.Body), data) {
		t.Fatal("conteúdo diferente")
	}

	// sem extensão serve o mesmo arquivo
	noExt := e.do("GET", "/"+r.ID, nil, nil)
	expectStatus(t, noExt, http.StatusOK)
	if !bytes.Equal(readAll(t, noExt.Body), data) || noExt.Header.Get("Content-Type") != "image/png" {
		t.Fatal("GET sem extensão errado")
	}

	// extensão errada → 404 em texto
	for _, bad := range []string{"/" + r.ID + ".jpg", "/" + r.ID + ".svg", "/" + r.ID + ".png.html", "/" + r.ID + "."} {
		resp := e.do("GET", bad, nil, nil)
		expectStatus(t, resp, http.StatusNotFound)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("%s: 404 de arquivo devia ser texto", bad)
		}
	}

	// HEAD: mesmos cabeçalhos, sem corpo
	head := e.do("HEAD", p, nil, nil)
	expectStatus(t, head, http.StatusOK)
	if head.ContentLength != int64(len(data)) || head.Header.Get("ETag") != etag || head.Header.Get("Content-Type") != "image/png" {
		t.Errorf("HEAD: len=%d etag=%q", head.ContentLength, head.Header.Get("ETag"))
	}
	if b := readAll(t, head.Body); len(b) != 0 {
		t.Error("HEAD devolveu corpo")
	}

	// Range
	rng := e.do("GET", p, nil, map[string]string{"Range": "bytes=0-7"})
	expectStatus(t, rng, http.StatusPartialContent)
	if b := readAll(t, rng.Body); !bytes.Equal(b, data[:8]) {
		t.Errorf("Range devolveu %q", b)
	}
	if !strings.HasPrefix(rng.Header.Get("Content-Range"), "bytes 0-7/") {
		t.Errorf("Content-Range = %q", rng.Header.Get("Content-Range"))
	}

	// If-None-Match
	nm := e.do("GET", p, nil, map[string]string{"If-None-Match": etag})
	expectStatus(t, nm, http.StatusNotModified)
}

func TestServeNotFound(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/aaaaaaaa.png", "/aaaaaaaa", "/curto", "/favicon.ico", "/..%2f..%2fetc%2fpasswd", "/aaaaaaa!.png"} {
		resp := e.do("GET", p, nil, nil)
		expectStatus(t, resp, http.StatusNotFound)
	}
	// arquivo some do disco mas a linha ficou: 404, não 500
	resp, r := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, http.StatusCreated)
	e.st.Delete(t.Context(), r.ID+".png")
	expectStatus(t, e.do("GET", path(r.URL), nil, nil), http.StatusNotFound)
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	resp, r := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, http.StatusCreated)
	del := path(r.DeleteURL)

	expectStatus(t, e.do("DELETE", del, nil, nil), http.StatusForbidden)
	wrong := e.do("DELETE", del, nil, map[string]string{"X-Delete-Token": "token-errado"})
	expectStatus(t, wrong, http.StatusForbidden)
	if m := decodeJSON(t, wrong.Body); m["error"] != "forbidden" {
		t.Fatalf("erro = %v", m)
	}
	// o token de outro arquivo não serve
	_, other := e.upload(pngBytes(t), nil)
	expectStatus(t, e.do("DELETE", del, nil, map[string]string{"X-Delete-Token": other.DeleteToken}), http.StatusForbidden)
	// bearer de chave/qualquer não é admin
	expectStatus(t, e.do("DELETE", del, nil, map[string]string{"Authorization": "Bearer nao-sou-admin"}), http.StatusForbidden)

	expectStatus(t, e.do("DELETE", del, nil, map[string]string{"X-Delete-Token": r.DeleteToken}), http.StatusNoContent)
	expectStatus(t, e.do("GET", path(r.URL), nil, nil), http.StatusNotFound)
	if _, err := e.st.Stat(t.Context(), r.ID+".png"); err == nil {
		t.Fatal("arquivo continuou no disco")
	}
	expectStatus(t, e.do("DELETE", del, nil, map[string]string{"X-Delete-Token": r.DeleteToken}), http.StatusNotFound)
	expectStatus(t, e.do("DELETE", "/v1/files/invalido!", nil, nil), http.StatusNotFound)

	// admin apaga qualquer coisa
	expectStatus(t, e.do("DELETE", path(other.DeleteURL), nil, map[string]string{"Authorization": "Bearer " + testAdmin}), http.StatusNoContent)
	expectStatus(t, e.do("GET", path(other.URL), nil, nil), http.StatusNotFound)
}

func TestDeleteWithAdminDisabled(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *janitor.Janitor) { o.AdminToken = "" })
	resp, r := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, http.StatusCreated)
	for _, auth := range []string{"Bearer ", "Bearer " + testAdmin, "Bearer"} {
		expectStatus(t, e.do("DELETE", path(r.DeleteURL), nil, map[string]string{"Authorization": auth}), http.StatusForbidden)
	}
	expectStatus(t, e.do("DELETE", path(r.DeleteURL), nil, map[string]string{"X-Delete-Token": r.DeleteToken}), http.StatusNoContent)
}
