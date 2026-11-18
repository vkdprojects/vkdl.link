// Package sniff identifica o formato de uma imagem pelos bytes mágicos.
// O nome do arquivo e o Content-Type enviados pelo cliente nunca são usados.
package sniff

import "bytes"

// Kind descreve um formato aceito.
type Kind struct {
	Ext  string // extensão usada na URL pública, sem ponto
	MIME string
}

var (
	PNG  = Kind{"png", "image/png"}
	JPEG = Kind{"jpg", "image/jpeg"}
	GIF  = Kind{"gif", "image/gif"}
	WebP = Kind{"webp", "image/webp"}
)

// HeaderLen é quantos bytes do início do arquivo bastam para Detect.
const HeaderLen = 16

// Detect devolve o formato de head (os primeiros bytes do arquivo) ou
// ok=false se não for PNG, JPEG, GIF ou WebP.
func Detect(head []byte) (Kind, bool) {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return PNG, true
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return JPEG, true
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return GIF, true
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return WebP, true
	}
	return Kind{}, false
}

// ByExt devolve o formato pela extensão pública (png, jpg, gif, webp).
func ByExt(ext string) (Kind, bool) {
	for _, k := range []Kind{PNG, JPEG, GIF, WebP} {
		if k.Ext == ext {
			return k, true
		}
	}
	return Kind{}, false
}
