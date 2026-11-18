package store

// Teste de integração do armazenamento S3. Só roda com VKDL_TEST_S3=1 e usa o
// bucket descrito pelas variáveis S3_* (as mesmas do servidor). Tudo é
// gravado num subprefixo aleatório "<S3_PREFIX>it-<aleatório>/" e apagado no
// fim; o teste confere que o subprefixo ficou vazio.
//
//	VKDL_TEST_S3=1 S3_ENDPOINT=http://127.0.0.1:9000 S3_BUCKET=... \
//	  S3_KEY_ID=... S3_KEY_SECRET=... go test ./internal/store/ -run S3Integration -v

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/vkdprojects/vkdl.link/internal/config"
	"github.com/vkdprojects/vkdl.link/internal/ident"
)

func integrationS3(t *testing.T) (*S3, S3Config) {
	t.Helper()
	if os.Getenv("VKDL_TEST_S3") != "1" {
		t.Skip("defina VKDL_TEST_S3=1 e as variáveis S3_* para rodar contra um bucket de verdade")
	}
	env := func(k string) string {
		if k == "VKDL_STORAGE" {
			return config.StorageS3
		}
		return os.Getenv(k)
	}
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	base, err := normalizePrefix(cfg.S3.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	sc := S3Config{
		Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket,
		KeyID: cfg.S3.KeyID, KeySecret: cfg.S3.KeySecret, UseSSL: cfg.S3.UseSSL,
		PathStyle: PathStyle(cfg.S3.PathStyle),
		Prefix:    base + "it-" + strings.ToLower(ident.Random(10)) + "/",
	}
	s, err := NewS3(sc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("bucket %s em %s, subprefixo %s", sc.Bucket, sc.Endpoint, s.prefix)
	t.Cleanup(func() { cleanupPrefix(t, s) })
	return s, sc
}

// cleanupPrefix apaga tudo dentro do subprefixo do teste (e só dele) e
// confere que não sobrou nada.
func cleanupPrefix(t *testing.T, s *S3) {
	if !strings.Contains(s.prefix, "it-") {
		t.Fatalf("recusando limpar fora de um subprefixo de teste: %q", s.prefix)
	}
	ctx := context.Background()
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix, Recursive: true}) {
		if obj.Err != nil {
			t.Errorf("listando para limpar: %v", obj.Err)
			return
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			t.Errorf("apagando %s: %v", obj.Key, err)
		}
	}
	left := 0
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix, Recursive: true}) {
		if obj.Err != nil {
			t.Errorf("conferindo limpeza: %v", obj.Err)
			return
		}
		left++
	}
	if left != 0 {
		t.Errorf("sobraram %d objeto(s) em %s", left, s.prefix)
	}
	t.Logf("subprefixo %s vazio", s.prefix)
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestS3Integration(t *testing.T) {
	s, _ := integrationS3(t)
	ctx := context.Background()
	data := randomBytes(t, 70_000)

	// Put a partir de um leitor sem tamanho (multipart em partes)
	n, err := s.Put(ctx, "aB3dE9xQ.png", bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("Put = %d, %v", n, err)
	}
	// Put a partir de *os.File (PUT único), maior que a parte mínima do minio-go
	big := randomBytes(t, 17<<20)
	p := filepath.Join(t.TempDir(), "spool")
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	fh, _ := os.Open(p)
	n, err = s.Put(ctx, "Zz000000.jpg", fh)
	fh.Close()
	if err != nil || n != int64(len(big)) {
		t.Fatalf("Put(*os.File) = %d, %v", n, err)
	}
	oi, err := s.client.StatObject(ctx, s.bucket, s.prefix+"Zz/Zz000000.jpg", minio.StatObjectOptions{})
	if err != nil || oi.ContentType != "image/jpeg" || oi.Size != int64(len(big)) {
		t.Fatalf("objeto no bucket: %+v, %v", oi, err)
	}

	// Stat
	info, err := s.Stat(ctx, "aB3dE9xQ.png")
	if err != nil || info.Size != int64(len(data)) || time.Since(info.ModTime) > time.Hour {
		t.Fatalf("Stat = %+v, %v", info, err)
	}

	// Open + leitura inteira, SeekEnd e leitura de um trecho
	f, err := s.Open(ctx, "aB3dE9xQ.png")
	if err != nil {
		t.Fatal(err)
	}
	if end, err := f.Seek(0, io.SeekEnd); err != nil || end != int64(len(data)) {
		t.Fatalf("SeekEnd = %d, %v", end, err)
	}
	f.Seek(0, io.SeekStart)
	all, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("leitura inteira: %d bytes, %v", len(all), err)
	}
	f.Seek(1000, io.SeekStart)
	part := make([]byte, 500)
	if _, err := io.ReadFull(f, part); err != nil || !bytes.Equal(part, data[1000:1500]) {
		t.Fatalf("leitura com Seek: %v", err)
	}
	f.Close()

	// objeto maior que a janela de leitura: vários GETs em sequência, e um
	// trecho que atravessa a fronteira entre duas janelas
	f, err = s.Open(ctx, "Zz000000.jpg")
	if err != nil {
		t.Fatal(err)
	}
	all, err = io.ReadAll(f)
	if err != nil || !bytes.Equal(all, big) {
		t.Fatalf("leitura em janelas: %d bytes, %v", len(all), err)
	}
	f.Seek(s3ReadWindow-100, io.SeekStart)
	cross := make([]byte, 300)
	if _, err := io.ReadFull(f, cross); err != nil || !bytes.Equal(cross, big[s3ReadWindow-100:s3ReadWindow+200]) {
		t.Fatalf("leitura entre janelas: %v", err)
	}
	f.Close()

	// http.ServeContent com Range, como o handler de arquivos usa
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := s.Open(r.Context(), "aB3dE9xQ.png")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, "", time.Time{}, f)
	}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[100:200]) {
		t.Fatalf("Range: status %d, %d bytes", resp.StatusCode, len(body))
	}
	req.Header.Set("Range", "bytes=-10")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[len(data)-10:]) {
		t.Fatalf("Range sufixo: status %d, %d bytes", resp.StatusCode, len(body))
	}

	// List: ordem crescente, só chaves válidas dentro do prefixo
	for _, junk := range []string{"lixo.txt", "xx/aB3dE9xQ.png", "aB/.put-1"} {
		if _, err := s.client.PutObject(ctx, s.bucket, s.prefix+junk, strings.NewReader("x"), 1, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put(ctx, "00000001.gif", strings.NewReader("gif")); err != nil {
		t.Fatal(err)
	}
	var keys []string
	sizes := map[string]int64{}
	if err := s.List(ctx, func(k string, i Info) error { keys = append(keys, k); sizes[k] = i.Size; return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{"00000001.gif", "Zz000000.jpg", "aB3dE9xQ.png"}
	if !slices.Equal(keys, want) || sizes["aB3dE9xQ.png"] != int64(len(data)) || sizes["00000001.gif"] != 3 {
		t.Fatalf("List = %v %v, quero %v", keys, sizes, want)
	}
	stop := errors.New("pare")
	if err := s.List(ctx, func(string, Info) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("List com erro do callback = %v", err)
	}

	// Delete e NotFound
	for _, k := range want {
		if err := s.Delete(ctx, k); err != nil {
			t.Fatalf("Delete(%s) = %v", k, err)
		}
	}
	if err := s.Delete(ctx, "aB3dE9xQ.png"); err != nil {
		t.Fatalf("Delete repetido no S3 devolve nil, veio %v", err)
	}
	if _, err := s.Stat(ctx, "aB3dE9xQ.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat após Delete = %v", err)
	}
	if _, err := s.Open(ctx, "aB3dE9xQ.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open após Delete = %v", err)
	}
	n2 := 0
	s.List(ctx, func(string, Info) error { n2++; return nil })
	if n2 != 0 {
		t.Fatalf("List depois de apagar tudo achou %d", n2)
	}
}

func TestS3IntegrationCheckFailures(t *testing.T) {
	_, sc := integrationS3(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	missing := sc
	missing.Bucket = "vkdl-nao-existe-" + strings.ToLower(ident.Random(12))
	s, err := NewS3(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx); err == nil {
		t.Fatal("bucket inexistente devia falhar no Check")
	} else if strings.Contains(err.Error(), sc.KeySecret) || strings.Contains(err.Error(), sc.KeyID) {
		t.Fatal("erro do Check vazou credencial")
	}

	wrong := sc
	wrong.KeyID, wrong.KeySecret = "FAKEID"+ident.Random(14), "FAKESECRET"+ident.Random(30)
	s, err = NewS3(wrong)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Check(ctx)
	if err == nil {
		t.Fatal("credenciais erradas deviam falhar no Check")
	}
	if strings.Contains(err.Error(), wrong.KeySecret) || strings.Contains(err.Error(), wrong.KeyID) {
		t.Fatal("erro do Check vazou credencial")
	}
	t.Logf("erro com credenciais erradas: %v", err)
}
