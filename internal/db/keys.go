package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// APIKey é uma chave de API. O segredo em si nunca é guardado, só o hash.
type APIKey struct {
	ID            string
	Name          string
	KeyHash       string
	RetentionDays int // 0 = para sempre
	CreatedAt     time.Time
	RevokedAt     *time.Time
}

// Active diz se a chave ainda pode ser usada.
func (k APIKey) Active() bool { return k.RevokedAt == nil }

const keyCols = `id, name, key_hash, retention_days, created_at, revoked_at`

func scanKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var created int64
	var revoked sql.NullInt64
	err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.RetentionDays, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	k.CreatedAt, k.RevokedAt = fromMillis(created), timePtr(revoked)
	return k, err
}

// CreateKey grava uma nova chave; ErrConflict se id ou hash repetirem.
func (d *DB) CreateKey(ctx context.Context, k APIKey) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO api_keys (`+keyCols+`) VALUES (?,?,?,?,?,?)`,
		k.ID, k.Name, k.KeyHash, k.RetentionDays, toMillis(k.CreatedAt), nullTime(k.RevokedAt))
	if isConstraint(err) {
		return ErrConflict
	}
	return err
}

// KeyByHash busca uma chave pelo hash do segredo (inclui revogadas).
func (d *DB) KeyByHash(ctx context.Context, hash string) (APIKey, error) {
	return scanKey(d.sql.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE key_hash = ?`, hash))
}

// ListKeys devolve todas as chaves, das mais novas para as mais antigas.
func (d *DB) ListKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marca a chave como revogada (idempotente); ErrNotFound se não existir.
func (d *DB) RevokeKey(ctx context.Context, id string, now time.Time) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, toMillis(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
