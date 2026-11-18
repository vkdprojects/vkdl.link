package janitor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

type fixture struct {
	j   *Janitor
	db  *db.DB
	st  *store.FS
	now time.Time
}

func newFixture(t *testing.T, maxStorage int64) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, _ := store.NewFS(filepath.Join(dir, "files"))
	now := time.Now()
	f := &fixture{db: d, st: st, now: now}
	f.j = &Janitor{DB: d, Store: st, MaxStorage: maxStorage, Now: func() time.Time { return now }}
	d.CreateKey(context.Background(), db.APIKey{ID: "k1", Name: "k", KeyHash: "h", RetentionDays: 7, CreatedAt: now})
	return f
}

// add grava um arquivo de size bytes com idade age; key vazio = anônimo.
func (f *fixture) add(t *testing.T, id string, size int, age time.Duration, key string, exp *time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.Put(ctx, id+".png", strings.NewReader(strings.Repeat("x", size))); err != nil {
		t.Fatal(err)
	}
	err := f.db.InsertFile(ctx, db.File{ID: id, Ext: "png", MIME: "image/png", Size: int64(size), SHA256: "s",
		DeleteTokenHash: "d", APIKeyID: key, CreatedAt: f.now.Add(-age), ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) exists(id string) bool {
	ok, _ := f.db.FileExists(context.Background(), id)
	_, err := f.st.Stat(context.Background(), id+".png")
	if ok != (err == nil) {
		panic("banco e armazenamento divergiram para " + id)
	}
	return ok
}

func TestEvictionOrderAnonBeforeUnexpiredKeyed(t *testing.T) {
	f := newFixture(t, 1000)
	future, past := f.now.Add(24*time.Hour), f.now.Add(-time.Second)
	f.add(t, "keyed001", 300, 10*time.Hour, "k1", &future) // mais antigo, protegido
	f.add(t, "expired1", 200, 9*time.Hour, "k1", &past)    // retenção vencida
	f.add(t, "anon0001", 200, 8*time.Hour, "", nil)
	f.add(t, "anon0002", 200, 7*time.Hour, "", nil)
	// total 900; entrar 400 exige liberar 300 → despeja expired1 e anon0001 (os mais antigos despejáveis)
	committed := false
	err := f.j.Reserve(context.Background(), 400, func(context.Context) error { committed = true; return nil })
	if err != nil || !committed {
		t.Fatalf("Reserve = %v, commit=%v", err, committed)
	}
	for id, want := range map[string]bool{"keyed001": true, "expired1": false, "anon0001": false, "anon0002": true} {
		if got := f.exists(id); got != want {
			t.Errorf("%s existe=%v, quero %v", id, got, want)
		}
	}
}

func TestReserveNoSpaceDeletesNothing(t *testing.T) {
	f := newFixture(t, 1000)
	future := f.now.Add(time.Hour)
	f.add(t, "keyed001", 700, time.Hour, "k1", &future)
	f.add(t, "anon0001", 200, time.Minute, "", nil)
	err := f.j.Reserve(context.Background(), 600, func(context.Context) error { t.Fatal("commit não devia rodar"); return nil })
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Reserve = %v, quero ErrNoSpace", err)
	}
	if !f.exists("anon0001") {
		t.Error("não devia despejar nada quando o arquivo não cabe de qualquer jeito")
	}
	// maior que o orçamento inteiro
	if err := f.j.Reserve(context.Background(), 5000, func(context.Context) error { return nil }); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("arquivo maior que o orçamento = %v", err)
	}
}

func TestReserveFitsWithoutEviction(t *testing.T) {
	f := newFixture(t, 1000)
	f.add(t, "anon0001", 100, time.Minute, "", nil)
	if err := f.j.Reserve(context.Background(), 900, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !f.exists("anon0001") {
		t.Error("despejou sem precisar")
	}
}

func TestDiskFreeTriggersEviction(t *testing.T) {
	f := newFixture(t, 0) // sem orçamento em bytes; só o disco importa
	var free int64
	f.j.MinFreeDisk = 900
	f.j.DiskFree = func() (int64, error) { return free, nil }
	f.add(t, "anon0001", 100, 2*time.Hour, "", nil)
	f.add(t, "anon0002", 100, time.Hour, "", nil)
	// o envio de 150 já está no spool: livre medido = 1000-150 = 850 < 900 →
	// precisa liberar 50 → despeja só o mais antigo
	free = 850
	if err := f.j.Reserve(context.Background(), 150, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if f.exists("anon0001") || !f.exists("anon0002") {
		t.Error("ordem de despejo por disco errada")
	}
	// livre 400 com um envio de 500 no spool: faltam 500, só há 100 despejáveis
	free = 400
	if err := f.j.Reserve(context.Background(), 500, func(context.Context) error { return nil }); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("disco cheio = %v", err)
	}
	if !f.exists("anon0002") {
		t.Error("não devia despejar nada quando não resolve")
	}
}

func TestSweep(t *testing.T) {
	f := newFixture(t, 250)
	f.add(t, "anon0001", 100, 3*time.Hour, "", nil)
	f.add(t, "anon0002", 100, 2*time.Hour, "", nil)
	f.add(t, "anon0003", 100, 1*time.Hour, "", nil)
	if err := f.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.exists("anon0001") || !f.exists("anon0002") || !f.exists("anon0003") {
		t.Error("Sweep devia despejar só o mais antigo")
	}
	// tudo protegido e acima do orçamento: Sweep só avisa
	g := newFixture(t, 10)
	g.add(t, "keyed001", 100, time.Hour, "k1", nil)
	if err := g.j.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep sem nada despejável = %v", err)
	}
}

