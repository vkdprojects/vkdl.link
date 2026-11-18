package ratelimit

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestBurstThenRefill(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := NewWithClock(3, c.now)
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("envio %d devia passar", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok {
		t.Fatal("4º envio no mesmo minuto devia ser bloqueado")
	}
	if wait <= 0 || wait > 20*time.Second {
		t.Fatalf("espera = %v, quero (0, 20s]", wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("outra chave não pode ser afetada")
	}
	c.t = c.t.Add(wait)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("depois da espera devia liberar uma ficha")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("só uma ficha devia ter voltado")
	}
}

func TestDisabled(t *testing.T) {
	l := New(0)
	for range 1000 {
		if ok, _ := l.Allow("x"); !ok {
			t.Fatal("limite 0 devia desativar")
		}
	}
}

func TestSweep(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := NewWithClock(10, c.now)
	l.Allow("a")
	l.Allow("b")
	if l.Len() != 2 {
		t.Fatalf("Len = %d", l.Len())
	}
	c.t = c.t.Add(2 * time.Minute)
	l.Allow("c")
	if l.Len() != 1 {
		t.Fatalf("baldes cheios deviam ser descartados; Len = %d", l.Len())
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for in, want := range map[time.Duration]int{0: 1, 300 * time.Millisecond: 1, 1500 * time.Millisecond: 2, 20 * time.Second: 20} {
		if got := RetryAfterSeconds(in); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d, quero %d", in, got, want)
		}
	}
}

// Quem forja um IP novo por requisição não pode crescer o mapa sem limite
// nem ganhar um balde novo cheio a cada vez.
func TestMaxKeysCap(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := NewWithClock(2, c.now)
	l.maxKeys = 3
	for _, k := range []string{"a", "b", "c"} {
		if ok, _ := l.Allow(k); !ok {
			t.Fatalf("%s devia passar", k)
		}
	}
	// mapa cheio: as chaves novas dividem um balde só (capacidade 2)
	okCount := 0
	for i := range 10 {
		if ok, _ := l.Allow("forjado-" + string(rune('0'+i))); ok {
			okCount++
		}
	}
	if okCount != 2 {
		t.Fatalf("chaves novas com o mapa cheio passaram %d vezes, quero 2", okCount)
	}
	if l.Len() > 4 {
		t.Fatalf("mapa cresceu além do limite: %d", l.Len())
	}
	// chaves que já existiam continuam com o próprio balde
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("chave existente não pode ser afetada")
	}
	// passado o tempo, os baldes cheios são descartados e há vaga de novo
	c.t = c.t.Add(2 * time.Minute)
	if ok, _ := l.Allow("nova"); !ok {
		t.Fatal("depois da limpeza a chave nova devia ter balde próprio")
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d depois da limpeza", l.Len())
	}
}
