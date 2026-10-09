// Package db owns the Postgres connection pool and the schema migrator.
//
// It deliberately knows nothing about any module's tables. Modules receive the pool and write
// their own SQL; see the module rule in docs/history/BACKEND.md.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a pool and proves it can reach the database before returning, so a bad URL fails
// startup instead of the first request. pgxpool connects lazily; Ping forces one connection.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// Not wrapped with the URL: it carries the password.
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	return OpenConfig(ctx, cfg)
}

// OpenConfig is Open for a caller that needs to adjust the config first, which is how tests pin
// search_path to a per-test schema.
func OpenConfig(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
