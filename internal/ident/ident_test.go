package ident

import (
	"strings"
	"testing"
)

func TestFileID(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id := NewFileID()
		if !ValidFileID(id) {
			t.Fatalf("id inválido: %q", id)
		}
		if seen[id] {
			t.Fatalf("colisão improvável: %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "abc", "abcdefg!", "abcdefghi", "../etc/p", "abcd.png"} {
		if ValidFileID(bad) {
			t.Errorf("ValidFileID(%q) devia ser false", bad)
		}
	}
}

func TestSecrets(t *testing.T) {
	k := NewAPIKey()
	if !strings.HasPrefix(k, APIKeyPrefix) || len(k) != 43 {
		t.Errorf("chave mal formada: %q", k)
	}
	if NewDeleteToken() == NewDeleteToken() {
		t.Error("tokens repetidos")
	}
	h := HashSecret("abc")
	if len(h) != 64 || !EqualHash(h, HashSecret("abc")) || EqualHash(h, HashSecret("abd")) {
		t.Error("HashSecret/EqualHash incoerentes")
	}
	if !EqualSecret("x", "x") || EqualSecret("x", "xy") {
		t.Error("EqualSecret incoerente")
	}
}

func TestHashIP(t *testing.T) {
	a := HashIP("1.2.3.4", []byte("k1"))
	if len(a) != 32 || a != HashIP("1.2.3.4", []byte("k1")) {
		t.Error("HashIP não determinístico")
	}
	if a == HashIP("1.2.3.4", []byte("k2")) || a == HashIP("1.2.3.5", []byte("k1")) {
		t.Error("HashIP devia depender do ip e do segredo")
	}
	if strings.Contains(a, "1.2.3.4") {
		t.Error("hash vaza o ip")
	}
}
