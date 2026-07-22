package middleware

import (
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	metricsOnce sync.Once

	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests by method, route template, and status class.",
		},
		[]string{"method", "path", "status_class"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by method and route template.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
)

// Metrics records RED (Rate, Errors, Duration) metrics for every request.
//
// Position 4 (before Auth) ensures rejected requests appear in dashboards.
// Path uses c.FullPath() (route template) not the raw URL — using the raw URL
// creates one time-series entry per unique flag key, blowing up cardinality.
func Metrics() gin.HandlerFunc {
	metricsOnce.Do(func() {
		// Register returns an error for already-registered metrics (e.g. when
		// tests call Metrics() multiple times). Silently ignore it — the first
		// registration wins and the metric still works correctly.
		_ = prometheus.Register(httpRequestsTotal)
		_ = prometheus.Register(httpRequestDuration)
	})

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}

		httpRequestsTotal.WithLabelValues(
			c.Request.Method,
			path,
			statusClass(c.Writer.Status()),
		).Inc()

		httpRequestDuration.WithLabelValues(
			c.Request.Method,
			path,
		).Observe(time.Since(start).Seconds())
	}
}

// statusClass buckets HTTP status codes to prevent label cardinality explosion.
func statusClass(code int) string {
	return fmt.Sprintf("%dxx", code/100)
}
