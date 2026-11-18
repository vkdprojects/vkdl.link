package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/vkdprojects/vkdl.link/internal/db"
	"github.com/vkdprojects/vkdl.link/internal/ident"
	"github.com/vkdprojects/vkdl.link/internal/janitor"
	"github.com/vkdprojects/vkdl.link/internal/ratelimit"
	"github.com/vkdprojects/vkdl.link/internal/sniff"
	"github.com/vkdprojects/vkdl.link/internal/store"
)

// multipartSlack é a folga sobre VKDL_MAX_UPLOAD para cabeçalhos e
// delimitadores do multipart; o arquivo em si nunca passa de MaxUpload.
const multipartSlack = 1 << 20

var (
	errNoFile   = errors.New("nenhum arquivo enviado")
	errTooLarge = errors.New("arquivo grande demais")
	// errStorageTimeout indica que o armazenamento não terminou o envio (ou a
	// trava do janitor não ficou livre) dentro do prazo.
	errStorageTimeout = errors.New("o armazenamento não respondeu a tempo")
)

// Prazos da gravação de um envio. O WriteTimeout do http.Server (120 s) conta
// desde o cabeçalho da requisição e não cancela o handler: um envio que
// terminasse depois dele ficaria gravado e o cliente receberia uma resposta
// vazia. Por isso o handler estende o prazo de escrita para cobrir estes
// prazos mais uma folga, e o trabalho sempre acaba antes da resposta.
const (
	putBudgetBase = 20 * time.Second
	putBudgetRate = 256 << 10 // bytes/s: a vazão mínima que esperamos do armazenamento
	putBudgetMax  = 2 * time.Minute
	// responseSlack cobre desfazer um envio que falhou (discardTimeout) e
	// escrever a resposta depois do último prazo.
	responseSlack  = 15 * time.Second
	discardTimeout = 10 * time.Second
)

// Variáveis só para os testes poderem encurtá-las.
var (
	// putBudget é o prazo para levar size bytes ao armazenamento.
	putBudget = func(size int64) time.Duration {
		return min(putBudgetBase+time.Duration(size)*time.Second/putBudgetRate, putBudgetMax)
	}
	// reserveBudget é o prazo para esperar a trava do janitor e gravar a linha.
	reserveBudget = 30 * time.Second
)

type uploadResponse struct {
	ID          string  `json:"id"`
	URL         string  `json:"url"`
	DeleteURL   string  `json:"delete_url"`
	DeleteToken string  `json:"delete_token"`
	Size        int64   `json:"size"`
	MIME        string  `json:"mime"`
	ExpiresAt   *string `json:"expires_at"`
}

func setUploadCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Expose-Headers", "Retry-After")
	h.Set("Access-Control-Max-Age", "86400")
}

