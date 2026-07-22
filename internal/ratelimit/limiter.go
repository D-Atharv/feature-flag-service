// Package ratelimit contains the hand-rolled token bucket used to limit each
// API key. There is no off-the-shelf limiter here and there must never be one:
// the assignment asks for the algorithm, and scripts/check-no-ratelimit-deps.sh
// fails the build if a limiter library appears in go.mod.
//
// This package holds contracts only — the Limiter interface, its Config and
// Decision types, and the circuit breaker that composes two implementations.
// The implementations live in subpackages (redis/ and memory/) so that the
// contract stays free of any network dependency.
package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
)

// Limiter decides whether one request may proceed under key's budget.
//
// Implementations must be safe for concurrent use and must never block for
// longer than the caller's context allows.
type Limiter interface {
	Allow(ctx context.Context, key string, cfg Config) (Decision, error)
}

// Config is one bucket's parameters, resolved per API key.
type Config struct {
	// Rate is the refill rate in tokens per second. Always > 0 — see ConfigFor.
	Rate float64
	// Burst is the bucket capacity, i.e. the most requests admissible at once.
	Burst int
	// Cost is the tokens one request consumes. Always 1 in this service;
	// present because the bucket math supports weighted requests and the
	// tests exercise the cost > burst edge.
	Cost int
}

// Decision is the outcome of one Allow call.
//
// Remaining, RetryAfter and ResetAfter are floats because the bucket holds
// fractional tokens. The HTTP layer rounds them; rounding here would lose
// information the middleware needs to round correctly.
type Decision struct {
	Allowed bool
	// Limit is the bucket capacity, echoed for the RateLimit-Limit header.
	Limit int
	// Remaining is the token count after this request.
	Remaining float64
	// RetryAfter is seconds until the request would succeed. 0 when allowed.
	RetryAfter float64
	// ResetAfter is seconds until the bucket is full again.
	ResetAfter float64
	// Degraded reports that this decision came from the in-memory fallback
	// rather than the authoritative store.
	Degraded bool
}

// Default bucket parameters, applied when an API key carries values the bucket
// math cannot use. They match the database defaults in 001_init.sql.
const (
	DefaultRate  = 50.0
	DefaultBurst = 100
)

// ConfigFor derives a bucket config from an API key, clamping values the
// arithmetic cannot survive.
//
// This clamp is load-bearing, not defensive boilerplate. The Lua script
// divides by rate in three places, so a key with rate_limit_rps = 0 yields
// inf, math.ceil(inf) makes PEXPIRE fail, and the script errors on every
// single call — which the circuit breaker would faithfully report as "Redis
// is down" while the real cause sat in one database row.
// The warnings below are emitted once per key per process. ConfigFor runs on
// every request, so warning unconditionally would turn one bad database row
// into one log line per request — the same self-inflicted outage the /evaluate
// access log avoids by sampling, reintroduced a layer further down. Bounded by
// the number of misconfigured API keys, which is bounded by the table.
var clampWarned sync.Map // string -> struct{}

func warnOnce(dedupeKey string, msg string, args ...any) {
	if _, seen := clampWarned.LoadOrStore(dedupeKey, struct{}{}); seen {
		return
	}
	slog.Warn(msg, args...)
}

func ConfigFor(key domain.APIKey) Config {
	cfg := Config{Rate: key.RateLimitRPS, Burst: key.RateLimitBurst, Cost: 1}

	if cfg.Rate <= 0 {
		warnOnce("rate:"+key.ID, "api key has a non-positive rate limit; using default",
			"key_id", key.ID, "configured_rps", key.RateLimitRPS, "default_rps", DefaultRate)
		cfg.Rate = DefaultRate
	}
	if cfg.Burst < 1 {
		warnOnce("burst:"+key.ID, "api key has a non-positive burst; using default",
			"key_id", key.ID, "configured_burst", key.RateLimitBurst, "default_burst", DefaultBurst)
		cfg.Burst = DefaultBurst
	}
	return cfg
}

// RetryAfterDuration converts a decision's RetryAfter into a duration rounded
// up to whole seconds, with a floor of one second.
//
// Retry-After is defined by RFC 9110 as an integer count of seconds; emitting
// "0" would invite an immediate retry that is certain to fail again.
func (d Decision) RetryAfterDuration() time.Duration {
	secs := int(d.RetryAfter)
	if float64(secs) < d.RetryAfter {
		secs++ // ceil
	}
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second
}
