package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "vkdl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestFilesCRUD(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	if err := d.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := File{ID: "aB3dE9xQ", Ext: "png", MIME: "image/png", Size: 10, SHA256: "ab", DeleteTokenHash: "cd", IPHash: "ef", CreatedAt: now}
	if err := d.InsertFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertFile(ctx, f); !errors.Is(err, ErrConflict) {
		t.Fatalf("id repetido = %v, quero ErrConflict", err)
	}
	got, err := d.GetFile(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key() != "aB3dE9xQ.png" || got.APIKeyID != "" || got.ExpiresAt != nil || !got.CreatedAt.Equal(now) || got.IPHash != "ef" {
		t.Fatalf("GetFile = %+v", got)
	}
	if ok, _ := d.FileExists(ctx, f.ID); !ok {
		t.Fatal("FileExists devia ser true")
	}
	if n, _ := d.TotalBytes(ctx); n != 10 {
		t.Fatalf("TotalBytes = %d", n)
	}
	if err := d.DeleteFile(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteFile(ctx, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteFile repetido = %v", err)
	}
	if _, err := d.GetFile(ctx, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFile após delete = %v", err)
	}
}

func TestKeys(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	now := time.Now()
	k := APIKey{ID: "key000000001", Name: "teste", KeyHash: "h1", RetentionDays: 30, CreatedAt: now}
	if err := d.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateKey(ctx, APIKey{ID: "key000000002", Name: "x", KeyHash: "h1", CreatedAt: now}); !errors.Is(err, ErrConflict) {
		t.Fatalf("hash repetido = %v", err)
	}
	got, err := d.KeyByHash(ctx, "h1")
	if err != nil || !got.Active() || got.RetentionDays != 30 || got.Name != "teste" {
		t.Fatalf("KeyByHash = %+v, %v", got, err)
	}
	if _, err := d.KeyByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hash inexistente = %v", err)
	}
	if err := d.RevokeKey(ctx, k.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := d.RevokeKey(ctx, k.ID, now.Add(time.Hour)); err != nil {
		t.Fatal("revogar de novo devia ser idempotente:", err)
	}
	got, _ = d.KeyByHash(ctx, "h1")
	if got.Active() || got.RevokedAt.UnixMilli() != now.UnixMilli() {
		t.Fatalf("revogação não registrada/alterada: %+v", got)
	}
	if err := d.RevokeKey(ctx, "inexistente", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revogar inexistente = %v", err)
	}
	keys, err := d.ListKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = %v, %v", keys, err)
	}
}

func TestEvictionOrderAndStats(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	now := time.Now()
	d.CreateKey(ctx, APIKey{ID: "k1", Name: "k", KeyHash: "h", RetentionDays: 1, CreatedAt: now})
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	ins := func(id string, age time.Duration, key string, exp *time.Time) {
		t.Helper()
		err := d.InsertFile(ctx, File{ID: id, Ext: "png", MIME: "image/png", Size: 100, SHA256: "s", DeleteTokenHash: "d",
			APIKeyID: key, CreatedAt: now.Add(-age), ExpiresAt: exp})
		if err != nil {
			t.Fatal(err)
		}
	}
	ins("keyedNew", 5*time.Hour, "k1", &future) // o mais antigo, mas com retenção vigente
	ins("keyedOld", 4*time.Hour, "k1", &past)   // retenção vencida: despejável
	ins("anon0001", 3*time.Hour, "", nil)       // anônimo
	ins("keyedFvr", 2*time.Hour, "k1", nil)     // chave com retenção 0 = para sempre
	ins("anon0002", 1*time.Hour, "", nil)       // anônimo mais novo
	list, err := d.OldestEvictable(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range list {
		ids = append(ids, f.ID)
	}
	want := []string{"keyedOld", "anon0001", "anon0002"}
	if len(ids) != len(want) {
		t.Fatalf("despejáveis = %v, quero %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("despejáveis = %v, quero %v", ids, want)
		}
	}
	s, err := d.Stats(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Files != 5 || s.AnonFiles != 2 || s.KeyedFiles != 3 || s.TotalBytes != 500 || s.EvictableFiles != 3 ||
		s.EvictableBytes != 300 || s.ActiveKeys != 1 || s.RevokedKeys != 0 {
		t.Fatalf("Stats = %+v", s)
	}
	page, _ := d.FilesAfter(ctx, "", 2)
	if len(page) != 2 || page[0].ID != "anon0001" {
		t.Fatalf("FilesAfter = %+v", page)
	}
	page, _ = d.FilesAfter(ctx, page[1].ID, 10)
	if len(page) != 3 {
		t.Fatalf("FilesAfter página 2 = %d", len(page))
	}
}

func TestEvictableBytesUpTo(t *testing.T) {
	ctx := context.Background()
	d := openTest(t)
	now := time.Now().UTC()
	d.CreateKey(ctx, APIKey{ID: "k1", Name: "k", KeyHash: "h", RetentionDays: 7, CreatedAt: now})
	future := now.Add(time.Hour)
	for i, f := range []File{
		{ID: "keyed001", Size: 1000, APIKeyID: "k1", ExpiresAt: &future}, // protegido: não conta
		{ID: "anon0001", Size: 10},
		{ID: "anon0002", Size: 20},
		{ID: "anon0003", Size: 40},
	} {
		f.Ext, f.MIME, f.SHA256, f.DeleteTokenHash = "png", "image/png", "s", "d"
		f.CreatedAt = now.Add(time.Duration(i-10) * time.Minute)
		if err := d.InsertFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	for need, want := range map[int64]int64{0: 0, 1: 10, 10: 10, 11: 30, 31: 70, 500: 70} {
		if got, err := d.EvictableBytesUpTo(ctx, now, need); err != nil || got != want {
			t.Errorf("EvictableBytesUpTo(%d) = %d, %v; quero %d", need, got, err, want)
		}
	}
}
