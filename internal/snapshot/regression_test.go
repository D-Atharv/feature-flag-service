package snapshot_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
	"github.com/D-Atharv/feature-flag-service/internal/snapshot"
)

// TestPrimeIsSynchronous: the caller must be able to load the snapshot BEFORE
// it starts serving. Run() loading in the background means every boot has a
// window where /evaluate answers 503 — small, but on every single start.
func TestPrimeIsSynchronous(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()

	err := snapshot.NewRefresher(s, loader).Prime(context.Background())

	require.NoError(t, err)
	assert.True(t, s.Loaded(), "Prime must return with the snapshot already usable")
	assert.Equal(t, 1, s.Len())
}

// TestPrimeReportsFailure: a caller that cannot reach the database still needs
// to start, but must be told so it can log it.
func TestPrimeReportsFailure(t *testing.T) {
	loader := &fakeLoader{err: errors.New("connection refused")}
	s := snapshot.New()

	err := snapshot.NewRefresher(s, loader).Prime(context.Background())

	require.Error(t, err)
	assert.False(t, s.Loaded())
}

// TestRunSelfHealsWhenNotPrimed: forgetting Prime must not leave the service
// unloaded until the first tick.
func TestRunSelfHealsWhenNotPrimed(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()
	r := snapshot.NewRefresher(s, loader).WithInterval(time.Hour) // no tick will save us

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	require.Eventually(t, s.Loaded, time.Second, 5*time.Millisecond,
		"Run must load when the store was never primed")
}

// TestRunDoesNotReloadWhenAlreadyPrimed: priming then running should not cause
// two loads at boot.
func TestRunDoesNotReloadWhenAlreadyPrimed(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()
	r := snapshot.NewRefresher(s, loader).WithInterval(time.Hour)

	require.NoError(t, r.Prime(context.Background()))
	require.Equal(t, 1, loader.callCount())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, loader.callCount(), "a primed store must not be reloaded at startup")
}

// TestListenerFailureDoesNotStopThePoller.
//
// The listener is an accelerator; the poller is the correctness mechanism. A
// database that accepts pool connections but refuses the LISTEN connection
// must degrade to "correct, slightly staler", not to "stale forever". This
// also pins that a failing connect backs off rather than spinning.
func TestListenerFailureDoesNotStopThePoller(t *testing.T) {
	loader := &fakeLoader{flags: []domain.Flag{flag("a", "prod", true)}}
	s := snapshot.New()

	var attempts atomic.Int64
	r := snapshot.NewRefresher(s, loader).
		WithInterval(20 * time.Millisecond).
		WithListener(func(context.Context) (*pgx.Conn, error) {
			attempts.Add(1)
			return nil, errors.New("connection refused")
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	require.Eventually(t, s.Loaded, time.Second, 5*time.Millisecond,
		"the poller must keep working while the listener cannot connect")

	loader.set([]domain.Flag{flag("a", "prod", true), flag("b", "prod", true)}, nil)
	require.Eventually(t, func() bool { return s.Len() == 2 }, time.Second, 5*time.Millisecond,
		"changes must still propagate via the poller alone")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	got := attempts.Load()
	require.Positive(t, got, "the listener must have tried to connect")
	assert.Less(t, got, int64(20), "a failing connect must back off, not spin: %d attempts", got)
}
