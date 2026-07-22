# Feature Flag Service

A feature flag service in Go — flag CRUD, sticky percentage rollouts via consistent
hashing, hand-rolled per-key rate limiting, and rate limit state that survives a restart.

> **Status: in progress.** Phases 0–5 are complete. This README is a skeleton carrying the
> proof artifacts produced so far; the full documentation lands in Phase 10.

## Quickstart

```bash
cp .env.example .env
make up        # api + postgres + redis
make migrate   # goose up
make seed      # prints the admin / demo / loadtest API keys once
```

```bash
make test          # go test -race ./...
make lint          # golangci-lint + govulncheck + off-the-shelf-limiter guard
make demo-restart  # proves rate limit quota survives a process restart
```

## Rate limiting

A token bucket per API key, hand-written — no off-the-shelf limiter, enforced by
`scripts/check-no-ratelimit-deps.sh` in CI rather than promised in prose.

The bucket's whole state is `(tokens, last_refill_timestamp)` and refill is a pure function
of elapsed time. Keeping that pair in Redis rather than in process memory is what makes the
quota outlive the process — and the read-modify-write happens inside a Lua script, so it is
atomic by construction rather than by luck.

Every response carries `RateLimit-Limit`, `RateLimit-Remaining` and `RateLimit-Reset`;
a 429 adds `Retry-After` and an RFC 7807 body.

```
$ curl -s -o /dev/null -D - "localhost:8080/evaluate/x?env=prod" -H "X-API-Key: $DEMO"
HTTP/1.1 429 Too Many Requests
Ratelimit-Limit: 10
Ratelimit-Remaining: 0
Ratelimit-Reset: 57
Retry-After: 3
```

### Proof: quota survives a restart

`make demo-restart`, run against docker-compose. The API process changes — note the PID —
while Redis is left alone, and the quota picks up exactly where it left off.

```
=== 1. Burn 8 of the demo key's 10-token burst ===
  request 1              status=200  RateLimit-Remaining=9
  request 2              status=200  RateLimit-Remaining=8
  request 3              status=200  RateLimit-Remaining=7
  request 4              status=200  RateLimit-Remaining=6
  request 5              status=200  RateLimit-Remaining=5
  request 6              status=200  RateLimit-Remaining=4
  request 7              status=200  RateLimit-Remaining=3
  request 8              status=200  RateLimit-Remaining=2

=== 2. Restart the API process ===
  $ docker compose restart api
  api process pid 15708 -> 15894  (healthy again in 0s)
  new process, empty process memory — Redis was never touched

=== 3. Probe until the limit bites ===
  post-restart 1         status=200  RateLimit-Remaining=1
  post-restart 2         status=200  RateLimit-Remaining=0
  post-restart 3         status=429  RateLimit-Remaining=0

=== Result ===
  remaining before restart : 2
  admitted after restart   : 2

PASS: quota survived the restart. The bucket lives in Redis, not in the process.
```

The same run in the playbook's form — twelve requests against a burst of ten, restart, then
one more:

```
$ for i in $(seq 1 12); do curl -s -o /dev/null -w '%{http_code} ' \
    "localhost:8080/evaluate/x?env=prod" -H "X-API-Key: $DEMO"; done
200 200 200 200 200 200 200 200 200 200 429 429

$ docker compose restart api && sleep 3
$ curl -s -o /dev/null -w '%{http_code}\n' "localhost:8080/evaluate/x?env=prod" -H "X-API-Key: $DEMO"
429
```

### What the persistence does and does not guarantee

State survives **application restarts, redeploys, and scale-out across instances**. It is
deliberately not described as durable: a managed Redis/Valkey node that loses its data
starts empty.

That is acceptable for a rate limiter specifically, and the reason is worth stating rather
than glossing. A missing bucket reads as *full*, so losing state fails **open** — the worst
case is one burst window of over-admission, never a lockout — and it self-heals on the next
request with no reconciliation job.

### Known limitations

- **While degraded, limits are per instance.** If Redis is unreachable the circuit breaker
  routes to an in-process fallback, so N instances admit up to N times the intended rate.
  A deliberate trade: a rate limiter outage must not become a service outage.
- **No pre-auth limiter.** Buckets are keyed on the API key ID, so the limiter necessarily
  runs after authentication and an unauthenticated flood still costs one auth check. That
  check is an in-memory SHA-256 map lookup — no database call, no slow KDF — so the cost is
  microseconds. A coarse per-IP limiter in front of auth is the missing piece.
- **API keys are loaded once at startup.** Re-seeding or adding a key requires a restart
  before the service will accept it. Flags refresh live; keys do not, yet.
- **One Redis round trip per request.** Past roughly 50k rps the answer is a two-tier design
  with instances leasing token batches locally; not built, and documented as the scale-out
  path rather than half-implemented.
