// Package db guarda os metadados (arquivos e chaves de API) em SQLite.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver puro Go (CGO_ENABLED=0)
)

// ErrNotFound indica que a linha procurada não existe.
var ErrNotFound = errors.New("db: não encontrado")

// ErrConflict indica violação de unicidade (ex.: id de arquivo repetido).
var ErrConflict = errors.New("db: conflito de chave")

// DB envolve a conexão SQLite.
type DB struct {
	sql *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	key_hash       TEXT NOT NULL UNIQUE,
	retention_days INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	revoked_at     INTEGER
);
CREATE TABLE IF NOT EXISTS files (
	id                TEXT PRIMARY KEY,
	ext               TEXT NOT NULL,
	mime              TEXT NOT NULL,
	size              INTEGER NOT NULL,
	sha256            TEXT NOT NULL,
	delete_token_hash TEXT NOT NULL,
	api_key_id        TEXT REFERENCES api_keys(id),
	ip_hash           TEXT,
	created_at        INTEGER NOT NULL,
	expires_at        INTEGER
);
CREATE INDEX IF NOT EXISTS files_created_at ON files(created_at);
CREATE INDEX IF NOT EXISTS files_api_key_id ON files(api_key_id);
PRAGMA user_version = 1;
`

// Open abre (ou cria) o banco em path com WAL, busy_timeout e chaves estrangeiras.
func Open(path string) (*DB, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(10000)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	conn, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(8)
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("criando schema: %w", err)
	}
	return &DB{sql: conn}, nil
}

// Close fecha o banco.
func (d *DB) Close() error { return d.sql.Close() }

// Ping verifica se o banco responde (usado pelo /healthz).
func (d *DB) Ping(ctx context.Context) error {
	var one int
	return d.sql.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func isConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

func toMillis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return toMillis(*t)
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMillis(n.Int64)
	return &t
}
