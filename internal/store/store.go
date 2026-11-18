// Package store guarda o conteúdo dos arquivos. Hoje só existe a implementação
// em sistema de arquivos; a interface permite trocar por S3 depois.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound indica que a chave não existe no armazenamento.
var ErrNotFound = errors.New("store: arquivo não encontrado")

// ErrInvalidKey indica uma chave fora do formato <id>.<ext>.
var ErrInvalidKey = errors.New("store: chave inválida")

// Info são os metadados básicos de um objeto guardado.
type Info struct {
	Size    int64
	ModTime time.Time
}

// File é o que Open devolve: precisa de Seek para Range/If-None-Match.
type File interface {
	io.ReadSeekCloser
}

// Store é o armazenamento de objetos. As chaves têm o formato "<id>.<ext>".
type Store interface {
	// Put grava r inteiro sob key de forma atômica e devolve o tamanho gravado.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (File, error)
	// Delete remove key; devolve ErrNotFound se ela não existir.
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (Info, error)
}

// FileAdopter é implementado por armazenamentos locais que conseguem adotar
// um arquivo temporário já gravado (rename), evitando copiar os bytes de novo.
type FileAdopter interface {
	Adopt(ctx context.Context, key, srcPath string) error
}
