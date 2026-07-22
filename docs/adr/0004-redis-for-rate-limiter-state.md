# ADR-0004: Rate limiter state lives in Redis, not Postgres or process memory

- **Status:** accepted
- **Date:** 2026-07-22
- **Context:** Phase 5

## Context

The brief requires that "the rate limiter's state must survive a service restart without
resetting everyone's quota". That is a distributed state question wearing a rate limiter
costume, and it has more than one defensible answer.

A token bucket's entire state is `(tokens, last_refill_timestamp)`, and refill is a pure
function of elapsed time. Two properties follow, and they shape the whole decision:

1. **Losing the state fails open, never closed.** A missing bucket reads as full. The worst
   case after total loss is one burst window of over-admission — never a lockout.
2. **It is self-healing.** No reconciliation and no repair job; the next request recomputes
   correctly from whatever timestamp survived.

Those two properties are what make a cache an acceptable home for this data, and they are
the reason the same choice would be wrong for, say, billing counters.

## Options considered

| Option | Survives restart | Correct across N instances | Hot path | Real cost |
| --- | --- | --- | --- | --- |
| In-memory only | no | no — N instances enforce N× the limit | ~0 | Disqualified by the requirement |
| Postgres bucket table | yes | yes | ~1–3 ms | **Write amplification.** Every request becomes an UPDATE → WAL record → dirty page → vacuum pressure, and a hot key serialises on one row lock. It turns a read-mostly database into a write-saturated one, and puts the limiter in contention with the very reads it protects. |
| **Redis + Lua (chosen)** | yes | yes | ~0.3–1 ms | One more component; one network hop per request |
| In-memory + async checkpoint | partial | no | ~0 | Still N× the limit across instances |
| Two-tier: local buckets leasing batches from Redis | yes | approximate | ~0 amortised | The right answer past ~50k rps. Not justifiable inside this budget; documented as the scale-out path |

## Decision

**Redis-authoritative token bucket, evaluated in a Lua script.**

Postgres is rejected specifically because a rate limiter's write pattern — every single
request mutates state — is the pathological case for an MVCC database.

Atomicity lives in the script rather than in Go. A read-modify-write issued from Go races by
construction: two concurrent requests both read `tokens = 1` and both allow. Redis executes
the script to completion before serving another command, which is why the concurrency test
can assert *exactly* `burst` admissions rather than "roughly".

Three details in the script are load-bearing:

- **`redis.call('TIME')` is the only clock.** Using each instance's `time.Now()` lets clock
  skew corrupt a shared bucket — an instance running two seconds fast writes a future
  timestamp and every other instance then computes negative elapsed time. Legal inside a
  script under effects replication, the default since Redis 5.
- **`PEXPIRE` on every call.** Without it, every API key that ever made a request leaks one
  Redis key forever. The TTL is the refill-from-empty time plus slack, so a key can only be
  dropped once it would have been full anyway.
- **Numbers are returned as strings.** Redis truncates Lua floats to integers on the way
  out, which would silently corrupt `RateLimit-Remaining` and `Retry-After`.

## Consequences

**Accepted:** one more piece of infrastructure, and one network round trip on the hot path.

**Degradation is explicit.** A circuit breaker routes to an in-process fallback after five
consecutive failures, increments `ratelimit_degraded_total`, and raises
`ratelimit_degraded_active`. The limiter fails **open**: while degraded, limits are enforced
per instance, so N instances admit up to N× the intended rate. That is a deliberate trade of
precision for availability — a rate limiter outage must not become a service outage.

**The persistence claim is bounded and stated.** State survives application restarts,
redeploys and scale-out. It is not described as durable, because a managed Redis or Valkey
node that loses its data starts empty. Saying precisely what the persistence does not
guarantee, and why that is fine for a rate limiter specifically, is a stronger position than
claiming a guarantee that does not exist.
