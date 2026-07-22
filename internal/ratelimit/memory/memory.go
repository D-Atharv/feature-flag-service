// Package memory implements the token bucket in process memory.
//
// It exists for one reason: to keep serving when the authoritative Redis
// bucket store is unreachable. It is deliberately NOT the primary limiter —
// per-process state means N instances enforce N times the intended limit, and
// the whole point of the assignment's third requirement is that quota outlives
// the process.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
)

// Limiter is a concurrency-safe in-process token bucket store.
//
// Memory growth: one small bucket per API key, never evicted. That is bounded
// by the number of API keys the service has ever authenticated, which is
// bounded by the api_keys table — three rows here. A janitor goroutine would
// add a lifecycle to start, stop and test in exchange for reclaiming a few
// hundred bytes, so the bound is documented rather than enforced. If this
// service ever keyed buckets on something unbounded (client IP, subject),
// that trade would flip and eviction would become mandatory.
type Limiter struct {
	mu      sync.RWMutex
	buckets map[string]*bucket

	// now is injectable so refill maths can be tested without sleeping.
	now func() time.Time
}

// bucket carries its own mutex so that two API keys never contend with each
// other — a single global lock would serialise the whole service on the
// hottest key.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	ts     time.Time
}

// New returns an empty in-memory limiter. Buckets are created on first use and
// start full, so a cold limiter fails open.
func New() *Limiter {
	return &Limiter{buckets: make(map[string]*bucket), now: time.Now}
}

// WithClock overrides the clock. Tests only.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allow implements ratelimit.Limiter. It never returns an error: this is the
// fallback path, and a fallback that can fail has not fallen back.
//
// The maths mirrors redis/bucket.lua exactly. The duplication is deliberate —
// the fallback cannot call Redis — and the two are pinned together by the
// parity test in the redis package, which runs one scenario table through both
// implementations and asserts identical decisions.
func (l *Limiter) Allow(_ context.Context, key string, cfg ratelimit.Config) (ratelimit.Decision, error) {
	b := l.bucketFor(key, cfg)

	b.mu.Lock()
	defer b.mu.Unlock()

	now := l.now()

	// Refill by elapsed time, clamped at capacity. A backwards clock jump
	// contributes zero rather than draining the bucket.
	elapsed := now.Sub(b.ts).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens = min(float64(cfg.Burst), b.tokens+elapsed*cfg.Rate)
	b.ts = now

	d := ratelimit.Decision{Limit: cfg.Burst}

	switch {
	case cfg.Cost > cfg.Burst:
		// Unsatisfiable at any point in the future. Report the time to fill an
		// empty bucket rather than an infinity, so the caller still gets a
		// usable Retry-After. Unreachable from the middleware, which always
		// costs 1 against a burst clamped to >= 1 — kept as defence in depth.
		d.RetryAfter = float64(cfg.Burst) / cfg.Rate
	case b.tokens >= float64(cfg.Cost):
		b.tokens -= float64(cfg.Cost)
		d.Allowed = true
	default:
		d.RetryAfter = (float64(cfg.Cost) - b.tokens) / cfg.Rate
	}

	d.Remaining = b.tokens
	d.ResetAfter = (float64(cfg.Burst) - b.tokens) / cfg.Rate
	return d, nil
}

// bucketFor returns the bucket for key, creating a full one on first use.
func (l *Limiter) bucketFor(key string, cfg ratelimit.Config) *bucket {
	l.mu.RLock()
	b, ok := l.buckets[key]
	l.mu.RUnlock()
	if ok {
		return b
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-check: another goroutine may have created it between the two locks.
	if b, ok = l.buckets[key]; ok {
		return b
	}
	b = &bucket{tokens: float64(cfg.Burst), ts: l.now()}
	l.buckets[key] = b
	return b
}

// Len reports how many buckets are held. Used in tests.
func (l *Limiter) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.buckets)
}
