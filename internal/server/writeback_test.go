package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// slowBucket faz o papel do S3: um FS cujo Put fica preso até release.
type slowBucket struct {
	*store.FS
	mu   sync.Mutex
	gate chan struct{}
}

func (b *slowBucket) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return b.FS.Put(ctx, key, r)
}

func (b *slowBucket) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gate != nil {
		close(b.gate)
		b.gate = nil
	}
}

func (b *slowBucket) has(t *testing.T, key string) bool {
	t.Helper()
	_, err := b.FS.Stat(context.Background(), key)
	return err == nil
}

// newWriteBackEnv sobe o servidor com um WriteBack na frente de um bucket
// travado, como em produção com S3.
func newWriteBackEnv(t *testing.T) (*env, *slowBucket, *store.WriteBack) {
	t.Helper()
	var bucket *slowBucket
	var wb *store.WriteBack
	e := newEnv(t, func(o *Options, j *janitor.Janitor) {
		bucket = &slowBucket{FS: j.Store.(*store.FS), gate: make(chan struct{})}
		var err error
		wb, err = store.NewWriteBack(bucket, filepath.Join(filepath.Dir(o.SpoolDir), "pending"),
			store.WriteBackOptions{Log: o.Log})
		if err != nil {
			t.Fatal(err)
		}
		j.Store = wb
	})
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	wb.Start(ctx)
	return e, bucket, wb
}

func waitPendingZero(t *testing.T, wb *store.WriteBack) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n, _ := wb.Pending(); n != 0; n, _ = wb.Pending() {
		if time.Now().After(deadline) {
			t.Fatal("o envio ao bucket não terminou")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestUploadRespondsBeforeBucketAndServesLocalCopy(t *testing.T) {
	e, bucket, wb := newWriteBackEnv(t)
	data := pngBytes(t)

	resp, up := e.upload(data, nil) // com o bucket travado, só volta se não esperar por ele
	expectStatus(t, resp, 201)
	spoolEmpty(t, e.spool)
	key := up.ID + ".png"
	if bucket.has(t, key) {
		t.Fatal("o bucket não devia ter o arquivo ainda")
	}

	got := e.do("GET", path(up.URL), nil, nil)
	expectStatus(t, got, 200)
	if b := readAll(t, got.Body); !bytes.Equal(b, data) {
		t.Fatal("a cópia local servida difere do enviado")
	}
	if cc := got.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	stats := e.do("GET", "/v1/admin/stats", nil, map[string]string{"Authorization": "Bearer " + testAdmin})
	expectStatus(t, stats, 200)
	m := decodeJSON(t, stats.Body)
	if m["pending_files"] != float64(1) || m["pending_bytes"] != float64(len(data)) {
		t.Fatalf("stats: pending_files=%v pending_bytes=%v", m["pending_files"], m["pending_bytes"])
	}

	// a reconciliação do boot não pode tomar o pendente por perdido
	if n, err := e.jan.Reconcile(context.Background()); err != nil || n != 0 {
		t.Fatalf("Reconcile removeu %d linha(s), err %v", n, err)
	}

	bucket.release()
	waitPendingZero(t, wb)
	if !bucket.has(t, key) {
		t.Fatal("o arquivo não chegou ao bucket")
	}
	got = e.do("GET", path(up.URL), nil, nil)
	expectStatus(t, got, 200)
	if b := readAll(t, got.Body); !bytes.Equal(b, data) {
		t.Fatal("depois do envio, o conteúdo servido difere do enviado")
	}
}

func TestDeleteWhileUploadPending(t *testing.T) {
	e, bucket, wb := newWriteBackEnv(t)
	resp, up := e.upload(pngBytes(t), nil)
	expectStatus(t, resp, 201)

	del := e.do("DELETE", "/v1/files/"+up.ID, nil, map[string]string{"X-Delete-Token": up.DeleteToken})
	expectStatus(t, del, 204)
	bucket.release()
	if n, _ := wb.Pending(); n != 0 {
		t.Fatalf("o apagado continuou na fila (%d)", n)
	}
	expectStatus(t, e.do("GET", path(up.URL), nil, nil), 404)
	if bucket.has(t, up.ID+".png") {
		t.Fatal("o envio do arquivo apagado chegou ao bucket")
	}
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(e.spool), "pending"))
	if len(entries) != 0 {
		t.Fatalf("sobrou cópia local: %v", entries)
	}
}
