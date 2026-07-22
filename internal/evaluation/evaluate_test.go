package evaluation

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- staticSource — test helper for Decide unit tests ----

type staticSource struct {
	flag domain.Flag
	err  error
}

func (s staticSource) GetByKeyEnv(_ context.Context, _, _ string) (domain.Flag, error) {
	return s.flag, s.err
}

// ---- 3.3a: Determinism (true golden file) ----
//
// Expected bucket values were computed once with the xxhash implementation and
// are hardcoded here as literals. A future change to the hash algorithm,
// separator, or field ordering will change these values and fail the test.
// Do NOT regenerate — a failing test here means a breaking change.

func TestBucketOf_Determinism(t *testing.T) {
	cases := []struct {
		flagKey string
		env     string
		subject string
		want    uint32
	}{
		// Golden values — computed once, never regenerated.
		{"dark-mode", "prod", "user-1", 56},
		{"checkout-v2", "staging", "user-42", 49},
		{"new-nav", "dev", "org-99", 7},
		{"dark-mode", "dev", "user-1", 46},  // same key+subject, different env → different bucket
		{"dark-mode", "prod", "user-2", 38}, // same key+env, different subject → different bucket
	}

	for _, tc := range cases {
		got := bucketOf(tc.flagKey, tc.env, tc.subject)
		assert.Equal(t, tc.want, got,
			"bucket changed for (%s, %s, %s) — hash algorithm or separator was modified",
			tc.flagKey, tc.env, tc.subject,
		)
	}
}

// TestBucketOf_EnvChangesTheBucket confirms that env is actually part of the
// hash (not just appended as a no-op). Uses the golden values above:
// "dark-mode"+"prod"+"user-1" → 56, "dark-mode"+"dev"+"user-1" → 46.
func TestBucketOf_EnvChangesTheBucket(t *testing.T) {
	assert.NotEqual(t,
		bucketOf("dark-mode", "prod", "user-1"),
		bucketOf("dark-mode", "dev", "user-1"),
		"changing env must change the bucket",
	)
}

// ---- 3.3b: Monotonicity ----
//
// If subject S is enabled at rollout R, it must still be enabled at R' > R.
// Raising rollout only adds subjects, never removes them.
// This is the property most implementations quietly break (by including the
// rollout value in the hash key — which re-randomises the cohort on every change).

func TestBucketOf_Monotonicity(t *testing.T) {
	const subjects = 10_000
	const lowRollout = uint32(10)
	const highRollout = uint32(20)

	for i := range subjects {
		subj := fmt.Sprintf("user-%d", i)
		bucket := bucketOf("flag", "prod", subj)
		if bucket < lowRollout {
			// Enabled at low — must also be enabled at high.
			assert.Less(t, bucket, highRollout,
				"subject %s (bucket=%d) enabled at rollout %d but excluded at rollout %d — monotonicity violated",
				subj, bucket, lowRollout, highRollout,
			)
		}
	}
}

// ---- 3.3c: Uniformity ----
//
// At rollout=50, approximately 50% of subjects should land in bucket < 50.
// Tolerance: ±4% absolute (48 000–52 000 out of 100 000).
// Tight enough to catch a broken hash; loose enough to never flake.

func TestBucketOf_Uniformity(t *testing.T) {
	const subjects = 100_000
	const rollout = uint32(50)
	const lower = 48_000
	const upper = 52_000

	enabled := 0
	for i := range subjects {
		if bucketOf("flag", "prod", fmt.Sprintf("user-%d", i)) < rollout {
			enabled++
		}
	}

	assert.GreaterOrEqual(t, enabled, lower,
		"uniformity too low: %d/%d enabled at rollout=50 (expected ≥ %d)", enabled, subjects, lower)
	assert.LessOrEqual(t, enabled, upper,
		"uniformity too high: %d/%d enabled at rollout=50 (expected ≤ %d)", enabled, subjects, upper)
}

// ---- 3.3d: Decide unit tests ----

func TestDecide_FlagNotFound(t *testing.T) {
	// Wrapped sentinel — errors.Is must unwrap it.
	src := staticSource{err: fmt.Errorf("wrap: %w", domain.ErrNotFound)}
	result, err := Decide(context.Background(), src, "f", "prod", "user-1")
	require.NoError(t, err, "FLAG_NOT_FOUND must not be returned as an error (callers return 200)")
	assert.False(t, result.Enabled)
	assert.Equal(t, ReasonFlagNotFound, result.Reason)
}

func TestDecide_FlagDisabled(t *testing.T) {
	src := staticSource{flag: domain.Flag{Enabled: false, RolloutPercentage: 100}}
	result, err := Decide(context.Background(), src, "f", "prod", "user-1")
	require.NoError(t, err)
	assert.False(t, result.Enabled)
	assert.Equal(t, ReasonFlagDisabled, result.Reason)
}

func TestDecide_DisabledTakesPrecedenceOverRollout(t *testing.T) {
	// enabled=false must short-circuit before the rollout check.
	src := staticSource{flag: domain.Flag{Enabled: false, RolloutPercentage: 50}}
	result, err := Decide(context.Background(), src, "f", "prod", "user-1")
	require.NoError(t, err)
	assert.False(t, result.Enabled)
	assert.Equal(t, ReasonFlagDisabled, result.Reason)
}

func TestDecide_RolloutFull(t *testing.T) {
	src := staticSource{flag: domain.Flag{Enabled: true, RolloutPercentage: 100}}
	for _, subj := range []string{"user-1", "user-99999", "org-abc"} {
		result, err := Decide(context.Background(), src, "f", "prod", subj)
		require.NoError(t, err)
		assert.True(t, result.Enabled, "subject %s should be enabled at rollout=100", subj)
		assert.Equal(t, ReasonRolloutFull, result.Reason)
	}
}

func TestDecide_RolloutZero(t *testing.T) {
	src := staticSource{flag: domain.Flag{Enabled: true, RolloutPercentage: 0}}
	for _, subj := range []string{"user-1", "user-99999", "org-abc"} {
		result, err := Decide(context.Background(), src, "f", "prod", subj)
		require.NoError(t, err)
		assert.False(t, result.Enabled, "subject %s should be disabled at rollout=0", subj)
		assert.Equal(t, ReasonRolloutZero, result.Reason)
	}
}

func TestDecide_RolloutIncludedExcluded_ConsistentWithBucketOf(t *testing.T) {
	const flagKey = "checkout-v2"
	const env = "prod"
	const rollout = 40

	src := staticSource{flag: domain.Flag{Enabled: true, RolloutPercentage: rollout}}

	for i := range 200 {
		subj := fmt.Sprintf("user-%d", i)
		bucket := bucketOf(flagKey, env, subj)
		result, err := Decide(context.Background(), src, flagKey, env, subj)
		require.NoError(t, err)

		if bucket < rollout {
			assert.True(t, result.Enabled, "subject %s (bucket=%d) should be included at rollout=%d", subj, bucket, rollout)
			assert.Equal(t, ReasonRolloutIncluded, result.Reason)
		} else {
			assert.False(t, result.Enabled, "subject %s (bucket=%d) should be excluded at rollout=%d", subj, bucket, rollout)
			assert.Equal(t, ReasonRolloutExcluded, result.Reason)
		}
	}
}

func TestDecide_StoreError_Propagated(t *testing.T) {
	storeErr := errors.New("connection refused")
	src := staticSource{err: storeErr}
	_, err := Decide(context.Background(), src, "f", "prod", "user-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, storeErr, "non-NotFound store errors must propagate so the handler renders 500")
}
