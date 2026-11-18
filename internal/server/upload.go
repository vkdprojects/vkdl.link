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
	defer os.Remove(sp.path) // no sucesso o arquivo já foi movido; aí isso é um no-op

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

	err = s.janitor.Reserve(ctx, f.Size, func(ctx context.Context) error { return s.commit(ctx, &f, sp.path) })
	if errors.Is(err, janitor.ErrNoSpace) {
		writeError(w, http.StatusInsufficientStorage, "storage_full", "Sem espaço no servidor no momento. Tente mais tarde.")
		return
	} else if err != nil {
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

// commit escolhe um id livre, grava a linha e depois move o arquivo para o
// armazenamento. Roda com a trava do janitor, então a checagem de id não tem
// corrida. A linha vem antes do arquivo de propósito: se o processo cair no
// meio, sobra uma linha sem arquivo, que a reconciliação do boot remove; o
// contrário (arquivo sem linha) ficaria invisível para o orçamento. O id só
// chega ao cliente depois daqui, então ninguém vê o intervalo.
func (s *Server) commit(ctx context.Context, f *db.File, spoolPath string) error {
	for range 10 {
		f.ID = ident.NewFileID()
		exists, err := s.db.FileExists(ctx, f.ID)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		err = s.db.InsertFile(ctx, *f)
		if errors.Is(err, db.ErrConflict) {
			continue // não deveria acontecer com a trava, mas não custa
		}
		if err != nil {
			return err
		}
		if err := s.putFile(ctx, f.Key(), spoolPath); err != nil {
			if derr := s.db.DeleteFile(ctx, f.ID); derr != nil {
				s.log.Error("desfazendo linha de envio que falhou", "id", f.ID, "err", derr)
			}
			return err
		}
		return nil
	}
	return errors.New("não foi possível gerar um id livre")
}

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
