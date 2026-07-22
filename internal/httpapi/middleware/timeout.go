package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

// RequestTimeout is the global deadline for the entire request lifecycle.
// Distinct from the per-operation timeout in handlers/helpers.go (10s DB ops).
const RequestTimeout = 30 * time.Second

// Timeout applies a global request deadline.
// If the handler exceeds the deadline, the client gets a 503 Service Unavailable.
func Timeout() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), RequestTimeout)
		defer cancel()

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		if ctx.Err() == context.DeadlineExceeded && !c.Writer.Written() {
			problem.Render(c, problem.NewServiceUnavailable("request timeout — try again"))
		}
	}
}
