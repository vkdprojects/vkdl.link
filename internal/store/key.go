package store

import (
	"fmt"
	"strings"

	"github.com/vkdprojects/vkdl.link/internal/ident"
)

// ValidKey diz se key tem o formato "<id>.<ext>" aceito pelos armazenamentos.
func ValidKey(key string) bool {
	id, ext, ok := strings.Cut(key, ".")
	return ok && ident.ValidFileID(id) && ext != "" && len(ext) <= 5 && !strings.ContainsAny(ext, `./\`)
}

// relPath valida a chave e devolve o caminho relativo "<id[0:2]>/<id>.<ext>"
// (sempre com "/"). Só aceitar esse formato impede qualquer path traversal,
// tanto no disco quanto no bucket. O layout é o mesmo nos dois, então dá para
// copiar o diretório de arquivos para o bucket sem renomear nada.
func relPath(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return key[:2] + "/" + key, nil
}

// keyFromRel é o inverso de relPath: devolve a chave de "<id[0:2]>/<id>.<ext>"
// e false para qualquer coisa fora desse formato (temporários, lixo de terceiros).
func keyFromRel(rel string) (string, bool) {
	dir, key, ok := strings.Cut(rel, "/")
	if !ok || !ValidKey(key) || dir != key[:2] {
		return "", false
	}
	return key, true
}
