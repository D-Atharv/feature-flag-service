// Package redis implements the authoritative token bucket, with state held in
// Redis so that it outlives any single API process.
//
// This is the answer to the assignment's third requirement. The bucket's whole
// state is (tokens, last_refill_timestamp) and refill is a pure function of
// elapsed time, so moving that pair out of process memory is all it takes for
// quota to survive a restart, a redeploy, or a scale-out to N instances.
package redis

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"

	goredis "github.com/redis/go-redis/v9"

	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
)

// keyPrefix namespaces bucket keys and carries a schema version. If the bucket
// maths ever changes shape, bumping v1 to v2 abandons the old buckets rather
// than reinterpreting them — and abandoning a bucket is safe, because a
// missing bucket reads as full.
const keyPrefix = "rl:v1:"

//go:embed bucket.lua
var bucketScript string

// Limiter runs the token bucket inside Redis.
type Limiter struct {
	client goredis.Scripter
	script *goredis.Script
}

// New returns a Limiter that evaluates the bucket script against client.
func New(client goredis.Scripter) *Limiter {
	return &Limiter{client: client, script: goredis.NewScript(bucketScript)}
}

// Allow implements ratelimit.Limiter with one round trip and no client-side
// state.
//
// script.Run is not a convenience wrapper — it is the difference between a
// working restart demo and a baffling one. Run tries EVALSHA, and on NOSCRIPT
// falls back to EVAL and re-caches. Redis's script cache is volatile and is
// emptied on restart, so a raw EVALSHA fails on precisely the restart this
// service exists to survive.
func (l *Limiter) Allow(ctx context.Context, key string, cfg ratelimit.Config) (ratelimit.Decision, error) {
	raw, err := l.script.Run(ctx, l.client,
		[]string{keyPrefix + key},
		cfg.Rate, cfg.Burst, cfg.Cost,
	).StringSlice()
	if err != nil {
		return ratelimit.Decision{}, fmt.Errorf("run bucket script: %w", err)
	}
	if len(raw) != 4 {
		return ratelimit.Decision{}, fmt.Errorf("bucket script returned %d values, want 4", len(raw))
	}

	vals := make([]float64, 4)
	for i, s := range raw {
		v, parseErr := strconv.ParseFloat(s, 64)
		if parseErr != nil {
			return ratelimit.Decision{}, fmt.Errorf("bucket script value %d (%q): %w", i, s, parseErr)
		}
		vals[i] = v
	}

	return ratelimit.Decision{
		Allowed:    vals[0] != 0,
		Limit:      cfg.Burst,
		Remaining:  vals[1],
		RetryAfter: vals[2],
		ResetAfter: vals[3],
	}, nil
}

// BucketKey returns the Redis key backing an API key's bucket. Exported for
// tests and for live debugging (HGETALL rl:v1:<id>) during the walkthrough.
func BucketKey(key string) string { return keyPrefix + key }
