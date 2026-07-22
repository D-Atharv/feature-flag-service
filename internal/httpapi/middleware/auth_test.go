package middleware_test

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/middleware"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

func init() { gin.SetMode(gin.TestMode) }

// ---- helpers ----

func hashKey(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func makeKeyMap(keys ...domain.APIKey) middleware.KeyMap {
	return middleware.NewKeyMap(keys)
}

func newAuthRouter(km middleware.KeyMap, extraMiddleware ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Auth(km))
	for _, m := range extraMiddleware {
		r.Use(m)
	}
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func newAdminRouter(km middleware.KeyMap) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Auth(km))
	admin := r.Group("/")
	admin.Use(middleware.RequireAdmin())
	admin.POST("/admin", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	r.GET("/open", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func doRequest(r *gin.Engine, method, path, apiKey string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest(method, path, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- Auth middleware tests ----

func TestAuth_MissingKey_Returns401(t *testing.T) {
	r := newAuthRouter(makeKeyMap())
	w := doRequest(r, http.MethodGet, "/test", "")

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))

	var p problem.Problem
	require.NoError(t, decodeJSON(w, &p))
	assert.Equal(t, http.StatusUnauthorized, p.Status)
	assert.NotEmpty(t, p.Detail)
}

func TestAuth_InvalidKey_Returns401(t *testing.T) {
	r := newAuthRouter(makeKeyMap()) // empty map — any key is invalid
	w := doRequest(r, http.MethodGet, "/test", "lo_live_notavalidkey")

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
}

func TestAuth_ValidKey_Passes(t *testing.T) {
	const rawKey = "lo_live_testkey1234567890abcdef12345678"
	key := domain.APIKey{
		ID:      "key-id-1",
		Name:    "test",
		KeyHash: hashKey(rawKey),
		Active:  true,
	}
	r := newAuthRouter(makeKeyMap(key))
	w := doRequest(r, http.MethodGet, "/test", rawKey)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuth_SetsActorIDInContext(t *testing.T) {
	const rawKey = "lo_live_testkey1234567890abcdef12345678"
	key := domain.APIKey{
		ID:      "actor-uuid-999",
		Name:    "test",
		KeyHash: hashKey(rawKey),
		Active:  true,
	}

	var capturedActorID string
	r := gin.New()
	r.Use(middleware.Auth(makeKeyMap(key)))
	r.GET("/test", func(c *gin.Context) {
		v, _ := c.Get(middleware.ContextKeyActorID)
		capturedActorID, _ = v.(string)
		c.Status(http.StatusOK)
	})

	w := doRequest(r, http.MethodGet, "/test", rawKey)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "actor-uuid-999", capturedActorID)
}

func TestAuth_SetsAPIKeyInContext(t *testing.T) {
	const rawKey = "lo_live_testkey1234567890abcdef12345678"
	key := domain.APIKey{
		ID:             "key-id-1",
		Name:           "test",
		KeyHash:        hashKey(rawKey),
		Active:         true,
		RateLimitRPS:   50,
		RateLimitBurst: 100,
	}

	var capturedKey domain.APIKey
	r := gin.New()
	r.Use(middleware.Auth(makeKeyMap(key)))
	r.GET("/test", func(c *gin.Context) {
		v, _ := c.Get(middleware.ContextKeyAPIKey)
		capturedKey, _ = v.(domain.APIKey)
		c.Status(http.StatusOK)
	})

	w := doRequest(r, http.MethodGet, "/test", rawKey)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, float64(50), capturedKey.RateLimitRPS)
	assert.Equal(t, 100, capturedKey.RateLimitBurst)
}

// ---- RequireAdmin tests ----

func TestRequireAdmin_AdminKey_Passes(t *testing.T) {
	const rawKey = "lo_live_adminkey1234567890abcdef1234"
	key := domain.APIKey{
		ID:      "admin-id",
		Name:    "admin",
		KeyHash: hashKey(rawKey),
		Active:  true,
		IsAdmin: true,
	}
	r := newAdminRouter(makeKeyMap(key))
	w := doRequest(r, http.MethodPost, "/admin", rawKey)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireAdmin_NonAdminKey_Returns403(t *testing.T) {
	const rawKey = "lo_live_demokey1234567890abcdef123456"
	key := domain.APIKey{
		ID:      "demo-id",
		Name:    "demo",
		KeyHash: hashKey(rawKey),
		Active:  true,
		IsAdmin: false,
	}
	r := newAdminRouter(makeKeyMap(key))
	w := doRequest(r, http.MethodPost, "/admin", rawKey)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))

	var p problem.Problem
	require.NoError(t, decodeJSON(w, &p))
	assert.Equal(t, http.StatusForbidden, p.Status)
}

func TestRequireAdmin_NonAdminKey_CanAccessOpenRoute(t *testing.T) {
	const rawKey = "lo_live_demokey1234567890abcdef123456"
	key := domain.APIKey{
		ID:      "demo-id",
		Name:    "demo",
		KeyHash: hashKey(rawKey),
		Active:  true,
		IsAdmin: false,
	}
	r := newAdminRouter(makeKeyMap(key))
	// /open does not have RequireAdmin middleware
	w := doRequest(r, http.MethodGet, "/open", rawKey)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ---- KeyMap tests ----

func TestKeyMap_EmptyKey_NotFound(t *testing.T) {
	km := makeKeyMap()
	_, ok := km.Lookup("")
	assert.False(t, ok)
}

func TestKeyMap_WrongKey_NotFound(t *testing.T) {
	const rawKey = "lo_live_realkey1234567890abcdef123456"
	key := domain.APIKey{ID: "1", KeyHash: hashKey(rawKey)}
	km := makeKeyMap(key)

	_, ok := km.Lookup("lo_live_wrongkey000000000000000000000")
	assert.False(t, ok)
}

func TestKeyMap_CorrectKey_Found(t *testing.T) {
	const rawKey = "lo_live_realkey1234567890abcdef123456"
	key := domain.APIKey{ID: "key-1", Name: "real", KeyHash: hashKey(rawKey), Active: true}
	km := makeKeyMap(key)

	found, ok := km.Lookup(rawKey)
	assert.True(t, ok)
	assert.Equal(t, "key-1", found.ID)
}

func TestKeyMap_SkipsKeysWithWrongHashLength(t *testing.T) {
	bad := domain.APIKey{ID: "bad", KeyHash: []byte("tooshort")}
	km := makeKeyMap(bad)
	// bad key should not be in the map
	assert.Equal(t, 0, km.Len())
}
