package snapshot_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/snapshot"
)

func flag(key, env string, enabled bool) domain.Flag {
	return domain.Flag{Key: key, Environment: env, Enabled: enabled, RolloutPercentage: 100}
}

// fakeLoader stands in for the repository.
type fakeLoader struct {
	mu    sync.Mutex
	flags []domain.Flag
	err   error
	calls int
}

func (l *fakeLoader) ListAll(context.Context) ([]domain.Flag, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return append([]domain.Flag(nil), l.flags...), nil
}

func (l *fakeLoader) set(flags []domain.Flag, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flags, l.err = flags, err
}

func (l *fakeLoader) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestUnloadedSnapshotIsUnavailableNotMissing is the one that matters most.
//
// If an unloaded snapshot answered ErrNotFound, /evaluate would return
// 200 {"enabled":false,"reason":"FLAG_NOT_FOUND"} for every flag in the
// system — every feature silently off, with a success status code, and nothing
// to alert on.
func TestUnloadedSnapshotIsUnavailableNotMissing(t *testing.T) {
	s := snapshot.New()

	_, err := s.GetByKeyEnv(context.Background(), "any", "prod")

	assert.ErrorIs(t, err, domain.ErrUnavailable)
	assert.NotErrorIs(t, err, domain.ErrNotFound, "not-loaded must never read as not-found")
	assert.False(t, s.Loaded())
}

func TestGetHitAndMiss(t *testing.T) {
	s := snapshot.New()
	s.Replace([]domain.Flag{flag("checkout", "prod", true), flag("checkout", "dev", false)})

	got, err := s.GetByKeyEnv(context.Background(), "checkout", "prod")
	require.NoError(t, err)
	assert.True(t, got.Enabled)

	// Same key, different environment: a distinct flag, per UNIQUE(key, environment).
	got, err = s.GetByKeyEnv(context.Background(), "checkout", "dev")
	require.NoError(t, err)
	assert.False(t, got.Enabled)

	_, err = s.GetByKeyEnv(context.Background(), "checkout", "staging")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	assert.True(t, s.Loaded())
	assert.Equal(t, 2, s.Len())
}

// TestReplaceDropsDeletedFlags: a refresher that merged into the existing map
// would keep serving a deleted flag forever.
func TestReplaceDropsDeletedFlags(t *testing.T) {
	s := snapshot.New()
	s.Replace([]domain.Flag{flag("old", "prod", true), flag("kept", "prod", true)})

	s.Replace([]domain.Flag{flag("kept", "prod", true)})

	_, err := s.GetByKeyEnv(context.Background(), "old", "prod")
	assert.ErrorIs(t, err, domain.ErrNotFound, "a deleted flag must leave the snapshot")
	assert.Equal(t, 1, s.Len())
}

func TestEmptySnapshotIsStillLoaded(t *testing.T) {
	s := snapshot.New()
	s.Replace(nil) // a database with no flags is a valid, loaded state

	assert.True(t, s.Loaded())
	_, err := s.GetByKeyEnv(context.Background(), "any", "prod")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.NotErrorIs(t, err, domain.ErrUnavailable)
}

// TestConcurrentReadsDuringReplace: readers must never see a half-built map or
// block on a writer.
func TestConcurrentReadsDuringReplace(t *testing.T) {
	s := snapshot.New()
	s.Replace([]domain.Flag{flag("hot", "prod", true)})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				f, err := s.GetByKeyEnv(ctx, "hot", "prod")
				if err == nil {
					assert.True(t, f.Enabled)
				}
			}
		}()
	}

	for range 200 {
		s.Replace([]domain.Flag{flag("hot", "prod", true), flag("noise", "prod", true)})
		s.Replace([]domain.Flag{flag("hot", "prod", true)})
	}

	cancel()
	wg.Wait()
}

func TestRefresherInitialLoad(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()

	require.NoError(t, snapshot.NewRefresher(s, loader).Refresh(context.Background(), "test"))

	assert.True(t, s.Loaded())
	assert.Equal(t, 1, s.Len())
}

// TestFailedRefreshKeepsPreviousSnapshot: stale beats wrong. Swapping in a
// failed read would report every flag as missing.
func TestFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()
	r := snapshot.NewRefresher(s, loader)

	require.NoError(t, r.Refresh(context.Background(), "initial"))

	loader.set(nil, errors.New("connection refused"))
	err := r.Refresh(context.Background(), "poll")

	require.Error(t, err)
	assert.Equal(t, 1, s.Len(), "the good snapshot must survive a failed reload")
	got, getErr := s.GetByKeyEnv(context.Background(), "a", "prod")
	require.NoError(t, getErr)
	assert.True(t, got.Enabled)
}

// TestPollerWorksWithoutListener: the poller alone is the correctness
// mechanism, so it must be sufficient on its own.
func TestPollerWorksWithoutListener(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()
	r := snapshot.NewRefresher(s, loader).WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	require.Eventually(t, s.Loaded, time.Second, 5*time.Millisecond)

	// A change made behind the service's back is still picked up.
	loader.set([]domain.Flag{flag("a", "prod", true), flag("b", "prod", true)}, nil)
	require.Eventually(t, func() bool { return s.Len() == 2 }, time.Second, 5*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	assert.Positive(t, loader.callCount())
}

// TestRunStopsOnContextCancel guards the shutdown path: a refresher that
// outlives its context leaks a goroutine and a connection.
func TestRunStopsOnContextCancel(t *testing.T) {
	loader := &fakeLoader{}
	r := snapshot.NewRefresher(snapshot.New(), loader).WithInterval(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
