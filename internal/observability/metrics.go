// Package observability holds the service's custom Prometheus metrics.
package observability

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// unknownLabel replaces caller-supplied values that do not name a real flag.
const unknownLabel = "unknown"

var (
	metricsOnce sync.Once

	// Labelled by flag and environment, both of which arrive in the URL. That
	// makes them caller-controlled, so they are only ever recorded when the
	// lookup actually matched a flag — otherwise a client could mint a new
	// time series per request and exhaust the metrics backend by typing.
	evaluationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "flag_evaluations_total",
			Help: "Flag evaluations by flag, environment, and outcome.",
		},
		[]string{"flag", "env", "result"},
	)
)

func registerMetrics() {
	metricsOnce.Do(func() { _ = prometheus.Register(evaluationsTotal) })
}

// RecordEvaluation counts one evaluation.
//
// found reports whether the flag resolved; when it did not, flag and env are
// discarded, because they came from the request and nothing bounds them.
func RecordEvaluation(flag, env, result string, found bool) {
	registerMetrics()

	if !found {
		flag, env = unknownLabel, unknownLabel
	}
	evaluationsTotal.WithLabelValues(flag, env, result).Inc()
}
