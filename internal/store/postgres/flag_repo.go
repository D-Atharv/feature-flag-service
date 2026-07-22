// Package postgres holds the pgxpool-backed repositories. Every query is
// parameterised — no string-built SQL, ever.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
)

// Postgres error codes this repo translates into domain sentinels.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

// wrapConstraintErr maps Postgres error codes to domain sentinels while
// keeping err in the chain, so the original detail still reaches a logger.
func wrapConstraintErr(err error, msg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case uniqueViolation:
			return fmt.Errorf("%s: %w: %w", msg, domain.ErrConflict, err)
		case foreignKeyViolation, checkViolation:
			return fmt.Errorf("%s: %w: %w", msg, domain.ErrInvalidInput, err)
		}
	}
	return fmt.Errorf("%s: %w", msg, err)
}

type FlagRepo struct {
	pool *pgxpool.Pool
}

func NewFlagRepo(pool *pgxpool.Pool) *FlagRepo {
	return &FlagRepo{pool: pool}
}

func (r *FlagRepo) Create(ctx context.Context, f domain.Flag) (domain.Flag, error) {
	const q = `
		INSERT INTO flags (key, environment, enabled, rollout_percentage)
		VALUES ($1, $2, $3, $4)
		RETURNING id, version, created_at, updated_at`

	err := r.pool.QueryRow(ctx, q, f.Key, f.Environment, f.Enabled, f.RolloutPercentage).
		Scan(&f.ID, &f.Version, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return domain.Flag{}, wrapConstraintErr(err, fmt.Sprintf("flag %s/%s", f.Key, f.Environment))
	}
	return f, nil
}

func (r *FlagRepo) GetByKeyEnv(ctx context.Context, key, environment string) (domain.Flag, error) {
	const q = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags WHERE key = $1 AND environment = $2`

	var f domain.Flag
	err := r.pool.QueryRow(ctx, q, key, environment).
		Scan(&f.ID, &f.Key, &f.Environment, &f.Enabled, &f.RolloutPercentage, &f.Version, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Flag{}, fmt.Errorf("get flag: %w", err)
	}
	return f, nil
}

// List is keyset-paginated, not OFFSET. Pass afterKey/afterEnv as "" for
// the first page — key can never legally be empty, so it sorts first.
func (r *FlagRepo) List(ctx context.Context, environment, afterKey, afterEnv string, limit int) ([]domain.Flag, error) {
	const q = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags
		WHERE ($1 = '' OR environment = $1)
		  AND (key, environment) > ($2, $3)
		ORDER BY key, environment
		LIMIT $4`

	rows, err := r.pool.Query(ctx, q, environment, afterKey, afterEnv, limit)
	if err != nil {
		return nil, fmt.Errorf("list flags: %w", err)
	}
	defer rows.Close()

	var flags []domain.Flag
	for rows.Next() {
		var f domain.Flag
		if err := rows.Scan(&f.ID, &f.Key, &f.Environment, &f.Enabled, &f.RolloutPercentage, &f.Version, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan flag: %w", err)
		}
		flags = append(flags, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list flags: %w", err)
	}
	return flags, nil
}

func (r *FlagRepo) Update(ctx context.Context, key, environment string, expectedVersion int, enabled bool, rolloutPercentage int) (domain.Flag, error) {
	const q = `
		UPDATE flags
		SET enabled = $1, rollout_percentage = $2, version = version + 1
		WHERE key = $3 AND environment = $4 AND version = $5
		RETURNING id, key, environment, enabled, rollout_percentage, version, created_at, updated_at`

	var f domain.Flag
	err := r.pool.QueryRow(ctx, q, enabled, rolloutPercentage, key, environment, expectedVersion).
		Scan(&f.ID, &f.Key, &f.Environment, &f.Enabled, &f.RolloutPercentage, &f.Version, &f.CreatedAt, &f.UpdatedAt)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Flag{}, wrapConstraintErr(err, fmt.Sprintf("flag %s/%s", key, environment))
	}

	// 0 rows means either "doesn't exist" or "version didn't match" —
	// disambiguate with a follow-up existence check.
	exists, existErr := r.exists(ctx, key, environment)
	if existErr != nil {
		return domain.Flag{}, fmt.Errorf("check existence: %w", existErr)
	}
	if exists {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrVersionMismatch)
	}
	return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrNotFound)
}

func (r *FlagRepo) Delete(ctx context.Context, key, environment string) error {
	const q = `DELETE FROM flags WHERE key = $1 AND environment = $2`

	tag, err := r.pool.Exec(ctx, q, key, environment)
	if err != nil {
		return fmt.Errorf("delete flag: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrNotFound)
	}
	return nil
}

func (r *FlagRepo) exists(ctx context.Context, key, environment string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM flags WHERE key = $1 AND environment = $2)`
	var exists bool
	err := r.pool.QueryRow(ctx, q, key, environment).Scan(&exists)
	return exists, err
}
