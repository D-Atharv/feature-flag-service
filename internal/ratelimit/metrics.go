package ratelimit

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Limiter metrics. Registration mirrors the pattern in middleware/metrics.go:
// prometheus.Register rather than MustRegister, because tests construct the
// chain more than once per process and a duplicate registration would panic
// the suite rather than fail one assertion.
var (
	metricsOnce sync.Once

	decisionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ratelimit_decisions_total",
			Help: "Rate limiter decisions by outcome.",
		},
		// Labelled by outcome only. Labelling by API key would be bounded here
		// (three seeded keys) but unbounded in any real deployment, and a
		// cardinality explosion in the metrics backend is a self-inflicted
		// outage.
		[]string{"outcome"},
	)

	degradedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ratelimit_degraded_total",
			Help: "Requests whose limit decision came from the in-memory fallback.",
		},
	)

	// A counter answers "how much did we serve degraded"; only a gauge answers
	// "are we degraded right now", which is the question an alert asks.
	degradedActive = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "ratelimit_degraded_active",
			Help: "1 while the limiter is running on the in-memory fallback, 0 otherwise.",
		},
	)

	storeErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ratelimit_store_errors_total",
			Help: "Errors returned by the authoritative limiter store.",
		},
	)
)

func registerMetrics() {
	metricsOnce.Do(func() {
		_ = prometheus.Register(decisionsTotal)
		_ = prometheus.Register(degradedTotal)
		_ = prometheus.Register(degradedActive)
		_ = prometheus.Register(storeErrorsTotal)
	})
}

// RecordDecision counts one limiter outcome. Called by the HTTP middleware,
// which is the layer that knows a decision became a response.
func RecordDecision(allowed bool) {
	registerMetrics()
	outcome := "limited"
	if allowed {
		outcome = "allowed"
	}
	decisionsTotal.WithLabelValues(outcome).Inc()
}
