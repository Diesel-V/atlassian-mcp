// Package ratelimit limits tool calls per principal.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a per-key token bucket. Idle keys are evicted.
type Limiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	idleTTL time.Duration
	entries map[string]*entry
	now     func() time.Time
}

type entry struct {
	l        *rate.Limiter
	lastSeen time.Time
}

// New allows perMinute calls per key with a burst of the same size.
func New(perMinute int) *Limiter {
	return &Limiter{
		limit:   rate.Limit(float64(perMinute) / 60),
		burst:   perMinute,
		idleTTL: 10 * time.Minute,
		entries: map[string]*entry{},
		now:     time.Now,
	}
}

// Allow reports whether key may make another call now.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) > 1000 {
			l.evict(now)
		}
		e = &entry{l: rate.NewLimiter(l.limit, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = now
	return e.l.AllowN(now, 1)
}

func (l *Limiter) evict(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.lastSeen) > l.idleTTL {
			delete(l.entries, k)
		}
	}
}
