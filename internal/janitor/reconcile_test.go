package janitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/store"
)

// statOnly esconde o List do FS: força o caminho antigo (um Stat por linha).
type statOnly struct{ store.Store }

// countingStore conta Stat e pode listar fora de ordem.
type countingStore struct {
	*store.FS
	stats    int
	unsorted bool
}

func (c *countingStore) Stat(ctx context.Context, key string) (store.Info, error) {
	c.stats++
	return c.FS.Stat(ctx, key)
}

func (c *countingStore) List(ctx context.Context, fn func(string, store.Info) error) error {
	if !c.unsorted {
		return c.FS.List(ctx, fn)
	}
	var keys []string
	infos := map[string]store.Info{}
	c.FS.List(ctx, func(k string, i store.Info) error { keys = append(keys, k); infos[k] = i; return nil })
	for i := len(keys) - 1; i >= 0; i-- { // ao contrário
		if err := fn(keys[i], infos[keys[i]]); err != nil {
			return err
		}
	}
	return nil
}

// putOrphan grava um objeto sem linha no banco, com a idade dada.
func (f *fixture) putOrphan(t *testing.T, key string, age time.Duration) {
	t.Helper()
	if _, err := f.st.Put(context.Background(), key, strings.NewReader("orfao")); err != nil {
		t.Fatal(err)
	}
	mt := f.now.Add(-age)
	if err := os.Chtimes(filepath.Join(f.st.Root(), key[:2], key), mt, mt); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) objectExists(key string) bool {
	_, err := f.st.Stat(context.Background(), key)
	return err == nil
}

func TestReconcileByListDoesNotStatEachRow(t *testing.T) {
	f := newFixture(t, 1<<20)
	cs := &countingStore{FS: f.st}
	f.j.Store = cs
	for _, id := range []string{"anon0001", "anon0002", "anon0003", "anon0004"} {
		f.add(t, id, 10, time.Minute, "", nil)
	}
	f.st.Delete(context.Background(), "anon0002.png")                       // sumiu
	f.st.Put(context.Background(), "anon0004.png", strings.NewReader("xx")) // truncado
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	if cs.stats != 0 {
		t.Errorf("com Lister não devia haver Stat por linha (houve %d)", cs.stats)
	}
	if !f.exists("anon0001") || !f.exists("anon0003") || f.exists("anon0002") || f.exists("anon0004") {
		t.Error("Reconcile removeu as linhas erradas")
	}
}

