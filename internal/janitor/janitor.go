// Package janitor mantém o armazenamento dentro do orçamento: despeja os
// arquivos despejáveis mais antigos primeiro e remove linhas órfãs no boot.
package janitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// ErrNoSpace indica que nem despejando tudo o que é despejável cabe o arquivo novo.
var ErrNoSpace = errors.New("janitor: sem espaço e nada despejável")

// OrphanMinAge é a idade mínima de um objeto sem linha no banco para a
// reconciliação apagá-lo. A folga protege uma gravação que ainda esteja
// chegando ao armazenamento.
const OrphanMinAge = 10 * time.Minute

// errUnsorted indica que o armazenamento listou fora de ordem; a reconciliação
// então volta ao Stat por linha.
var errUnsorted = errors.New("janitor: listagem do armazenamento fora de ordem")

// Janitor coordena despejo e gravação. Todos os campos exportados devem ser
// preenchidos antes do primeiro uso; DiskFree e Now são opcionais.
//
// O espaço livre medido por DiskFree já desconta o envio que está entrando,
// porque ele é gravado no spool (mesmo volume) e adotado (um rename, que não
// ocupa mais nada) antes de Reserve. Com o armazenamento S3 os arquivos
// não ficam no disco local (só os que esperam o envio, limitados por
// VKDL_PENDING_MAX): DiskFree e MinFreeDisk ficam zerados e o único limite é
// MaxStorage.
type Janitor struct {
	DB          *db.DB
	Store       store.Store
	MaxStorage  int64                 // orçamento total em bytes; <= 0 desativa
	MinFreeDisk int64                 // espaço livre mínimo no volume; <= 0 desativa
	DiskFree    func() (int64, error) // espaço livre no volume de dados (nil = não verifica)
	Now         func() time.Time
	Log         *slog.Logger

	// sem é a trava (semáforo de uma vaga) que serializa despejo + gravação +
	// remoção para o orçamento não estourar em paralelo. Diferente de um
	// sync.Mutex, a espera respeita o contexto: quem desistiu sai da fila.
	sem     chan struct{}
	semOnce sync.Once
	total   int64 // soma dos tamanhos no banco (cache); vale só quando totalOK
	totalOK bool  // false = recarregar total do banco na próxima passada
}

