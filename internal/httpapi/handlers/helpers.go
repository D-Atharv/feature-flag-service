package handlers

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

const handlerTimeout = 10 * time.Second

// withTimeout derives a context with the standard handler deadline.
func withTimeout(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), handlerTimeout)
}

// actorID returns the API key ID stored in the Gin context by auth middleware.
// Returns "" when the auth middleware hasn't run (e.g. tests without the full chain).
func actorID(c *gin.Context) string {
	v, _ := c.Get("actor_key_id")
	s, _ := v.(string)
	return s
}
