package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Put(ctx, "aB3dE9xQ.png", strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "aB", "aB3dE9xQ.png")); err != nil {
		t.Fatalf("layout <id[0:2]>/<id>.<ext> não respeitado: %v", err)
	}
	info, err := s.Stat(ctx, "aB3dE9xQ.png")
	if err != nil || info.Size != 5 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	f, err := s.Open(ctx, "aB3dE9xQ.png")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "hello" {
		t.Fatalf("conteúdo = %q", b)
	}
	if err := s.Delete(ctx, "aB3dE9xQ.png"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "aB3dE9xQ.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete repetido = %v, quero ErrNotFound", err)
	}
	if _, err := s.Open(ctx, "aB3dE9xQ.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open após Delete = %v", err)
	}
	if _, err := s.Stat(ctx, "aB3dE9xQ.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat após Delete = %v", err)
	}
	// nenhum temporário pode sobrar
	filepath.Walk(root, func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && strings.HasPrefix(fi.Name(), ".put-") {
			t.Errorf("temporário esquecido: %s", p)
		}
		return nil
	})
}

func TestFSInvalidKeys(t *testing.T) {
	s, _ := NewFS(t.TempDir())
	for _, k := range []string{"../../etc/passwd", "short.png", "aB3dE9xQ", "aB3dE9xQ.", "aB3dE9xQ.p/g", "aB3dE9xQ.png.exe", "aB3dE9x/.png"} {
		if _, err := s.Put(context.Background(), k, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) = %v, quero ErrInvalidKey", k, err)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestFSPutFailureLeavesNothing(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFS(root)
	if _, err := s.Put(context.Background(), "aB3dE9xQ.png", failingReader{}); err == nil {
		t.Fatal("Put devia falhar")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "aB"))
	if len(entries) != 0 {
		t.Fatalf("sobrou lixo: %v", entries)
	}
}

func TestFSAdopt(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFS(filepath.Join(root, "files"))
	src := filepath.Join(root, "spool")
	os.WriteFile(src, []byte("data"), 0o644)
	if err := s.Adopt(context.Background(), "Zz000000.gif", src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("origem devia ter sido movida")
	}
	if info, err := s.Stat(context.Background(), "Zz000000.gif"); err != nil || info.Size != 4 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
}
