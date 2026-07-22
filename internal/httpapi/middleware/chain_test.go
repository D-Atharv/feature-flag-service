package middleware_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

// newFullChainRouter builds a router with the complete middleware stack wired
// in the documented order, exactly as main.go does it.
func newFullChainRouter(t *testing.T, logBuf *bytes.Buffer, km middleware.KeyMap) *gin.Engine {
	t.Helper()
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	r := gin.New()
	r.Use(
		middleware.RequestIDWithSecurity(),
		middleware.Recovery(),
		middleware.Logger(),
		middleware.Metrics(),
		middleware.BodyLimit(),
		middleware.Timeout(),
		middleware.Auth(km),
	)

	adminGroup := r.Group("/")
	adminGroup.Use(middleware.RequireAdmin())
	adminGroup.GET("/admin-only", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	r.GET("/open", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	r.GET("/panic-route", func(_ *gin.Context) {
		panic("test panic")
	})

	return r
}

func makeTestKeyMap() (middleware.KeyMap, string, string) {
	const adminRaw = "lo_live_adminkey_chain_test_000001234"
	const evalRaw = "lo_live_evalkey_chain_test_0000012345"

	adminHash := sha256.Sum256([]byte(adminRaw))
	evalHash := sha256.Sum256([]byte(evalRaw))

	km := middleware.NewKeyMap([]domain.APIKey{
		{ID: "admin-id", Name: "admin", KeyHash: adminHash[:], IsAdmin: true, Active: true},
		{ID: "eval-id", Name: "eval", KeyHash: evalHash[:], IsAdmin: false, Active: true},
	})
	return km, adminRaw, evalRaw
}

// TestChain_RequestIDFlowsInto401Body checks that the request_id set by
// RequestID middleware appears inside the problem body returned by Auth.
func TestChain_RequestIDFlowsInto401Body(t *testing.T) {
	var logBuf bytes.Buffer
	km, _, _ := makeTestKeyMap()
	r := newFullChainRouter(t, &logBuf, km)

	req, _ := http.NewRequest(http.MethodGet, "/open", nil)
	req.Header.Set("X-Request-ID", "trace-abc-123")
	// No X-API-Key — Auth will reject with 401.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))

	var p problem.Problem
	require.NoError(t, json.NewDecoder(w.Body).Decode(&p))
	assert.Equal(t, "trace-abc-123", p.RequestID,
		"request_id set by RequestID middleware must appear in the 401 problem body")
}

// TestChain_401IsLogged checks that Logger (position 3, outside Auth) logs
// rejected requests — moving Logger inside Auth would lose this.
func TestChain_401IsLogged(t *testing.T) {
	var logBuf bytes.Buffer
	km, _, _ := makeTestKeyMap()
	r := newFullChainRouter(t, &logBuf, km)

	req, _ := http.NewRequest(http.MethodGet, "/open", nil)
	// No key — will produce 401.
	httptest.NewRecorder()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)

	var entry map[string]any
	require.NoError(t, json.NewDecoder(strings.NewReader(logBuf.String())).Decode(&entry),
		"logger must emit a log line for rejected requests")
	assert.Equal(t, float64(http.StatusUnauthorized), entry["status"],
		"log line must record the 401 status")
}

// TestChain_403ForNonAdminOnAdminRoute confirms RequireAdmin blocks evaluate keys.
func TestChain_403ForNonAdminOnAdminRoute(t *testing.T) {
	var logBuf bytes.Buffer
	km, _, evalRaw := makeTestKeyMap()
	r := newFullChainRouter(t, &logBuf, km)

	req, _ := http.NewRequest(http.MethodGet, "/admin-only", nil)
	req.Header.Set("X-API-Key", evalRaw)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

// TestChain_AdminKeyPassesRequireAdmin confirms admin keys reach the handler.
func TestChain_AdminKeyPassesRequireAdmin(t *testing.T) {
	var logBuf bytes.Buffer
	km, adminRaw, _ := makeTestKeyMap()
	r := newFullChainRouter(t, &logBuf, km)

	req, _ := http.NewRequest(http.MethodGet, "/admin-only", nil)
	req.Header.Set("X-API-Key", adminRaw)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestChain_PanicIsContained confirms Recovery turns a panic into a 500
// and the next request still works (chain not corrupted).
func TestChain_PanicIsContained(t *testing.T) {
	var logBuf bytes.Buffer
	km, adminRaw, _ := makeTestKeyMap()
	r := newFullChainRouter(t, &logBuf, km)

	req, _ := http.NewRequest(http.MethodGet, "/panic-route", nil)
	req.Header.Set("X-API-Key", adminRaw)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	// Next request must work normally.
	req2, _ := http.NewRequest(http.MethodGet, "/admin-only", nil)
	req2.Header.Set("X-API-Key", adminRaw)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

// TestChain_SecurityHeadersPresentOnAllResponses checks that security headers
// are set by RequestIDWithSecurity regardless of auth outcome.
func TestChain_SecurityHeadersPresentOnAllResponses(t *testing.T) {
	var logBuf bytes.Buffer
	km, adminRaw, _ := makeTestKeyMap()
	r := newFullChainRouter(t, &logBuf, km)

	for _, rawKey := range []string{adminRaw, ""} {
		req, _ := http.NewRequest(http.MethodGet, "/admin-only", nil)
		if rawKey != "" {
			req.Header.Set("X-API-Key", rawKey)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"),
			"X-Content-Type-Options must be set for key=%q", rawKey)
		assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"),
			"X-Frame-Options must be set for key=%q", rawKey)
	}
}
