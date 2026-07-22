package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

func newRecoveryRouter(panicValue any) *gin.Engine {
	r := gin.New()
	r.Use(middleware.RequestIDWithSecurity()) // RequestID first so recovery has the ID
	r.Use(middleware.Recovery())
	r.GET("/panic", func(c *gin.Context) {
		panic(panicValue)
	})
	r.GET("/ok", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func TestRecovery_PanicBecomesInternal500(t *testing.T) {
	r := newRecoveryRouter("something went wrong")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/panic", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))

	var p problem.Problem
	require.NoError(t, decodeJSON(w, &p))
	assert.Equal(t, http.StatusInternalServerError, p.Status)
	// Internal errors must never leak the panic value to the client.
	assert.NotContains(t, p.Detail, "something went wrong")
}

func TestRecovery_ChainContinuesAfterPanic(t *testing.T) {
	r := newRecoveryRouter("boom")

	// First request panics.
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/panic", nil)
	r.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusInternalServerError, w1.Code)

	// Second request on the same router must work normally.
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/ok", nil)
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

func TestRecovery_PanicWithError(t *testing.T) {
	r := newRecoveryRouter(assert.AnError)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/panic", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestRecovery_ResponseBodyIsValidJSON(t *testing.T) {
	r := newRecoveryRouter("any panic value")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/panic", nil)
	r.ServeHTTP(w, req)

	var p problem.Problem
	require.NoError(t, decodeJSON(w, &p), "response body must be valid JSON")
	assert.NotEmpty(t, p.Type)
	assert.NotEmpty(t, p.Title)
}
