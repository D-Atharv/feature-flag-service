package ratelimit_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
)

// captureLogs redirects slog for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf
}

// TestMisconfiguredKeyLogsOncePerKey guards against a log flood.
//
// ConfigFor runs on every single request, so warning unconditionally means one
// misconfigured row produces one log line per request — at any real request
// rate that is the self-inflicted outage the /evaluate log sampling exists to
// prevent, reintroduced one layer down.
func TestMisconfiguredKeyLogsOncePerKey(t *testing.T) {
	buf := captureLogs(t)

	key := domain.APIKey{ID: "flood-probe-key", RateLimitRPS: 0, RateLimitBurst: 10}
	for range 100 {
		ratelimit.ConfigFor(key)
	}

	got := strings.Count(buf.String(), "non-positive rate limit")
	assert.Equal(t, 1, got, "expected exactly one warning for 100 requests, got %d", got)
}

// TestMisconfiguredKeysLogIndependently: deduplicating must not silence a
// second broken key.
func TestMisconfiguredKeysLogIndependently(t *testing.T) {
	buf := captureLogs(t)

	for range 5 {
		ratelimit.ConfigFor(domain.APIKey{ID: "independent-a", RateLimitRPS: 0, RateLimitBurst: 10})
		ratelimit.ConfigFor(domain.APIKey{ID: "independent-b", RateLimitRPS: 0, RateLimitBurst: 10})
	}

	assert.Equal(t, 2, strings.Count(buf.String(), "non-positive rate limit"),
		"each distinct key must warn once")
}

// modalLimiter is a primary that can succeed, fail, or panic on command.
type modalLimiter struct {
	mu   sync.Mutex
	mode string // "ok" | "fail" | "panic"
}

func (m *modalLimiter) Allow(_ context.Context, _ string, cfg ratelimit.Config) (ratelimit.Decision, error) {
	m.mu.Lock()
	mode := m.mode
	m.mu.Unlock()

	switch mode {
	case "panic":
		panic("client library bug")
	case "fail":
		return ratelimit.Decision{}, errors.New("dial tcp: connection refused")
	default:
		return ratelimit.Decision{Allowed: true, Limit: cfg.Burst, Remaining: 42}, nil
	}
}

func (m *modalLimiter) setMode(v string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = v
}

// TestPanicOnTheProbeDoesNotWedgeTheBreaker.
//
// The half-open gate is only ever held by a probe, so this is the sequence
// that matters: breaker opens, cooldown lapses, the one permitted probe panics
// before it can release the gate. The recovery middleware turns that panic
// into a 500 and the process carries on — but the gate is never handed back,
// so every subsequent request is refused entry to the authoritative store and
// the service serves from the per-instance fallback forever, with
// ratelimit_degraded_active stuck at 1 and no path back.
func TestPanicOnTheProbeDoesNotWedgeTheBreaker(t *testing.T) {
	primary := &modalLimiter{}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(1).WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 100, Cost: 1}
	ctx := context.Background()

	// 1. Open the breaker.
	primary.setMode("fail")
	_, err := b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	require.True(t, b.Degraded())

	// 2. Cooldown lapses; the single permitted probe panics.
	clock.Advance(ratelimit.DefaultCooldown + time.Second)
	primary.setMode("panic")
	func() {
		defer func() { _ = recover() }() // as the recovery middleware would
		_, _ = b.Allow(ctx, "k", cfg)
	}()

	// 3. The store is healthy again and the cooldown has passed. The next
	//    request must be allowed to probe.
	primary.setMode("ok")
	clock.Advance(ratelimit.DefaultCooldown + time.Second)

	d, err := b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	assert.False(t, d.Degraded, "a panicking probe must not strand the breaker on the fallback")
	assert.InDelta(t, 42, d.Remaining, 0.01, "the answer must come from the primary")
}

// TestHalfOpenProbeIsSingleFlight guards against a stampede.
//
// The moment the cooldown lapses, every concurrent request sees a closed gate
// open at once and dials a store that is still down — each paying the full
// dial timeout. At any real request rate that is a latency cliff every
// cooldown period for as long as the outage lasts. Exactly one request should
// probe; the rest keep using the fallback.
func TestHalfOpenProbeIsSingleFlight(t *testing.T) {
	primary := &flakyLimiter{}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(1).WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 1000, Cost: 1}
	ctx := context.Background()

	primary.setFailing(true)
	_, err := b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	require.True(t, b.Degraded())

	callsBefore := primary.callCount()
	clock.Advance(ratelimit.DefaultCooldown + 1)

	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(50)
	for range 50 {
		go func() {
			defer done.Done()
			start.Wait()
			_, _ = b.Allow(ctx, "k", cfg)
		}()
	}
	start.Done()
	done.Wait()

	probes := primary.callCount() - callsBefore
	assert.Equal(t, 1, probes,
		"exactly one request may probe a store known to be down, got %d", probes)
}
