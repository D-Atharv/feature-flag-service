package domain

import "time"

// Flag is scoped to one environment; the same Key can exist in several.
type Flag struct {
	ID                string
	Key               string
	Environment       string
	Enabled           bool
	RolloutPercentage int
	Version           int // optimistic concurrency (If-Match / 412)
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
