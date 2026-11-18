package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// File são os metadados de um arquivo enviado.
type File struct {
	ID              string
	Ext             string
	MIME            string
	Size            int64
	SHA256          string // hex
	DeleteTokenHash string // hex do SHA-256 do token; o token nunca é guardado
	APIKeyID        string // vazio = envio anônimo
	IPHash          string // HMAC do IP; nunca o IP cru
	CreatedAt       time.Time
	ExpiresAt       *time.Time // nil = sem garantia (anônimo) ou para sempre (chave com retention 0)
}

// Key é a chave do arquivo no armazenamento: "<id>.<ext>".
func (f File) Key() string { return f.ID + "." + f.Ext }

const fileCols = `id, ext, mime, size, sha256, delete_token_hash, api_key_id, ip_hash, created_at, expires_at`

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var f File
	var keyID, ipHash sql.NullString
	var created int64
	var expires sql.NullInt64
	err := row.Scan(&f.ID, &f.Ext, &f.MIME, &f.Size, &f.SHA256, &f.DeleteTokenHash, &keyID, &ipHash, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	f.APIKeyID, f.IPHash = keyID.String, ipHash.String
	f.CreatedAt, f.ExpiresAt = fromMillis(created), timePtr(expires)
	return f, err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// InsertFile grava os metadados; devolve ErrConflict se o id já existir.
func (d *DB) InsertFile(ctx context.Context, f File) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO files (`+fileCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.Ext, f.MIME, f.Size, f.SHA256, f.DeleteTokenHash, nullString(f.APIKeyID), nullString(f.IPHash),
		toMillis(f.CreatedAt), nullTime(f.ExpiresAt))
	if isConstraint(err) {
		return ErrConflict
	}
	return err
}

// GetFile busca um arquivo pelo id.
func (d *DB) GetFile(ctx context.Context, id string) (File, error) {
	return scanFile(d.sql.QueryRowContext(ctx, `SELECT `+fileCols+` FROM files WHERE id = ?`, id))
}

// FileExists diz se já existe um arquivo com esse id.
func (d *DB) FileExists(ctx context.Context, id string) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// DeleteFile remove a linha do arquivo; ErrNotFound se não existir.
func (d *DB) DeleteFile(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TotalBytes soma o tamanho de todos os arquivos.
func (d *DB) TotalBytes(ctx context.Context) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM files`).Scan(&n)
	return n, err
}

const evictable = `(api_key_id IS NULL OR expires_at <= ?)`

// OldestEvictable devolve até limit arquivos despejáveis, do mais antigo para
// o mais novo: anônimos e os de chave cuja retenção garantida já venceu.
func (d *DB) OldestEvictable(ctx context.Context, now time.Time, limit int) ([]File, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+fileCols+` FROM files WHERE `+evictable+
		` ORDER BY created_at, rowid LIMIT ?`, toMillis(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EvictableBytesUpTo soma os tamanhos dos arquivos despejáveis na ordem de
// despejo (mais antigo primeiro) e para assim que a soma alcança need. O
// custo é proporcional ao que seria despejado, não à tabela inteira; só
// percorre tudo quando nem despejando tudo se chega a need.
func (d *DB) EvictableBytesUpTo(ctx context.Context, now time.Time, need int64) (int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT size FROM files WHERE `+evictable+
		` ORDER BY created_at, rowid`, toMillis(now))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var sum int64
	for sum < need && rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return sum, err
		}
		sum += n
	}
	return sum, rows.Err()
}

// FilesAfter lista arquivos em ordem de id, depois de afterID (paginação).
func (d *DB) FilesAfter(ctx context.Context, afterID string, limit int) ([]File, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+fileCols+` FROM files WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Stats são os números agregados para /v1/admin/stats.
type Stats struct {
	Files          int64
	AnonFiles      int64
	KeyedFiles     int64
	TotalBytes     int64
	EvictableFiles int64
	EvictableBytes int64
	ActiveKeys     int64
	RevokedKeys    int64
}

// Stats calcula os agregados no instante now.
func (d *DB) Stats(ctx context.Context, now time.Time) (Stats, error) {
	var s Stats
	err := d.sql.QueryRowContext(ctx, `SELECT
		count(*),
		COALESCE(SUM(api_key_id IS NULL), 0),
		COALESCE(SUM(api_key_id IS NOT NULL), 0),
		COALESCE(SUM(size), 0),
		COALESCE(SUM(CASE WHEN `+evictable+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN `+evictable+` THEN size ELSE 0 END), 0)
		FROM files`, toMillis(now), toMillis(now)).
		Scan(&s.Files, &s.AnonFiles, &s.KeyedFiles, &s.TotalBytes, &s.EvictableFiles, &s.EvictableBytes)
	if err != nil {
		return s, err
	}
	err = d.sql.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(revoked_at IS NULL), 0), COALESCE(SUM(revoked_at IS NOT NULL), 0) FROM api_keys`).
		Scan(&s.ActiveKeys, &s.RevokedKeys)
	return s, err
}
