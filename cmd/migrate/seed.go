package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

type seedKey struct {
	name         string
	isAdmin      bool
	rateLimitRPS float64
	burst        int
}

var seedKeys = []seedKey{
	{name: "admin", isAdmin: true, rateLimitRPS: 100, burst: 100},
	{name: "demo", isAdmin: false, rateLimitRPS: 10.0 / 60.0, burst: 10},
	{name: "loadtest", isAdmin: false, rateLimitRPS: 50000, burst: 50000},
}

// runSeed prints raw key values once — they're never stored, only their
// hash. Re-running replaces existing keys by name rather than duplicating.
func runSeed(ctx context.Context, db *sql.DB) error {
	fmt.Println()
	for _, sk := range seedKeys {
		raw, hash, prefix, err := generateAPIKey()
		if err != nil {
			return fmt.Errorf("generate %s key: %w", sk.name, err)
		}

		if _, err := db.ExecContext(ctx, `DELETE FROM api_keys WHERE name = $1`, sk.name); err != nil {
			return fmt.Errorf("clear existing %s key: %w", sk.name, err)
		}

		const q = `
			INSERT INTO api_keys (name, key_hash, key_prefix, is_admin, rate_limit_rps, rate_limit_burst)
			VALUES ($1, $2, $3, $4, $5, $6)`
		if _, err := db.ExecContext(ctx, q, sk.name, hash, prefix, sk.isAdmin, sk.rateLimitRPS, sk.burst); err != nil {
			return fmt.Errorf("insert %s key: %w", sk.name, err)
		}

		fmt.Printf("  %-10s %s\n", sk.name, raw)
	}
	fmt.Println("\nraw values above are shown once — save them now.")
	return nil
}

func generateAPIKey() (raw string, hash []byte, prefix string, err error) {
	buf := make([]byte, 32) // 256 bits
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("read random bytes: %w", err)
	}
	raw = "lo_live_" + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], raw[:12], nil
}