func TestRemoveMissingFileIsNotFatal(t *testing.T) {
	f := newFixture(t, 1000)
	f.add(t, "anon0001", 10, time.Minute, "", nil)
	f.st.Delete(context.Background(), "anon0001.png")
	file, _ := f.db.GetFile(context.Background(), "anon0001")
	if err := f.j.Remove(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.db.FileExists(context.Background(), "anon0001"); ok {
		t.Error("linha devia ter sido removida")
	}
}

func TestReconcile(t *testing.T) {
	f := newFixture(t, 1000)
	for _, id := range []string{"anon0001", "anon0002", "anon0003"} {
		f.add(t, id, 10, time.Minute, "", nil)
	}
	f.st.Delete(context.Background(), "anon0002.png")
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	if !f.exists("anon0001") || !f.exists("anon0003") {
		t.Error("Reconcile removeu linha com arquivo presente")
	}
	if ok, _ := f.db.FileExists(context.Background(), "anon0002"); ok {
		t.Error("linha órfã continuou")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	f := newFixture(t, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.j.Run(ctx, 5*time.Millisecond); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run não parou")
	}
}

func TestDiskFreeReal(t *testing.T) {
	fn := DiskFree(t.TempDir())
	if fn == nil {
		t.Skip("plataforma sem statfs")
	}
	n, err := fn()
	if err != nil || n <= 0 {
		t.Fatalf("DiskFree = %d, %v", n, err)
	}
}

// Disco baixo por causa externa (nem despejando tudo resolveria): a passada
// periódica não pode apagar os arquivos à toa.
func TestSweepKeepsFilesWhenDiskLowForExternalReasons(t *testing.T) {
	f := newFixture(t, 0)
	f.j.MinFreeDisk = 1 << 30
	f.j.DiskFree = func() (int64, error) { return 100 << 20, nil } // sempre 100MB livres
	for _, id := range []string{"anon0001", "anon0002", "anon0003"} {
		f.add(t, id, 100, time.Hour, "", nil)
	}
	if err := f.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"anon0001", "anon0002", "anon0003"} {
		if !f.exists(id) {
			t.Errorf("%s foi despejado sem adiantar nada", id)
		}
	}
	// ...mas o orçamento continua valendo na mesma passada
	f.j.MaxStorage = 250
	if err := f.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.exists("anon0001") || !f.exists("anon0002") || !f.exists("anon0003") {
		t.Error("com disco ignorado, o orçamento devia despejar só o mais antigo")
	}
	// envio com o disco nessa situação: 507 sem apagar nada
	err := f.j.Reserve(context.Background(), 10, func(context.Context) error { t.Fatal("commit não devia rodar"); return nil })
	if !errors.Is(err, ErrNoSpace) || !f.exists("anon0002") {
		t.Fatalf("Reserve = %v; anon0002 existe=%v", err, f.exists("anon0002"))
	}
}

// O envio já está no spool (mesmo volume), então o espaço livre medido já o
// desconta: não pode ser contado de novo.
func TestReserveDoesNotCountIncomingTwiceOnDisk(t *testing.T) {
	f := newFixture(t, 0)
	f.j.MinFreeDisk = 100
	f.j.DiskFree = func() (int64, error) { return 150, nil }
	f.add(t, "anon0001", 100, time.Hour, "", nil)
	committed := false
	if err := f.j.Reserve(context.Background(), 60, func(context.Context) error { committed = true; return nil }); err != nil || !committed {
		t.Fatalf("Reserve = %v, commit=%v", err, committed)
	}
	if !f.exists("anon0001") {
		t.Error("despejou sem precisar")
	}
}

// Cancelar o contexto (cliente desconectou) no meio da remoção não pode
// deixar a linha sem o arquivo.
func TestRemoveIgnoresCancelledContext(t *testing.T) {
	f := newFixture(t, 1000)
	f.add(t, "anon0001", 100, time.Minute, "", nil)
	file, _ := f.db.GetFile(context.Background(), "anon0001")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.j.Remove(ctx, file); err != nil {
		t.Fatal(err)
	}
	if f.exists("anon0001") {
		t.Error("arquivo/linha continuaram")
	}
	if n, _ := f.db.TotalBytes(context.Background()); n != 0 {
		t.Errorf("TotalBytes = %d depois de apagar tudo", n)
	}
}

// commit recebe um contexto que não é cancelado com a requisição.
func TestReserveCommitContextNotCancelled(t *testing.T) {
	f := newFixture(t, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	err := f.j.Reserve(ctx, 10, func(c context.Context) error {
		cancel()
		return c.Err()
	})
	if err != nil {
		t.Fatalf("commit viu o cancelamento: %v", err)
	}
}

// O total em cache acompanha envios e remoções sem reler o banco.
func TestRunningTotalTracksCommitsAndRemoves(t *testing.T) {
	f := newFixture(t, 300)
	f.add(t, "anon0001", 100, 3*time.Hour, "", nil)
	commit := func(id string, size int) func(context.Context) error {
		return func(ctx context.Context) error {
			f.add(t, id, size, time.Duration(0), "", nil)
			return nil
		}
	}
	if err := f.j.Reserve(context.Background(), 100, commit("anon0002", 100)); err != nil {
		t.Fatal(err)
	}
	file, _ := f.db.GetFile(context.Background(), "anon0002")
	if err := f.j.Remove(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	// total real = 100; 100 + 200 = 300 cabe sem despejar
	if err := f.j.Reserve(context.Background(), 200, commit("anon0003", 200)); err != nil {
		t.Fatal(err)
	}
	if !f.exists("anon0001") {
		t.Error("total em cache errado: despejou sem precisar")
	}
	// agora total = 300; mais 50 exige despejar o mais antigo
	if err := f.j.Reserve(context.Background(), 50, commit("anon0004", 50)); err != nil {
		t.Fatal(err)
	}
	if f.exists("anon0001") || !f.exists("anon0003") {
		t.Error("total em cache errado: devia despejar só o mais antigo")
	}
	if want, _ := f.db.TotalBytes(context.Background()); f.j.total != want {
		t.Errorf("total em cache = %d, banco = %d", f.j.total, want)
	}
}

// Arquivo com tamanho diferente do registrado (gravação truncada numa queda)
// é tratado como ausente: sai a linha e o arquivo.
func TestReconcileRemovesTruncatedFile(t *testing.T) {
	f := newFixture(t, 1000)
	f.add(t, "anon0001", 10, time.Minute, "", nil)
	f.add(t, "anon0002", 10, time.Minute, "", nil)
	f.st.Put(context.Background(), "anon0002.png", strings.NewReader("")) // truncado
	n, err := f.j.Reconcile(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Reconcile = %d, %v", n, err)
	}
	if !f.exists("anon0001") || f.exists("anon0002") {
		t.Error("Reconcile devia remover só o truncado (linha e arquivo)")
	}
}

// Quem desiste enquanto espera a trava sai da fila sem gravar nada.
func TestReserveGivesUpWhileWaitingForLock(t *testing.T) {
	f := newFixture(t, 1000)
	held, done := make(chan struct{}), make(chan struct{})
	go f.j.Reserve(context.Background(), 0, func(context.Context) error { close(held); <-done; return nil })
	<-held
	defer close(done)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	errc := make(chan error, 2)
	var called atomic.Bool
	go func() { errc <- f.j.Reserve(ctx, 10, func(context.Context) error { called.Store(true); return nil }) }()
	go func() { errc <- f.j.Sweep(ctx) }()
	for range 2 {
		select {
		case err := <-errc:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Reserve/Sweep = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Reserve/Sweep continuaram esperando a trava depois de o contexto acabar")
		}
	}
	if called.Load() {
		t.Fatal("commit rodou para quem desistiu")
	}
}

// Contexto já cancelado com a trava livre: commit não roda.
func TestReserveCancelledBeforeLock(t *testing.T) {
	f := newFixture(t, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := f.j.Reserve(ctx, 10, func(context.Context) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("Reserve = %v, commit chamado=%v", err, called)
	}
	// a trava não ficou presa
	if err := f.j.Reserve(context.Background(), 10, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
