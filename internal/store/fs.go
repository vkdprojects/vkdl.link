package store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FS guarda os arquivos em <root>/<id[0:2]>/<id>.<ext>.
type FS struct {
	root string
}

// NewFS cria (se preciso) o diretório raiz e devolve o armazenamento.
func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &FS{root: root}, nil
}

// Root devolve o diretório raiz dos arquivos.
func (s *FS) Root() string { return s.root }

// path valida a chave e devolve o caminho no disco. Só aceita "<id>.<ext>",
// o que impede qualquer tentativa de path traversal.
func (s *FS) path(key string) (string, error) {
	rel, err := relPath(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(rel)), nil
}

func (s *FS) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	dst, err := s.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	syncDir(filepath.Dir(dst))
	return n, nil
}

// Adopt move srcPath para a posição de key (rename atômico). srcPath já
// deve estar com fsync feito. Se o rename falhar (ex.: outro sistema de
// arquivos), copia e apaga a origem.
func (s *FS) Adopt(ctx context.Context, key, srcPath string) error {
	dst, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcPath, dst); err == nil {
		syncDir(filepath.Dir(dst))
		return nil
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := s.Put(ctx, key, f); err != nil {
		return err
	}
	return os.Remove(srcPath)
}

// syncDir faz fsync do diretório para o rename sobreviver a uma queda do
// sistema. É o melhor esforço: o rename já aconteceu e falhar aqui não deve
// desfazer a gravação (e em alguns sistemas não dá para abrir diretórios).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

func (s *FS) Open(ctx context.Context, key string) (File, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (s *FS) Delete(ctx context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err == nil {
		// remove o diretório do prefixo se ficou vazio (ignora erro: pode não estar vazio)
		os.Remove(filepath.Dir(p))
	}
	return err
}

func (s *FS) Stat(ctx context.Context, key string) (Info, error) {
	p, err := s.path(key)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// List percorre os arquivos em ordem crescente de chave (os diretórios
// <id[0:2]> e os nomes dentro deles saem ordenados de os.ReadDir). Ignora
// temporários e qualquer arquivo fora do formato <id[0:2]>/<id>.<ext>.
func (s *FS) List(ctx context.Context, fn func(key string, info Info) error) error {
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, d.Name()))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			key, ok := keyFromRel(d.Name() + "/" + e.Name())
			if !ok || !e.Type().IsRegular() {
				continue
			}
			fi, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // apagado durante a listagem
			}
			if err != nil {
				return err
			}
			if err := fn(key, Info{Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
				return err
			}
		}
	}
	return nil
}
