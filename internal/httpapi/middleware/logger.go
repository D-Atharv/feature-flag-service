package middleware

import (
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// evaluateSampleRate controls what fraction of successful /evaluate requests
// are logged. 1% prevents full-fidelity logging from becoming a self-inflicted
// outage at high rps. Errors (status >= 400) are never sampled.
const evaluateSampleRate = 0.01

// Logger emits a structured log/slog JSON line for every request.
//
// Emit is deferred so it runs after the handler returns — the log line
// therefore has the final status code and accurate latency.
//
// Position 3 (outside Auth) is intentional: 401s, 403s, and 429s must be
// logged. Moving Logger inside Auth makes the service blind during exactly
// the incidents that need investigation.
//
// X-API-Key is never included in any log field.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		latency := time.Since(start)
		path := c.FullPath() // route template e.g. /api/v1/flags/:key
		if path == "" {
			path = c.Request.URL.Path // fallback for unmatched routes
		}

		// Sampling: drop most successful /evaluate logs to protect throughput.
		if isEvaluatePath(path) && status < 400 {
			if rand.Float64() >= evaluateSampleRate {
				return
			}
		}

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		slog.Log(c.Request.Context(), level, "request",
			"request_id", requestIDFromCtx(c),
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"latency_ms", latency.Milliseconds(),
			"ip", c.ClientIP(),
			"user_agent", c.Request.UserAgent(),
			// X-API-Key is deliberately omitted — never log auth credentials.
		)
	}
}

func isEvaluatePath(path string) bool {
	return strings.HasPrefix(path, "/evaluate") ||
		strings.HasPrefix(path, "/api/v1/evaluate")
}
