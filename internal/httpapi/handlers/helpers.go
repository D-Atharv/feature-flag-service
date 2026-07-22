package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

const handlerTimeout = 10 * time.Second

// withTimeout derives a context with the standard handler deadline.
func withTimeout(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), handlerTimeout)
}

// actorID returns the API key ID stored in the Gin context by auth middleware.
// Returns "" when the auth middleware hasn't run (e.g. tests without the full chain).
// The string literal matches middleware.ContextKeyActorID — kept as a literal
// here to avoid a circular import (handlers → middleware → problem → handlers).
func actorID(c *gin.Context) string {
	v, _ := c.Get("actor_key_id")
	s, _ := v.(string)
	return s
}

// HandleBindError inspects err from ShouldBindJSON / ShouldBindQuery and
// renders the appropriate problem response:
//   - *http.MaxBytesError → 413 Request Entity Too Large
//   - validator.ValidationErrors → 400 with per-field detail
//   - anything else → 400 Invalid Input
//
// Returns true if an error was handled (caller should return).
func HandleBindError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		problem.Render(c, problem.NewRequestEntityTooLarge("request body exceeds 1 MiB limit"))
		return true
	}
	problem.WriteValidation(c, err)
	return true
}
