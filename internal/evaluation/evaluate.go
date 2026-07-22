package evaluation

import (
	"context"
	"errors"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
)

// FlagSource is the read interface Decide needs.
// *store.FlagRepo satisfies it directly; the in-memory snapshot (Phase 6)
// will satisfy it too — the handler never changes.
type FlagSource interface {
	GetByKeyEnv(ctx context.Context, key, env string) (domain.Flag, error)
}

// Reason is a machine-readable string explaining the evaluation outcome.
// Clients should switch on this field, not on enabled alone.
type Reason string

const (
	ReasonFlagNotFound    Reason = "FLAG_NOT_FOUND"
	ReasonFlagDisabled    Reason = "FLAG_DISABLED"
	ReasonRolloutFull     Reason = "ROLLOUT_FULL"
	ReasonRolloutZero     Reason = "ROLLOUT_ZERO"
	ReasonRolloutIncluded Reason = "ROLLOUT_INCLUDED"
	ReasonRolloutExcluded Reason = "ROLLOUT_EXCLUDED"
)

// Result is the output of Decide.
type Result struct {
	Enabled bool
	Reason  Reason
}

// Decide evaluates a feature flag for a given subject.
//
// Decision tree (evaluated in order):
//  1. Flag not found   → {false, FLAG_NOT_FOUND},   nil  (not an error — callers return 200)
//  2. Flag disabled    → {false, FLAG_DISABLED},    nil
//  3. Rollout == 100   → {true,  ROLLOUT_FULL},     nil
//  4. Rollout == 0     → {false, ROLLOUT_ZERO},     nil
//  5. bucket < rollout → {true,  ROLLOUT_INCLUDED}, nil
//  6. default          → {false, ROLLOUT_EXCLUDED}, nil
//
// Any non-ErrNotFound error from src propagates so the handler can render a 500.
func Decide(ctx context.Context, src FlagSource, flagKey, env, subject string) (Result, error) {
	f, err := src.GetByKeyEnv(ctx, flagKey, env)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Result{Enabled: false, Reason: ReasonFlagNotFound}, nil
		}
		return Result{}, err
	}

	if !f.Enabled {
		return Result{Enabled: false, Reason: ReasonFlagDisabled}, nil
	}

	switch f.RolloutPercentage {
	case 100:
		return Result{Enabled: true, Reason: ReasonRolloutFull}, nil
	case 0:
		return Result{Enabled: false, Reason: ReasonRolloutZero}, nil
	}

	// Widen the bucket rather than narrowing the percentage: bucketOf returns
	// a value in [0,100) so int always holds it, whereas int -> uint32 is a
	// conversion gosec cannot prove safe (G115) even though the column is
	// CHECK-constrained to 0..100.
	if int(bucketOf(flagKey, env, subject)) < f.RolloutPercentage {
		return Result{Enabled: true, Reason: ReasonRolloutIncluded}, nil
	}
	return Result{Enabled: false, Reason: ReasonRolloutExcluded}, nil
}
