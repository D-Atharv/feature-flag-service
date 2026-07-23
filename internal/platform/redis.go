package platform

import (
	"context"
	"log/slog"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	redisDialTimeout  = 2 * time.Second
	redisReadTimeout  = time.Second
	redisWriteTimeout = time.Second
	redisPoolSize     = 20
	redisPingTimeout  = 2 * time.Second
)


func NewRedisClient(addr string) *goredis.Client {
	opts := &goredis.Options{
		Addr:         addr,
		DialTimeout:  redisDialTimeout,
		ReadTimeout:  redisReadTimeout,
		WriteTimeout: redisWriteTimeout,
		PoolSize:     redisPoolSize,
	}

	if strings.HasPrefix(addr, "redis://") || strings.HasPrefix(addr, "rediss://") {
		if parsed, err := goredis.ParseURL(addr); err != nil {
			slog.Error("REDIS_ADDR looks like a URL but failed to parse; connecting will fail",
				"error", err.Error())
		} else {
			parsed.DialTimeout = redisDialTimeout
			parsed.ReadTimeout = redisReadTimeout
			parsed.WriteTimeout = redisWriteTimeout
			parsed.PoolSize = redisPoolSize
			opts = parsed
		}
	}

	client := goredis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("redis unreachable at startup; the rate limiter will start degraded",
			"addr", addr, "error", err.Error())
		return client
	}

	slog.Info("redis connected", "addr", opts.Addr, "tls", opts.TLSConfig != nil)
	return client
}
