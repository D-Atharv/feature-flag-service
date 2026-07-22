// Package evaluation implements consistent flag bucketing and the evaluation
// decision tree. Zero I/O — all network calls go through the FlagSource interface.
package evaluation

import "github.com/cespare/xxhash/v2"

// bucketOf returns a stable bucket in [0, 100) for the (flagKey, env, subject) triple.
// flagKey and env are included to prevent correlated exposure across flags.
// xxhash is stable across Go versions and deployments — the bucket must never change.
func bucketOf(flagKey, env, subject string) uint32 {
	h := xxhash.New()
	_, _ = h.WriteString(flagKey)
	_, _ = h.WriteString(":")
	_, _ = h.WriteString(env)
	_, _ = h.WriteString(":")
	_, _ = h.WriteString(subject)
	return uint32(h.Sum64() % 100)
}
