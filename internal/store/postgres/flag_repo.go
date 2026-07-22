// Package postgres holds the pgxpool-backed repositories. Every query is
// parameterised — no string-built SQL, ever.
package postgres

import (
	"context"
	"encoding/json"
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

// wrapConstraintErr maps Postgres constraint codes to domain sentinels while
// keeping the original error in the chain for logging.
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

// scanRow scans one flags row into a domain.Flag.
func scanRow(row pgx.Row, f *domain.Flag) error {
	return row.Scan(&f.ID, &f.Key, &f.Environment, &f.Enabled, &f.RolloutPercentage,
		&f.Version, &f.CreatedAt, &f.UpdatedAt)
}

type FlagRepo struct {
	pool *pgxpool.Pool
}

func NewFlagRepo(pool *pgxpool.Pool) *FlagRepo {
	return &FlagRepo{pool: pool}
}

// GetByKeyEnv fetches a single flag by (key, environment).
func (r *FlagRepo) GetByKeyEnv(ctx context.Context, key, environment string) (domain.Flag, error) {
	const q = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags WHERE key = $1 AND environment = $2`

	var f domain.Flag
	err := scanRow(r.pool.QueryRow(ctx, q, key, environment), &f)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Flag{}, fmt.Errorf("get flag: %w", err)
	}
	return f, nil
}

// ListByKey returns all flags for a given key across every environment.
// Used by GET /flags/:key without ?environment=.
func (r *FlagRepo) ListByKey(ctx context.Context, key string) ([]domain.Flag, error) {
	const q = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags WHERE key = $1
		ORDER BY environment`

	rows, err := r.pool.Query(ctx, q, key)
	if err != nil {
		return nil, fmt.Errorf("list by key: %w", err)
	}
	defer rows.Close()

	var flags []domain.Flag
	for rows.Next() {
		var f domain.Flag
		if err := rows.Scan(&f.ID, &f.Key, &f.Environment, &f.Enabled, &f.RolloutPercentage,
			&f.Version, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan flag: %w", err)
		}
		flags = append(flags, f)
	}
	return flags, rows.Err()
}

// List is keyset-paginated, not OFFSET.
// Pass afterKey/afterEnv as "" for the first page.
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
		if err := rows.Scan(&f.ID, &f.Key, &f.Environment, &f.Enabled, &f.RolloutPercentage,
			&f.Version, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan flag: %w", err)
		}
		flags = append(flags, f)
	}
	return flags, rows.Err()
}

// ListAll returns every flag in one query, for the in-memory snapshot.
// Unpaginated on purpose: a paged read can interleave with a write and yield a
// torn snapshot.
func (r *FlagRepo) ListAll(ctx context.Context) ([]domain.Flag, error) {
	const q = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list all flags: %w", err)
	}
	defer rows.Close()

	var flags []domain.Flag
	for rows.Next() {
		var f domain.Flag
		if err := scanRow(rows, &f); err != nil {
			return nil, fmt.Errorf("scan flag: %w", err)
		}
		flags = append(flags, f)
	}
	return flags, rows.Err()
}

// Create inserts a flag and writes a 'created' audit row in one transaction.
func (r *FlagRepo) Create(ctx context.Context, f domain.Flag, actorKeyID string) (domain.Flag, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Flag{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
		INSERT INTO flags (key, environment, enabled, rollout_percentage)
		VALUES ($1, $2, $3, $4)
		RETURNING id, version, created_at, updated_at`

	if err = tx.QueryRow(ctx, q, f.Key, f.Environment, f.Enabled, f.RolloutPercentage).
		Scan(&f.ID, &f.Version, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return domain.Flag{}, wrapConstraintErr(err, fmt.Sprintf("flag %s/%s", f.Key, f.Environment))
	}

	afterJSON, _ := json.Marshal(f)
	if err := insertAuditTx(ctx, tx, f.Key, f.Environment, "created", actorKeyID, nil, afterJSON); err != nil {
		return domain.Flag{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Flag{}, fmt.Errorf("commit: %w", err)
	}
	return f, nil
}

// Update updates a flag with optimistic concurrency and writes an 'updated' audit row.
// enabled and rollout may be nil to leave the current value unchanged.
func (r *FlagRepo) Update(ctx context.Context, key, environment string, expectedVersion int, enabled *bool, rollout *int, actorKeyID string) (domain.Flag, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Flag{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectQ = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags WHERE key = $1 AND environment = $2`

	var before domain.Flag
	if err = tx.QueryRow(ctx, selectQ, key, environment).
		Scan(&before.ID, &before.Key, &before.Environment, &before.Enabled,
			&before.RolloutPercentage, &before.Version, &before.CreatedAt, &before.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrNotFound)
	} else if err != nil {
		return domain.Flag{}, fmt.Errorf("fetch flag for update: %w", err)
	}

	if before.Version != expectedVersion {
		return domain.Flag{}, fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrVersionMismatch)
	}

	// Merge: apply only the fields the caller supplied.
	newEnabled := before.Enabled
	if enabled != nil {
		newEnabled = *enabled
	}
	newRollout := before.RolloutPercentage
	if rollout != nil {
		newRollout = *rollout
	}

	const updateQ = `
		UPDATE flags
		SET enabled = $1, rollout_percentage = $2, version = version + 1
		WHERE key = $3 AND environment = $4 AND version = $5
		RETURNING id, key, environment, enabled, rollout_percentage, version, created_at, updated_at`

	var after domain.Flag
	if err = tx.QueryRow(ctx, updateQ, newEnabled, newRollout, key, environment, expectedVersion).
		Scan(&after.ID, &after.Key, &after.Environment, &after.Enabled,
			&after.RolloutPercentage, &after.Version, &after.CreatedAt, &after.UpdatedAt); err != nil {
		return domain.Flag{}, wrapConstraintErr(err, fmt.Sprintf("update flag %s/%s", key, environment))
	}

	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if err := insertAuditTx(ctx, tx, key, environment, "updated", actorKeyID, beforeJSON, afterJSON); err != nil {
		return domain.Flag{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Flag{}, fmt.Errorf("commit: %w", err)
	}
	return after, nil
}

// Delete removes a flag and writes a 'deleted' audit row in one transaction.
func (r *FlagRepo) Delete(ctx context.Context, key, environment, actorKeyID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectQ = `
		SELECT id, key, environment, enabled, rollout_percentage, version, created_at, updated_at
		FROM flags WHERE key = $1 AND environment = $2`

	var snap domain.Flag
	if err = tx.QueryRow(ctx, selectQ, key, environment).
		Scan(&snap.ID, &snap.Key, &snap.Environment, &snap.Enabled,
			&snap.RolloutPercentage, &snap.Version, &snap.CreatedAt, &snap.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("flag %s/%s: %w", key, environment, domain.ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("fetch flag for delete: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM flags WHERE key = $1 AND environment = $2`, key, environment); err != nil {
		return fmt.Errorf("delete flag: %w", err)
	}

	beforeJSON, _ := json.Marshal(snap)
	if err := insertAuditTx(ctx, tx, key, environment, "deleted", actorKeyID, beforeJSON, nil); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
