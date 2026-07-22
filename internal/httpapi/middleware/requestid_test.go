package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
)

// decodeJSON is a shared helper for all middleware tests.
func decodeJSON(w *httptest.ResponseRecorder, v any) error {
	return json.NewDecoder(w.Body).Decode(v)
}

func newRIDRouter() *gin.Engine {
	r := gin.New()
	r.Use(middleware.RequestIDWithSecurity())
	r.GET("/test", func(c *gin.Context) {
		rid, _ := c.Get(middleware.ContextKeyRequestID)
		c.JSON(http.StatusOK, gin.H{"request_id": rid})
	})
	return r
}

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	newRIDRouter().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	rid := w.Header().Get("X-Request-ID")
	assert.NotEmpty(t, rid, "response must carry X-Request-ID header")
	assert.Len(t, rid, 32, "generated ID must be 32-char hex")
}

func TestRequestID_EchoesInboundID(t *testing.T) {
	const inbound = "my-correlation-id-abc123"
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", inbound)
	newRIDRouter().ServeHTTP(w, req)

	assert.Equal(t, inbound, w.Header().Get("X-Request-ID"))
}

func TestRequestID_StoredInGinContext(t *testing.T) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	newRIDRouter().ServeHTTP(w, req)

	var body map[string]string
	require.NoError(t, decodeJSON(w, &body))
	assert.NotEmpty(t, body["request_id"], "request_id must be set in Gin context")
}

func TestRequestID_EchoedIDMatchesContextValue(t *testing.T) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	newRIDRouter().ServeHTTP(w, req)

	var body map[string]string
	require.NoError(t, decodeJSON(w, &body))

	assert.Equal(t, w.Header().Get("X-Request-ID"), body["request_id"],
		"response header and context value must be identical")
}

func TestRequestID_TwoRequestsGetDifferentIDs(t *testing.T) {
	r := newRIDRouter()

	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/test", nil)
	r.ServeHTTP(w1, req1)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/test", nil)
	r.ServeHTTP(w2, req2)

	assert.NotEqual(t, w1.Header().Get("X-Request-ID"), w2.Header().Get("X-Request-ID"),
		"two requests must get different IDs")
}

func TestRequestID_SecurityHeadersPresent(t *testing.T) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	newRIDRouter().ServeHTTP(w, req)

	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
}
