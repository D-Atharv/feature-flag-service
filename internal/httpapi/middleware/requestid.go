package middleware

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gin-gonic/gin"
)

// RequestIDWithSecurity accepts an inbound X-Request-ID header or generates a
// new 16-byte random hex ID. The value is stored in the Gin context and echoed
// back in the X-Request-ID response header. Minimal security headers are also
// set here so every response gets them in one place.
//
// Must be first in the chain — everything downstream (including the panic log
// in Recovery) reads the request ID from context.
func RequestIDWithSecurity() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-ID")
		if rid == "" {
			rid = newRequestID()
		}
		c.Set(ContextKeyRequestID, rid)
		c.Header("X-Request-ID", rid)
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Next()
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// requestIDFromCtx reads the request ID from the Gin context.
// Returns "" when RequestIDWithSecurity hasn't run (e.g. unit tests).
func requestIDFromCtx(c *gin.Context) string {
	v, _ := c.Get(ContextKeyRequestID)
	s, _ := v.(string)
	return s
}
