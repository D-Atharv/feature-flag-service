package integration_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	pgstore "github.com/D-Atharv/feature-flag-service/internal/store/postgres"
)

// testPool connects to a real Postgres — no mocks in the data layer. Reads
// TEST_DATABASE_URL so it works both in CI (a bare "localhost" GH Actions
// service) and locally against docker-compose, whose published port varies
// by machine (see docker-compose.yml's port-conflict comment).
// ptr is for the optional Update fields: nil means "leave this column alone",
// which is what makes PATCH semantics distinguishable from "set it to false".
func ptr[T any](v T) *T { return &v }

// Writes here pass an empty actor id, stored as NULL in flag_audit — these are
// repository-level tests with no authenticated caller behind them.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://postgres:postgres@localhost:5433/feature_flags?sslmode=disable"
	}

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestFlagRepo_CreateGetUpdateDelete(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key, env := "integration-crud-flag", "dev"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key) })

	created, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env, Enabled: true, RolloutPercentage: 50}, "")
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, 1, created.Version)

	got, err := repo.GetByKeyEnv(ctx, key, env)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	updated, err := repo.Update(ctx, key, env, got.Version, ptr(false), ptr(75), "")
	require.NoError(t, err)
	assert.False(t, updated.Enabled)
	assert.Equal(t, 75, updated.RolloutPercentage)
	assert.Equal(t, 2, updated.Version)
	assert.True(t, !updated.UpdatedAt.Before(created.UpdatedAt))

	require.NoError(t, repo.Delete(ctx, key, env, ""))

	_, err = repo.GetByKeyEnv(ctx, key, env)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestFlagRepo_Create_DuplicateKeyEnvironment_Conflict(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key := "integration-dup-flag"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key) })

	_, err := repo.Create(ctx, domain.Flag{Key: key, Environment: "dev"}, "")
	require.NoError(t, err)

	_, err = repo.Create(ctx, domain.Flag{Key: key, Environment: "dev"}, "")
	assert.ErrorIs(t, err, domain.ErrConflict)

	// Same key, different environment must succeed — that's the entire
	// point of UNIQUE (key, environment) instead of UNIQUE (key).
	_, err = repo.Create(ctx, domain.Flag{Key: key, Environment: "staging"}, "")
	assert.NoError(t, err)
}

func TestFlagRepo_Create_ConstraintViolations_InvalidInput(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	cases := []struct {
		name string
		flag domain.Flag
	}{
		{"unknown environment (FK)", domain.Flag{Key: "ci-bad-env", Environment: "nonexistent"}},
		{"rollout over 100 (CHECK)", domain.Flag{Key: "ci-bad-rollout-high", Environment: "dev", RolloutPercentage: 150}},
		{"rollout negative (CHECK)", domain.Flag{Key: "ci-bad-rollout-low", Environment: "dev", RolloutPercentage: -5}},
		{"uppercase key (CHECK)", domain.Flag{Key: "CI-Bad-Key", Environment: "dev"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", c.flag.Key) })

			_, err := repo.Create(ctx, c.flag, "")
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidInput)
			assert.NotErrorIs(t, err, domain.ErrConflict)
			assert.NotErrorIs(t, err, domain.ErrNotFound)
		})
	}
}

func TestFlagRepo_Update_RolloutOverRange_InvalidInput(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key, env := "ci-update-bad-rollout", "dev"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key) })

	created, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env}, "")
	require.NoError(t, err)

	_, err = repo.Update(ctx, key, env, created.Version, ptr(true), ptr(999), "")
	assert.ErrorIs(t, err, domain.ErrInvalidInput)
}

func TestFlagRepo_Update_VersionMismatch(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key, env := "integration-version-flag", "dev"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key) })

	created, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env}, "")
	require.NoError(t, err)

	_, err = repo.Update(ctx, key, env, created.Version+1, ptr(true), ptr(10), "")
	assert.ErrorIs(t, err, domain.ErrVersionMismatch)
}

func TestFlagRepo_Update_NotFound(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)

	_, err := repo.Update(context.Background(), "no-such-flag", "dev", 1, ptr(true), ptr(10), "")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestFlagRepo_Delete_NotFound(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)

	err := repo.Delete(context.Background(), "no-such-flag", "dev", "")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestFlagRepo_List(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	env := "integration-list-env"
	_, err := pool.Exec(ctx, "INSERT INTO environments (name) VALUES ($1) ON CONFLICT DO NOTHING", env)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM flags WHERE environment = $1", env)
		_, _ = pool.Exec(ctx, "DELETE FROM environments WHERE name = $1", env)
	})

	for _, key := range []string{"a-flag", "b-flag", "c-flag"} {
		_, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env}, "")
		require.NoError(t, err)
	}

	page1, err := repo.List(ctx, env, "", "", 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.Equal(t, "a-flag", page1[0].Key)
	assert.Equal(t, "b-flag", page1[1].Key)

	last := page1[len(page1)-1]
	page2, err := repo.List(ctx, env, last.Key, last.Environment, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Equal(t, "c-flag", page2[0].Key)
}
