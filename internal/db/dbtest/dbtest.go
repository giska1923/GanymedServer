// Package dbtest gives a test its own migrated Postgres schema.
//
// Each call creates a fresh schema, points a pool's search_path at it, applies every migration, and
// drops the schema when the test ends. Tests therefore run in parallel against one database
// without seeing each other's rows, and the real database does the correctness work instead of
// a mock.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/giska1923/GanymedServer/internal/db"
	"github.com/giska1923/GanymedServer/migrations"
)

// New returns a pool on a fresh, migrated schema, or skips the test when TEST_DATABASE_URL is
// unset, so `go test ./...` still passes on a machine with no database.
func New(t testing.TB) *pgxpool.Pool {
	pool, _ := NewUnmigrated(t)
	if err := db.Migrate(context.Background(), pool, migrations.FS, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return pool
}

// NewUnmigrated returns a pool on a fresh, empty schema and the URL-level config to open more
// pools on the same schema, for tests of the migrator itself.
func NewUnmigrated(t testing.TB) (*pgxpool.Pool, *pgxpool.Config) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := db.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open schema pool: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		admin.Close()
	})
	return pool, cfg.Copy()
}
