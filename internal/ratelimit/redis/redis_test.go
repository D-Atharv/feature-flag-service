package redis_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
	rlredis "github.com/D-Atharv/feature-flag-service/internal/ratelimit/redis"
)

// These tests run against a real Redis, deliberately. The one property worth
// proving here — that concurrent requests cannot over-admit — is a property of
// Redis executing the script atomically, and a mock would prove only that the
// mock is atomic.
//
// They are NOT behind a build tag: the phase's exit test is
// `go test -race ./internal/ratelimit/...`, and a test the exit gate cannot
// reach is not an exit gate.

func testClient(t *testing.T) *goredis.Client {
	t.Helper()

	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		// docker-compose publishes Redis on 6380; CI's service is on 6379 and
		// sets TEST_REDIS_ADDR explicitly.
		addr = "localhost:6380"
	}

	client := goredis.NewClient(&goredis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err(), "no Redis at %s — run `make up`", addr)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// uniqueKey keeps reruns independent: every test starts from a cold bucket
// without flushing a database it does not own.
func uniqueKey(t *testing.T, client *goredis.Client) string {
	t.Helper()
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	key := t.Name() + "-" + hex.EncodeToString(buf)
	t.Cleanup(func() {
		_ = client.Del(context.Background(), rlredis.BucketKey(key)).Err()
	})
	return key
}

func TestColdBucketStartsFullThenDenies(t *testing.T) {
	client := testClient(t)
	l := rlredis.New(client)
	key := uniqueKey(t, client)
	ctx := context.Background()

	cfg := ratelimit.Config{Rate: 0.01, Burst: 5, Cost: 1}

	for i := range 5 {
		d, err := l.Allow(ctx, key, cfg)
		require.NoError(t, err)
		require.True(t, d.Allowed, "request %d of the burst must be admitted", i+1)
		assert.Equal(t, 5, d.Limit)
	}

	d, err := l.Allow(ctx, key, cfg)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "burst+1 must be denied")
	assert.InDelta(t, 0, d.Remaining, 0.01)
	assert.Positive(t, d.RetryAfter)
	assert.Positive(t, d.ResetAfter)
}

// TestStateOutlivesTheClient is the restart requirement in miniature: a brand
// new client with brand new process memory sees the quota already spent.
func TestStateOutlivesTheClient(t *testing.T) {
	client := testClient(t)
	key := uniqueKey(t, client)
	ctx := context.Background()
	cfg := ratelimit.Config{Rate: 0.01, Burst: 3, Cost: 1}

	first := rlredis.New(client)
	for range 3 {
		d, err := first.Allow(ctx, key, cfg)
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}

	// A second client and a second Limiter: nothing carries over in Go.
	second := rlredis.New(testClient(t))
	d, err := second.Allow(ctx, key, cfg)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "quota must live in Redis, not in the process")
}

// TestBucketKeyExpires guards the leak: without PEXPIRE, every API key that
// ever made a request keeps one Redis key forever.
func TestBucketKeyExpires(t *testing.T) {
	client := testClient(t)
	l := rlredis.New(client)
	key := uniqueKey(t, client)
	ctx := context.Background()

	_, err := l.Allow(ctx, key, ratelimit.Config{Rate: 1, Burst: 10, Cost: 1})
	require.NoError(t, err)

	ttl, err := client.PTTL(ctx, rlredis.BucketKey(key)).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl, "bucket key must carry a TTL")
	// burst/rate + 1s slack = 11s for this config.
	assert.LessOrEqual(t, ttl, 11*time.Second)
}

// TestTTLIsCappedForVerySlowRefill covers the Lua stringification trap: an
// uncapped TTL for a slow bucket becomes "1e+14", which Redis rejects as a
// PEXPIRE argument, failing every call for that key.
func TestTTLIsCappedForVerySlowRefill(t *testing.T) {
	client := testClient(t)
	l := rlredis.New(client)
	key := uniqueKey(t, client)
	ctx := context.Background()

	_, err := l.Allow(ctx, key, ratelimit.Config{Rate: 1e-9, Burst: 1000, Cost: 1})
	require.NoError(t, err, "an absurdly slow refill must not error")

	ttl, err := client.PTTL(ctx, rlredis.BucketKey(key)).Result()
	require.NoError(t, err)
	assert.InDelta(t, float64(24*time.Hour), float64(ttl), float64(time.Minute))
}

