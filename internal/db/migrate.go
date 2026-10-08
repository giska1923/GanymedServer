package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateLockKey is the pg_advisory_lock key that serializes migration runs. Any constant works;
// it only has to be the same in every process and unused by anything else.
const migrateLockKey int64 = 0x47616e796d6564 // "Ganymed"

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies every migration in fsys that the database has not seen, in version order.
//
// Rules, each of which exists to rule out a specific failure:
//
//   - One session-level advisory lock around the whole run, held on a single connection, so two
//     replicas starting together cannot both apply 0002. The second blocks, then finds nothing
//     left to do. A session lock rather than a transaction lock, because the run spans several
//     transactions.
//   - One transaction per migration, recording its version in the same transaction, so a
//     failed migration leaves no trace and the next start retries it.
//   - Forward-only. A mistake is fixed by a new migration, never by editing an applied one.
//   - A database that has applied a version this binary does not have is refused: an older
//     binary must not run against a newer schema.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	migrations, err := load(fsys)
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Background context: the unlock must run even when ctx is what failed. If it cannot run
		// (the connection is gone), Postgres drops session locks with the session anyway.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer     PRIMARY KEY,
		name       text        NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn.Conn())
	if err != nil {
		return err
	}

	known := make(map[int]bool, len(migrations))
	for _, m := range migrations {
		known[m.version] = true
	}
	for v := range applied {
		if !known[v] {
			return fmt.Errorf("database has migration %04d, which this binary does not know: refusing to run against a newer schema", v)
		}
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments, so pgx uses the simple protocol, which accepts a file of several
			// statements in one call.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
		}
		log.Info("migration applied", "version", m.version, "name", m.name)
	}
	return nil
}

func load(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationName.FindStringSubmatch(e.Name())
		if match == nil {
			if strings.HasSuffix(e.Name(), ".sql") {
				return nil, fmt.Errorf("migration %q does not match NNNN_name.sql", e.Name())
			}
			continue
		}
		version, _ := strconv.Atoi(match[1])
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %04d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: match[2], sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })

	// Gaps are refused: a missing 0003 is almost always a file that did not get committed.
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must run 0001, 0002, ... without gaps; found %04d at position %d", m.version, i+1)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no migrations found")
	}
	return out, nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	out := make(map[int]bool, len(versions))
	for _, v := range versions {
		out[v] = true
	}
	return out, nil
}