func (s *Server) handleUploadPreflight(w http.ResponseWriter, r *http.Request) {
	setUploadCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	setUploadCORS(w)
	ctx := r.Context()

	key, err := s.apiKey(r)
	if errors.Is(err, errBadKey) {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "Chave de API inválida ou revogada.")
		return
	} else if err != nil {
		s.internal(w, "consultando chave de API", err)
		return
	}

	limiter, bucket := s.anonRL, "ip:"+ipHashFrom(r)
	if key != nil {
		limiter, bucket = s.keyRL, "key:"+key.ID
	}
	if ok, wait := limiter.Allow(bucket); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(wait)))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Muitos envios em pouco tempo. Aguarde e tente de novo.")
		return
	}

	limit := s.opt.MaxUpload + multipartSlack
	if r.ContentLength > limit {
		s.tooLarge(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	src, err := uploadSource(r)
	if err != nil {
		s.uploadReadError(w, err)
		return
	}
	br := bufio.NewReaderSize(src, 4096)
	head, perr := br.Peek(sniff.HeaderLen)
	if len(head) == 0 {
		if perr != nil && !errors.Is(perr, io.EOF) {
			s.uploadReadError(w, clientError{perr})
			return
		}
		writeError(w, http.StatusBadRequest, "no_file", "Nenhum arquivo enviado (use o campo multipart \"file\" ou o corpo cru).")
		return
	}
	kind, ok := sniff.Detect(head)
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "Só aceitamos imagens PNG, JPEG, GIF ou WebP.")
		return
	}

	sp, err := s.spool(br)
	if err != nil {
		s.uploadReadError(w, err)
		return
	}
	// no FS, o arquivo já foi movido e isso é um no-op; no S3 e em qualquer
	// falha antes da adoção é aqui que o temporário some
	defer os.Remove(sp.path)

	// a resposta precisa poder sair depois de todo o trabalho (veja putBudget)
	deadline := time.Now().Add(putBudget(sp.size) + reserveBudget + responseSlack)
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.log.Warn("estendendo o prazo da resposta do envio", "err", err)
	}

	now := s.opt.Now().UTC()
	token := ident.NewDeleteToken()
	f := db.File{
		Ext:             kind.Ext,
		MIME:            kind.MIME,
		Size:            sp.size,
		SHA256:          sp.sha256,
		DeleteTokenHash: ident.HashSecret(token),
		IPHash:          ipHashFrom(r),
		CreatedAt:       now,
	}
	if key != nil {
		f.APIKeyID = key.ID
		if key.RetentionDays > 0 {
			exp := now.Add(time.Duration(key.RetentionDays) * 24 * time.Hour)
			f.ExpiresAt = &exp
		}
	}

	err = s.commit(ctx, &f, sp.path)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// o cliente desistiu (ou caiu): não há para quem responder, e commit
		// já desfez o que tinha gravado
		s.log.Info("envio abandonado pelo cliente", "size", f.Size, "err", err)
		return
	case errors.Is(err, janitor.ErrNoSpace):
		writeError(w, http.StatusInsufficientStorage, "storage_full", "Sem espaço no servidor no momento. Tente mais tarde.")
		return
	case errors.Is(err, errStorageTimeout):
		s.log.Error("erro interno", "op", "gravando envio", "err", err)
		writeError(w, http.StatusServiceUnavailable, "storage_timeout", "O armazenamento demorou demais para responder. Tente de novo.")
		return
	default:
		s.internal(w, "gravando envio", err)
		return
	}

	resp := uploadResponse{
		ID:          f.ID,
		URL:         s.opt.BaseURL + "/" + f.Key(),
		DeleteURL:   s.opt.BaseURL + "/v1/files/" + f.ID,
		DeleteToken: token,
		Size:        f.Size,
		MIME:        f.MIME,
	}
	if f.ExpiresAt != nil {
		v := f.ExpiresAt.UTC().Format(time.RFC3339)
		resp.ExpiresAt = &v
	}
	w.Header().Set("Location", resp.URL)
	writeJSON(w, http.StatusCreated, resp)
}

// commit grava o envio: escolhe um id livre, leva o arquivo ao armazenamento
// e só então, com a trava do janitor, abre espaço e grava a linha.
//
// O envio dos bytes fica fora da trava de propósito: no S3 ele leva o tempo
// da rede, e com a trava cada envio esperaria todos os da fila (e o despejo e
// os DELETE esperariam também). Se o processo cair entre o arquivo e a linha,
// sobra um objeto sem linha, que a reconciliação do boot apaga depois de
// OrphanMinAge (bem maior que putBudgetMax). Se qualquer passo falhar ou o
// cliente desistir antes da linha, o objeto é apagado. O id só chega ao
// cliente depois daqui, então ninguém vê o intervalo.
func (s *Server) commit(ctx context.Context, f *db.File, spoolPath string) error {
	id, release, err := s.claimID(ctx)
	if err != nil {
		return err
	}
	defer release()
	f.ID = id

	pctx, cancel := context.WithTimeout(ctx, putBudget(f.Size))
	err = s.putFile(pctx, f.Key(), spoolPath)
	err = budgetErr(ctx, pctx, err)
	cancel()
	if err == nil {
		rctx, cancel := context.WithTimeout(ctx, reserveBudget)
		err = s.janitor.Reserve(rctx, f.Size, func(ctx context.Context) error { return s.db.InsertFile(ctx, *f) })
		err = budgetErr(ctx, rctx, err)
		cancel()
	}
	if err != nil {
		s.discard(ctx, f)
		return err
	}
	return nil
}

// budgetErr marca com errStorageTimeout o erro de um passo que estourou o
// próprio prazo (sub), desde que o cliente (parent) ainda esteja lá.
func budgetErr(parent, sub context.Context, err error) error {
	if err != nil && parent.Err() == nil && errors.Is(sub.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", errStorageTimeout, err)
	}
	return err
}

// discard apaga o objeto de um envio que não virou linha. Um PUT que falhou
// por tempo esgotado pode ter chegado ao bucket mesmo assim; a chave é nova e
// só deste envio (claimID), então apagar é seguro. Roda com prazo curto e sem
// a trava; se falhar, a reconciliação do boot pega o órfão.
func (s *Server) discard(ctx context.Context, f *db.File) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	if err := s.store.Delete(ctx, f.Key()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("apagando objeto de envio que falhou", "id", f.ID, "err", err)
	}
}

