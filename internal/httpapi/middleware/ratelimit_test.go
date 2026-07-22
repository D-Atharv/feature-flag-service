package middleware_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit"
	"github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
)

// newLimitedRouter wires Auth then RateLimit, as main.go does. The in-memory
// limiter stands in for Redis here on purpose: this file tests the HTTP
// contract (status, headers, body shape), not the bucket maths, which is
// proven against a real Redis in internal/ratelimit/redis.
func newLimitedRouter(km middleware.KeyMap, limiter ratelimit.Limiter) *gin.Engine {
	r := gin.New()
	r.Use(
		middleware.RequestIDWithSecurity(),
		middleware.Auth(km),
		middleware.RateLimit(limiter),
	)
	r.GET("/evaluate/:key", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
	})
	return r
}

func sha256Sum(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func limitedKeyMap(rps float64, burst int) (middleware.KeyMap, string) {
	const raw = "lo_live_ratelimit_test_key_000000001"
	sum := sha256Sum(raw)
	return middleware.NewKeyMap([]domain.APIKey{{
		ID:             "key-under-test",
		Name:           "ratelimit-test",
		KeyHash:        sum,
		RateLimitRPS:   rps,
		RateLimitBurst: burst,
		Active:         true,
	}}), raw
}

func doLimitedRequest(t *testing.T, r *gin.Engine, rawKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/evaluate/x?env=prod", nil)
	req.Header.Set("X-API-Key", rawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRateLimitHeadersOnAllowedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km, raw := limitedKeyMap(0.01, 10)
	r := newLimitedRouter(km, memory.New())

	w := doLimitedRequest(t, r, raw)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "10", w.Header().Get("RateLimit-Limit"))
	assert.Equal(t, "9", w.Header().Get("RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("RateLimit-Reset"), "headers are emitted on success too")
	assert.Empty(t, w.Header().Get("Retry-After"), "Retry-After belongs only on a 429")
}

func TestRateLimitDeniesAfterBurst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km, raw := limitedKeyMap(0.01, 3)
	r := newLimitedRouter(km, memory.New())

	for i := range 3 {
		w := doLimitedRequest(t, r, raw)
		require.Equal(t, http.StatusOK, w.Code, "request %d is within burst", i+1)
	}

	w := doLimitedRequest(t, r, raw)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "0", w.Header().Get("RateLimit-Remaining"))
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))

	var p problem.Problem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
	assert.Equal(t, http.StatusTooManyRequests, p.Status)
	assert.Equal(t, "Too Many Requests", p.Title)
	assert.NotEmpty(t, p.RequestID, "a 429 must still carry its request id")
}

// TestRetryAfterIsAnInteger guards the trap that makes the header unusable:
// the bucket returns fractional seconds, and RFC 9110 requires whole ones.
func TestRetryAfterIsAnInteger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km, raw := limitedKeyMap(10.0/60.0, 1) // the seeded demo key's shape
	r := newLimitedRouter(km, memory.New())

	require.Equal(t, http.StatusOK, doLimitedRequest(t, r, raw).Code)
	w := doLimitedRequest(t, r, raw)
	require.Equal(t, http.StatusTooManyRequests, w.Code)

	raw429 := w.Header().Get("Retry-After")
	require.NotEmpty(t, raw429)
	secs, err := strconv.Atoi(raw429)
	require.NoError(t, err, "Retry-After must parse as an integer, got %q", raw429)
	assert.GreaterOrEqual(t, secs, 1, "never advertise an immediate retry that is certain to fail")
}

func TestAllRateLimitHeadersAreIntegers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km, raw := limitedKeyMap(0.3, 7)
	r := newLimitedRouter(km, memory.New())

	w := doLimitedRequest(t, r, raw)
	require.Equal(t, http.StatusOK, w.Code)

	for _, h := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"} {
		v := w.Header().Get(h)
		_, err := strconv.Atoi(v)
		assert.NoError(t, err, "%s must be an integer, got %q", h, v)
	}
}

// failingLimiter stands in for a limiter whose every call errors.
type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, string, ratelimit.Config) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, assert.AnError
}

// TestLimiterErrorFailsOpen is the rule that keeps a limiter outage from
// becoming a service outage.
func TestLimiterErrorFailsOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	km, raw := limitedKeyMap(1, 10)
	r := newLimitedRouter(km, failingLimiter{})

	w := doLimitedRequest(t, r, raw)
	assert.Equal(t, http.StatusOK, w.Code, "a broken limiter must not reject traffic")
}

// TestMissingAPIKeyInContextFailsOpen covers the wiring bug: RateLimit mounted
// without Auth in front of it.
func TestMissingAPIKeyInContextFailsOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(middleware.RateLimit(memory.New())) // deliberately no Auth
	r.GET("/open", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/open", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestBucketsAreScopedPerAPIKey proves the limit is per key, as the assignment
// requires, and not global.
func TestBucketsAreScopedPerAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const rawA = "lo_live_ratelimit_test_key_00000000A"
	const rawB = "lo_live_ratelimit_test_key_00000000B"
	km := middleware.NewKeyMap([]domain.APIKey{
		{ID: "key-a", KeyHash: sha256Sum(rawA), RateLimitRPS: 0.01, RateLimitBurst: 1, Active: true},
		{ID: "key-b", KeyHash: sha256Sum(rawB), RateLimitRPS: 0.01, RateLimitBurst: 1, Active: true},
	})
	r := newLimitedRouter(km, memory.New())

	require.Equal(t, http.StatusOK, doLimitedRequest(t, r, rawA).Code)
	require.Equal(t, http.StatusTooManyRequests, doLimitedRequest(t, r, rawA).Code)

	assert.Equal(t, http.StatusOK, doLimitedRequest(t, r, rawB).Code,
		"key B must have its own budget")
}
