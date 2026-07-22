package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
)

const (
	// DefaultInterval is the reconcile period.
	DefaultInterval = 30 * time.Second

	// debounce collapses a burst of notifications into one reload: a script
	// updating 100 flags should cause one refresh, not 100 table scans.
	debounce = 100 * time.Millisecond

	maxBackoff = 30 * time.Second

	// Matches the trigger in migrations/001_init.sql.
	channel = "flags_changed"
)

// Loader reads every flag. *store.FlagRepo satisfies it.
type Loader interface {
	ListAll(ctx context.Context) ([]domain.Flag, error)
}

// Refresher keeps a Store current by two independent means.
//
// The poller is what makes the snapshot correct. LISTEN/NOTIFY has no delivery
// guarantee — a notification raised while the listener is reconnecting is just
// lost — so if NOTIFY never fires the system is still correct, only staler.
type Refresher struct {
	store    *Store
	loader   Loader
	interval time.Duration

	// connect opens the dedicated LISTEN connection. Nil leaves the poller to
	// work alone, which it must be able to do.
	connect func(context.Context) (*pgx.Conn, error)
}

func NewRefresher(store *Store, loader Loader) *Refresher {
	registerMetrics()
	return &Refresher{store: store, loader: loader, interval: DefaultInterval}
}

// WithListener enables LISTEN/NOTIFY on its own connection.
//
// Its own, not one hijacked from the pool: Hijack() works but permanently
// removes that connection, quietly shrinking the pool for the process's life.
func (r *Refresher) WithListener(connect func(context.Context) (*pgx.Conn, error)) *Refresher {
	r.connect = connect
	return r
}

// WithInterval overrides the poll period. Tests only.
func (r *Refresher) WithInterval(d time.Duration) *Refresher {
	r.interval = d
	return r
}

// Refresh loads every flag and swaps the snapshot.
//
// On error the previous snapshot is kept: stale beats wrong, and swapping in a
// partial read would report flags as missing.
func (r *Refresher) Refresh(ctx context.Context, trigger string) error {
	flags, err := r.loader.ListAll(ctx)
	if err != nil {
		refreshErrors.Inc()
		return fmt.Errorf("load flags: %w", err)
	}

	r.store.Replace(flags)
	refreshTotal.WithLabelValues(trigger).Inc()
	flagsGauge.Set(float64(len(flags)))
	return nil
}

// Prime loads the snapshot synchronously, before the caller starts serving.
//
// Callers must not treat a failure as fatal — the service should still start,
// report not-ready, and let the poller catch up — but it must be able to load
// first, or every boot has a window where /evaluate answers 503.
func (r *Refresher) Prime(ctx context.Context) error {
	if err := r.Refresh(ctx, "initial"); err != nil {
		return err
	}
	slog.Info("flag snapshot loaded", "flags", r.store.Len())
	return nil
}

// Run blocks until ctx is cancelled, keeping the snapshot fresh.
func (r *Refresher) Run(ctx context.Context) {
	// Self-heal a caller that skipped Prime; a primed store is not reloaded.
	if !r.store.Loaded() {
		if err := r.Prime(ctx); err != nil {
			slog.Warn("initial flag snapshot load failed; not ready until it succeeds",
				"error", err.Error())
		}
	}

	notified := make(chan struct{}, 1)

	var wg sync.WaitGroup
	if r.connect != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.listen(ctx, notified)
		}()
	}
	defer wg.Wait()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			r.refreshLogged(ctx, "poll")

		case <-notified:
			select {
			case <-time.After(debounce): // collapse the burst
			case <-ctx.Done():
				return
			}
			drain(notified)
			r.refreshLogged(ctx, "notify")
		}
	}
}

func (r *Refresher) refreshLogged(ctx context.Context, trigger string) {
	if err := r.Refresh(ctx, trigger); err != nil && ctx.Err() == nil {
		slog.Warn("snapshot refresh failed; serving the previous snapshot",
			"trigger", trigger, "error", err.Error())
	}
}

// listen reconnects forever, signalling on every notification.
func (r *Refresher) listen(ctx context.Context, notified chan<- struct{}) {
	backoff := time.Second

	for ctx.Err() == nil {
		conn, err := r.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("flag listener connect failed", "error", err.Error(), "retry_in", backoff)
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}

		// Refresh on every (re)connect: notifications raised while
		// disconnected are gone, so the gap needs a full read to close it.
		signal(notified)
		backoff = time.Second

		err = r.consume(ctx, conn, notified)
		_ = conn.Close(context.WithoutCancel(ctx))

		if ctx.Err() != nil {
			return
		}
		slog.Warn("flag listener dropped; reconnecting", "error", err.Error())
		sleep(ctx, backoff)
		backoff = nextBackoff(backoff)
	}
}

// consume runs LISTEN and blocks until the connection breaks.
func (r *Refresher) consume(ctx context.Context, conn *pgx.Conn, notified chan<- struct{}) error {
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	slog.Info("listening for flag changes", "channel", channel)

	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}
		signal(notified)
	}
}

// signal is non-blocking: one pending refresh is enough, a second notification
// arriving before it is served adds nothing.
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func nextBackoff(d time.Duration) time.Duration {
	if d *= 2; d > maxBackoff {
		return maxBackoff
	}
	return d
}
