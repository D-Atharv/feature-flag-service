package snapshot

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Register, not MustRegister: tests build the refresher more than once per
// process and a duplicate registration would panic the suite.
var (
	metricsOnce sync.Once

	refreshTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "snapshot_refresh_total",
			Help: "Flag snapshot refreshes by trigger.",
		},
		[]string{"trigger"},
	)

	refreshErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "snapshot_refresh_errors_total",
		Help: "Failed flag snapshot refreshes.",
	})

	flagsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "snapshot_flags",
		Help: "Flags in the current snapshot.",
	})
)

func registerMetrics() {
	metricsOnce.Do(func() {
		_ = prometheus.Register(refreshTotal)
		_ = prometheus.Register(refreshErrors)
		_ = prometheus.Register(flagsGauge)
	})
}
