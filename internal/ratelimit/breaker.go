package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Breaker defaults. Five consecutive failures is enough that a single dropped
// connection does not move the whole service onto the fallback, and few enough
// that a genuine outage is detected within a handful of requests. The cooldown
// bounds how often a hard-down store is probed.
const (
	DefaultFailureThreshold = 5
	DefaultCooldown         = 5 * time.Second
)

// Breaker routes limit decisions to an authoritative store, and to a local
// fallback when that store is failing.
//
// The governing rule is that the limiter fails OPEN. A rate limiter exists to
// protect the service; if its own unavailability took the service down, it
// would have become the outage it was installed to prevent. "We start
// rejecting everything" is the wrong answer to "what happens when Redis dies",
// and it is the obvious follow-up question.
//
// The cost of failing open is bounded and worth stating plainly: while
// degraded, each instance keeps its own buckets, so N instances admit up to N
// times the intended limit. That is a deliberate trade of precision for
// availability, for as long as the outage lasts.
type Breaker struct {
	primary   Limiter
	fallback  Limiter
	threshold int
	cooldown  time.Duration

	// now is injectable so cooldown behaviour is tested by arithmetic rather
	// than by sleeping.
	now func() time.Time

	mu          sync.Mutex
	consecutive int
	openUntil   time.Time
	degraded    bool
	probing     bool
}

// NewBreaker wraps primary with a fallback used while primary is failing.
func NewBreaker(primary, fallback Limiter) *Breaker {
	registerMetrics()
	return &Breaker{
		primary:   primary,
		fallback:  fallback,
		threshold: DefaultFailureThreshold,
		cooldown:  DefaultCooldown,
		now:       time.Now,
	}
}

// WithClock overrides the clock. Tests only.
func (b *Breaker) WithClock(now func() time.Time) *Breaker {
	b.now = now
	return b
}

// WithThreshold overrides the consecutive-failure count that opens the
// breaker. Tests only.
func (b *Breaker) WithThreshold(n int) *Breaker {
	b.threshold = n
	return b
}

// Allow implements Limiter. It never propagates a store error to the caller:
// every failure path ends in a fallback decision, because an error returned
// here would have to become either a 500 or an unlimited request, and neither
// is better than an approximate limit.
func (b *Breaker) Allow(ctx context.Context, key string, cfg Config) (Decision, error) {
	if !b.acquirePrimary() {
		return b.serveDegraded(ctx, key, cfg)
	}
	// Deferred, not merely released on the two return paths: a panic inside
	// the primary would otherwise keep the half-open gate forever. Recovery
	// turns that panic into a 500 and the process carries on, so the breaker
	// would sit wedged on the per-instance fallback with no path back to the
	// authoritative store and no error to explain it.
	defer b.releasePrimary()

	d, err := b.primary.Allow(ctx, key, cfg)
	if err != nil {
		storeErrorsTotal.Inc()
		b.recordFailure(err)
		return b.serveDegraded(ctx, key, cfg)
	}

	b.recordSuccess()
	return d, nil
}

// Degraded reports whether the breaker is currently open. Read by /debug/status
// and by tests; not on the hot path.
func (b *Breaker) Degraded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.degraded
}

// acquirePrimary reports whether this request may call the authoritative
// store.
//
// While closed, everyone may. While open, nobody may. In between — once the
// cooldown lapses — exactly ONE request probes and the rest keep using the
// fallback. That single-flight gate is the difference between a half-open
// breaker and a stampede: without it every concurrent request sees the gate
// open at the same instant and dials a store that is still down, each paying
// the full dial timeout, producing a latency cliff once per cooldown for the
// whole duration of the outage.
func (b *Breaker) acquirePrimary() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.degraded {
		return true
	}
	if b.now().Before(b.openUntil) {
		return false
	}
	if b.probing {
		return false
	}
	b.probing = true
	return true
}

// releasePrimary hands the half-open gate back. Idempotent: it is safe to call
// when the breaker was closed and no gate was ever taken.
func (b *Breaker) releasePrimary() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (b *Breaker) recordFailure(cause error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutive++
	if b.consecutive < b.threshold {
		return
	}

	b.openUntil = b.now().Add(b.cooldown)
	if b.degraded {
		return // already open; do not re-log every request
	}

	b.degraded = true
	degradedActive.Set(1)
	slog.Warn("rate limiter degraded: serving from the in-memory fallback",
		"consecutive_failures", b.consecutive,
		"cooldown", b.cooldown,
		"error", cause.Error(),
		"impact", "limits are enforced per instance until the store recovers")
}

func (b *Breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutive = 0
	if !b.degraded {
		return
	}

	b.degraded = false
	b.openUntil = time.Time{}
	degradedActive.Set(0)
	slog.Warn("rate limiter recovered: authoritative store is answering again")
}

// serveDegraded answers from the fallback and accounts for it.
func (b *Breaker) serveDegraded(ctx context.Context, key string, cfg Config) (Decision, error) {
	degradedTotal.Inc()

	d, err := b.fallback.Allow(ctx, key, cfg)
	if err != nil {
		// The in-memory fallback does not return errors. If a future one does,
		// fail open rather than inventing a rejection.
		slog.Error("rate limiter fallback failed; allowing the request",
			"error", err.Error())
		return Decision{Allowed: true, Limit: cfg.Burst, Remaining: float64(cfg.Burst), Degraded: true}, nil
	}

	d.Degraded = true
	return d, nil
}
