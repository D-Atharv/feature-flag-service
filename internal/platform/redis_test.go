package platform_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/platform"
)

// TestNewRedisClient_AcceptsBareHostPort is the docker-compose / local shape:
// REDIS_ADDR=redis:6379. Must keep working unchanged.
func TestNewRedisClient_AcceptsBareHostPort(t *testing.T) {
	client := platform.NewRedisClient("localhost:6380")
	require.NotNil(t, client)
	assert.Equal(t, "localhost:6380", client.Options().Addr)
	assert.Nil(t, client.Options().TLSConfig, "a bare host:port must never imply TLS")
}

// TestNewRedisClient_ParsesRedisURL is the managed-provider shape: a
// redis:// connection string (Render's internal Key Value address, or any
// provider that hands out a URL instead of host:port).
func TestNewRedisClient_ParsesRedisURL(t *testing.T) {
	client := platform.NewRedisClient("redis://user:pass@localhost:6380/2")
	require.NotNil(t, client)
	opts := client.Options()
	assert.Equal(t, "localhost:6380", opts.Addr)
	assert.Equal(t, "pass", opts.Password)
	assert.Equal(t, 2, opts.DB)
	assert.Nil(t, opts.TLSConfig, "redis:// (not rediss://) must not enable TLS")
}

// TestNewRedisClient_RedissURLEnablesTLS is the scheme that gates TLS: a
// provider's internet-reachable endpoint. Confirms the client would actually
// attempt a TLS handshake rather than silently downgrading to plaintext.
func TestNewRedisClient_RedissURLEnablesTLS(t *testing.T) {
	client := platform.NewRedisClient("rediss://localhost:6380")
	require.NotNil(t, client)
	assert.NotNil(t, client.Options().TLSConfig, "rediss:// must enable TLS")
}

// TestNewRedisClient_MalformedURLFailsFastAtBoot: a bad REDIS_ADDR that looks
// like a URL should surface immediately rather than degrade the limiter
// mysteriously on the first request.
func TestNewRedisClient_MalformedURLFailsFastAtBoot(t *testing.T) {
	client := platform.NewRedisClient("redis://[::not-a-valid-url")
	require.NotNil(t, client, "must still return a usable (degraded) client, never nil")
	_ = client.Close()
}

// TestNewRedisClient_TimeoutsAppliedRegardlessOfShape guards against the
// URL-parsing branch silently dropping the fail-open timeout budget that
// internal/ratelimit's circuit breaker depends on.
func TestNewRedisClient_TimeoutsAppliedRegardlessOfShape(t *testing.T) {
	for _, addr := range []string{"localhost:6380", "redis://localhost:6380"} {
		client := platform.NewRedisClient(addr)
		opts := client.Options()
		assert.NotZero(t, opts.DialTimeout, "addr=%s", addr)
		assert.NotZero(t, opts.ReadTimeout, "addr=%s", addr)
		assert.NotZero(t, opts.WriteTimeout, "addr=%s", addr)
		assert.Equal(t, 20, opts.PoolSize, "addr=%s", addr)
	}
}
