#!/usr/bin/env bash
# 10-step acceptance suite, runnable against localhost or a live deployment.
#
# Usage:
#   ADMIN_KEY=... DEMO_KEY=... LOADTEST_KEY=... ./scripts/smoke.sh https://ffs-api.onrender.com
#
# Three keys, deliberately: the bulk rollout sweeps (steps 5-6) need the
# loadtest key's high limit, or they measure the rate limiter instead of
# rollout correctness — a request denied with 429 never says "enabled":true,
# which would corrupt the percentage count, not just slow the run down.
# Step 7 deliberately keeps DEMO_KEY, because hammering it past its burst is
# the point of that step.
#
# Restart-survival is deliberately NOT step 8 here — demo-restart.sh proves it
# properly, with a real quota burn and an actual process restart. Folding a
# weaker version into this suite would just create two ways to get it wrong.
set -euo pipefail

BASE_URL="${1:?usage: smoke.sh <base-url>}"
ADMIN_KEY="${ADMIN_KEY:?set ADMIN_KEY}"
DEMO_KEY="${DEMO_KEY:?set DEMO_KEY}"
LOADTEST_KEY="${LOADTEST_KEY:?set LOADTEST_KEY}"

PASS=0
step() { printf '\n=== %s ===\n' "$1"; }
ok()   { PASS=$((PASS + 1)); printf '  OK: %s\n' "$1"; }
fail() { printf '  FAIL: %s\n' "$1" >&2; exit 1; }

code_of() { curl -sS -o /dev/null -w '%{http_code}' "$@"; }

FLAG_KEY="smoke-test-$$-$RANDOM"

step "1. Public surface: /, /healthz, /readyz"
sha=$(curl -sS "$BASE_URL/" | grep -o '"git_sha":"[^"]*"' || true)
[ -n "$sha" ] || fail "GET / did not return git_sha"
ok "GET / -> $sha"
[ "$(code_of "$BASE_URL/healthz")" = "200" ] || fail "healthz not 200"
ok "GET /healthz -> 200"
[ "$(code_of "$BASE_URL/readyz")" = "200" ] || fail "readyz not 200 (snapshot not loaded?)"
ok "GET /readyz -> 200"

step "2. Auth scoping"
[ "$(code_of "$BASE_URL/api/v1/flags")" = "401" ] || fail "no key must be 401"
ok "no key -> 401"
[ "$(code_of "$BASE_URL/api/v1/flags" -H "X-API-Key: $DEMO_KEY")" = "403" ] || fail "evaluate-scoped key must be 403 on admin route"
ok "evaluate-scoped key on /flags -> 403"
[ "$(code_of "$BASE_URL/api/v1/flags" -H "X-API-Key: $ADMIN_KEY")" = "200" ] || fail "admin key must be 200"
ok "admin key -> 200"

step "3. Create, read, optimistic-concurrency conflict"
create_code=$(code_of -X POST "$BASE_URL/api/v1/flags" -H "X-API-Key: $ADMIN_KEY" \
  -d "{\"key\":\"$FLAG_KEY\",\"environment\":\"prod\",\"enabled\":true,\"rollout_percentage\":50}")
[ "$create_code" = "201" ] || fail "create returned $create_code, want 201"
ok "POST /flags -> 201"
got=$(curl -sS "$BASE_URL/api/v1/flags/$FLAG_KEY?environment=prod" -H "X-API-Key: $ADMIN_KEY")
echo "$got" | grep -q "\"rollout_percentage\":50" || fail "GET did not return the flag just created"
ok "GET /flags/:key -> matches"
stale_code=$(code_of -X PATCH "$BASE_URL/api/v1/flags/$FLAG_KEY?environment=prod" \
  -H "X-API-Key: $ADMIN_KEY" -H 'If-Match: "99"' -d '{"enabled":false}')
[ "$stale_code" = "412" ] || fail "stale If-Match returned $stale_code, want 412"
ok "PATCH with stale If-Match -> 412"

step "4. Stickiness: identical subject, identical result, twice"
# Strip evaluated_at before comparing — it's a real timestamp and will always
# differ between two calls, even when the decision itself is perfectly sticky.
strip_ts() { sed -E 's/"evaluated_at":"[^"]*"//'; }
r1=$(curl -sS "$BASE_URL/evaluate/$FLAG_KEY?env=prod&subject=sticky-user" -H "X-API-Key: $DEMO_KEY" | strip_ts)
r2=$(curl -sS "$BASE_URL/evaluate/$FLAG_KEY?env=prod&subject=sticky-user" -H "X-API-Key: $DEMO_KEY" | strip_ts)
[ "$r1" = "$r2" ] || fail "same subject gave two different answers"
ok "same subject, same answer, twice"

# probe_enabled <subject> <out-file>: hits /evaluate and, if enabled:true,
# touches out-file. Run under xargs -P so a live network round trip per
# subject doesn't make these steps take minutes; the loadtest key's burst
# is large enough that concurrency itself is never the bottleneck.
probe_enabled() {
  local subject="$1" outfile="$2"
  local r
  r=$(curl -sS "$BASE_URL/evaluate/$FLAG_KEY?env=prod&subject=$subject" -H "X-API-Key: $LOADTEST_KEY")
  case "$r" in *'"enabled":true'*) : > "$outfile" ;; esac
}
export -f probe_enabled
export BASE_URL FLAG_KEY LOADTEST_KEY

