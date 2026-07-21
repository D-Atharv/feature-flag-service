#!/usr/bin/env bash
# Guard against off-the-shelf rate limiters. The assignment requires a
# hand-rolled token bucket. golang.org/x/time/rate is the likeliest
# accidental violation — it IS a token bucket, it ships with Go's extended
# stdlib, and an AI assistant will suggest it unprompted. See BUILD-PLAN.md
# §3.5.
set -euo pipefail

FORBIDDEN='golang.org/x/time/rate|go-redis/redis_rate|ulule/limiter|didip/tollbooth|throttled|uber-go/ratelimit'

found=0

# Manifests and source tree are searched separately on purpose: --include
# filters out go.mod/go.sum even when they are named explicitly, so folding
# both into one grep silently skips the manifests. That would miss the most
# likely violation of all (`go get golang.org/x/time/rate`) and every
# transitive dependency, which only ever appear in go.sum.
if grep -En "$FORBIDDEN" go.mod go.sum 2>/dev/null; then
  found=1
fi

if grep -REn --include='*.go' "$FORBIDDEN" . 2>/dev/null; then
  found=1
fi

if [ "$found" -eq 1 ]; then
  echo "" >&2
  echo "ERROR: off-the-shelf rate limiter dependency detected (see above)." >&2
  echo "The assignment requires a hand-rolled token bucket." >&2
  exit 1
fi

echo "ok: no off-the-shelf rate limiter dependency found"
