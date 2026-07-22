package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxBodyBytes is the maximum accepted request body size (1 MiB).
const MaxBodyBytes = int64(1 << 20)

// BodyLimit wraps the request body with http.MaxBytesReader.
// The error only surfaces when a handler reads the body (via ShouldBindJSON).
// Handlers must check for *http.MaxBytesError and return 413.
// The HandleBindError helper in handlers/helpers.go does this automatically.
func BodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
		}
		c.Next()
	}
}
