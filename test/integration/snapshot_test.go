package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/snapshot"
	pgstore "github.com/D-Atharv/feature-flag-service/internal/store/postgres"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://postgres:postgres@localhost:5433/feature_flags?sslmode=disable"
}

// TestSnapshotPropagationUnderOneSecond exercises the real path: a write goes
// to Postgres, the AFTER trigger fires pg_notify, the listener wakes, and the
// snapshot swaps. The poll interval is set to an hour so that only NOTIFY can
// possibly explain the refresh.
func TestSnapshotPropagationUnderOneSecond(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key, env := "integration-propagation-flag", "dev"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM flag_audit WHERE flag_key = $1", key)
		_, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key)
	})

	store := snapshot.New()
	url := testDatabaseURL()
	refresher := snapshot.NewRefresher(store, repo).
		WithInterval(time.Hour).
		WithListener(func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, url) })

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); refresher.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("refresher did not stop")
		}
	})

	require.Eventually(t, store.Loaded, 5*time.Second, 10*time.Millisecond,
		"initial load must complete")

	_, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env, Enabled: true, RolloutPercentage: 100}, "")
	require.NoError(t, err)
	written := time.Now()

	require.Eventually(t, func() bool {
		_, err := store.GetByKeyEnv(ctx, key, env)
		return err == nil
	}, time.Second, 5*time.Millisecond, "NOTIFY must propagate to the snapshot within 1s")

	t.Logf("propagation took %s", time.Since(written))

	// A delete must leave the snapshot too — the refresher rebuilds the map
	// rather than merging into it.
	require.NoError(t, repo.Delete(ctx, key, env, ""))
	require.Eventually(t, func() bool {
		_, err := store.GetByKeyEnv(ctx, key, env)
		return err != nil
	}, time.Second, 5*time.Millisecond, "a deleted flag must leave the snapshot")
}

// TestListAllReturnsEveryEnvironment: the snapshot loader must not inherit the
// pagination or environment filter that List applies.
func TestListAllReturnsEveryEnvironment(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key := "integration-listall-flag"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM flag_audit WHERE flag_key = $1", key)
		_, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key)
	})

	for _, env := range []string{"dev", "staging", "prod"} {
		_, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env}, "")
		require.NoError(t, err)
	}

	all, err := repo.ListAll(ctx)
	require.NoError(t, err)

	var seen int
	for _, f := range all {
		if f.Key == key {
			seen++
		}
	}
	assert.Equal(t, 3, seen, "every environment's row must be in the snapshot load")
}

// TestListenerReconnectsAndFullRefreshes kills the listener's backend
// connection from the server side, then changes a flag while it is down.
//
// That notification is lost forever — LISTEN/NOTIFY has no delivery guarantee
// and no replay. The only thing that can close the gap is the full refresh the
// listener performs on every reconnect, which is what this asserts. The poll
// interval is an hour so the poller cannot be the explanation.
func TestListenerReconnectsAndFullRefreshes(t *testing.T) {
	pool := testPool(t)
	repo := pgstore.NewFlagRepo(pool)
	ctx := context.Background()

	key, env := "integration-reconnect-flag", "dev"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM flag_audit WHERE flag_key = $1", key)
		_, _ = pool.Exec(ctx, "DELETE FROM flags WHERE key = $1", key)
	})

	store := snapshot.New()
	url := testDatabaseURL()
	refresher := snapshot.NewRefresher(store, repo).
		WithInterval(time.Hour).
		WithListener(func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, url) })

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); refresher.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("refresher did not stop")
		}
	})

	require.Eventually(t, store.Loaded, 5*time.Second, 10*time.Millisecond)

	// Kill the listener's connection from the server side.
	var killed int64
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE query = 'LISTEN flags_changed' AND pid <> pg_backend_pid()
			) t`).Scan(&killed)
		return err == nil && killed > 0
	}, 5*time.Second, 50*time.Millisecond, "expected a LISTEN backend to terminate")

	// Change a flag while the listener is disconnected: this notification is
	// delivered to nobody.
	_, err := repo.Create(ctx, domain.Flag{Key: key, Environment: env, Enabled: true}, "")
	require.NoError(t, err)

	// Backoff is 1s, so allow room for the reconnect plus the refresh.
	require.Eventually(t, func() bool {
		_, err := store.GetByKeyEnv(ctx, key, env)
		return err == nil
	}, 15*time.Second, 100*time.Millisecond,
		"a change missed while disconnected must be picked up by the reconnect refresh")
}
