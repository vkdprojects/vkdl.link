package store

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func testS3(t *testing.T, prefix string) *S3 {
	t.Helper()
	s, err := NewS3(S3Config{Endpoint: "127.0.0.1:1", Bucket: "b", KeyID: "id", KeySecret: "segredo", Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewS3Validation(t *testing.T) {
	ok := S3Config{Endpoint: "s3.example.com", Bucket: "b", KeyID: "ID-SECRETO", KeySecret: "CHAVE-SECRETA"}
	for name, mut := range map[string]func(*S3Config){
		"sem endpoint":  func(c *S3Config) { c.Endpoint = "" },
		"sem bucket":    func(c *S3Config) { c.Bucket = "" },
		"sem id":        func(c *S3Config) { c.KeyID = "" },
		"sem segredo":   func(c *S3Config) { c.KeySecret = "" },
		"path style":    func(c *S3Config) { c.PathStyle = "talvez" },
		"prefixo ..":    func(c *S3Config) { c.Prefix = "a/../b" },
		"prefixo vazio": func(c *S3Config) { c.Prefix = "a//b" },
	} {
		c := ok
		mut(&c)
		_, err := NewS3(c)
		if err == nil {
			t.Errorf("%s: devia falhar", name)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: erro vazou credencial: %v", name, err)
		}
	}
	for _, ps := range []PathStyle{"", PathStyleAuto, PathStyleTrue, PathStyleFalse} {
		c := ok
		c.PathStyle = ps
		if _, err := NewS3(c); err != nil {
			t.Errorf("path style %q: %v", ps, err)
		}
	}
}

func TestS3ObjectNames(t *testing.T) {
	for prefix, want := range map[string]string{
		"":                  "aB/aB3dE9xQ.png",
		"vkdl":              "vkdl/aB/aB3dE9xQ.png",
		"/vkdl/prod/":       "vkdl/prod/aB/aB3dE9xQ.png",
		" vkdl-ci-test/x/ ": "vkdl-ci-test/x/aB/aB3dE9xQ.png",
	} {
		s := testS3(t, prefix)
		got, err := s.object("aB3dE9xQ.png")
		if err != nil || got != want {
			t.Errorf("prefixo %q: object = %q, %v; quero %q", prefix, got, err, want)
		}
	}
	s := testS3(t, "p")
	ctx := context.Background()
	// chaves inválidas são recusadas antes de qualquer requisição
	for _, k := range []string{"../../etc/passwd", "aB3dE9xQ.p/g", "aB/aB3dE9xQ.png"} {
		if _, err := s.Put(ctx, k, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) = %v", k, err)
		}
		if _, err := s.Open(ctx, k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Open(%q) = %v", k, err)
		}
		if _, err := s.Stat(ctx, k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Stat(%q) = %v", k, err)
		}
		if err := s.Delete(ctx, k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q) = %v", k, err)
		}
	}
}

func TestContentType(t *testing.T) {
	for k, want := range map[string]string{
		"aB3dE9xQ.png": "image/png", "aB3dE9xQ.jpg": "image/jpeg", "aB3dE9xQ.jpeg": "image/jpeg",
		"aB3dE9xQ.gif": "image/gif", "aB3dE9xQ.webp": "image/webp", "aB3dE9xQ.bin": "application/octet-stream",
	} {
		if got := contentType(k); got != want {
			t.Errorf("contentType(%q) = %q", k, got)
		}
	}
}

// Seek só muda a posição (o ServeContent faz SeekEnd para saber o tamanho);
// nenhuma requisição sai enquanto não houver leitura.
func TestS3FileSeek(t *testing.T) {
	f := &s3File{s: testS3(t, ""), key: "aB3dE9xQ.png", size: 100}
	for _, c := range []struct {
		off    int64
		whence int
		want   int64
	}{
		{0, io.SeekEnd, 100}, {0, io.SeekStart, 0}, {10, io.SeekCurrent, 10}, {-5, io.SeekEnd, 95}, {200, io.SeekStart, 200},
	} {
		got, err := f.Seek(c.off, c.whence)
		if err != nil || got != c.want {
			t.Errorf("Seek(%d, %d) = %d, %v; quero %d", c.off, c.whence, got, err, c.want)
		}
	}
	if n, err := f.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Errorf("Read depois do fim = %d, %v", n, err)
	}
	if _, err := f.Seek(-1, io.SeekStart); err == nil {
		t.Error("posição negativa devia falhar")
	}
	if _, err := f.Seek(0, 42); err == nil {
		t.Error("whence inválido devia falhar")
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}
