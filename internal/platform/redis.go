package platform

import (
	"context"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Redis client timeouts. These are the limiter's real safety net: without
// them, a Redis that accepts connections but stops answering would hold every
// request until the 30s global request timeout, turning a limiter blip into an
// outage. One second is generous for a sub-millisecond operation on the same
// network, and a timeout is just another failure the circuit breaker counts.
const (
	redisDialTimeout  = 2 * time.Second
	redisReadTimeout  = time.Second
	redisWriteTimeout = time.Second
	redisPoolSize     = 20
	redisPingTimeout  = 2 * time.Second
)

// NewRedisClient builds the client used for rate limiter state.
//
// Unlike NewPostgresPool this does NOT fail startup when the server is
// unreachable — it warns and returns a usable client. That asymmetry is
// deliberate: Postgres is required to serve, whereas the limiter is designed
// to degrade to an in-process fallback. A service that refuses to boot because
// its rate limiter is down has inverted the entire point of failing open.
func NewRedisClient(addr string) *goredis.Client {
	client := goredis.NewClient(&goredis.Options{
		Addr:         addr,
		DialTimeout:  redisDialTimeout,
		ReadTimeout:  redisReadTimeout,
		WriteTimeout: redisWriteTimeout,
		PoolSize:     redisPoolSize,
	})

	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("redis unreachable at startup; the rate limiter will start degraded",
			"addr", addr, "error", err.Error())
		return client
	}

	slog.Info("redis connected", "addr", addr)
	return client
}
