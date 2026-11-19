package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Remote é o armazenamento por trás do WriteBack (na prática, o S3).
type Remote interface {
	Store
	Lister
}

// Variáveis (e não constantes) só para os testes poderem encurtá-las.
var (
	// writeBackRetryMin e writeBackRetryMax limitam a espera entre tentativas
	// de envio (dobra a cada falha).
	writeBackRetryMin = time.Second
	writeBackRetryMax = time.Minute
	// writeBackPutTimeout é o prazo de uma tentativa de envio de size bytes:
	// 20 s + 1 s a cada 256 KiB, no máximo 2 min (o mesmo do envio síncrono).
	writeBackPutTimeout = func(size int64) time.Duration {
		return min(20*time.Second+time.Duration(size)*time.Second/(256<<10), 2*time.Minute)
	}
)

// WriteBackOptions configura o WriteBack.
type WriteBackOptions struct {
	// MaxBytes limita o que pode ficar no disco esperando o envio. Acima
	// disso, Adopt envia na hora (como sem o WriteBack), o que devolve ao
	// cliente o erro de um bucket fora do ar em vez de encher o disco.
	// <= 0 = sem limite.
	MaxBytes int64
	// Workers é quantos envios rodam ao mesmo tempo (padrão 4).
	Workers int
	Log     *slog.Logger
}

// WriteBack põe um disco local na frente de um armazenamento remoto: Adopt
// move o arquivo do spool para <dir>/<chave> (um rename) e volta na hora; uma
// goroutine leva o arquivo ao remoto em segundo plano, tentando de novo até
// conseguir, e só então apaga a cópia local. Enquanto isso Open e Stat
// respondem com a cópia local, então o link funciona desde o primeiro
// instante com a imagem de verdade.
//
// A cópia local é a fonte de verdade até o envio terminar: se o processo
// cair, o próximo NewWriteBack encontra os arquivos em dir e Start retoma os
// envios. List junta o remoto com os pendentes para a reconciliação do boot
// não tomar um arquivo ainda não enviado por perdido.
//
// dir precisa estar no mesmo sistema de arquivos do spool (o rename não
// copia nada) e num volume persistente.
type WriteBack struct {
	remote   Remote
	dir      string
	maxBytes int64
	sem      chan struct{}
	log      *slog.Logger

	mu    sync.Mutex
	items map[string]*wbItem // chaves com cópia local ainda não enviada
	bytes int64              // soma dos tamanhos em items
	ctx   context.Context    // nil até Start
}

type wbItem struct {
	size    int64
	modTime time.Time
	cancel  context.CancelFunc // nil enquanto o envio não começou (antes de Start)
	done    chan struct{}      // fechado quando a goroutine de envio termina
}

