# Feature Flag Service

A Go + Gin + Postgres service for managing feature flags with per-environment targeting, sticky percentage rollouts via consistent hashing, a hand-rolled per-API-key token-bucket rate limiter, and rate-limiter state that survives a service restart.

**Live:** https://ffs-api-u2mn.onrender.com  ·  **Interactive API docs:** https://ffs-api-u2mn.onrender.com/docs  ·  **Repo:** <https://github.com/D-Atharv/feature-flag-service> ·  **Keys:** DEMO= lo_live_c8226e925379de47ec64af97c84605d9617d6d082e9ad03bb13041cc267f9319\
ADMIN= lo_live_5a103b78b495adc672a1a1c3e72cda4272a4bdd3a348e9af9eb8896e73df6850

---

## Table of contents

 1. [Try it live](#try-it-live)
 2. [What this is](#what-this-is)
 3. [Quickstart (local)](#quickstart-local)
 4. [API reference](#api-reference)
 5. [Architecture](#architecture)
 6. [Data model](#data-model)
 7. [Design decisions (and why)](#design-decisions-and-why)
 8. [Rate limiter — the centrepiece](#rate-limiter--the-centrepiece)
 9. [Testing & proof](#testing--proof)
10. [Deployment](#deployment)
11. [Configuration](#configuration)
12. [Project layout](#project-layout)
13. [Known limitations](#known-limitations)
14. [What I'd do differently with more time](#what-id-do-differently-with-more-time)

---

## Try it live

A published **demo key** is included below on purpose. It is *evaluate-scoped*(cannot modify flags) and rate-limited to **10 requests/minute** — low, so the 429 behaviour is easy to see. Never the admin key.

```bash
BASE=https://ffs-api-u2mn.onrender.com
DEMO=lo_live_c8226e925379de47ec64af97c84605d9617d6d082e9ad03bb13041cc267f9319
ADMIN=lo_live_15eb9bc330c687ac8bd29fc690fda374a0fcc9d918f74499d919fcfa39010049
LOADTEST=lo_live_5a103b78b495adc672a1a1c3e72cda4272a4bdd3a348e9af9eb8896e73df6850

# 1. Service index — version, git SHA, links (no auth)
curl -s $BASE/ | jq

# 2. Evaluate a flag for a subject (a flag "new-checkout-flow" is pre-seeded)
curl -s "$BASE/evaluate/new-checkout-flow?env=prod&subject=user-42" -H "X-API-Key: $DEMO" | jq

# 3. Watch the rate limiter bite — 11th+ request in a minute → 429
for i in $(seq 1 12); do
  curl -s -o /dev/null -w '%{http_code} ' "$BASE/evaluate/new-checkout-flow?env=prod&subject=u$i" -H "X-API-Key: $DEMO"
done; echo
# → 200 200 200 200 200 200 200 200 200 200 429 429
```

> The free Render web instance sleeps after 15 min idle, so the **first** request may take \~50s to cold-start. Subsequent requests are fast.
>
> If the demo key ever returns `401`, the database was re-seeded (keys rotate on every seed) — see [Configuration](#configuration).

Or open [**/docs**](https://ffs-api-u2mn.onrender.com/docs) in a browser, paste the demo key into the auth box, and run requests from the page.

---

## What this is

Feature flags let teams toggle features on/off *per environment* without redeploying. This service exposes CRUD for flags plus an **evaluation endpoint**that answers "is this flag on for this caller?" — respecting a rollout percentage via consistent hashing so a given caller always gets the same answer.

The assignment probes three things specifically, and each is treated as a first-class concern here:

| \# | Requirement | Where it lives |
| --- | --- | --- |
| **1** | **Consistent hashing** for sticky rollouts | `internal/evaluation` — pure, no I/O |
| **2** | **Hand-rolled token bucket**, no off-the-shelf library, per API key | `internal/ratelimit` — Lua + Go |
| **3** | **Rate-limiter state survives a restart** | Redis/Valkey holds the bucket; proven by `make demo-restart` |

Plus a stated grading dimension — *how AI was used* — documented in `docs/AI_USAGE.md`.

---

## Quickstart (local)

**Prerequisites:** Docker + Docker Compose, and Go 1.26 (only if you want to run tests outside the container).

```bash
git clone https://github.com/D-Atharv/feature-flag-service.git
cd feature-flag-service
cp .env.example .env

make up        # docker-compose: api + postgres + redis
make migrate   # apply schema (goose)
make seed      # prints the admin / demo / loadtest API keys ONCE — copy them
```

Verify it's up:

```bash
curl -s localhost:8080/healthz     # {"status":"ok"}
docker compose ps                  # api, postgres, redis all Up/healthy
```

Create and evaluate a flag (use the **admin** key printed by `make seed`):

```bash
K="X-API-Key: <admin key>"
B=localhost:8080

curl -s -X POST $B/api/v1/flags -H "$K" \
  -d '{"key":"new-checkout-flow","environment":"prod","enabled":true,"rollout_percentage":50}' | jq

curl -s "$B/evaluate/new-checkout-flow?env=prod&subject=user-42" -H "$K" | jq
```

Everyday targets:

```bash
make test          # go test -race ./...   (integration tests included)
make lint          # golangci-lint + gosec + govulncheck + off-the-shelf-limiter guard
make demo-restart  # PROOF: rate-limit quota survives a process restart
make demo-shutdown # PROOF: SIGTERM drains in-flight requests (0 truncated)
make load          # k6 load test against /evaluate (loadtest key)
./scripts/smoke.sh <base-url>   # 10-step acceptance suite, local or live
```

---

## API reference

Full, executable OpenAPI 3.1 spec at `/docs`.

| Method | Path | Scope | Notes |
| --- | --- | --- | --- |
| `POST` | `/api/v1/flags` | admin | `409` on duplicate `(key, environment)` |
| `GET` | `/api/v1/flags?environment=&limit=&cursor=` | admin | **keyset** pagination, not OFFSET |
| `GET` | `/api/v1/flags/:key?environment=` | admin | no `environment` → all envs for that key |
| `PATCH` | `/api/v1/flags/:key?environment=` | admin | `If-Match: "3"` → **412** on stale version |
| `DELETE` | `/api/v1/flags/:key?environment=` | admin | `204`, writes an audit row |
| `GET` | `/evaluate/:key?env=&subject=` | evaluate | unknown flag → **200** `FLAG_NOT_FOUND` (not 404) |
| `GET` | `/api/v1/evaluate/:key?env=&subject=` | evaluate | canonical path, identical behaviour |
| `GET` | `/` `/healthz` `/readyz` `/metrics` `/docs` | none | ops & docs, no auth |

**Auth:** every request sends `X-API-Key: <key>`. Keys are scoped — an evaluate-scoped key gets `403` on any `/flags` route.

**Errors:** RFC 7807 `application/problem+json` with `type`, `title`, `status`, `detail`, `request_id`.

**Rate-limit headers:** every response carries `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`; a `429` adds `Retry-After` (whole seconds).

**Evaluation response:**

```json
{
  "enabled": true,
  "reason": "ROLLOUT_INCLUDED",
  "flag_key": "new-checkout-flow",
  "environment": "prod",
  "subject": "user-42",
  "evaluated_at": "2026-07-23T03:23:21Z"
}
```

`reason` is machine-readable — switch on it, not on `enabled` alone. Values: `FLAG_NOT_FOUND`, `FLAG_DISABLED`, `ROLLOUT_FULL`, `ROLLOUT_ZERO`, `ROLLOUT_INCLUDED`, `ROLLOUT_EXCLUDED`.

---

## Architecture

### Deployed shape (Render)

```
                          HTTPS (Render-managed TLS)
   client / browser / curl ──────────────────────────►  ffs-api
   /docs console                                          (Web Service · Docker
                                                            · your Dockerfile)
                                          ┌───────────────────┴───────────────────┐
                                          │ in-process, loaded at boot:            │
                                          │  • KeyMap  (auth, O(1), no DB)         │
                                          │  • flag snapshot (atomic.Pointer[map]) │
                                          │ background goroutine:                  │
                                          │  • 30s poller + LISTEN/NOTIFY refresh  │
                                          └───────┬───────────────────┬────────────┘
                              DATABASE_URL        │                   │  REDIS_ADDR
                              (internal)          │                   │  (internal)
                                    ┌─────────────▼──────┐   ┌────────▼─────────────┐
                                    │ ffs-postgres        │   │ ffs-keyvalue          │
                                    │ managed Postgres 16 │   │ managed Valkey 8      │
                                    │ flags · api_keys ·  │   │ (Redis-compatible)    │
                                    │ flag_audit · envs   │   │ rl:v1:<key> → bucket  │
                                    └─────────────────────┘   └──────────────────────┘
                                    source of truth (writes)   rate-limit state
                                    read path never queries it  survives app restart
```

Three independent Render services. `ffs-api` is built from the repo's `Dockerfile`; Postgres and Valkey are managed products wired in by connection string. `docker-compose.yml` is used only for local dev.

### Request path — the middleware chain

Hand-rolled with `gin.New()` (never `gin.Default()`), assembled in a fixed, defensible order in `cmd/api/main.go`:

```
RequestID → Recovery → Logger → Metrics → BodyLimit → Timeout → Auth → RateLimit → handler
```

Each position is load-bearing:

| Stage | Why here |
| --- | --- |
| **RequestID** first | Everything downstream (incl. the panic log) correlates on it; it cannot itself panic. |
| **Recovery** second | Wraps *all* user code, including the logger — a panic becomes a 500, never a dropped connection. |
| **Logger** third — *outside Auth* | So `401`s and `429`s are logged. Inside Auth you go blind during exactly the incidents you'd investigate. Emits on the way **out** (has final status + latency). `/evaluate` access logs sampled at 1%. |
| **Metrics** early | Otherwise rejected requests never appear in dashboards. |
| **BodyLimit / Timeout** | Cheap guards precede expensive work; `http.MaxBytesReader` caps bodies (→ **413**). |
| **Auth** before RateLimit | The bucket is keyed on the resolved API-key ID, so identity must exist first. |
| **RateLimit** last | Everything before it is bounded and cheap; the handler is what we're protecting. |

### The zero-dependency read path

`/evaluate` is the hot path and **touches no network dependency**:

- **Auth** reads an in-memory `map[sha256]APIKey` built once at boot — no DB query.
- **Evaluation** reads an in-memory snapshot of all flags — no DB query.
- Only the **rate limiter** makes a network call (to Valkey), and that fails open.

Consequence: **Postgres can be fully down and evaluations keep serving.** Proven below.

```
docker compose stop postgres
evaluate → 200   (served from the in-memory snapshot)
readyz   → 200   (gates on "snapshot loaded", not on Postgres)
CRUD     → 500   (writes need the DB, and say so)
```

The snapshot is kept fresh out-of-band by two mechanisms:

- A **30-second reconcile poller** — the correctness guarantee.
- `LISTEN/NOTIFY` — a latency accelerator. A flag write fires a Postgres trigger → `pg_notify` → the listener reloads (measured propagation \~95ms). Notifications have no delivery guarantee, so if one is lost the poller still catches it within 30s. The listener does a **full refresh on every reconnect**to close any gap.

---

## Data model

Four tables. DDL in `migrations/001_init.sql` and `002_flag_audit.sql`.

```
 environments ──1:N──► flags ──1:N (logical, no FK)──► flag_audit ◄──N:1── api_keys
   (FK target)          UNIQUE(key, environment)        append-only history      (auth + limits)
```

| Table | Purpose | Key columns |
| --- | --- | --- |
| `environments` | Allow-list of valid envs (`dev`/`staging`/`prod`) | `name` (PK, CHECK) |
| `flags` | The flags themselves | `key`, `environment` (FK), `enabled`, `rollout_percentage`, `version`, `UNIQUE(key, environment)` |
| `flag_audit` | Who changed what, when | `flag_key`, `action`, `actor_key_id`, `before`/`after` (jsonb) |
| `api_keys` | Auth + per-key rate limits | `key_hash` (sha256, UNIQUE), `is_admin`, `rate_limit_rps`, `rate_limit_burst` |

Relationship notes:

- `environments` **→** `flags` **is an enforced FK.** A typo like `"prod "` is a `400` at write time, not a phantom flag nothing reads.
- `flags` **→** `flag_audit` **deliberately has no FK.** Audit history must survive a flag being deleted, so the link is logical (`flag_key`), not constrained.
- `UNIQUE(key, environment)`**, not** `UNIQUE(key)`**.** The same flag key exists *independently* per environment — the entire point of the service.

Every write (`POST`/`PATCH`/`DELETE`) writes the flag change **and** its audit row in **one transaction** — you can never have a change without its audit record.

---

## Design decisions (and why)

### 1. Consistent hashing for rollouts

```
bucket = xxhash64(flagKey + ":" + env + ":" + subject) % 100
enabled = flag.enabled AND bucket < rollout_percentage
```

- `xxhash`**, not** `maphash` — `maphash` is randomly seeded per process, so a restart would re-bucket everyone. `xxhash` is deterministic and versioned; the bucket must never change across restarts, deploys, or instances.
- **Flag key + env are *in* the hash.** Without them, every flag at 10% would expose the *same* 10% of subjects — correlated experiments. Including them makes each flag's rollout statistically independent.
- **Three properties, each proven by a test** (not asserted in prose):
  - **Deterministic** — same subject always gets the same answer (stickiness).
  - **Monotonic** — raising rollout 50→60 only *adds* subjects, never removes one. (Most naive implementations get this wrong by re-hashing the percentage.)
  - **Uniform** — 1000 synthetic subjects at 50% → \~500 enabled (measured live: 501/1000).

### 2. Rate limiter state → Redis, not Postgres or memory

See the [dedicated section](#rate-limiter--the-centrepiece). In short: a token bucket's whole state is `(tokens, timestamp)` and refill is a pure function of time, so it belongs in a fast store outside the process. Postgres would write-amplify (every request an UPDATE → WAL → vacuum); in-memory dies on restart and gives N× the limit across instances.

### 3. Auth: API keys, SHA-256 (not bcrypt), in-memory map

- **API keys, not username/password** — callers are *services*, not people. Sessionless: the key is on every request, and it carries the caller's scope and rate-limit budget in one row.
- **SHA-256, not bcrypt/argon2** — slow KDFs defend *low-entropy human passwords*. A 256-bit random key (`crypto/rand`) has no brute-force surface, so bcrypt buys nothing and would cost \~100ms/request, making auth the bottleneck and the rate limiter irrelevant. Constant-time compare guards against timing.
- **Raw key never stored** — only `sha256(raw)`. The raw value is printed once at seed time.
- **Loaded once into an in-memory map** — auth is an O(1) lookup with zero DB queries, which is what lets `/evaluate` survive high volume.

### 4. Unknown flag on `/evaluate` → `200 {"reason":"FLAG_NOT_FOUND"}`, not 404

Evaluation is on the caller's hot path; a 404 becomes an exception in most HTTP clients. A typo'd flag key should not throw in the caller's code. `?strict=true`opts into 404 semantics. CRUD routes still 404 properly.

### 5. Optimistic concurrency on updates

`PATCH` requires `If-Match: "<version>"`; a stale version returns **412** instead of silently overwriting a concurrent edit. `version` increments on every update.

### 6. RFC 7807 problem responses, one central mapper

Domain errors (`ErrNotFound`, `ErrConflict`, `ErrVersionMismatch`, `ErrInvalidInput`, `ErrUnavailable`) map to HTTP status in one place. Handlers never write status codes directly; internal error detail is logged, never returned.

### 7. Migrations never run in the API process

A separate `/app/migrate` binary (same image, different entrypoint) applies migrations, so N API instances can't race the same DDL.

---

## Rate limiter — the centrepiece

A token bucket per API key, hand-written — **no off-the-shelf limiter**, enforced by `scripts/check-no-ratelimit-deps.sh` in CI (fails the build if `golang.org/x/time/rate` or similar appears) rather than promised in prose.

**Atomicity lives in a Lua script, not in Go.** A read-modify-write from Go races by construction — two concurrent requests both read `tokens=1` and both allow. Redis runs the script to completion before serving another command, so the check-and-decrement is indivisible. Proven by a test firing 250 goroutines at one bucket and asserting *exactly* `burst` admissions.

Four details in the Lua script, each a trap that silently corrupts the limiter if missed:

- `redis.call('TIME')` **is the clock**, never the Go process's `time.Now()` — client clocks skew between instances and corrupt a shared bucket.
- **Return numbers as strings** — Redis truncates Lua floats to integers, which would silently round `RateLimit-Remaining`/`Retry-After`.
- `PEXPIRE` **on every call** — else every API key that ever made a request leaks a Redis key forever.
- `redis.NewScript(...).Run()`**, never raw** `EVALSHA` — the script cache is flushed on restart, so `EVALSHA` returns `NOSCRIPT` on *exactly* the restart being demoed. `Script.Run` falls back to `EVAL` and re-caches.

**Fails open, never closed.** After N consecutive Redis failures a circuit breaker routes to an in-memory fallback, increments `ratelimit_degraded_total`, and logs a warning — a limiter outage must not become a service outage. The cost, stated honestly: while degraded, N instances enforce the limit *per instance*.

Every response carries `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`; a 429 adds `Retry-After` and an RFC 7807 body:

```
$ curl -s -o /dev/null -D - "localhost:8080/evaluate/x?env=prod" -H "X-API-Key: $DEMO"
HTTP/1.1 429 Too Many Requests
RateLimit-Limit: 10
RateLimit-Remaining: 0
RateLimit-Reset: 57
Retry-After: 3
```

### Proof: quota survives a restart

`make demo-restart`, against docker-compose. The API **process changes** — note the PID — while Redis is left untouched, and the quota picks up exactly where it left off.

```
=== 1. Burn 8 of the demo key's 10-token burst ===
  request 1              status=200  RateLimit-Remaining=9
  ...
  request 8              status=200  RateLimit-Remaining=2

=== 2. Restart the API process ===
  $ docker compose restart api
  api process pid 15708 -> 15894  (healthy again in 0s)
  new process, empty process memory — Redis was never touched

=== 3. Probe until the limit bites ===
  post-restart 1         status=200  RateLimit-Remaining=1
  post-restart 2         status=200  RateLimit-Remaining=0
  post-restart 3         status=429  RateLimit-Remaining=0

PASS: quota survived the restart. The bucket lives in Redis, not in the process.
```

The same requirement, proven **on the live Render deployment** (restart via deploy hook), and in the assignment's twelve-request form:

```
$ for i in $(seq 1 12); do curl -s -o /dev/null -w '%{http_code} ' \
    "$BASE/evaluate/x?env=prod" -H "X-API-Key: $DEMO"; done
200 200 200 200 200 200 200 200 200 200 429 429

$ # trigger a redeploy (new process), then immediately:
$ curl -s -o /dev/null -w '%{http_code}\n' "$BASE/evaluate/x?env=prod" -H "X-API-Key: $DEMO"
429   ← quota survived the restart
```

### What the persistence does and does not guarantee

State survives **application restarts, redeploys, and scale-out across instances**. It is deliberately **not** called durable: a managed Redis/Valkey node that loses its data starts empty.

That is acceptable *for a rate limiter specifically*. A missing bucket reads as **full**, so losing state fails **open** — worst case is one burst window of over-admission, never a lockout — and it self-heals on the next request with no reconciliation job. (A self-hosted Redis with `appendonly yes` on a disk would add real AOF durability; managed Valkey free tier does not, and the honest claim is the stronger interview answer.)

---

## Testing & proof

```bash
make test          # go test -race ./... — unit + integration (real Postgres & Redis)
make lint          # golangci-lint + gosec + govulncheck + dep guard
./scripts/smoke.sh <base-url>   # 10-step acceptance suite, local or live
make demo-restart  # quota survives restart
make demo-shutdown # SIGTERM drains in-flight requests, 0 truncated
make load          # k6: p50/p95/p99 + rps against /evaluate (loadtest key)
```

- **Unit** — hashing (determinism golden file, monotonicity property, uniformity χ²), token-bucket math, the circuit breaker, config validation. Coverage focused on `domain`/`evaluation`/`ratelimit`.
- **Integration** — repositories against real Postgres; the limiter against real Redis, including the N-goroutine concurrency test and the reconnect/full-refresh test. Run in CI via GitHub Actions `services:`.
- `-race` **in CI, always.** The one test to keep if you keep only one: N goroutines against a single bucket asserting *exactly* `burst` admissions.

The 10-step `smoke.sh` suite is parameterised by URL and passes against the **live Render deployment**: public surface, auth scoping (401/403/200), CRUD + optimistic-concurrency 412, stickiness, rollout distribution (501/1000 at 50%), monotonicity (50→60 removes nobody), rate-limit headers, metrics, `/docs`, and delete propagation.

---

## Deployment

Deployed on **Render** as three services, provisioned by `render.yaml`:

| Service | What | How |
| --- | --- | --- |
| `ffs-api` | The Go service | Built from `Dockerfile` (multi-stage → distroless, non-root, `CGO_ENABLED=0`) |
| `ffs-postgres` | Managed Postgres 16 | `DATABASE_URL` wired via `fromDatabase` (internal) |
| `ffs-keyvalue` | Managed Valkey 8 (Redis-compatible) | `REDIS_ADDR` wired via `fromService` (internal) |

The same image, same five env vars, runs locally under `docker-compose.yml` and on Render — the app never knows which platform populated its environment.

**Migrations** run from outside Render (the free web plan rejects `preDeployCommand`, and the distroless image has no shell): the free Postgres exposes an external connection string, and `scripts/render-migrate.sh` builds the same image locally and runs `/app/migrate up|seed` against it — the same shell-free entrypoint-override that `docker-compose` uses, pointed at the remote host.

```bash
DATABASE_URL='<render external url>' ./scripts/render-migrate.sh up
DATABASE_URL='<render external url>' ./scripts/render-migrate.sh seed
```

---

## Configuration

Five environment variables, validated at startup (fail-fast — a missing var is a boot error, not a first-request surprise). See `.env.example`.

| Var | Example | Notes |
| --- | --- | --- |
| `ENV` | `prod` | `dev` |
| `PORT` | `8080` | On Render, injected automatically |
| `DATABASE_URL` | `postgres://…` | pgxpool |
| `REDIS_ADDR` | `redis:6379` or `redis://…` | accepts host:port *or* a `redis://`/`rediss://` URL |
| `LOG_LEVEL` | `info` | `debug` |

**Three seeded API keys** (`make seed`), deliberately distinct:

| Key | Scope | Limit | Purpose |
| --- | --- | --- | --- |
| `admin` | admin | 100 rps | Full CRUD. Never published. |
| `demo` | evaluate | 10/min | Published in this README. Low limit so the 429 demo works. |
| `loadtest` | evaluate | 50 000 rps | So the load test measures the service, not the limiter. |

> Seeding rotates all three keys. The `demo` key published above is valid against the current deployment; re-seeding invalidates it.

---

## Project layout

```
cmd/api/          server: wiring, middleware chain, graceful shutdown
cmd/migrate/      separate binary: goose migrations + seed (never runs in the API)
internal/
  config/         env parsing + validation, fail-fast
  domain/         Flag, APIKey, typed errors — zero project imports, no I/O
  evaluation/     consistent hashing + decision tree — pure, no I/O
  ratelimit/      Limiter interface; redis/ (Lua) + memory/ impls; circuit breaker
  snapshot/       in-memory flag store + LISTEN/NOTIFY refresher + poller
  store/postgres/ repositories + embedded migrations
  httpapi/        router, handlers, middleware, RFC 7807 problem, OpenAPI /docs
  observability/  prometheus metrics
  platform/       pg pool, redis client, listener connector
migrations/       001_init.sql, 002_flag_audit.sql (embedded via embed.FS)
test/integration/ real Postgres + Redis
test/load/        k6
scripts/          smoke.sh, demo-restart.sh, demo-shutdown.sh, dep guard, render-migrate.sh
docs/             AI_USAGE.md
```

`domain/` and `evaluation/` import nothing from the project and no I/O packages — everything points inward (hexagonal-lite). This is what let the read path swap from DB to in-memory snapshot, and the limiter swap between Redis and memory, without touching a handler.

**Stack:** Go 1.26 · Gin · pgx/v5 (no ORM, hand-written repositories) · goose + `embed.FS` · go-redis/v9 + Lua (Valkey on Render) · `cespare/xxhash/v2` · `log/slog` · `prometheus/client_golang` · testify · k6 · distroless container. \~8 direct dependencies — a short list makes the "no off-the-shelf limiter" claim trivially auditable.

---

## Known limitations

Named up front rather than left to be discovered — all are deliberate trades for a single-region take-home, not oversights.

- **Single region, single instance.** Correct for a take-home, wrong for production.
- **While degraded, limits are per instance.** If Redis is unreachable the breaker routes to an in-process fallback, so N instances admit up to N× the intended rate. A limiter outage must not become a service outage.
- **No pre-auth limiter.** Buckets are keyed on the API-key ID, so the limiter runs after auth; an unauthenticated flood still costs one auth check (an in-memory SHA-256 lookup — microseconds). A coarse per-IP limiter in front of auth is the missing piece.
- **API keys load once at startup.** Adding/rotating a key needs a restart before the service accepts it. Flags refresh live; keys do not, yet.
- **Read-your-writes is not immediate.** A `PATCH` then an instant `/evaluate` may see the old value for up to \~100ms (notification debounce) or 30s (if the notification is lost and the poller must catch it). Deliberate: coalescing means a bulk update causes one reload, not one per flag.
- **One Redis round trip per request.** Past \~50k rps the answer is a two-tier design with instances leasing token batches locally — documented as the scale-out path rather than half-built.
- **Persistence is not durable.** "Survives restarts, redeploys, scale-out," never "durable" — managed Valkey free tier has no AOF (see above).
- **No distributed tracing.** OpenTelemetry deferred.
- **Build metadata on Render.** `GET /` reports `version: dev` there — Render's Blueprint has no field to pass Docker build args. Cosmetic; `git_sha` is correct in local/CI builds.

---