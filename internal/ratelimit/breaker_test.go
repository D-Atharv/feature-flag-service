package ratelimit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
)

// flakyLimiter is a primary that fails on demand.
type flakyLimiter struct {
	mu    sync.Mutex
	fail  bool
	calls int
}

func (f *flakyLimiter) Allow(_ context.Context, _ string, cfg ratelimit.Config) (ratelimit.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return ratelimit.Decision{}, errors.New("dial tcp: connection refused")
	}
	// A distinguishable answer, so tests can tell which limiter replied.
	return ratelimit.Decision{Allowed: true, Limit: cfg.Burst, Remaining: 42}, nil
}

func (f *flakyLimiter) setFailing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *flakyLimiter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBreakerUsesPrimaryWhileHealthy(t *testing.T) {
	primary := &flakyLimiter{}
	b := ratelimit.NewBreaker(primary, memory.New())

	d, err := b.Allow(context.Background(), "k", ratelimit.Config{Rate: 1, Burst: 10, Cost: 1})
	require.NoError(t, err)
	assert.InDelta(t, 42, d.Remaining, 0.01, "answer must come from the primary")
	assert.False(t, d.Degraded)
	assert.False(t, b.Degraded())
}

func TestBreakerFallsBackAndFailsOpen(t *testing.T) {
	primary := &flakyLimiter{}
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(3)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 10, Cost: 1}
	ctx := context.Background()

	primary.setFailing(true)

	// Every failing request is still served — never rejected, never an error.
	for i := range 3 {
		d, err := b.Allow(ctx, "k", cfg)
		require.NoError(t, err, "a store failure must not surface as an error")
		assert.True(t, d.Allowed, "request %d must be admitted while degraded", i+1)
		assert.True(t, d.Degraded)
	}

	assert.True(t, b.Degraded(), "three consecutive failures must open the breaker")
}

func TestBreakerStopsCallingPrimaryWhileOpen(t *testing.T) {
	primary := &flakyLimiter{}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(2).WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 100, Cost: 1}
	ctx := context.Background()

	primary.setFailing(true)
	for range 2 {
		_, err := b.Allow(ctx, "k", cfg)
		require.NoError(t, err)
	}
	require.True(t, b.Degraded())

	callsWhenOpened := primary.callCount()
	for range 10 {
		_, err := b.Allow(ctx, "k", cfg)
		require.NoError(t, err)
	}
	assert.Equal(t, callsWhenOpened, primary.callCount(),
		"an open breaker must not keep dialling a store known to be down")
}

func TestBreakerProbesAfterCooldownAndRecovers(t *testing.T) {
	primary := &flakyLimiter{}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(2).WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 100, Cost: 1}
	ctx := context.Background()

	primary.setFailing(true)
	for range 2 {
		_, err := b.Allow(ctx, "k", cfg)
		require.NoError(t, err)
	}
	require.True(t, b.Degraded())

	// Store comes back, but the breaker is still inside its cooldown.
	primary.setFailing(false)
	d, err := b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	assert.True(t, d.Degraded, "still degraded until the cooldown elapses")

	clock.Advance(ratelimit.DefaultCooldown + time.Second)

	d, err = b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	assert.False(t, d.Degraded, "the probe after cooldown must return to the primary")
	assert.InDelta(t, 42, d.Remaining, 0.01)
	assert.False(t, b.Degraded())
}

func TestBreakerReopensWhenTheProbeFails(t *testing.T) {
	primary := &flakyLimiter{}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(1).WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 100, Cost: 1}
	ctx := context.Background()

	primary.setFailing(true)
	_, err := b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	require.True(t, b.Degraded())

	clock.Advance(ratelimit.DefaultCooldown + time.Second)
	_, err = b.Allow(ctx, "k", cfg) // probe, still failing
	require.NoError(t, err)
	assert.True(t, b.Degraded(), "a failed probe must re-open the breaker")
}

// TestDegradedLimiterStillLimits guards against reading "fail open" as "stop
// limiting". The fallback is a real bucket: it admits burst and then denies.
func TestDegradedLimiterStillLimits(t *testing.T) {
	primary := &flakyLimiter{}
	b := ratelimit.NewBreaker(primary, memory.New()).WithThreshold(1)
	cfg := ratelimit.Config{Rate: 0.01, Burst: 5, Cost: 1}
	ctx := context.Background()

	primary.setFailing(true)

	for i := range 5 {
		d, err := b.Allow(ctx, "k", cfg)
		require.NoError(t, err)
		require.True(t, d.Allowed, "request %d is within burst", i+1)
	}

	d, err := b.Allow(ctx, "k", cfg)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "the fallback still enforces the burst, just per instance")
	assert.True(t, d.Degraded)
}

func TestConfigForClampsUnusableValues(t *testing.T) {
	tests := []struct {
		name      string
		key       domain.APIKey
		wantRate  float64
		wantBurst int
	}{
		{
			name:      "sane values pass through",
			key:       domain.APIKey{ID: "a", RateLimitRPS: 10, RateLimitBurst: 20},
			wantRate:  10,
			wantBurst: 20,
		},
		{
			// Without the clamp this divides by zero in Lua, PEXPIRE receives
			// inf, and every call for this key errors — which the breaker
			// would report as "the store is down".
			name:      "zero rate falls back to the default",
			key:       domain.APIKey{ID: "b", RateLimitRPS: 0, RateLimitBurst: 20},
			wantRate:  ratelimit.DefaultRate,
			wantBurst: 20,
		},
		{
			name:      "negative rate falls back to the default",
			key:       domain.APIKey{ID: "c", RateLimitRPS: -1, RateLimitBurst: 20},
			wantRate:  ratelimit.DefaultRate,
			wantBurst: 20,
		},
		{
			// A burst below one would deny every request forever: a lockout,
			// which is the one outcome a rate limiter must never produce by
			// accident.
			name:      "zero burst falls back to the default",
			key:       domain.APIKey{ID: "d", RateLimitRPS: 10, RateLimitBurst: 0},
			wantRate:  10,
			wantBurst: ratelimit.DefaultBurst,
		},
		{
			name:      "fractional rate is preserved",
			key:       domain.APIKey{ID: "e", RateLimitRPS: 10.0 / 60.0, RateLimitBurst: 10},
			wantRate:  10.0 / 60.0,
			wantBurst: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ratelimit.ConfigFor(tt.key)
			assert.InDelta(t, tt.wantRate, cfg.Rate, 0.0001)
			assert.Equal(t, tt.wantBurst, cfg.Burst)
			assert.Equal(t, 1, cfg.Cost, "this service always spends exactly one token")
		})
	}
}

func TestRetryAfterDurationRoundsUpWithAOneSecondFloor(t *testing.T) {
	tests := []struct {
		retryAfter float64
		want       time.Duration
	}{
		{0, time.Second},   // never advertise an immediate retry that must fail
		{0.2, time.Second}, // sub-second rounds up
		{1.0, time.Second}, // exact
		{1.2, 2 * time.Second},
		{59.9, time.Minute},
	}

	for _, tt := range tests {
		d := ratelimit.Decision{RetryAfter: tt.retryAfter}
		assert.Equal(t, tt.want, d.RetryAfterDuration(), "retry after %v", tt.retryAfter)
	}
}
