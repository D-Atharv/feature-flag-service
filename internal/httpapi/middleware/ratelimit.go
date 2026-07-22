package middleware

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
)

// RateLimit enforces one token bucket per API key.
//
// Position 8, immediately after Auth and last before the handler. It cannot
// move earlier: the bucket is keyed on the API key's ID, so the identity has
// to be resolved first. That ordering has a cost worth naming — an
// unauthenticated flood still pays for one auth check — but auth is an
// in-memory SHA-256 map lookup with no database call and no slow KDF, so a
// rejected request costs microseconds. A coarse pre-auth per-IP limiter is the
// missing piece; it is named in the README rather than half-built.
//
// Every failure that is not a genuine rate-limit decision fails OPEN. A
// limiter that turns its own bugs into 500s is worse than no limiter.
func RateLimit(limiter ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := apiKeyFromContext(c)
		if !ok {
			// Auth did not run, or ran and stored something unexpected. That
			// is a wiring bug in this repo, not a client error — log it loudly
			// and let the request through rather than failing a caller for it.
			slog.Error("rate limiter reached without an authenticated API key; allowing the request",
				"path", c.FullPath())
			c.Next()
			return
		}

		cfg := ratelimit.ConfigFor(key)

		d, err := limiter.Allow(c.Request.Context(), key.ID, cfg)
		if err != nil {
			// The breaker already absorbs store failures, so reaching here
			// means something unanticipated. Still fail open.
			slog.Warn("rate limiter error; allowing the request",
				"key_id", key.ID, "error", err.Error())
			c.Next()
			return
		}

		// Headers first, always, on allowed and denied alike: a client can only
		// pace itself if it is told the budget before it runs out. They must be
		// written before problem.Render, which flushes the header block.
		setRateLimitHeaders(c, d)
		ratelimit.RecordDecision(d.Allowed)

		if !d.Allowed {
			retry := d.RetryAfterDuration()
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())))
			problem.Render(c, problem.NewTooManyRequests(fmt.Sprintf(
				"rate limit exceeded: %d requests per burst; retry in %s",
				d.Limit, retry)))
			return
		}

		c.Next()
	}
}

// apiKeyFromContext reads the key Auth stored.
func apiKeyFromContext(c *gin.Context) (domain.APIKey, bool) {
	v, exists := c.Get(ContextKeyAPIKey)
	if !exists {
		return domain.APIKey{}, false
	}
	key, ok := v.(domain.APIKey)
	return key, ok
}

// setRateLimitHeaders emits the draft IETF RateLimit header trio.
//
// All three are integers. The bucket holds fractional tokens, but a client
// parsing "RateLimit-Remaining: 3.7" is entitled to reject it, and Retry-After
// is defined by RFC 9110 as whole seconds. Remaining rounds DOWN so the header
// never promises a token that is not there; Reset rounds UP so a client that
// waits exactly that long finds the bucket full.
func setRateLimitHeaders(c *gin.Context, d ratelimit.Decision) {
	remaining := int(math.Floor(d.Remaining))
	if remaining < 0 {
		remaining = 0
	}
	reset := int(math.Ceil(d.ResetAfter))
	if reset < 0 {
		reset = 0
	}

	c.Header("RateLimit-Limit", strconv.Itoa(d.Limit))
	c.Header("RateLimit-Remaining", strconv.Itoa(remaining))
	// Delta-seconds, not a Unix timestamp — the draft defines Reset as a
	// duration, and a timestamp would also force the client to trust our clock.
	c.Header("RateLimit-Reset", strconv.Itoa(reset))
}
