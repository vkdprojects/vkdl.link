package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/janitor"
)

func TestAdminAuth(t *testing.T) {
	e := newEnv(t)
	routes := [][2]string{{"POST", "/v1/admin/keys"}, {"GET", "/v1/admin/keys"}, {"DELETE", "/v1/admin/keys/x"}, {"GET", "/v1/admin/stats"}}
	for _, rt := range routes {
		expectStatus(t, e.do(rt[0], rt[1], strings.NewReader(`{}`), nil), http.StatusUnauthorized)
		expectStatus(t, e.do(rt[0], rt[1], strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer errado"}), http.StatusUnauthorized)
		expectStatus(t, e.do(rt[0], rt[1], strings.NewReader(`{}`), map[string]string{"Authorization": "Basic " + testAdmin}), http.StatusUnauthorized)
	}
	off := newEnv(t, func(o *Options, _ *janitor.Janitor) { o.AdminToken = "" })
	for _, rt := range routes {
		resp := off.do(rt[0], rt[1], strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer " + testAdmin})
		expectStatus(t, resp, http.StatusNotFound)
		if m := decodeJSON(t, resp.Body); m["error"] != "not_found" {
			t.Fatalf("admin desativado devia ser 404 JSON: %v", m)
		}
	}
}

func TestAdminKeysLifecycle(t *testing.T) {
	e := newEnv(t)
	admin := map[string]string{"Authorization": "Bearer " + testAdmin}

	for _, bad := range []string{`{}`, `{"name":""}`, `{"name":"x","retention_days":-1}`, `{"name":"x","retention_days":99999}`, `nao-json`,
		`{"name":"` + strings.Repeat("a", 101) + `"}`} {
		expectStatus(t, e.do("POST", "/v1/admin/keys", strings.NewReader(bad), admin), http.StatusBadRequest)
	}

	resp := e.do("POST", "/v1/admin/keys", strings.NewReader(`{"name":"Recortei Pro","retention_days":30}`), admin)
	expectStatus(t, resp, http.StatusCreated)
	created := decodeJSON(t, resp.Body)
	secret, _ := created["key"].(string)
	keyID, _ := created["id"].(string)
	if !strings.HasPrefix(secret, "vk_") || keyID == "" || created["name"] != "Recortei Pro" || created["retention_days"] != float64(30) {
		t.Fatalf("criação = %v", created)
	}
	auth := map[string]string{"Authorization": "Bearer " + secret}

	// envio com chave: retenção garantida de 30 dias
	up, r := e.upload(pngBytes(t), auth)
	expectStatus(t, up, http.StatusCreated)
	if r.ExpiresAt == nil {
		t.Fatal("envio com chave devia ter expires_at")
	}
	exp, err := time.Parse(time.RFC3339, *r.ExpiresAt)
	if err != nil || exp.Sub(*e.now) < 30*24*time.Hour-time.Second || exp.Sub(*e.now) > 30*24*time.Hour+time.Second {
		t.Fatalf("expires_at = %v (now %v)", *r.ExpiresAt, *e.now)
	}
	if f, _ := e.db.GetFile(t.Context(), r.ID); f.APIKeyID != keyID {
		t.Fatalf("arquivo não ligado à chave: %+v", f)
	}

	// chave com retention 0 = para sempre → expires_at null
	forever := e.createKey(t, "eterna", 0)
	up, r0 := e.upload(pngBytes(t), map[string]string{"Authorization": "Bearer " + forever})
	expectStatus(t, up, http.StatusCreated)
	if r0.ExpiresAt != nil {
		t.Fatalf("retention 0 devia dar expires_at null, veio %v", *r0.ExpiresAt)
	}

	// listagem sem segredos
	list := e.do("GET", "/v1/admin/keys", nil, admin)
	expectStatus(t, list, http.StatusOK)
	body := string(readAll(t, list.Body))
	if strings.Contains(body, secret) || strings.Contains(body, forever) || strings.Contains(body, "hash") || !strings.Contains(body, keyID) {
		t.Fatalf("listagem expõe segredo ou não traz a chave: %s", body)
	}

	// chaves inválidas → 401 (nunca cai para anônimo)
	for _, h := range []string{"Bearer vk_inexistente", "Bearer qualquer", "Basic abc", "Bearer " + testAdmin} {
		up, res := e.upload(pngBytes(t), map[string]string{"Authorization": h})
		expectStatus(t, up, http.StatusUnauthorized)
		if res.Error != "invalid_api_key" {
			t.Fatalf("%s: erro = %+v", h, res)
		}
	}

	// revogação
	expectStatus(t, e.do("DELETE", "/v1/admin/keys/"+keyID, nil, admin), http.StatusNoContent)
	expectStatus(t, e.do("DELETE", "/v1/admin/keys/"+keyID, nil, admin), http.StatusNoContent)
	expectStatus(t, e.do("DELETE", "/v1/admin/keys/naoexiste", nil, admin), http.StatusNotFound)
	up, _ = e.upload(pngBytes(t), auth)
	expectStatus(t, up, http.StatusUnauthorized)
	// o arquivo enviado antes continua lá
	expectStatus(t, e.do("GET", path(r.URL), nil, nil), http.StatusOK)

	list = e.do("GET", "/v1/admin/keys", nil, admin)
	keys := decodeJSON(t, list.Body)["keys"].([]any)
	for _, k := range keys {
		km := k.(map[string]any)
		if km["id"] == keyID && (km["active"] != false || km["revoked_at"] == nil) {
			t.Fatalf("chave revogada aparece ativa: %v", km)
		}
	}
}

func TestAdminStats(t *testing.T) {
	e := newEnv(t, func(_ *Options, j *janitor.Janitor) {
		j.MinFreeDisk = 1
		j.DiskFree = func() (int64, error) { return 123456789, nil }
	})
	admin := map[string]string{"Authorization": "Bearer " + testAdmin}
	key := e.createKey(t, "k", 7)
	a, _ := e.upload(bigPNG(t, 500), nil)
	expectStatus(t, a, http.StatusCreated)
	b, _ := e.upload(bigPNG(t, 700), map[string]string{"Authorization": "Bearer " + key})
	expectStatus(t, b, http.StatusCreated)

	resp := e.do("GET", "/v1/admin/stats", nil, admin)
	expectStatus(t, resp, http.StatusOK)
	m := decodeJSON(t, resp.Body)
	want := map[string]float64{
		"files": 2, "anon_files": 1, "keyed_files": 1, "total_bytes": 1200, "max_storage": 1 << 30,
		"min_free_disk": 1, "disk_free": 123456789, "evictable_files": 1, "evictable_bytes": 500, "active_keys": 1,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, quero %v", k, m[k], v)
		}
	}
}
