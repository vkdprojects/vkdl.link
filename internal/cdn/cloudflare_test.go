package cdn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCloudflarePurge(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string][]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"success":true,"errors":[]}`))
	}))
	defer ts.Close()
	c := &Cloudflare{ZoneID: "zona1", Token: "tok-secreto", APIBase: ts.URL}
	urls := []string{"https://vkdl.link/aB3dE9xQ.png", "https://vkdl.link/aB3dE9xQ"}
	if err := c.Purge(context.Background(), urls); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /zones/zona1/purge_cache" || gotAuth != "Bearer tok-secreto" {
		t.Errorf("requisição = %q, auth = %q", gotPath, gotAuth)
	}
	if !reflect.DeepEqual(gotBody["files"], urls) {
		t.Errorf("files = %v", gotBody["files"])
	}
}

func TestCloudflarePurgeErrorHidesToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
	}))
	defer ts.Close()
	c := &Cloudflare{ZoneID: "z", Token: "tok-secreto", APIBase: ts.URL}
	err := c.Purge(context.Background(), []string{"https://vkdl.link/x"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "10000") {
		t.Fatalf("erro = %v", err)
	}
	if strings.Contains(err.Error(), "tok-secreto") {
		t.Fatal("o token vazou na mensagem de erro")
	}
}
