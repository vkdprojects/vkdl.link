package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Prazos das operações no S3. Stat e Delete rodam também com a trava do
// janitor (despejo, reconciliação) e com contextos que não são cancelados com
// a requisição; sem prazo, um endpoint travado seguraria tudo por minutos.
const (
	// s3PutTimeout é só o teto de segurança: quem envia (o handler de upload)
	// passa um prazo menor, proporcional ao tamanho.
	s3PutTimeout  = 5 * time.Minute
	s3MetaTimeout = 30 * time.Second
	// s3MaxRetries limita as tentativas do minio-go (o padrão é 10, que com um
	// endpoint mudo dá mais de 10 minutos por operação).
	s3MaxRetries = 3
	// s3StreamPartSize é o tamanho de parte (e do buffer em memória) quando o
	// tamanho do envio não é conhecido. Sem isso o minio-go escolheria partes
	// pensando em objetos de 5 TiB.
	s3StreamPartSize = 16 << 20
)

// Variáveis (e não constantes) só para os testes poderem encurtá-las.
var (
	// s3HeaderTimeout é quanto esperar pelos cabeçalhos da resposta depois de
	// mandar a requisição inteira. O padrão do minio-go é 1 minuto.
	s3HeaderTimeout = 15 * time.Second
	// s3OpenTimeout limita o HEAD do Open (o GET seguinte não tem prazo fixo,
	// para não cortar downloads lentos; ele fica limitado por s3HeaderTimeout).
	s3OpenTimeout = 10 * time.Second
	// s3ReadWindow é o maior trecho pedido num único GET. Um GET aberto até o
	// fim do objeto que é abandonado (Range de 1 byte, cliente que desiste)
	// ainda pode trazer muito mais do que foi lido.
	s3ReadWindow int64 = 4 << 20
)

// PathStyle escolhe como o bucket entra na URL.
type PathStyle string

const (
	PathStyleAuto  PathStyle = "auto"  // o minio-go decide (DNS na AWS, caminho no resto)
	PathStyleTrue  PathStyle = "true"  // https://endpoint/bucket/chave (MinIO, Garage)
	PathStyleFalse PathStyle = "false" // https://bucket.endpoint/chave
)

// S3Config é o necessário para falar com um bucket compatível com S3.
type S3Config struct {
	Endpoint  string // host[:porta], sem esquema
	Region    string // vazio = descobre pelo GetBucketLocation
	Bucket    string
	KeyID     string // nunca logar
	KeySecret string // nunca logar
	Prefix    string // opcional; normalizado para terminar em "/"
	UseSSL    bool
	PathStyle PathStyle
}

// S3 guarda os arquivos em <prefix><id[0:2]>/<id>.<ext> de um bucket S3 (AWS,
// iDrive e2, MinIO, Garage...). O layout é o mesmo do FS, então migrar é
// copiar o diretório de arquivos para dentro do prefixo.
//
// Diferente do FS, Delete de uma chave que não existe devolve nil: o S3 não
// distingue os dois casos no DeleteObject e descobrir custaria uma requisição
// a mais por remoção. Quem chama (janitor) já trata ErrNotFound como sucesso.
type S3 struct {
	client   *minio.Client
	core     minio.Core
	bucket   string
	prefix   string
	endpoint string
}

// NewS3 valida a configuração e cria o cliente. Não fala com a rede; use Check.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.KeyID == "" || cfg.KeySecret == "" {
		return nil, errors.New("s3: endpoint, bucket e credenciais são obrigatórios")
	}
	lookup := minio.BucketLookupAuto
	switch cfg.PathStyle {
	case PathStyleAuto, "":
	case PathStyleTrue:
		lookup = minio.BucketLookupPath
	case PathStyleFalse:
		lookup = minio.BucketLookupDNS
	default:
		return nil, fmt.Errorf("s3: path style inválido %q (auto, true ou false)", cfg.PathStyle)
	}
	prefix, err := normalizePrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	tr, err := minio.DefaultTransport(cfg.UseSSL)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	tr.ResponseHeaderTimeout = s3HeaderTimeout
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.KeyID, cfg.KeySecret, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    tr,
		MaxRetries:   s3MaxRetries,
	})
	if err != nil {
		// a mensagem do minio-go fala só do endpoint, nunca das credenciais
		return nil, fmt.Errorf("s3: %w", err)
	}
	return &S3{client: c, core: minio.Core{Client: c}, bucket: cfg.Bucket, prefix: prefix, endpoint: cfg.Endpoint}, nil
}

// normalizePrefix tira barras do começo e garante uma no fim ("a/b" → "a/b/").
func normalizePrefix(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("s3: prefixo inválido %q", p)
		}
	}
	return p + "/", nil
}

// Endpoint, Bucket e Prefix servem para o log de boot (sem credenciais).
func (s *S3) Endpoint() string { return s.endpoint }
func (s *S3) Bucket() string   { return s.bucket }
func (s *S3) Prefix() string   { return s.prefix }

// Check confirma que o bucket existe e que as credenciais funcionam.
func (s *S3) Check(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("s3: consultando o bucket %q em %s: %w", s.bucket, s.endpoint, err)
	}
	if !ok {
		return fmt.Errorf("s3: o bucket %q não existe em %s", s.bucket, s.endpoint)
	}
	return nil
}

func (s *S3) object(key string) (string, error) {
	rel, err := relPath(key)
	if err != nil {
		return "", err
	}
	return s.prefix + rel, nil
}

