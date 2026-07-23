#!/usr/bin/env bash
# Prove that SIGTERM drains in-flight requests instead of dropping them.
#
# docker compose stop sends SIGTERM and waits. A server that exits immediately
# severs live connections, which clients see as a truncated response — far
# worse than a clean refusal, because a retry is not obviously safe.
set -euo pipefail

env_port=""
if [ -f .env ]; then
  env_port=$(grep -E '^PORT=' .env | tail -1 | cut -d= -f2 | tr -d " \"'\r")
fi
BASE_URL="${BASE_URL:-http://localhost:${env_port:-8080}}"
WORKERS="${WORKERS:-20}"

fail() { printf '\nFAIL: %s\n' "$1" >&2; exit 1; }

wait_for_health() {
  local deadline=$((SECONDS + 60))
  until curl -fsS -o /dev/null "${BASE_URL}/healthz" 2>/dev/null; do
    [ $SECONDS -lt $deadline ] || fail "service did not become healthy"
    sleep 0.25
  done
}

wait_for_health

if [ -z "${API_KEY:-}" ]; then
  API_KEY=$(docker compose run --build --rm --entrypoint /app/migrate api seed 2>/dev/null \
            | awk '$1 == "loadtest" { print $2 }')
  [ -n "$API_KEY" ] || fail "could not parse the loadtest key"
  docker compose restart api >/dev/null
  wait_for_health
fi

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

echo "=== Driving ${WORKERS} concurrent clients ==="
for w in $(seq 1 "$WORKERS"); do
  (
    while :; do
      # curl exit 18 = partial transfer, i.e. a connection cut mid-response.
      code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
             "${BASE_URL}/evaluate/shutdown-probe?env=prod" -H "X-API-Key: ${API_KEY}" 2>/dev/null) \
        && echo "$code" >> "$workdir/ok.$w" \
        || echo "$?" >> "$workdir/err.$w"
      sleep 0.02
    done
  ) &
done
drivers=$(jobs -p)
# shellcheck disable=SC2064
trap "{ kill $drivers; rm -rf '$workdir'; } >/dev/null 2>&1 || true" EXIT

sleep 2
echo "=== SIGTERM (docker compose stop api) ==="
docker compose stop api >/dev/null 2>&1
sleep 1

{ kill $drivers; wait; } >/dev/null 2>&1 || true

ok=$(cat "$workdir"/ok.* 2>/dev/null | grep -c '^200$' || true)
truncated=$(cat "$workdir"/err.* 2>/dev/null | grep -c '^18$' || true)
other=$(cat "$workdir"/err.* 2>/dev/null | grep -vc '^18$' || true)

echo
echo "=== Result ==="
echo "  completed 200s        : ${ok}"
echo "  truncated mid-response: ${truncated}   <- must be 0"
echo "  refused after shutdown: ${other}       (expected: the socket is closed)"

echo
echo "=== Shutdown log ==="
docker compose logs api --tail 5 2>&1 | sed 's/^/  /'

docker compose start api >/dev/null 2>&1
wait_for_health

[ "$ok" -gt 0 ] || fail "no requests completed at all — the probe never ran"
[ "$truncated" -eq 0 ] || fail "${truncated} responses were cut mid-flight; shutdown is not draining"

echo
echo "PASS: in-flight requests completed; SIGTERM drained rather than dropped."
