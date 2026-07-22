#!/usr/bin/env bash
# Prove that rate limiter state survives a service restart.
#
#   burn most of a key's quota -> restart the API process -> the quota is
#   still spent, because it never lived in the process to begin with.
#
# Parameterised by env var so P9 can run the identical proof against the
# deployed service:
#   BASE_URL=https://... RESTART_CMD="aws ecs update-service ..." ./scripts/demo-restart.sh
set -euo pipefail

# docker compose reads PORT from .env, so this script has to as well — otherwise
# it probes 8080 while the service listens somewhere else and every request
# fails in a way that looks like a limiter bug.
env_port=""
if [ -f .env ]; then
  env_port=$(grep -E '^PORT=' .env | tail -1 | cut -d= -f2 | tr -d " \"'\r")
fi

BASE_URL="${BASE_URL:-http://localhost:${env_port:-8080}}"
RESTART_CMD="${RESTART_CMD:-docker compose restart api}"
PROBE_PATH="/evaluate/demo-restart-probe?env=prod"

BURN=8          # the seeded demo key has a burst of 10
MAX_PROBES=12   # bound on the post-restart probing loop

LAST_CODE=""
LAST_REMAINING=""

fail() { printf '\nFAIL: %s\n' "$1" >&2; exit 1; }

# hit performs one request and records its status and remaining quota.
hit() {
  local label="$1" out
  out=$(curl -sS -o /dev/null -D - -w '\n%{http_code}' \
        "${BASE_URL}${PROBE_PATH}" -H "X-API-Key: ${DEMO_KEY}")
  LAST_CODE=$(printf '%s' "$out" | tail -1)
  LAST_REMAINING=$(printf '%s' "$out" | grep -i '^ratelimit-remaining:' | tr -d '\r' | awk '{print $2}')
  printf '  %-22s status=%s  RateLimit-Remaining=%s\n' \
         "$label" "$LAST_CODE" "${LAST_REMAINING:-n/a}"
}

# api_pid reports the host PID of the API container's process, so the transcript
# can show that the process genuinely changed rather than asserting it. Silent
# when the restart mechanism is not docker (P9 runs this against ECS/Render).
api_pid() {
  docker inspect -f '{{.State.Pid}}' "$(docker compose ps -q api 2>/dev/null)" 2>/dev/null || true
}

wait_for_health() {
  local deadline=$((SECONDS + 60))
  until curl -fsS -o /dev/null "${BASE_URL}/healthz" 2>/dev/null; do
    [ $SECONDS -lt $deadline ] || fail "service did not become healthy within 60s"
    sleep 0.25
  done
}

# --- Key ---------------------------------------------------------------------
# The API loads its API keys into memory ONCE at startup. Seeding replaces every
# key row, so a seed against a running API invalidates the key it is holding and
# every request 401s. Seeding therefore has to be followed by a restart, before
# the demo starts.
if [ -z "${DEMO_KEY:-}" ]; then
  echo "DEMO_KEY not set — seeding and restarting the API to pick up the new keys"
  DEMO_KEY=$(docker compose run --build --rm --entrypoint /app/migrate api seed 2>/dev/null \
             | awk '$1 == "demo" { print $2 }')
  [ -n "$DEMO_KEY" ] || fail "could not parse the demo key out of the seeder output"
  eval "$RESTART_CMD" >/dev/null
  wait_for_health
fi

wait_for_health

echo
echo "=== 1. Burn ${BURN} of the demo key's 10-token burst ==="
for i in $(seq 1 "$BURN"); do
  hit "request ${i}"
  [ "$LAST_CODE" = "200" ] || fail "request ${i} returned ${LAST_CODE}, expected 200 (is the key valid?)"
done
before_restart="${LAST_REMAINING:-unknown}"

echo
echo "=== 2. Restart the API process ==="
pid_before=$(api_pid)
echo "  \$ ${RESTART_CMD}"
restart_start=$SECONDS
eval "$RESTART_CMD" >/dev/null
wait_for_health
restart_secs=$((SECONDS - restart_start))
pid_after=$(api_pid)
if [ -n "$pid_before" ] && [ -n "$pid_after" ]; then
  echo "  api process pid ${pid_before} -> ${pid_after}  (healthy again in ${restart_secs}s)"
  [ "$pid_before" != "$pid_after" ] || fail "the API process did not actually restart"
else
  echo "  healthy again in ${restart_secs}s"
fi
echo "  new process, empty process memory — Redis was never touched"

echo
echo "=== 3. Probe until the limit bites ==="
admitted=0
limited=0
for i in $(seq 1 "$MAX_PROBES"); do
  hit "post-restart ${i}"
  case "$LAST_CODE" in
    200) admitted=$((admitted + 1)) ;;
    429) limited=1; break ;;
    *)   fail "unexpected status ${LAST_CODE} after the restart" ;;
  esac
done

echo
echo "=== Result ==="
echo "  remaining before restart : ${before_restart}"
echo "  admitted after restart   : ${admitted}"
echo "  restart took             : ${restart_secs}s"

[ "$limited" = "1" ] || fail "never hit 429 after the restart — quota appears to have reset"

# A process-local limiter would hand back the full burst of 10 after a restart.
# Anything at or below the pre-restart remainder plus a few seconds of refill
# proves the bucket outlived the process. The demo key refills at 10/min, so
# each second of restart is worth about 0.17 of a token.
budget=$((10 - BURN + 2))
[ "$admitted" -le "$budget" ] || \
  fail "admitted ${admitted} after restart (budget ${budget}) — quota reset with the process"

echo
echo "PASS: quota survived the restart. The bucket lives in Redis, not in the process."
