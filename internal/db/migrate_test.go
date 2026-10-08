package db_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/giska1923/GanymedServer/internal/db"
	"github.com/giska1923/GanymedServer/internal/db/dbtest"
)

var discard = slog.New(slog.DiscardHandler)

func TestMigrateRejectsBadMigrationSets(t *testing.T) {
	// load() runs before any database access, so these need no database: the pool is never used.
	cases := map[string]fstest.MapFS{
		"gap":       {"0001_a.sql": {Data: []byte("SELECT 1")}, "0003_c.sql": {Data: []byte("SELECT 1")}},
		"duplicate": {"0001_a.sql": {Data: []byte("SELECT 1")}, "0001_b.sql": {Data: []byte("SELECT 1")}},
		"bad name":  {"1_a.sql": {Data: []byte("SELECT 1")}},
		"empty":     {},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if err := db.Migrate(context.Background(), nil, fsys, discard); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

var twoMigrations = fstest.MapFS{
	"0001_first.sql":  {Data: []byte("CREATE TABLE a (id int); CREATE TABLE b (id int);")},
	"0002_second.sql": {Data: []byte("ALTER TABLE a ADD COLUMN name text;")},
}

// Two processes starting at once must not both apply a migration. Without the advisory lock the
// second CREATE TABLE fails, or worse, a non-idempotent migration applies twice.
func TestMigrateConcurrentRunsApplyOnce(t *testing.T) {
	first, cfg := dbtest.NewUnmigrated(t)
	second, err := db.OpenConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	var wg sync.WaitGroup
	pools := []*pgxpool.Pool{first, second}
	errs := make([]error, len(pools))
	for i, pool := range pools {
		wg.Go(func() { errs[i] = db.Migrate(context.Background(), pool, twoMigrations, discard) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}

	var n int
	if err := first.QueryRow(context.Background(), "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("schema_migrations has %d rows, want 2", n)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	pool, _ := dbtest.NewUnmigrated(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool, twoMigrations, discard); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES (3, 'from_the_future')"); err != nil {
		t.Fatal(err)
	}
	err := db.Migrate(ctx, pool, twoMigrations, discard)
	if err == nil || !strings.Contains(err.Error(), "newer schema") {
		t.Fatalf("got %v, want a refusal to run against a newer schema", err)
	}
}

func TestMigrateFailedMigrationLeavesNoTrace(t *testing.T) {
	pool, _ := dbtest.NewUnmigrated(t)
	ctx := context.Background()
	broken := fstest.MapFS{
		"0001_ok.sql":     {Data: []byte("CREATE TABLE ok (id int);")},
		"0002_broken.sql": {Data: []byte("CREATE TABLE half (id int); SELECT no_such_function();")},
	}
	if err := db.Migrate(ctx, pool, broken, discard); err == nil {
		t.Fatal("expected the broken migration to fail")
	}

	var version int
	if err := pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("recorded version %d, want 1", version)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('half') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("table from the failed migration exists: its transaction did not roll back")
	}
}
