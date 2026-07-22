# How AI was used on this project

The brief says the evaluation is not *whether* AI was used but *how*. This file answers that
with specifics: what was delegated, what deliberately was not, and — the part that actually
carries signal — where the AI proposed something wrong and how it was caught.

## The delegation boundary

| Delegated | Not delegated |
| --- | --- |
| Table-driven test scaffolding | The Lua bucket script |
| RFC 7807 problem plumbing, handler boilerplate | Middleware ordering and its rationale |
| Repository CRUD around hand-written SQL | The rate limiter persistence trade-off |
| Doc comments, README prose | The consistent-hashing input (flag key + env in the hash) |

The rule was simple: anything that has to be defended line by line in a walkthrough gets
written and understood, not generated. Anything mechanical, where correctness is verifiable
by a test, is fair game.

## Catches — where the AI was wrong

These are the ones that changed the code. Each is concrete and each cost real time to find,
which is the point: a generic "AI helped me move faster" claim is unfalsifiable.

### 1. `golang.org/x/time/rate`, suggested unprompted

The reflex suggestion for any Go rate limiting question. It *is* a token bucket and it ships
under the `golang.org/x` umbrella, which makes it read like standard library rather than a
third-party limiter — and the brief prohibits exactly this.

Rather than relying on discipline, this is a CI check:
`scripts/check-no-ratelimit-deps.sh` greps `go.mod`, `go.sum` and the source tree, and fails
the build. A green badge proving the constraint was honoured beats a sentence claiming it.

### 2. That same guard would have failed the build on an English word

Found by probing the guard rather than trusting it. The original pattern matched the bare
string `throttled`, across all `.go` files — so a doc comment reading "requests are
throttled" fails `make lint`, in the one package guaranteed to discuss rate limiting.

```
$ printf 'package ratelimit\n\n// requests are throttled here\n' > internal/ratelimit/zz_probe.go
$ ./scripts/check-no-ratelimit-deps.sh
./internal/ratelimit/zz_probe.go:3:// requests are throttled here
ERROR: off-the-shelf rate limiter dependency detected
```

Fixed by splitting the pattern: manifests are still matched on bare module names, while the
source tree is matched on import-path forms only. Coverage is unchanged — an import that
isn't in `go.mod` cannot compile — and both directions are now verified by probe.

### 3. `EVALSHA` without an `EVAL` fallback

Redis's script cache is volatile and is emptied on restart. A raw `EVALSHA` therefore
returns `NOSCRIPT` on *precisely* the restart this service exists to demonstrate surviving —
the demo would fail in the most confusing way available. `redis.NewScript(...).Run()` does
EVALSHA → NOSCRIPT → EVAL → re-cache.

Not left as a rule in a document: `TestSurvivesScriptCacheFlush` issues `SCRIPT FLUSH`
mid-test and asserts the next call still succeeds with the bucket intact.

### 4. `StringSlice()` against a reply that mixes types

The proposed Lua returned `{allowed, tostring(tokens), ...}` — element one a number, the
rest strings — parsed with go-redis's `StringSlice()`, which rejects non-string elements.

The failure mode is what makes this worth recording: every call returns a type error, the
circuit breaker counts five of them, and the service quietly runs on the in-memory fallback.
It still answers 200s and 429s. It looks like it works. Fixed by returning
`tostring(allowed)` so all four values are strings.

This is the second instance of the same root cause — Lua values crossing the wire badly. The
first is the documented float-truncation rule (Redis truncates Lua floats to integers, which
would silently corrupt `RateLimit-Remaining` and `Retry-After`). The existing rule did not
cover this case because `allowed` is not a float.

### 5. Divide-by-zero reachable from one database row

The bucket script divides by `rate` in three places. A key with `rate_limit_rps = 0` yields
`inf`, `math.ceil(inf)` makes `PEXPIRE` fail, and every call for that key errors — which the
circuit breaker faithfully reports as "Redis is down" while the real cause sits in a single
table row. `ratelimit.ConfigFor` now clamps non-positive rate and burst to documented
defaults and logs a warning. Table-tested.

### 6. An uncapped TTL that Redis rejects

`PEXPIRE` is computed from `burst / rate`. For a slow enough refill that value exceeds what
Lua prints as a plain integer — it stringifies as `1e+14`, which Redis rejects as a
non-integer argument, failing every call for that key. Capped at 24h, which changes no
observable behaviour: a bucket idle for a day is full regardless, and expiry is
indistinguishable from a full bucket to the next caller.

### 7. A proposed plan that would have hidden the phase's most important test

An alternative plan put the Redis-backed tests behind `//go:build integration`. The phase's
exit test is `go test -race ./internal/ratelimit/...`, so the concurrency test — the one
test that catches a non-atomic implementation — would never have compiled in. The exit gate
would have gone green having proven nothing. Rejected: a test the exit criterion cannot
reach is not an exit criterion.

### 8. A demo script that would have 401'd on every request

The restart demo seeds an API key and immediately uses it. But the service loads its API
keys into memory once at startup, and seeding replaces every key row — so seeding against a
running API invalidates the key it is holding, and every request comes back 401, looking
exactly like a broken limiter. The script now restarts the API after seeding, and the
constraint is written down as a known limitation rather than left as a trap.

## Catches from reviewing the finished code

The first eight were caught while planning or building. These three were caught by an
adversarial pass over code that was already written, already tested, and already passing its
exit criteria — which is the more useful demonstration, because "the tests are green" was
true the whole time. Each was reproduced by a failing test before being fixed.

### 9. A log line per request from one bad database row

`ConfigFor` clamps unusable rate limits and warned every time it did so. It runs on every
request, so a single misconfigured key produced one warning per request — the same
self-inflicted outage the `/evaluate` access log avoids by sampling at 1%, reintroduced one
layer further down. Reproduced by asserting one log line for 100 calls (got 100), fixed with
per-key deduplication, and covered by a second test proving a *different* broken key still
warns.

### 10. A stampede every cooldown, hiding inside a working breaker

When the circuit breaker's cooldown lapsed, *every* concurrent request saw the gate open in
the same instant and dialled a store that was still down, each paying the full dial timeout.
At any real request rate that is a latency cliff once per cooldown for the entire outage —
and it does not show up in any of the sequential tests, all of which passed. The half-open
state is now single-flight: exactly one probe, everyone else keeps using the fallback.
Reproduced with 50 concurrent requests (3 probes with an instant-failing fake; all 50 with a
real 2s dial timeout).

### 11. A panic that wedged the limiter permanently

Introduced *by the fix for #10*. The half-open gate was released on both return paths, so a
panic inside the primary limiter kept it forever. Recovery middleware turns that panic into
a 500 and the process carries on — leaving the breaker stuck on the per-instance fallback
with `ratelimit_degraded_active` pinned at 1, no error to explain it, and no path back.

Worth recording for two reasons. First, the first attempt at the test *passed against the
broken code*, because a healthy breaker never takes the gate — the wedge only reproduces if
the panic lands on the probe itself, which meant getting the test wrong before getting it
right. Second, it is a reminder that a fix is new code and deserves the same suspicion as
the code it replaced.

## Verification, not vibes

Every claim in this file was checked by running something. The guard false-positive was
found by probing the guard; the restart demo asserts the container PID actually changed;
the concurrency test asserts *exactly* `burst` admissions rather than "about right"; and the
parity test runs one scenario table through both the Redis and in-memory limiters to keep
the fallback from silently disagreeing with the primary.