// lock pega a trava ou desiste quando ctx acaba.
func (j *Janitor) lock(ctx context.Context) error {
	j.semOnce.Do(func() { j.sem = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case j.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil { // o select escolhe ao acaso se os dois estavam prontos
		j.unlock()
		return err
	}
	return nil
}

func (j *Janitor) unlock() { <-j.sem }

func (j *Janitor) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Janitor) log() *slog.Logger {
	if j.Log != nil {
		return j.Log
	}
	return slog.Default()
}

// Reserve garante espaço para incoming bytes (despejando se preciso) e, ainda
// com a trava, executa commit, que deve gravar a linha no banco. commit roda
// com a trava, então deve ser rápido: o envio dos bytes ao armazenamento
// acontece antes, fora daqui. Se ctx acabar enquanto espera a trava (cliente
// que desistiu), devolve ctx.Err() sem chamar commit. Depois que commit
// começou, o contexto dele não é mais cancelado com a requisição.
// Devolve ErrNoSpace se não houver como abrir espaço.
func (j *Janitor) Reserve(ctx context.Context, incoming int64, commit func(context.Context) error) error {
	if err := j.lock(ctx); err != nil {
		return err
	}
	defer j.unlock()
	if err := j.ensure(ctx, incoming); err != nil {
		return err
	}
	if err := commit(context.WithoutCancel(ctx)); err != nil {
		j.totalOK = false // não sabemos o que ficou no banco; recarrega na próxima
		return err
	}
	if j.totalOK {
		j.total += incoming
	}
	return nil
}

// Sweep roda uma passada de despejo sem arquivo entrando. Ficar sem nada
// para despejar aqui não é erro: só registra um aviso.
func (j *Janitor) Sweep(ctx context.Context) error {
	if err := j.lock(ctx); err != nil {
		return err
	}
	defer j.unlock()
	err := j.ensure(ctx, 0)
	if errors.Is(err, ErrNoSpace) {
		j.log().Warn("armazenamento acima do limite e nada mais é despejável")
		return nil
	}
	return err
}

// Remove apaga um arquivo de forma consistente: primeiro o conteúdo (se já
// não existir, tudo bem), depois a linha. Se o conteúdo não puder ser
// apagado, a linha fica para uma nova tentativa. A remoção pedida acontece
// mesmo que o cliente desista enquanto espera a trava.
func (j *Janitor) Remove(ctx context.Context, f db.File) error {
	if err := j.lock(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	defer j.unlock()
	return j.remove(ctx, f)
}

// remove é Remove sem a trava (quem chama já a tem). Ignora o cancelamento de
// ctx: com o conteúdo apagado, a linha precisa sair junto, senão a contagem
// de bytes fica errada até o próximo boot.
func (j *Janitor) remove(ctx context.Context, f db.File) error {
	ctx = context.WithoutCancel(ctx)
	if err := j.Store.Delete(ctx, f.Key()); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	err := j.DB.DeleteFile(ctx, f.ID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.totalOK {
		j.total -= f.Size
	}
	return nil
}

func (j *Janitor) ensure(ctx context.Context, incoming int64) error {
	if !j.totalOK {
		t, err := j.DB.TotalBytes(ctx)
		if err != nil {
			return err
		}
		j.total, j.totalOK = t, true
	}
	free := int64(-1) // -1 = desconhecido
	if j.DiskFree != nil && j.MinFreeDisk > 0 {
		var err error
		if free, err = j.DiskFree(); err != nil {
			j.log().Warn("não foi possível medir o espaço livre", "err", err)
			free = -1
		}
	}

	// Quantos bytes precisam sair por cada critério. free já desconta o envio
	// (veja o comentário de Janitor), então incoming não entra na conta do disco.
	var needBudget, needDisk int64
	if j.MaxStorage > 0 && j.total+incoming > j.MaxStorage {
		needBudget = j.total + incoming - j.MaxStorage
	}
	if free >= 0 && free < j.MinFreeDisk {
		needDisk = j.MinFreeDisk - free
	}
	if needBudget == 0 && needDisk == 0 {
		return nil
	}

	// Antes de apagar qualquer coisa, confere se despejar resolveria; se não
	// resolver, não vale a pena perder arquivos à toa. A soma para assim que
	// alcança o necessário, então no caso comum só lê os poucos mais antigos.
	now := j.now()
	avail, err := j.DB.EvictableBytesUpTo(ctx, now, max(needBudget, needDisk))
	if err != nil {
		return err
	}
	if avail < needDisk {
		if incoming > 0 {
			return ErrNoSpace
		}
		// Na passada periódica, disco baixo que nem despejando tudo se resolve
		// vem de fora do vkdl (imagens Docker, cache de build...). Apagar os
		// arquivos não adiantaria; segue só com o orçamento.
		j.log().Warn("disco baixo por causa externa; despejo não resolveria",
			"free", free, "min_free", j.MinFreeDisk, "evictable", avail)
		needDisk = 0
	}
	if avail < needBudget && incoming > 0 {
		return ErrNoSpace
	}
	need := max(needBudget, needDisk)
	if need == 0 {
		return nil
	}

	evicted, freed := 0, int64(0)
	defer func() {
		if evicted > 0 {
			j.log().Info("despejo", "files", evicted, "bytes", freed, "incoming", incoming)
		}
	}()
	for freed < need {
		batch, err := j.DB.OldestEvictable(ctx, now, 100)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return ErrNoSpace
		}
		for _, f := range batch {
			if freed >= need {
				break
			}
			if err := j.remove(ctx, f); err != nil {
				return err
			}
			evicted++
			freed += f.Size
		}
	}
	return nil
}

// Reconcile remove as linhas cujo arquivo sumiu do armazenamento ou não tem o
// tamanho registrado (gravação truncada por uma queda do sistema); nesse
// caso o arquivo também é apagado. Devolve quantas linhas saíram.
//
// Se o armazenamento implementa store.Lister, a conferência é feita com uma
// listagem só (no S3, um Stat por linha seria uma requisição por arquivo) e,
// de quebra, apaga objetos sem linha mais velhos que OrphanMinAge, desde que
// a listagem não pareça vir de um banco vazio ou trocado (veja reconcileByList).
func (j *Janitor) Reconcile(ctx context.Context) (int, error) {
	if err := j.lock(ctx); err != nil {
		return 0, err
	}
	defer j.unlock()
	j.totalOK = false // recalcula o total do banco na próxima passada
	if l, ok := j.Store.(store.Lister); ok {
		n, err := j.reconcileByList(ctx, l)
		if !errors.Is(err, errUnsorted) {
			return n, err
		}
		j.log().Warn("reconciliação: listagem fora de ordem; conferindo arquivo por arquivo")
	}
	return j.reconcileByStat(ctx)
}

func (j *Janitor) reconcileByStat(ctx context.Context) (int, error) {
	removed, after := 0, ""
	for {
		page, err := j.DB.FilesAfter(ctx, after, 500)
		if err != nil {
			return removed, err
		}
		if len(page) == 0 {
			return removed, nil
		}
		for _, f := range page {
			info, err := j.Store.Stat(ctx, f.Key())
			missing := errors.Is(err, store.ErrNotFound)
			if err != nil && !missing {
				return removed, err
			}
			if !missing && info.Size == f.Size {
				continue
			}
			if !missing {
				j.log().Warn("reconciliação: tamanho diverge do banco", "id", f.ID, "want", f.Size, "got", info.Size)
			}
			if err := j.remove(ctx, f); err != nil {
				return removed, err
			}
			removed++
		}
		after = page[len(page)-1].ID
	}
}

// reconcileByList cruza a listagem do armazenamento com as linhas do banco,
// as duas em ordem crescente (chave "<id>.<ext>" e id têm a mesma ordem, já
// que os ids têm tamanho fixo). Nada é apagado antes de a listagem terminar,
// então uma listagem que falha ou sai fora de ordem não deixa estrago.
//
// Objetos órfãos só são apagados quando o banco tem linhas e os órfãos são no
// máximo metade do que foi listado. Banco vazio (volume novo, VKDL_DATA_DIR
// errado) ou banco que não conhece a maior parte do armazenamento (backup
// antigo, banco de outra instância) apontado para o bucket de produção
// apagaria tudo sem volta; nesses casos só registra um erro.
func (j *Janitor) reconcileByList(ctx context.Context, l store.Lister) (int, error) {
	rows := &rowCursor{db: j.DB}
	var bad []db.File    // linhas sem objeto ou com tamanho diferente
	var orphans []string // objetos sem linha, velhos o bastante
	listed, matched, prev := 0, 0, ""
	cutoff := j.now().Add(-OrphanMinAge)
	err := l.List(ctx, func(key string, info store.Info) error {
		if listed > 0 && key <= prev {
			return errUnsorted
		}
		listed, prev = listed+1, key
		id, ext, _ := strings.Cut(key, ".")
		for {
			r, ok, err := rows.peek(ctx)
			if err != nil {
				return err
			}
			if !ok || r.ID > id {
				break
			}
			if r.ID < id { // a linha ficou para trás na listagem: o objeto não existe
				bad = append(bad, r)
				rows.next()
				continue
			}
			if r.Ext != ext {
				break // mesmo id, outra extensão: este objeto é órfão; a linha pode casar com a próxima chave
			}
			rows.next()
			matched++
			if info.Size != r.Size {
				j.log().Warn("reconciliação: tamanho diverge do banco", "id", r.ID, "want", r.Size, "got", info.Size)
				bad = append(bad, r)
			}
			return nil
		}
		if info.ModTime.Before(cutoff) {
			orphans = append(orphans, key)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for {
		r, ok, err := rows.peek(ctx)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		bad = append(bad, r)
		rows.next()
	}

	// Armazenamento vazio com linhas no banco quase sempre é configuração
	// errada (bucket, prefixo ou volume trocados, migração de fs para s3 sem
	// copiar os arquivos). Apagar tudo seria irreversível; só avisa.
	if listed == 0 && len(bad) > 0 {
		j.log().Error("reconciliação: o armazenamento está vazio mas o banco tem arquivos; nada foi removido (confira bucket/prefixo/volume)",
			"rows", len(bad))
		return 0, nil
	}

	if nrows := matched + len(bad); len(orphans) > 0 && nrows == 0 {
		j.log().Error("reconciliação: o banco está vazio mas o armazenamento tem objetos; nada foi apagado (confira VKDL_DATA_DIR/volume e bucket/prefixo)",
			"objects", listed, "orphans", len(orphans))
		orphans = nil
	} else if len(orphans)*2 > listed {
		j.log().Error("reconciliação: a maioria dos objetos não tem linha no banco; nenhum objeto foi apagado (confira VKDL_DATA_DIR/volume e bucket/prefixo)",
			"objects", listed, "orphans", len(orphans), "rows", nrows)
		orphans = nil
	}

	removed := 0
	for _, f := range bad {
		if err := j.remove(ctx, f); err != nil {
			return removed, err
		}
		removed++
	}
	for _, key := range orphans {
		if err := j.Store.Delete(ctx, key); err != nil && !errors.Is(err, store.ErrNotFound) {
			return removed, fmt.Errorf("apagando objeto órfão %s: %w", key, err)
		}
	}
	if len(orphans) > 0 {
		j.log().Warn("reconciliação: objetos sem linha no banco apagados", "objects", len(orphans))
	}
	return removed, nil
}

// rowCursor percorre as linhas de files em ordem de id, uma página por vez.
type rowCursor struct {
	db    *db.DB
	page  []db.File
	after string
	done  bool
}

// peek devolve a linha atual sem avançar; ok=false quando acabaram.
func (c *rowCursor) peek(ctx context.Context) (db.File, bool, error) {
	if len(c.page) == 0 && !c.done {
		page, err := c.db.FilesAfter(ctx, c.after, 500)
		if err != nil {
			return db.File{}, false, err
		}
		if len(page) == 0 {
			c.done = true
		} else {
			c.page, c.after = page, page[len(page)-1].ID
		}
	}
	if len(c.page) == 0 {
		return db.File{}, false, nil
	}
	return c.page[0], true, nil
}

func (c *rowCursor) next() { c.page = c.page[1:] }

// Run executa Sweep a cada interval até ctx ser cancelado.
func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := j.Sweep(ctx); err != nil && ctx.Err() == nil {
				j.log().Error("falha no despejo periódico", "err", err)
			}
		}
	}
}
