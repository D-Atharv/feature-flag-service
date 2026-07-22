package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/D-Atharv/feature-flag-service/internal/httpapi/problem"
)

// Recovery catches panics in downstream handlers, logs them with the request ID
// and stack trace, then renders a 500 problem+json response.
//
// Must be position 2 (after RequestID) so:
//   - The panic log includes the request ID.
//   - A panic inside Logger is still caught (Logger is position 3).
//
// http.ErrAbortHandler is re-panicked: Gin uses it internally to abort a
// handler chain cleanly; swallowing it would break c.Abort() semantics.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				// Re-panic Gin's internal abort signal — never swallow it.
				if r == http.ErrAbortHandler {
					panic(r)
				}

				rid := requestIDFromCtx(c)
				slog.Error("panic recovered",
					"request_id", rid,
					"error", r,
					"stack", string(debug.Stack()),
				)

				// Only write a response if nothing has been written yet.
				if !c.Writer.Written() {
					problem.Render(c, problem.NewInternal())
				}
			}
		}()
		c.Next()
	}
}
