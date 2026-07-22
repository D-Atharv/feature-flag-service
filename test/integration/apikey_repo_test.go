package integration_test

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	pgstore "github.com/D-Atharv/feature-flag-service/internal/store/postgres"
)

func TestAPIKeyRepo_GetByHash(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewAPIKeyRepo(pool)
	ctx := context.Background()

	sum := sha256.Sum256([]byte("integration-test-raw-key"))
	hash := sum[:]

	name := "integration-test-key"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM api_keys WHERE name = $1", name) })

	_, err := pool.Exec(ctx, `
		INSERT INTO api_keys (name, key_hash, key_prefix, is_admin, rate_limit_rps, rate_limit_burst)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		name, hash, "lo_live_test", true, 42.0, 7)
	require.NoError(t, err)

	got, err := repo.GetByHash(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, name, got.Name)
	assert.True(t, got.IsAdmin)
	assert.InDelta(t, 42.0, got.RateLimitRPS, 0.001)
	assert.Equal(t, 7, got.RateLimitBurst)
	assert.Nil(t, got.LastUsedAt)
}

func TestAPIKeyRepo_GetByHash_NotFound(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewAPIKeyRepo(pool)

	sum := sha256.Sum256([]byte("no-such-key"))
	_, err := repo.GetByHash(context.Background(), sum[:])
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAPIKeyRepo_GetByHash_InactiveTreatedAsNotFound(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewAPIKeyRepo(pool)
	ctx := context.Background()

	sum := sha256.Sum256([]byte("integration-inactive-key"))
	hash := sum[:]

	name := "integration-inactive-test-key"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM api_keys WHERE name = $1", name) })

	_, err := pool.Exec(ctx, `
		INSERT INTO api_keys (name, key_hash, key_prefix, is_admin, rate_limit_rps, rate_limit_burst, active)
		VALUES ($1, $2, $3, $4, $5, $6, false)`,
		name, hash, "lo_live_test", false, 1.0, 1)
	require.NoError(t, err)

	_, err = repo.GetByHash(ctx, hash)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