step "5. Rollout correctness: 1000 synthetic subjects at 50% (loadtest key, parallel)"
sweep_dir=$(mktemp -d)
seq 1 1000 | xargs -P 20 -I{} bash -c 'probe_enabled "sweep-user-{}" "$1/{}"' _ "$sweep_dir"
enabled=$(find "$sweep_dir" -type f | wc -l | tr -d ' ')
rm -rf "$sweep_dir"
if [ "$enabled" -lt 400 ] || [ "$enabled" -gt 600 ]; then
  fail "expected roughly 500/1000 enabled at rollout=50, got $enabled"
fi
ok "$enabled/1000 enabled at rollout=50 (expected ~500)"

step "6. Monotonicity: raise rollout 50 -> 60, no one already-enabled loses access (loadtest key)"
mono_dir=$(mktemp -d)
seq 1 200 | xargs -P 20 -I{} bash -c 'probe_enabled "mono-user-{}" "$1/{}"' _ "$mono_dir"
before_enabled=$(find "$mono_dir" -type f -exec basename {} \;)

curl -sS -X PATCH "$BASE_URL/api/v1/flags/$FLAG_KEY?environment=prod" \
  -H "X-API-Key: $ADMIN_KEY" -d '{"rollout_percentage":60}' >/dev/null

after_dir=$(mktemp -d)
if [ -n "$before_enabled" ]; then
  printf '%s\n' "$before_enabled" | xargs -P 20 -I{} bash -c 'probe_enabled "mono-user-{}" "$1/{}"' _ "$after_dir"
fi
before_count=$(printf '%s\n' "$before_enabled" | grep -c . || true)
after_count=$(find "$after_dir" -type f | wc -l | tr -d ' ')
rm -rf "$mono_dir" "$after_dir"

lost=$((before_count - after_count))
[ "$lost" -le 0 ] || fail "$lost/$before_count subjects lost access after raising rollout — monotonicity violated"
ok "raising rollout 50->60 removed nobody ($before_count/$before_count still enabled)"

step "7. Rate limit: hammer the demo key past its burst"
got_429=0
for i in $(seq 1 15); do
  code=$(code_of "$BASE_URL/evaluate/$FLAG_KEY?env=prod&subject=limit-probe-$i" -H "X-API-Key: $DEMO_KEY")
  [ "$code" = "429" ] && { got_429=1; break; }
done
[ "$got_429" = "1" ] || fail "never got a 429 — is the demo key's limit misconfigured?"
headers=$(curl -sS -D - -o /dev/null "$BASE_URL/evaluate/$FLAG_KEY?env=prod&subject=limit-probe-final" -H "X-API-Key: $DEMO_KEY")
echo "$headers" | grep -qi '^retry-after:' || fail "429 missing Retry-After"
echo "$headers" | grep -qi '^ratelimit-limit:' || fail "missing RateLimit-Limit"
ok "429 with Retry-After and RateLimit-* headers present"

step "8. Metrics"
metrics=$(curl -sS "$BASE_URL/metrics")
echo "$metrics" | grep -q '^flag_evaluations_total' || fail "flag_evaluations_total missing from /metrics"
echo "$metrics" | grep -q '^ratelimit_decisions_total' || fail "ratelimit_decisions_total missing from /metrics"
ok "/metrics exposes flag and limiter counters"

step "9. Docs console"
[ "$(code_of "$BASE_URL/docs")" = "200" ] || fail "/docs not reachable"
[ "$(code_of "$BASE_URL/docs/openapi.json")" = "200" ] || fail "/docs/openapi.json not reachable"
ok "/docs and /docs/openapi.json both 200, unauthenticated"

# Cleanup: remove the flag this run created.
curl -sS -X DELETE "$BASE_URL/api/v1/flags/$FLAG_KEY?environment=prod" -H "X-API-Key: $ADMIN_KEY" >/dev/null

step "10. Delete confirmed gone"
# Uses LOADTEST_KEY, not DEMO_KEY: step 7 deliberately exhausted DEMO_KEY's
# burst, and a 429 body here would fail this check for the wrong reason.
#
# Retries rather than checking once: the write lands in Postgres immediately,
# but /evaluate reads the in-memory snapshot, which only catches up once
# LISTEN/NOTIFY propagates (~100ms typically, no hard guarantee) or the 30s
# poller runs. A single immediate check races the snapshot by construction.
gone=0
for _ in $(seq 1 20); do
  after_delete=$(curl -sS "$BASE_URL/evaluate/$FLAG_KEY?env=prod&subject=u1" -H "X-API-Key: $LOADTEST_KEY")
  case "$after_delete" in *'"reason":"FLAG_NOT_FOUND"'*) gone=1; break ;; esac
  sleep 0.5
done
[ "$gone" = "1" ] || fail "flag still evaluates after delete (waited 10s)"
ok "deleted flag evaluates as FLAG_NOT_FOUND"

echo
echo "PASS: all 10 steps green against $BASE_URL"
