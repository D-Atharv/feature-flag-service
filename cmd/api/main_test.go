package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apidocs "github.com/D-Atharv/feature-flag-service/internal/httpapi/docs"
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

// TestIndexAndDocsAreReachableWithoutAKey.
//
// The playbook's rule is that an interviewer clicking the bare URL must never
// see a 404 — but a 401 fails that intent just as completely, which is why
// these are registered ahead of the auth chain.
func TestIndexAndDocsAreReachableWithoutAKey(t *testing.T) {
	router := testRouter()

	for _, path := range []string{"/", "/docs", "/docs/openapi.json"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, "%s must be public", path)
	}
}

// TestIndexReportsBuildMetadata: the exit test is "curl / and see the git SHA
// of what's running", so the fields have to be present and named.
func TestIndexReportsBuildMetadata(t *testing.T) {
	rec := httptest.NewRecorder()
	testRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, field := range []string{"version", "git_sha", "build_time", "docs", "health", "metrics"} {
		assert.Contains(t, body, field)
	}
}

// TestOpenAPISpecIsValidAndComplete guards against a spec that parses but
// silently stops describing the API.
func TestOpenAPISpecIsValidAndComplete(t *testing.T) {
	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(apidocs.Spec(), &spec), "openapi.json must be valid JSON")

	assert.Equal(t, "3.1.0", spec.OpenAPI)

	for _, path := range []string{
		"/evaluate/{key}",
		"/api/v1/evaluate/{key}",
		"/api/v1/flags",
		"/api/v1/flags/{key}",
		"/healthz",
		"/readyz",
		"/metrics",
	} {
		assert.Contains(t, spec.Paths, path, "spec must document %s", path)
	}

	// Admin-only diagnostics are not advertised.
	assert.NotContains(t, spec.Paths, "/debug/status")
}

// TestSpecDocumentsTheSurprisingBehaviours. A spec that omits these describes a
// different service than the one running: an unknown flag answers 200, and
// every response carries rate-limit headers.
func TestSpecDocumentsTheSurprisingBehaviours(t *testing.T) {
	spec := string(apidocs.Spec())

	assert.Contains(t, spec, "FLAG_NOT_FOUND")
	assert.Contains(t, spec, "RateLimit-Remaining")
	assert.Contains(t, spec, "Retry-After")
}

// TestSpecMatchesTheHandlersItDocuments.
//
// A spec that parses but lies is worse than no spec: a generated client breaks
// at runtime with no clue why. These are the three shapes hand-writing got
// wrong, pinned against the handler types so drift fails the build.
func TestSpecMatchesTheHandlersItDocuments(t *testing.T) {
	var spec struct {
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(apidocs.Spec(), &spec))

	// The list endpoint returns {"data":[...],"next_cursor":"..."}, not a bare
	// array, because handlers.listResponse wraps it.
	require.Contains(t, spec.Components.Schemas, "FlagList")
	list, _ := json.Marshal(spec.Components.Schemas["FlagList"])
	assert.Contains(t, string(list), "next_cursor")

	listGet, _ := json.Marshal(spec.Paths["/api/v1/flags"]["get"])
	assert.Contains(t, string(listGet), "FlagList",
		"GET /flags returns the envelope, not a bare array")

	// GET /flags/{key} is shape-polymorphic on ?environment=.
	byKeyGet, _ := json.Marshal(spec.Paths["/api/v1/flags/{key}"]["get"])
	assert.Contains(t, string(byKeyGet), "oneOf",
		"GET /flags/{key} returns a Flag with ?environment= and an envelope without it")
}

// TestScalarVersionIsPinned: an unpinned CDN script lets a third party change
// what runs in a reviewer's browser, and a breaking release takes the demo
// with it.
func TestScalarVersionIsPinned(t *testing.T) {
	rec := httptest.NewRecorder()
	testRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Regexp(t, `@scalar/api-reference@\d+\.\d+\.\d+`, rec.Body.String(),
		"the console bundle must be pinned to an exact version")
}
