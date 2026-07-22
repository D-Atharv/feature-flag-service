package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
)

type APIKeyRepo struct {
	pool *pgxpool.Pool
}

func NewAPIKeyRepo(pool *pgxpool.Pool) *APIKeyRepo {
	return &APIKeyRepo{pool: pool}
}

// GetByHash treats an inactive key as not found — auth has no use for
// "found but revoked" as a distinct outcome.
func (r *APIKeyRepo) GetByHash(ctx context.Context, hash []byte) (domain.APIKey, error) {
	const q = `
		SELECT id, name, key_hash, key_prefix, is_admin, rate_limit_rps, rate_limit_burst, active, created_at, last_used_at
		FROM api_keys WHERE key_hash = $1 AND active = true`

	var k domain.APIKey
	err := r.pool.QueryRow(ctx, q, hash).Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.IsAdmin,
		&k.RateLimitRPS, &k.RateLimitBurst, &k.Active, &k.CreatedAt, &k.LastUsedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.APIKey{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("get api key: %w", err)
	}
	return k, nil
}
