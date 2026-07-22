package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
)

// newLoggerRouter returns a router that redirects slog output to buf.
// Callers must call t.Cleanup(restoreLogger) to restore the global logger.
func newLoggerRouter(t *testing.T, buf *bytes.Buffer, path string, status int) *gin.Engine {
	t.Helper()
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })

	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))

	r := gin.New()
	r.Use(middleware.RequestIDWithSecurity())
	r.Use(middleware.Logger())
	r.GET(path, func(c *gin.Context) {
		c.Status(status)
	})
	return r
}

func TestLogger_EmitsStructuredLog(t *testing.T) {
	var buf bytes.Buffer
	r := newLoggerRouter(t, &buf, "/api/v1/flags", http.StatusOK)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/flags", nil)
	r.ServeHTTP(w, req)

	line := buf.String()
	assert.NotEmpty(t, line, "logger must emit at least one log line")

	var entry map[string]any
	require.NoError(t, json.NewDecoder(strings.NewReader(line)).Decode(&entry))
	assert.Contains(t, entry, "request_id")
	assert.Contains(t, entry, "method")
	assert.Contains(t, entry, "path")
	assert.Contains(t, entry, "status")
	assert.Contains(t, entry, "latency_ms")
}

func TestLogger_RedactsAPIKey(t *testing.T) {
	var buf bytes.Buffer
	r := newLoggerRouter(t, &buf, "/test", http.StatusOK)

	const sensitiveKey = "lo_live_supersecretapikey1234567890ab"
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Key", sensitiveKey)
	r.ServeHTTP(w, req)

	assert.NotContains(t, buf.String(), sensitiveKey,
		"X-API-Key must never appear in log output")
}

func TestLogger_ErrorsAlwaysLogged(t *testing.T) {
	var buf bytes.Buffer
	r := newLoggerRouter(t, &buf, "/evaluate/:key", http.StatusBadRequest)

	// Run 100 requests — all must be logged (errors skip sampling).
	for range 100 {
		buf.Reset()
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/evaluate/my-flag", nil)
		r.ServeHTTP(w, req)
		assert.NotEmpty(t, buf.String(), "4xx on /evaluate must always be logged")
	}
}

func TestLogger_EvaluateSuccessIsSampled(t *testing.T) {
	var buf bytes.Buffer
	r := newLoggerRouter(t, &buf, "/evaluate/:key", http.StatusOK)

	logged := 0
	const requests = 10_000
	for range requests {
		buf.Reset()
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/evaluate/my-flag", nil)
		r.ServeHTTP(w, req)
		if buf.Len() > 0 {
			logged++
		}
	}

	// At 1% sample rate over 10 000 requests, expect ~100.
	// Tolerance: ±150 (very loose — prevents flake, still catches a broken sampler).
	assert.GreaterOrEqual(t, logged, 30,
		"sampling rate too low: %d/%d logged (expected ~100)", logged, requests)
	assert.LessOrEqual(t, logged, 200,
		"sampling rate too high: %d/%d logged (expected ~100)", logged, requests)
}

func TestLogger_NonEvaluatePathAlwaysLogged(t *testing.T) {
	var buf bytes.Buffer
	r := newLoggerRouter(t, &buf, "/api/v1/flags", http.StatusOK)

	for range 10 {
		buf.Reset()
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/flags", nil)
		r.ServeHTTP(w, req)
		assert.NotEmpty(t, buf.String(), "non-evaluate path must always be logged")
	}
}
