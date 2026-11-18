// Package ratelimit implementa um limite de envios por minuto (token bucket em memória).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// DefaultMaxKeys limita quantas chaves (IPs/chaves de API) ficam na memória.
// Quem forja um IP novo a cada requisição não consegue crescer o mapa além disso.
const DefaultMaxKeys = 100_000

// overflowKey é o balde compartilhado usado quando o mapa está cheio.
const overflowKey = "\x00overflow"

// Limiter permite até perMinute eventos por minuto para cada chave, com rajada
// de até perMinute. perMinute <= 0 desativa o limite.
type Limiter struct {
	perMinute int
	maxKeys   int
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New cria um Limiter com o relógio real.
func New(perMinute int) *Limiter { return NewWithClock(perMinute, time.Now) }

// NewWithClock cria um Limiter com relógio injetável (testes).
func NewWithClock(perMinute int, now func() time.Time) *Limiter {
	return &Limiter{perMinute: perMinute, maxKeys: DefaultMaxKeys, now: now, buckets: map[string]*bucket{}, lastSweep: now()}
}

// Allow consome uma ficha de key. Se não houver, devolve false e quanto
// esperar até a próxima ficha (para o cabeçalho Retry-After).
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l.perMinute <= 0 {
		return true, 0
	}
	now := l.now()
	rate := float64(l.perMinute) / 60 // fichas por segundo
	capacity := float64(l.perMinute)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now, capacity, rate, time.Minute)

	b, ok := l.buckets[key]
	if !ok && len(l.buckets) >= l.maxKeys {
		// mapa cheio: tenta liberar (no máximo 1x por segundo); se continuar
		// cheio, a chave nova divide um balde único com as outras novatas
		l.sweep(now, capacity, rate, time.Second)
		if len(l.buckets) >= l.maxKeys {
			key = overflowKey
			b, ok = l.buckets[key]
		}
	}
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(capacity, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	return false, wait
}

// sweep descarta, no máximo uma vez a cada every, baldes que já estariam cheios
// de novo; assim a memória não cresce com IPs que passaram uma vez só.
func (l *Limiter) sweep(now time.Time, capacity, rate float64, every time.Duration) {
	if now.Sub(l.lastSweep) < every {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*rate >= capacity {
			delete(l.buckets, k)
		}
	}
}

// Len devolve quantas chaves estão sendo acompanhadas (testes/métricas).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// RetryAfterSeconds arredonda wait para cima, com mínimo de 1 segundo.
func RetryAfterSeconds(wait time.Duration) int {
	s := int(math.Ceil(wait.Seconds()))
	if s < 1 {
		s = 1
	}
	return s
}
