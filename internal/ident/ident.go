// Package ident gera identificadores e segredos aleatórios e calcula seus hashes.
package ident

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"regexp"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// FileIDLen é o tamanho dos ids públicos de arquivo.
const FileIDLen = 8

// APIKeyPrefix aparece no começo de toda chave de API.
const APIKeyPrefix = "vk_"

var fileIDRe = regexp.MustCompile(`^[0-9A-Za-z]{8}$`)

// Random devolve n caracteres base62 tirados de crypto/rand (sem viés de módulo).
func Random(n int) string {
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		rand.Read(buf)
		for _, b := range buf {
			// 248 = 62*4: descarta bytes acima disso para manter a distribuição uniforme.
			if b < 248 {
				out = append(out, base62[b%62])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out)
}

// NewFileID gera um id público de arquivo (8 caracteres base62).
func NewFileID() string { return Random(FileIDLen) }

// ValidFileID diz se s tem o formato de um id de arquivo.
func ValidFileID(s string) bool { return fileIDRe.MatchString(s) }

// NewDeleteToken gera um token de exclusão opaco (~238 bits de entropia).
func NewDeleteToken() string { return Random(40) }

// NewAPIKey gera uma chave de API no formato vk_<40 base62>.
func NewAPIKey() string { return APIKeyPrefix + Random(40) }

// NewKeyID gera o id público (não secreto) de uma chave de API.
func NewKeyID() string { return Random(12) }

// HashSecret devolve o SHA-256 em hex de um segredo de alta entropia
// (token de exclusão, chave de API). Não depende de VKDL_SECRET, então os
// hashes continuam válidos mesmo se o segredo do servidor mudar.
func HashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// EqualHash compara dois hashes em tempo constante.
func EqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// EqualSecret compara dois segredos em tempo constante, sem vazar o tamanho.
func EqualSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// HashIP devolve HMAC-SHA256(ip, secret) truncado (hex, 32 caracteres).
// O IP em si nunca é guardado nem registrado em log.
func HashIP(ip string, secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ip))
	return hex.EncodeToString(m.Sum(nil))[:32]
}
