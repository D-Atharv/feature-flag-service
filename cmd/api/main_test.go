package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	rlmemory "github.com/D-Atharv/feature-flag-service/internal/ratelimit/memory"
	"github.com/D-Atharv/feature-flag-service/internal/snapshot"
)

// testRouter builds the real router with empty dependencies. The probes are
// registered before Use(), so nothing downstream is exercised and the nil
// repository, pool and Redis client are never dereferenced.
func testRouter() http.Handler {
	return newRouter(nil, snapshot.New(), middleware.NewKeyMap(nil), rlmemory.New(), nil, nil)
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

// TestReadyzGatesOnSnapshotNotPostgres: an unloaded snapshot is not ready, and
// a loaded one is ready regardless of the database, which is the entire point
// of the read path.
func TestReadyzGatesOnSnapshotNotPostgres(t *testing.T) {
	flags := snapshot.New()
	router := newRouter(nil, flags, middleware.NewKeyMap(nil), rlmemory.New(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "no snapshot yet")

	// Snapshot loaded, no Postgres anywhere in sight.
	flags.Replace(nil)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "loaded snapshot is ready without a database")
}