func TestReconcileDeletesOldOrphansOnly(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.add(t, "anon0001", 10, time.Hour, "", nil)
	f.add(t, "anon0003", 10, time.Hour, "", nil)
	f.putOrphan(t, "anon0002.png", time.Hour)                                       // órfão velho: sai
	f.putOrphan(t, "anon0009.png", time.Minute)                                     // órfão novo: fica (pode estar chegando)
	f.putOrphan(t, "anon0001.gif", time.Hour)                                       // mesmo id de uma linha, outra extensão: órfão
	f.putOrphan(t, "ZZZZ0000.webp", 2*OrphanMinAge)                                 // antes de todas as linhas
	os.WriteFile(filepath.Join(f.st.Root(), "an", "notas.txt"), []byte("x"), 0o644) // fora do formato: intocado
	backdate := f.now.Add(-time.Hour)
	os.Chtimes(filepath.Join(f.st.Root(), "an", "notas.txt"), backdate, backdate)

	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	for key, want := range map[string]bool{
		"anon0001.png": true, "anon0003.png": true, "anon0009.png": true,
		"anon0002.png": false, "anon0001.gif": false, "ZZZZ0000.webp": false,
	} {
		if got := f.objectExists(key); got != want {
			t.Errorf("%s existe=%v, quero %v", key, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.st.Root(), "an", "notas.txt")); err != nil {
		t.Error("arquivo fora do formato não podia ser apagado")
	}
}

// Armazenamento vazio com linhas no banco é quase sempre bucket/volume
// errado: nada pode ser apagado.
func TestReconcileEmptyStoreKeepsRows(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.add(t, "anon0001", 10, time.Hour, "", nil)
	f.add(t, "anon0002", 10, time.Hour, "", nil)
	os.RemoveAll(f.st.Root())
	os.MkdirAll(f.st.Root(), 0o755)
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	for _, id := range []string{"anon0001", "anon0002"} {
		if ok, _ := f.db.FileExists(context.Background(), id); !ok {
			t.Errorf("linha %s foi apagada com o armazenamento vazio", id)
		}
	}
}

// Listagem fora de ordem: não apaga nada com base nela e volta ao Stat por linha.
func TestReconcileUnsortedListingFallsBack(t *testing.T) {
	f := newFixture(t, 1<<20)
	cs := &countingStore{FS: f.st, unsorted: true}
	f.j.Store = cs
	for _, id := range []string{"anon0001", "anon0002", "anon0003"} {
		f.add(t, id, 10, time.Hour, "", nil)
	}
	f.st.Delete(context.Background(), "anon0002.png")
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	if cs.stats != 3 {
		t.Errorf("devia ter voltado ao Stat por linha (Stat = %d)", cs.stats)
	}
	if !f.exists("anon0001") || !f.exists("anon0003") {
		t.Error("listagem fora de ordem apagou o que não devia")
	}
}

// Sem Lister o comportamento antigo continua: um Stat por linha.
func TestReconcileWithoutLister(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.j.Store = statOnly{f.st}
	f.add(t, "anon0001", 10, time.Hour, "", nil)
	f.add(t, "anon0002", 10, time.Hour, "", nil)
	f.putOrphan(t, "anon0005.png", time.Hour)
	f.st.Delete(context.Background(), "anon0001.png")
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 1 || f.exists("anon0002") != true {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	if !f.objectExists("anon0005.png") {
		t.Error("sem Lister não há como achar órfãos; nada além das linhas devia mudar")
	}
}

// Muitas linhas (mais de uma página do cursor) e objetos intercalados.
func TestReconcileByListPaging(t *testing.T) {
	f := newFixture(t, 1<<30)
	ctx := context.Background()
	for i := range 1200 {
		id := fmt.Sprintf("p%07d", i)
		f.add(t, id, 1, time.Hour, "", nil)
		if i%7 == 0 {
			f.st.Delete(ctx, id+".png")
		}
	}
	n, err := f.j.Reconcile(ctx)
	if err != nil || n != 172 { // 0, 7, ..., 1197 → 172 linhas
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	if total, _ := f.db.TotalBytes(ctx); total != 1200-172 {
		t.Fatalf("TotalBytes = %d", total)
	}
}

// Banco vazio (volume novo, VKDL_DATA_DIR errado) apontado para um
// armazenamento com objetos: nada pode ser apagado.
func TestReconcileEmptyDBKeepsObjects(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.putOrphan(t, "anon0001.png", time.Hour)
	f.putOrphan(t, "anon0002.png", 24*time.Hour)
	// objetos novos (não contam como órfãos) fazem os velhos serem minoria:
	// só a regra do banco vazio protege aqui
	for _, k := range []string{"anon0003.png", "anon0004.png", "anon0005.png"} {
		f.putOrphan(t, k, time.Minute)
	}
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	for _, k := range []string{"anon0001.png", "anon0002.png", "anon0003.png"} {
		if !f.objectExists(k) {
			t.Errorf("%s foi apagado com o banco vazio", k)
		}
	}
}

// Banco que não conhece a maior parte do armazenamento (backup antigo, banco
// de outra instância): os órfãos ficam; linhas sem objeto continuam saindo.
func TestReconcileMostlyOrphansKeepsObjects(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.add(t, "anon0001", 10, time.Hour, "", nil)
	f.add(t, "anon0005", 10, time.Hour, "", nil)
	f.st.Delete(context.Background(), "anon0005.png") // linha sem objeto: sai
	for _, k := range []string{"anon0002.png", "anon0003.png", "anon0004.png"} {
		f.putOrphan(t, k, time.Hour)
	}
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	for _, k := range []string{"anon0001.png", "anon0002.png", "anon0003.png", "anon0004.png"} {
		if !f.objectExists(k) {
			t.Errorf("%s foi apagado embora a maioria dos objetos não tivesse linha", k)
		}
	}
	if f.exists("anon0005") {
		t.Error("a linha sem objeto devia sair")
	}
}