// contentType vem da extensão da chave; o servidor manda o MIME do banco ao
// servir, isto só deixa o objeto bem rotulado para quem olhar o bucket.
func contentType(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

// isNotFound diz se o erro do S3 é "objeto não existe". NoSuchBucket fica de
// fora de propósito: bucket sumido é falha de configuração, não 404 de arquivo.
func isNotFound(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case minio.NoSuchKey, "NotFound":
		return true
	}
	return false
}

func (s *S3) wrap(op, key string, err error) error {
	if isNotFound(err) {
		return ErrNotFound
	}
	return fmt.Errorf("s3 %s %s: %w", op, key, err)
}

// Put envia r num único PUT quando o tamanho é conhecido (*os.File, caso do
// spool de envios); senão faz upload multipart em partes de 16 MiB. Nos dois
// casos o objeto só aparece no bucket inteiro.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	name, err := s.object(key)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, s3PutTimeout)
	defer cancel()
	opts := minio.PutObjectOptions{ContentType: contentType(key)}
	size := int64(-1)
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
				size = fi.Size() - pos
			}
		}
	}
	if size >= 0 {
		opts.DisableMultipart = true
	} else {
		opts.PartSize = s3StreamPartSize
	}
	info, err := s.client.PutObject(ctx, s.bucket, name, r, size, opts)
	if err != nil {
		return 0, s.wrap("put", key, err)
	}
	return info.Size, nil
}

// Open confere a existência com um HEAD (o GET do minio-go é preguiçoso e só
// falharia na primeira leitura) e devolve um leitor com Seek que baixa só o
// trecho pedido, o que basta para o http.ServeContent atender Range.
// O arquivo usa ctx em cada leitura: deve viver só durante a requisição.
func (s *S3) Open(ctx context.Context, key string) (File, error) {
	name, err := s.object(key)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, s3OpenTimeout)
	defer cancel()
	info, err := s.client.StatObject(sctx, s.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		return nil, s.wrap("stat", key, err)
	}
	return &s3File{ctx: ctx, s: s, key: key, name: name, size: info.Size}, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	name, err := s.object(key)
	if err != nil {
		return Info{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s3MetaTimeout)
	defer cancel()
	info, err := s.client.StatObject(ctx, s.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		return Info{}, s.wrap("stat", key, err)
	}
	return Info{Size: info.Size, ModTime: info.LastModified}, nil
}

// Delete remove o objeto. Chave inexistente devolve nil (veja o tipo S3).
func (s *S3) Delete(ctx context.Context, key string) error {
	name, err := s.object(key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s3MetaTimeout)
	defer cancel()
	if err := s.client.RemoveObject(ctx, s.bucket, name, minio.RemoveObjectOptions{}); err != nil {
		return s.wrap("delete", key, err)
	}
	return nil
}

// List percorre os objetos dentro do prefixo em ordem crescente de chave (o
// ListObjectsV2 devolve em ordem binária UTF-8). Objetos fora do formato
// <id[0:2]>/<id>.<ext> são ignorados.
//
// Usa o iterador do minio-go, que não cria goroutine: o ListObjects (canal)
// deixaria a goroutine dele presa para sempre quando fn para a listagem no
// meio, a menos que o canal fosse esvaziado até o fim.
func (s *S3) List(ctx context.Context, fn func(key string, info Info) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range s.client.ListObjectsIter(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("s3 list: %w", obj.Err)
		}
		key, ok := keyFromRel(strings.TrimPrefix(obj.Key, s.prefix))
		if !ok {
			continue
		}
		if err := fn(key, Info{Size: obj.Size, ModTime: obj.LastModified}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// s3File é um io.ReadSeekCloser sobre um objeto de tamanho conhecido. Seek só
// muda a posição; a próxima leitura abre um GET com Range a partir dela, de
// no máximo s3ReadWindow bytes. Ler além da janela abre o GET seguinte.
type s3File struct {
	ctx  context.Context
	s    *S3
	key  string
	name string
	size int64
	off  int64
	end  int64         // fim (exclusivo) do trecho pedido no GET em andamento
	body io.ReadCloser // GET em andamento, de off até end; nil = abrir na próxima leitura
}

func (f *s3File) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if f.off >= f.size {
			return 0, io.EOF
		}
		if f.body == nil {
			end := min(f.size, f.off+s3ReadWindow)
			var opts minio.GetObjectOptions
			if err := opts.SetRange(f.off, end-1); err != nil {
				return 0, err
			}
			body, _, _, err := f.s.core.GetObject(f.ctx, f.s.bucket, f.name, opts)
			if err != nil {
				return 0, f.s.wrap("get", f.key, err)
			}
			f.body, f.end = body, end
		}
		n, err := f.body.Read(p[:min(int64(len(p)), f.end-f.off)])
		f.off += int64(n)
		if f.off >= f.end {
			// janela lida inteira: a próxima leitura abre a seguinte
			f.closeBody()
			if f.off < f.size {
				err = nil
			} else {
				err = io.EOF
			}
		} else if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, errors.New("s3: whence inválido")
	}
	if abs < 0 {
		return 0, errors.New("s3: posição negativa")
	}
	if abs != f.off {
		f.closeBody()
	}
	f.off = abs
	return abs, nil
}

func (f *s3File) Close() error {
	f.closeBody()
	return nil
}

func (f *s3File) closeBody() {
	if f.body != nil {
		f.body.Close()
		f.body = nil
	}
}

var (
	_ Store  = (*S3)(nil)
	_ Lister = (*S3)(nil)
	_ Lister = (*FS)(nil)
)
