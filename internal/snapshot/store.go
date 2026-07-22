// Package snapshot holds every flag in process memory so /evaluate issues no
// database query. Postgres can be down and evaluations keep serving.
package snapshot

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/D-Atharv/feature-flag-service/internal/domain"
)

// key identifies a flag. A struct rather than a joined string: no delimiter to
// collide with a flag key.
type key struct {
	flag string
	env  string
}

// version is one immutable snapshot: the flags and when they were loaded.
// Kept behind a single pointer so that the count and timestamp always describe
// the map they came with — three separate atomics could be read mid-swap and
// report a count from one load with a timestamp from another.
type version struct {
	flags map[key]domain.Flag
	at    time.Time
}

// Store is a read-optimised flag cache.
//
// Readers take no lock. The whole version is swapped by an atomic pointer, so
// a reader sees either the old one or the new one, never a half-built one.
type Store struct {
	current atomic.Pointer[version]
}

func New() *Store { return &Store{} }

// GetByKeyEnv implements evaluation.FlagSource. One map lookup, no I/O.
//
// Both error returns are bare sentinels. Wrapping them with the key and
// environment would allocate a formatted string on every miss, on the one path
// this whole package exists to keep fast — and nothing reads the message: a
// miss becomes a FLAG_NOT_FOUND result, not a logged error.
func (s *Store) GetByKeyEnv(_ context.Context, flagKey, env string) (domain.Flag, error) {
	v := s.current.Load()
	if v == nil {
		// Not ErrNotFound: that would answer 200 {"enabled":false,
		// "reason":"FLAG_NOT_FOUND"} for every flag until the first load
		// lands — every feature silently off, with a success status.
		return domain.Flag{}, domain.ErrUnavailable
	}

	f, ok := v.flags[key{flag: flagKey, env: env}]
	if !ok {
		return domain.Flag{}, domain.ErrNotFound
	}
	return f, nil
}

// Replace swaps in a whole new snapshot.
//
// Built fresh rather than merged: merging would leave a deleted flag in the
// snapshot forever, still serving as if it existed.
func (s *Store) Replace(flags []domain.Flag) {
	m := make(map[key]domain.Flag, len(flags))
	for _, f := range flags {
		m[key{flag: f.Key, env: f.Environment}] = f
	}
	s.current.Store(&version{flags: m, at: time.Now()})
}

// Loaded reports whether a first load has succeeded. /readyz gates on this.
func (s *Store) Loaded() bool { return s.current.Load() != nil }

// Len is the flag count in the current snapshot.
func (s *Store) Len() int {
	if v := s.current.Load(); v != nil {
		return len(v.flags)
	}
	return 0
}

// LastRefresh is when the snapshot was last replaced; zero if never.
func (s *Store) LastRefresh() time.Time {
	if v := s.current.Load(); v != nil {
		return v.at
	}
	return time.Time{}
}
