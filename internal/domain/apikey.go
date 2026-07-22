package domain

import "time"

// APIKey authenticates and rate-limits a caller. The raw key is never
// stored — only its hash.
type APIKey struct {
	ID             string
	Name           string
	KeyHash        []byte // sha256(raw)
	KeyPrefix      string // e.g. "lo_live_a1b2", display only
	IsAdmin        bool
	RateLimitRPS   float64
	RateLimitBurst int
	Active         bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time // nil until first use
}
