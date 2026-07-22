package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" as a database/sql driver, for goose only
	"github.com/pressly/goose/v3"

	"github.com/D-Atharv/feature-flag-service/internal/config"
	"github.com/D-Atharv/feature-flag-service/migrations"
)

// Set via -ldflags at build time (see Dockerfile); "dev"/"unknown" locally.
var (
	version   = "dev"
	gitSHA    = "unknown"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: migrate <up|status|seed>")
	}
	cmd := os.Args[1]

	//nolint:gosec // cmd is a CLI arg from a trusted operator/CI, not network input; %q escapes control chars
	log.Printf("migrate %s (sha=%s built=%s) running %q", version, gitSHA, buildTime, cmd)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	switch cmd {
	case "up":
		if err := goose.Up(db, "."); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		log.Println("migrations applied")
	case "status":
		if err := goose.Status(db, "."); err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
	case "seed":
		if err := runSeed(context.Background(), db); err != nil {
			return fmt.Errorf("migrate seed: %w", err)
		}
	default:
		return fmt.Errorf("unknown command %q; usage: migrate <up|status|seed>", cmd)
	}

	return nil
}
