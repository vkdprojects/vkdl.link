// Package janitor mantém o armazenamento dentro do orçamento: despeja os
// arquivos despejáveis mais antigos primeiro e remove linhas órfãs no boot.
package janitor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// ErrNoSpace indica que nem despejando tudo o que é despejável cabe o arquivo novo.
var ErrNoSpace = errors.New("janitor: sem espaço e nada despejável")

// Janitor coordena despejo e gravação. Todos os campos exportados devem ser
// preenchidos antes do primeiro uso; DiskFree e Now são opcionais.
//
// O espaço livre medido por DiskFree já desconta o envio que está entrando,
// porque ele é gravado no spool (mesmo volume) antes de Reserve; adotá-lo é
// só um rename, que não ocupa mais nada.
type Janitor struct {
	DB          *db.DB
	Store       store.Store
	MaxStorage  int64                 // orçamento total em bytes; <= 0 desativa
	MinFreeDisk int64                 // espaço livre mínimo no volume; <= 0 desativa
	DiskFree    func() (int64, error) // espaço livre no volume de dados (nil = não verifica)
	Now         func() time.Time
	Log         *slog.Logger

	mu      sync.Mutex // serializa despejo + gravação + remoção para o orçamento não estourar em paralelo
	total   int64      // soma dos tamanhos no banco (cache); vale só quando totalOK
	totalOK bool       // false = recarregar total do banco na próxima passada
}

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
// com a trava, executa commit, que deve gravar o arquivo e a linha no banco.
// commit recebe um contexto que não é cancelado com a requisição: depois que
// começou, a gravação sempre termina (ou é desfeita) por inteiro.
// Devolve ErrNoSpace se não houver como abrir espaço.
func (j *Janitor) Reserve(ctx context.Context, incoming int64, commit func(context.Context) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
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
	j.mu.Lock()
	defer j.mu.Unlock()
	err := j.ensure(ctx, 0)
	if errors.Is(err, ErrNoSpace) {
		j.log().Warn("armazenamento acima do limite e nada mais é despejável")
		return nil
	}
	return err
}

// Remove apaga um arquivo de forma consistente: primeiro o conteúdo (se já
// não existir, tudo bem), depois a linha. Se o conteúdo não puder ser
// apagado, a linha fica para uma nova tentativa.
func (j *Janitor) Remove(ctx context.Context, f db.File) error {
	j.mu.Lock()
	defer j.mu.Unlock()
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
// caso o arquivo também é apagado.
func (j *Janitor) Reconcile(ctx context.Context) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.totalOK = false // recalcula o total do banco na próxima passada
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
