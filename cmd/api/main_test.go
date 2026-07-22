package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	rlmemory "github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
)

// testRouter builds the real router with empty dependencies. /healthz is
// registered before Use(), so nothing downstream of it is exercised and nil
// repositories are never dereferenced.
func testRouter() http.Handler {
	return newRouter(nil, middleware.NewKeyMap(nil), rlmemory.New())
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	testRouter().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

// TestHealthzBypassesAuthAndRateLimit pins the registration order in
// newRouter. If /healthz were ever moved below Use(), liveness would start
// requiring an API key and consuming that key's quota — and a liveness probe
// that can be rate limited eventually restarts a healthy instance.
func TestHealthzBypassesAuthAndRateLimit(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil) // no X-API-Key
	rec := httptest.NewRecorder()

	testRouter().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "liveness must not require authentication")
	assert.Empty(t, rec.Header().Get("RateLimit-Limit"), "liveness must not pass through the limiter")
}