// NewWriteBack cria (se preciso) dir e registra os arquivos que ficaram nele
// de uma execução anterior. Os envios só começam com Start.
func NewWriteBack(remote Remote, dir string, opt WriteBackOptions) (*WriteBack, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if opt.Workers <= 0 {
		opt.Workers = 4
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	w := &WriteBack{
		remote:   remote,
		dir:      dir,
		maxBytes: opt.MaxBytes,
		sem:      make(chan struct{}, opt.Workers),
		log:      opt.Log,
		items:    map[string]*wbItem{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !ValidKey(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		w.items[e.Name()] = &wbItem{size: fi.Size(), modTime: fi.ModTime()}
		w.bytes += fi.Size()
	}
	return w, nil
}

// Start começa os envios pendentes e passa a enviar os novos assim que
// chegam. Quando ctx acaba, os envios param; o que não foi enviado continua
// em dir para a próxima execução.
func (w *WriteBack) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ctx = ctx
	for key, it := range w.items {
		w.launch(key, it)
	}
}

// Pending devolve quantos arquivos (e quantos bytes) esperam o envio.
func (w *WriteBack) Pending() (int, int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.items), w.bytes
}

func (w *WriteBack) local(key string) string { return filepath.Join(w.dir, key) }

// launch dispara o envio de key. Chamar com w.mu e depois de Start.
func (w *WriteBack) launch(key string, it *wbItem) {
	ctx, cancel := context.WithCancel(w.ctx)
	it.cancel, it.done = cancel, make(chan struct{})
	go w.run(ctx, key, it)
}

// run tenta enviar key até conseguir ou ctx acabar (desligamento ou Delete).
func (w *WriteBack) run(ctx context.Context, key string, it *wbItem) {
	defer close(it.done)
	defer it.cancel()
	start := time.Now()
	wait := writeBackRetryMin
	for attempt := 1; ; attempt++ {
		err := w.send(ctx, key, it.size)
		if err == nil {
			if attempt > 1 {
				w.log.Info("arquivo enviado ao armazenamento depois de falhas", "key", key, "attempts", attempt,
					"elapsed", time.Since(start).Round(time.Millisecond).String())
			}
			break
		}
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("falha enviando arquivo ao armazenamento; tentando de novo", "key", key, "attempt", attempt,
			"retry_in", wait.String(), "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, writeBackRetryMax)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// se um Delete tirou o item no meio do caminho, é ele quem cuida da cópia local
	if w.items[key] == it {
		if err := os.Remove(w.local(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.log.Warn("apagando cópia local já enviada", "key", key, "err", err)
		}
		delete(w.items, key)
		w.bytes -= it.size
	}
}

// send faz uma tentativa de envio, respeitando o limite de envios simultâneos.
func (w *WriteBack) send(ctx context.Context, key string, size int64) error {
	select {
	case w.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.sem }()
	ctx, cancel := context.WithTimeout(ctx, writeBackPutTimeout(size))
	defer cancel()
	fh, err := os.Open(w.local(key))
	if err != nil {
		return err
	}
	defer fh.Close()
	n, err := w.remote.Put(ctx, key, fh)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("o armazenamento gravou %d bytes, esperado %d", n, size)
	}
	return nil
}

// Adopt move srcPath (já com fsync) para a fila de envio e volta sem esperar
// o remoto. Se a fila passou de MaxBytes ou o rename falhar, envia na hora
// com ctx, e quem chama continua responsável por apagar srcPath.
func (w *WriteBack) Adopt(ctx context.Context, key, srcPath string) error {
	if !ValidKey(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	fi, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	size := fi.Size()

	w.mu.Lock()
	if _, dup := w.items[key]; dup {
		w.mu.Unlock()
		return fmt.Errorf("writeback: %s já está na fila", key)
	}
	if w.maxBytes > 0 && w.bytes+size > w.maxBytes {
		pending, bytes := len(w.items), w.bytes
		w.mu.Unlock()
		w.log.Warn("fila de envio ao armazenamento cheia; enviando na hora", "key", key,
			"pending", pending, "pending_bytes", bytes, "max", w.maxBytes)
		return w.putNow(ctx, key, srcPath)
	}
	w.bytes += size // reserva o espaço enquanto o rename acontece fora da trava
	w.mu.Unlock()

	if err := os.Rename(srcPath, w.local(key)); err != nil {
		w.mu.Lock()
		w.bytes -= size
		w.mu.Unlock()
		w.log.Warn("não foi possível pôr o arquivo na fila de envio; enviando na hora", "key", key, "err", err)
		return w.putNow(ctx, key, srcPath)
	}
	syncDir(w.dir)

	w.mu.Lock()
	defer w.mu.Unlock()
	it := &wbItem{size: size, modTime: time.Now()}
	w.items[key] = it
	if w.ctx != nil {
		w.launch(key, it)
	}
	return nil
}

func (w *WriteBack) putNow(ctx context.Context, key, srcPath string) error {
	fh, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = w.remote.Put(ctx, key, fh)
	return err
}

// Put envia direto ao remoto, sem passar pela fila.
func (w *WriteBack) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	return w.remote.Put(ctx, key, r)
}

// Open serve a cópia local enquanto ela existir; depois, o remoto.
func (w *WriteBack) Open(ctx context.Context, key string) (File, error) {
	w.mu.Lock()
	if w.items[key] != nil {
		// com a trava, run não apaga o arquivo entre a consulta e o open; e
		// depois de aberto ele continua legível mesmo se for apagado
		f, err := os.Open(w.local(key))
		w.mu.Unlock()
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	} else {
		w.mu.Unlock()
	}
	return w.remote.Open(ctx, key)
}

// Stat consulta a cópia local enquanto ela existir; depois, o remoto.
func (w *WriteBack) Stat(ctx context.Context, key string) (Info, error) {
	w.mu.Lock()
	if it := w.items[key]; it != nil {
		info := Info{Size: it.size, ModTime: it.modTime}
		w.mu.Unlock()
		return info, nil
	}
	w.mu.Unlock()
	return w.remote.Stat(ctx, key)
}

// Delete cancela o envio pendente (esperando a tentativa em curso parar),
// apaga a cópia local e apaga no remoto, onde o objeto pode já ter chegado.
func (w *WriteBack) Delete(ctx context.Context, key string) error {
	w.mu.Lock()
	it := w.items[key]
	if it != nil {
		delete(w.items, key)
		w.bytes -= it.size
		if it.cancel != nil {
			it.cancel()
		}
	}
	w.mu.Unlock()
	if it != nil {
		if it.done != nil {
			select {
			case <-it.done:
			case <-ctx.Done():
				// segue: um PUT que ainda chegue ao bucket vira órfão, e a
				// reconciliação do boot o apaga
			}
		}
		if err := os.Remove(w.local(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	err := w.remote.Delete(ctx, key)
	if it != nil && errors.Is(err, ErrNotFound) {
		return nil // existia (localmente) e foi apagado
	}
	return err
}

// List percorre o remoto e os pendentes juntos, em ordem crescente de chave e
// sem repetir uma chave que esteja nos dois (vale a cópia local).
func (w *WriteBack) List(ctx context.Context, fn func(key string, info Info) error) error {
	type entry struct {
		key  string
		info Info
	}
	w.mu.Lock()
	local := make([]entry, 0, len(w.items))
	for k, it := range w.items {
		local = append(local, entry{k, Info{Size: it.size, ModTime: it.modTime}})
	}
	w.mu.Unlock()
	slices.SortFunc(local, func(a, b entry) int { return strings.Compare(a.key, b.key) })

	i := 0
	err := w.remote.List(ctx, func(key string, info Info) error {
		for ; i < len(local) && local[i].key < key; i++ {
			if err := fn(local[i].key, local[i].info); err != nil {
				return err
			}
		}
		if i < len(local) && local[i].key == key {
			info = local[i].info
			i++
		}
		return fn(key, info)
	})
	if err != nil {
		return err
	}
	for ; i < len(local); i++ {
		if err := fn(local[i].key, local[i].info); err != nil {
			return err
		}
	}
	return nil
}