// claimID escolhe um id que não está no banco nem em outro envio em
// andamento e o reserva até release. Como o arquivo vai ao armazenamento antes
// da linha, sem a reserva dois envios simultâneos com o mesmo id gravariam na
// mesma chave, e o que falhasse no banco apagaria o arquivo do outro.
func (s *Server) claimID(ctx context.Context) (string, func(), error) {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	for range 10 {
		id := ident.NewFileID()
		if _, busy := s.pendingIDs[id]; busy {
			continue
		}
		exists, err := s.db.FileExists(ctx, id)
		if err != nil {
			return "", nil, err
		}
		if exists {
			continue
		}
		s.pendingIDs[id] = struct{}{}
		return id, func() {
			s.idMu.Lock()
			delete(s.pendingIDs, id)
			s.idMu.Unlock()
		}, nil
	}
	return "", nil, errors.New("não foi possível gerar um id livre")
}

// putFile leva o temporário do spool para o armazenamento: rename no disco
// (FileAdopter) ou envio dos bytes (S3). Em qualquer caso quem chama apaga o
// temporário depois (handleUpload faz isso com defer).
func (s *Server) putFile(ctx context.Context, key, path string) error {
	if a, ok := s.store.(store.FileAdopter); ok {
		return a.Adopt(ctx, key, path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = s.store.Put(ctx, key, fh)
	return err
}

// uploadSource devolve o leitor do conteúdo: a parte "file" de um
// multipart/form-data ou, para qualquer outro Content-Type, o corpo cru.
func uploadSource(r *http.Request) (io.Reader, error) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "multipart/form-data" {
		return r.Body, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, clientError{err}
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errNoFile
		}
		if err != nil {
			return nil, clientError{err}
		}
		if part.FormName() == "file" {
			return part, nil
		}
		// outros campos são descartados (e contam no limite do corpo)
		if _, err := io.Copy(io.Discard, part); err != nil {
			return nil, clientError{err}
		}
	}
}

// clientError marca falhas ao ler o que o cliente mandou (corpo truncado,
// multipart malformado, limite estourado), em oposição a falhas do servidor.
type clientError struct{ err error }

func (e clientError) Error() string { return e.err.Error() }
func (e clientError) Unwrap() error { return e.err }

// trackingReader lembra o erro de leitura para separar culpa do cliente de falha de disco.
type trackingReader struct {
	r   io.Reader
	err error
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

type spooled struct {
	path   string
	size   int64
	sha256 string
}

// spool grava o envio num temporário do disco de dados calculando tamanho e SHA-256.
func (s *Server) spool(r io.Reader) (spooled, error) {
	tmp, err := os.CreateTemp(s.opt.SpoolDir, "up-*")
	if err != nil {
		return spooled{}, fmt.Errorf("criando temporário: %w", err)
	}
	h := sha256.New()
	tr := &trackingReader{r: r}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(tr, s.opt.MaxUpload+1))
	if err != nil && tr.err != nil {
		err = clientError{err}
	}
	if err == nil && n <= s.opt.MaxUpload {
		// garante os bytes no disco antes do rename e da linha no banco;
		// senão uma queda do sistema pode deixar a linha apontando para um
		// arquivo vazio ou truncado
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > s.opt.MaxUpload {
		err = errTooLarge
	}
	if err != nil {
		os.Remove(tmp.Name())
		return spooled{}, err
	}
	return spooled{path: tmp.Name(), size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s *Server) tooLarge(w http.ResponseWriter) {
	writeError(w, http.StatusRequestEntityTooLarge, "too_large",
		fmt.Sprintf("Arquivo grande demais (máximo %s).", humanBytes(s.opt.MaxUpload)))
}

// uploadReadError traduz falhas de leitura do corpo para o status certo.
func (s *Server) uploadReadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	var ce clientError
	switch {
	case errors.Is(err, errTooLarge), errors.As(err, &mbe), errors.Is(err, multipart.ErrMessageTooLarge):
		s.tooLarge(w)
	case errors.Is(err, errNoFile):
		writeError(w, http.StatusBadRequest, "no_file", "Nenhum arquivo enviado (use o campo multipart \"file\").")
	case errors.As(err, &ce):
		writeError(w, http.StatusBadRequest, "bad_request", "Não foi possível ler o envio.")
	default:
		s.internal(w, "recebendo envio", err)
	}
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error("erro interno", "op", what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "Erro interno.")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
