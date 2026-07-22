#!/usr/bin/env bash
# Guard against off-the-shelf rate limiters. The assignment requires a
# hand-rolled token bucket. golang.org/x/time/rate is the likeliest
# accidental violation — it IS a token bucket, it ships with Go's extended
# stdlib, and an AI assistant will suggest it unprompted. See BUILD-PLAN.md
# §3.5.
set -euo pipefail

# Manifest pattern: bare module names are fine here, because go.mod/go.sum
# contain nothing but module paths.
FORBIDDEN_MOD='golang.org/x/time/rate|go-redis/redis_rate|ulule/limiter|didip/tollbooth|throttled|uber-go/ratelimit'

# Source pattern: import-path forms only. The manifest pattern cannot be
# reused here — `throttled` is also an ordinary English word, so a doc comment
# reading "requests are throttled" would fail the build. Verified: it did.
# The manifest grep is what actually proves compliance; an import that isn't
# in go.mod cannot compile, so narrowing the source grep costs no coverage.
FORBIDDEN_SRC='golang\.org/x/time/rate|github\.com/go-redis/redis_rate|github\.com/ulule/limiter|github\.com/didip/tollbooth|github\.com/throttled/throttled|go\.uber\.org/ratelimit'

found=0

# Manifests and source tree are searched separately on purpose: --include
# filters out go.mod/go.sum even when they are named explicitly, so folding
# both into one grep silently skips the manifests. That would miss the most
# likely violation of all (`go get golang.org/x/time/rate`) and every
# transitive dependency, which only ever appear in go.sum.
if grep -En "$FORBIDDEN_MOD" go.mod go.sum 2>/dev/null; then
  found=1
fi

if grep -REn --include='*.go' "$FORBIDDEN_SRC" . 2>/dev/null; then
  found=1
fi

if [ "$found" -eq 1 ]; then
  echo "" >&2
  echo "ERROR: off-the-shelf rate limiter dependency detected (see above)." >&2
  echo "The assignment requires a hand-rolled token bucket." >&2
  exit 1
fi

echo "ok: no off-the-shelf rate limiter dependency found"
