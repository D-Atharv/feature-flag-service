package middleware

import (
	"crypto/sha256"
	"crypto/subtle"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

// KeyMap is an immutable in-memory lookup table built once at startup.
// Map key is the raw SHA-256 hash of the API key — never the plaintext.
// Lookups are O(1) and never touch the database.
type KeyMap struct {
	keys map[[32]byte]domain.APIKey
}

// NewKeyMap builds a KeyMap from a slice of active API keys.
// Call once at startup after loading all keys from the database.
func NewKeyMap(keys []domain.APIKey) KeyMap {
	m := make(map[[32]byte]domain.APIKey, len(keys))
	for _, k := range keys {
		if len(k.KeyHash) == 32 {
			var arr [32]byte
			copy(arr[:], k.KeyHash)
			m[arr] = k
		}
	}
	return KeyMap{keys: m}
}

// Lookup hashes rawKey with SHA-256 and checks the map.
// Uses subtle.ConstantTimeCompare on the stored hash to prevent timing attacks
// from confirming key existence via response latency.
func (km KeyMap) Lookup(rawKey string) (domain.APIKey, bool) {
	computed := sha256.Sum256([]byte(rawKey))
	stored, ok := km.keys[computed]
	if !ok {
		return domain.APIKey{}, false
	}
	// Constant-time comparison prevents timing side-channels.
	if subtle.ConstantTimeCompare(stored.KeyHash, computed[:]) != 1 {
		return domain.APIKey{}, false
	}
	return stored, true
}

// Auth authenticates every request via the X-API-Key header.
// On success it stores the key ID and full APIKey in the Gin context so
// downstream middleware (rate limiter, Phase 5) and handlers can read them.
//
// Position 6 (after BodyLimit/Timeout, before handler) — must run after all
// cheap guards so the expensive hash is never reached for invalid requests,
// and must run before the rate limiter so the key identity is known.
func Auth(km KeyMap) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("X-API-Key")
		if raw == "" {
			problem.Render(c, problem.NewUnauthorized("X-API-Key header is required"))
			return
		}

		key, ok := km.Lookup(raw)
		if !ok {
			problem.Render(c, problem.NewUnauthorized("invalid or inactive API key"))
			return
		}

		c.Set(ContextKeyActorID, key.ID)
		c.Set(ContextKeyAPIKey, key)
		c.Next()
	}
}

// RequireAdmin aborts with 403 if the authenticated key is not an admin key.
// Apply to CRUD write routes (POST/PATCH/DELETE) and GET /flags routes.
//
// Must run after Auth — it reads the api_key from context.
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		v, exists := c.Get(ContextKeyAPIKey)
		if !exists {
			// Auth middleware did not run — this is a wiring bug.
			problem.Render(c, problem.NewInternal())
			return
		}
		key, ok := v.(domain.APIKey)
		if !ok || !key.IsAdmin {
			problem.Render(c, problem.NewForbidden("admin scope required for this operation"))
			return
		}
		c.Next()
	}
}

// Len returns the number of keys in the map. Used in tests.
func (km KeyMap) Len() int { return len(km.keys) }
