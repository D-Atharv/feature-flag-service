package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
)

// fakeClock is a manually advanced clock, so refill maths is tested by
// arithmetic rather than by sleeping.
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

func TestColdBucketStartsFull(t *testing.T) {
	l := memory.New()
	cfg := ratelimit.Config{Rate: 1, Burst: 10, Cost: 1}

	d, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)

	assert.True(t, d.Allowed, "a bucket nobody has used must fail open")
	assert.Equal(t, 10, d.Limit)
	assert.InDelta(t, 9, d.Remaining, 0.01)
}

func TestBurstThenDenial(t *testing.T) {
	clock := newFakeClock()
	l := memory.New().WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 1, Burst: 5, Cost: 1}

	for i := range 5 {
		d, err := l.Allow(context.Background(), "k", cfg)
		require.NoError(t, err)
		require.True(t, d.Allowed, "request %d of the burst must be admitted", i+1)
	}

	d, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "the burst+1 request must be denied")
	assert.InDelta(t, 0, d.Remaining, 0.01)
	assert.InDelta(t, 1, d.RetryAfter, 0.01, "at 1 tok/s the next token is 1s away")
	assert.InDelta(t, 5, d.ResetAfter, 0.01, "a full refill of 5 tokens at 1 tok/s")
}

func TestRefill(t *testing.T) {
	clock := newFakeClock()
	l := memory.New().WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 10, Burst: 100, Cost: 1}

	// Drain 50 tokens.
	for range 50 {
		_, err := l.Allow(context.Background(), "k", cfg)
		require.NoError(t, err)
	}

	// 1s at 10 tok/s = +10 tokens. 50 remain, minus this request, plus 10.
	clock.Advance(time.Second)
	d, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	assert.InDelta(t, 59, d.Remaining, 0.01)
}

func TestRefillClampsAtBurst(t *testing.T) {
	clock := newFakeClock()
	l := memory.New().WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 10, Burst: 20, Cost: 1}

	_, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)

	// An hour of refill at 10 tok/s is 36 000 tokens into a 20-token bucket.
	clock.Advance(time.Hour)
	d, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	assert.InDelta(t, 19, d.Remaining, 0.01, "tokens must never exceed burst")
}

func TestFractionalRate(t *testing.T) {
	clock := newFakeClock()
	l := memory.New().WithClock(clock.Now)
	// The seeded demo key: 10 per minute.
	cfg := ratelimit.Config{Rate: 10.0 / 60.0, Burst: 1, Cost: 1}

	d, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	require.True(t, d.Allowed)

	d, err = l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	assert.InDelta(t, 6, d.RetryAfter, 0.1, "one token every six seconds")

	clock.Advance(6 * time.Second)
	d, err = l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "one token must have refilled after 6s")
}

func TestCostExceedingBurstIsAlwaysDenied(t *testing.T) {
	clock := newFakeClock()
	l := memory.New().WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 2, Burst: 5, Cost: 6}

	for range 3 {
		d, err := l.Allow(context.Background(), "k", cfg)
		require.NoError(t, err)
		require.False(t, d.Allowed, "cost > burst can never be satisfied")
		// Finite and usable, not an infinity: time to fill an empty bucket.
		assert.InDelta(t, 2.5, d.RetryAfter, 0.01)
		clock.Advance(time.Hour)
	}
}

func TestBackwardsClockDoesNotDrain(t *testing.T) {
	clock := newFakeClock()
	l := memory.New().WithClock(clock.Now)
	cfg := ratelimit.Config{Rate: 1, Burst: 10, Cost: 1}

	_, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)

	clock.Advance(-time.Hour) // NTP correction, suspended laptop, VM migration
	d, err := l.Allow(context.Background(), "k", cfg)
	require.NoError(t, err)
	assert.InDelta(t, 8, d.Remaining, 0.01, "negative elapsed contributes zero refill")
}

func TestBucketsAreIndependentPerKey(t *testing.T) {
	l := memory.New()
	cfg := ratelimit.Config{Rate: 0.01, Burst: 2, Cost: 1}

	for range 2 {
		d, err := l.Allow(context.Background(), "key-a", cfg)
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}

	d, err := l.Allow(context.Background(), "key-a", cfg)
	require.NoError(t, err)
	require.False(t, d.Allowed, "key-a is exhausted")

	d, err = l.Allow(context.Background(), "key-b", cfg)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "key-b must not be affected by key-a")
	assert.Equal(t, 2, l.Len())
}

// TestConcurrentAllowAdmitsExactlyBurst is the test to keep if only one
// survives. A non-atomic read-modify-write admits more than burst, and the
// -race detector alone would not necessarily catch it: two goroutines can read
// tokens=1 and both allow without ever racing on the same word.
//
// Rate is deliberately 0.01 tok/s (one token every 100 seconds) so that no
// refill can land mid-test and turn "exactly 100" into a flaky 101.
func TestConcurrentAllowAdmitsExactlyBurst(t *testing.T) {
	const (
		burst      = 100
		goroutines = 250
	)

	l := memory.New()
	cfg := ratelimit.Config{Rate: 0.01, Burst: burst, Cost: 1}

	var (
		start   sync.WaitGroup // released all at once, to maximise contention
		done    sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	start.Add(1)
	done.Add(goroutines)

	for range goroutines {
		go func() {
			defer done.Done()
			start.Wait()
			d, err := l.Allow(context.Background(), "hot", cfg)
			if err == nil && d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}

	start.Done()
	done.Wait()

	assert.Equal(t, burst, allowed,
		"exactly burst admissions: fewer means lost updates, more means a lost mutex")
}
