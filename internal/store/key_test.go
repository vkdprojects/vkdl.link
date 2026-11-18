package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidKey(t *testing.T) {
	for _, k := range []string{"aB3dE9xQ.png", "00000000.jpeg", "Zz000000.gif", "abcdefgh.webp"} {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false", k)
		}
	}
	for _, k := range []string{"", "../../etc/passwd", "short.png", "aB3dE9xQ", "aB3dE9xQ.", "aB3dE9xQ.p/g",
		"aB3dE9xQ.png.exe", "aB3dE9x/.png", "aB3dE9xQ.toolong", `aB3dE9xQ.p\g`, "aB3dE9x-.png", "aB/aB3dE9xQ.png"} {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true", k)
		}
		if _, err := relPath(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("relPath(%q) = %v, quero ErrInvalidKey", k, err)
		}
	}
}

func TestRelPathRoundTrip(t *testing.T) {
	rel, err := relPath("aB3dE9xQ.png")
	if err != nil || rel != "aB/aB3dE9xQ.png" {
		t.Fatalf("relPath = %q, %v", rel, err)
	}
	if k, ok := keyFromRel(rel); !ok || k != "aB3dE9xQ.png" {
		t.Fatalf("keyFromRel = %q, %v", k, ok)
	}
	for _, rel := range []string{"aB3dE9xQ.png", "xx/aB3dE9xQ.png", "aB/.put-123", "aB/sub/aB3dE9xQ.png", "aB/aB3dE9xQ.png/", "lixo.txt"} {
		if k, ok := keyFromRel(rel); ok {
			t.Errorf("keyFromRel(%q) = %q, devia recusar", rel, k)
		}
	}
}

func TestFSList(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, _ := NewFS(root)
	keys := []string{"zz000001.png", "00000001.gif", "AB000001.png", "ab000001.webp", "ab000000.jpg"}
	for _, k := range keys {
		if _, err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatal(err)
		}
	}
	// lixo que precisa ser ignorado
	os.WriteFile(filepath.Join(root, "ab", ".put-999"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "ab", "zz000009.png"), []byte("x"), 0o644) // diretório errado
	os.WriteFile(filepath.Join(root, "solto.png"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "abc"), 0o755)

	var got []string
	err := s.List(ctx, func(key string, info Info) error {
		if info.Size != int64(len(key)) || time.Since(info.ModTime) > time.Hour {
			t.Errorf("%s: info %+v", key, info)
		}
		got = append(got, key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "00000001.gif AB000001.png ab000000.jpg ab000001.webp zz000001.png"
	if strings.Join(got, " ") != want {
		t.Fatalf("List = %v, quero %s (ordem crescente, sem lixo)", got, want)
	}
	stop := errors.New("pare")
	n := 0
	if err := s.List(ctx, func(string, Info) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Fatalf("List com erro do callback = %v depois de %d", err, n)
	}
}