// TestSurvivesScriptCacheFlush is the restart demo's failure mode, isolated.
// Redis's script cache is volatile and empty after a restart, so an EVALSHA
// with no EVAL fallback fails on exactly the restart being demonstrated.
func TestSurvivesScriptCacheFlush(t *testing.T) {
	client := testClient(t)
	l := rlredis.New(client)
	key := uniqueKey(t, client)
	ctx := context.Background()
	cfg := ratelimit.Config{Rate: 0.01, Burst: 5, Cost: 1}

	_, err := l.Allow(ctx, key, cfg)
	require.NoError(t, err)

	require.NoError(t, client.ScriptFlush(ctx).Err())

	d, err := l.Allow(ctx, key, cfg)
	require.NoError(t, err, "Script.Run must recover from NOSCRIPT by re-sending EVAL")
	assert.True(t, d.Allowed)
	assert.InDelta(t, 3, d.Remaining, 0.01, "and the bucket must be unchanged by the flush")
}

func TestCostExceedingBurstIsAlwaysDenied(t *testing.T) {
	client := testClient(t)
	l := rlredis.New(client)
	key := uniqueKey(t, client)
	ctx := context.Background()

	d, err := l.Allow(ctx, key, ratelimit.Config{Rate: 2, Burst: 5, Cost: 6})
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.InDelta(t, 2.5, d.RetryAfter, 0.01, "finite and usable, not an infinity")
}

// TestConcurrentAllowAdmitsExactlyBurst is the definitive test of this phase.
// It is the one that fails if the read-modify-write ever moves out of Lua and
// into Go.
//
// Rate is 0.01 tok/s so that no refill can land mid-test and turn "exactly
// 100" into an intermittent 101.
func TestConcurrentAllowAdmitsExactlyBurst(t *testing.T) {
	const (
		burst      = 100
		goroutines = 250
	)

	client := testClient(t)
	l := rlredis.New(client)
	key := uniqueKey(t, client)
	cfg := ratelimit.Config{Rate: 0.01, Burst: burst, Cost: 1}

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		allowed int
		errs    []error
	)
	start.Add(1)
	done.Add(goroutines)

	for range goroutines {
		go func() {
			defer done.Done()
			start.Wait()
			d, err := l.Allow(context.Background(), key, cfg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if d.Allowed {
				allowed++
			}
		}()
	}

	start.Done()
	done.Wait()

	require.Empty(t, errs)
	assert.Equal(t, burst, allowed,
		"exactly burst admissions — more means the read-modify-write is not atomic")
}

// TestParityWithMemoryLimiter pins the two implementations together. The
// fallback duplicates the bucket maths in Go because it cannot call Redis, and
// a fallback that answers differently from the primary is a silent behaviour
// change the moment Redis blips.
//
// Rate is 0.01 tok/s throughout so that Redis's own clock advancing during the
// test cannot introduce a difference the in-memory clock did not see.
func TestParityWithMemoryLimiter(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()

	scenarios := []struct {
		name  string
		cfg   ratelimit.Config
		calls int
	}{
		{"drain past the burst", ratelimit.Config{Rate: 0.01, Burst: 5, Cost: 1}, 7},
		{"single token bucket", ratelimit.Config{Rate: 0.01, Burst: 1, Cost: 1}, 3},
		{"cost above burst", ratelimit.Config{Rate: 0.01, Burst: 3, Cost: 5}, 2},
		{"weighted cost", ratelimit.Config{Rate: 0.01, Burst: 10, Cost: 3}, 5},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			redisLimiter := rlredis.New(client)
			memLimiter := memory.New()
			key := uniqueKey(t, client)

			for i := range sc.calls {
				rd, err := redisLimiter.Allow(ctx, key, sc.cfg)
				require.NoError(t, err)
				md, err := memLimiter.Allow(ctx, key, sc.cfg)
				require.NoError(t, err)

				assert.Equal(t, md.Allowed, rd.Allowed, "call %d: allowed", i+1)
				assert.Equal(t, md.Limit, rd.Limit, "call %d: limit", i+1)
				assert.InDelta(t, md.Remaining, rd.Remaining, 0.05, "call %d: remaining", i+1)
				assert.InDelta(t, md.RetryAfter, rd.RetryAfter, 0.5, "call %d: retry after", i+1)
				assert.InDelta(t, md.ResetAfter, rd.ResetAfter, 0.5, "call %d: reset after", i+1)
			}
		})
	}
}
