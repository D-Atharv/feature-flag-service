-- Token bucket, evaluated atomically inside Redis.
--
-- KEYS[1]  bucket key
-- ARGV[1]  rate  -- tokens per second, always > 0 (ratelimit.ConfigFor guarantees it)
-- ARGV[2]  burst -- bucket capacity
-- ARGV[3]  cost  -- tokens this request consumes
--
-- Returns { allowed, tokens, retry_after, reset_after }, ALL AS STRINGS.
--
-- Atomicity lives here, not in Go. A read-modify-write issued from Go races by
-- construction: two concurrent requests both read tokens=1 and both allow.
-- Redis runs this script to completion before serving another command.

local key   = KEYS[1]
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local cost  = tonumber(ARGV[3])

-- One clock, and it belongs to Redis. Using each instance's own time.Now()
-- lets clock skew between API instances corrupt a shared bucket: an instance
-- running two seconds fast writes a future timestamp, and every other instance
-- then computes a negative elapsed time. redis.call('TIME') is legal inside a
-- script under effects replication, the default since Redis 5.
local t   = redis.call('TIME')
local now = tonumber(t[1]) + tonumber(t[2]) / 1000000

local b      = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(b[1])
local ts     = tonumber(b[2])

-- A bucket that is missing -- never used, or expired, or lost with the whole
-- data store -- reads as full. Losing state therefore fails OPEN: the worst
-- case after total Redis loss is one burst window of over-admission, never a
-- lockout, and it self-heals on the next request with no reconciliation.
if tokens == nil or ts == nil then
  tokens = burst
  ts     = now
end

-- Refill is a pure function of elapsed time, clamped at capacity. math.max
-- keeps a backwards clock jump from draining the bucket.
tokens = math.min(burst, tokens + math.max(0, now - ts) * rate)

local allowed = 0
local retry   = 0

if cost > burst then
  -- Unsatisfiable at any future time. Report the time to fill an empty bucket
  -- rather than an infinity, so the caller still gets a usable Retry-After.
  -- Unreachable from the middleware, which always costs 1 against a burst
  -- clamped to >= 1; kept as defence in depth.
  retry = burst / rate
elseif tokens >= cost then
  allowed = 1
  tokens  = tokens - cost
else
  retry = (cost - tokens) / rate
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)

-- Reclaim idle buckets. Without a TTL, every API key that ever made a request
-- leaks one Redis key forever. The TTL is the time to refill from empty plus a
-- second of slack, so a key can only be dropped once it would have been full
-- anyway -- expiry and a full bucket are indistinguishable to the next caller.
--
-- Capped at 24h: Lua stringifies large numbers in scientific notation
-- ("1e+14"), which Redis rejects as a PEXPIRE argument. Any bucket idle for a
-- day is full regardless, so the cap changes no observable behaviour.
local ttl = math.ceil(((burst / rate) + 1) * 1000)
if ttl > 86400000 then
  ttl = 86400000
end
redis.call('PEXPIRE', key, ttl)

-- Every element is stringified, for two separate reasons:
--   * Redis truncates Lua floats to integers on the way out, which would
--     silently round RateLimit-Remaining and Retry-After down to a lie.
--   * go-redis's StringSlice() rejects a reply that mixes integers with
--     strings, so returning `allowed` as a bare number makes the very first
--     call fail with a type error -- which the circuit breaker would then
--     report as "Redis is down".
return { tostring(allowed), tostring(tokens), tostring(retry), tostring((burst - tokens) / rate) }
