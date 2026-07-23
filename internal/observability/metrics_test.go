package observability_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/observability"
)

// series returns the label sets currently recorded for flag_evaluations_total.
func series(t *testing.T) []string {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var out []string
	for _, f := range families {
		if f.GetName() != "flag_evaluations_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var pairs []string
			for _, l := range m.GetLabel() {
				pairs = append(pairs, l.GetName()+"="+l.GetValue())
			}
			out = append(out, strings.Join(pairs, ","))
		}
	}
	return out
}

// TestUnknownFlagsCollapseToOneSeries is the cardinality guard.
//
// Both the flag key and the environment arrive in the URL, so they are
// caller-controlled. Recording them verbatim would let anyone holding the
// published demo key mint an unbounded number of Prometheus time series — a
// memory-exhaustion attack on your own monitoring, carried out by typing.
func TestUnknownFlagsCollapseToOneSeries(t *testing.T) {
	before := len(series(t))

	for i := range 100 {
		observability.RecordEvaluation(
			fmt.Sprintf("attacker-invented-flag-%d", i),
			fmt.Sprintf("attacker-invented-env-%d", i),
			"FLAG_NOT_FOUND",
			false, // not found: labels must not be trusted
		)
	}

	got := series(t)
	assert.LessOrEqual(t, len(got)-before, 1,
		"100 unknown flags must add at most one series, got %d", len(got)-before)

	for _, s := range got {
		assert.NotContains(t, s, "attacker-invented",
			"a caller-supplied label value reached the metric: %s", s)
	}
}

// TestKnownFlagsKeepTheirLabels: the guard must not blind the metric for real
// flags, which are bounded by the flags table.
func TestKnownFlagsKeepTheirLabels(t *testing.T) {
	observability.RecordEvaluation("real-flag", "prod", "ROLLOUT_FULL", true)

	joined := strings.Join(series(t), " | ")
	assert.Contains(t, joined, "flag=real-flag")
	assert.Contains(t, joined, "env=prod")
	assert.Contains(t, joined, "result=ROLLOUT_FULL")
}
