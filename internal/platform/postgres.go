// Package platform wires up shared infrastructure clients — Postgres pool,
// Redis client, health checks — used across the service.
package platform

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPostgresPool connects and pings before returning, so a misconfigured
// DATABASE_URL fails at startup rather than on the first request.
func NewPostgresPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return pool, nil
}

// NewListenerConnector returns a dialer for a dedicated connection.
// LISTEN holds a connection for the process's life, so it gets its own rather
// than one hijacked out of the pool.
func NewListenerConnector(databaseURL string) func(context.Context) (*pgx.Conn, error) {
	return func(ctx context.Context) (*pgx.Conn, error) {
		conn, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			return nil, fmt.Errorf("connect listener: %w", err)
		}
		return conn, nil
	}
}
