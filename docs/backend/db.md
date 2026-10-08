# Database: the pool and the migrator

[`internal/db`](../../internal/db) owns the connection pool and the schema migrator, and
**knows nothing about any module's tables**. Modules receive the pool and write their own SQL.
See the module rule in [BACKEND.md](../ToDo/BACKEND.md#the-module-rule-every-module-owns-its-tables).

## The pool

`db.Open` wraps `pgxpool` (`jackc/pgx/v5`) and **pings once** before returning. `pgxpool`
connects lazily, so without the ping a wrong URL would surface on the first request instead of
at startup. `db.OpenConfig` takes a parsed config for callers that adjust it first; the test
helper uses it to pin `search_path`.

## Migrations

Numbered SQL files in [`migrations/`](../../migrations), embedded into the binary with `embed.FS`
so a deployed backend needs nothing from disk. `db.Migrate` runs at every startup.

| Rule | What it prevents |
|---|---|
| Files are `NNNN_name.sql`, numbered from `0001` **without gaps or duplicates** | A migration that was never committed going unnoticed (a gap), or two branches both adding `0003` |
| One **session-level `pg_advisory_lock`**, on one connection, around the whole run | Two replicas starting together both applying `0002`. The second blocks, then finds nothing to do. Session-level, not transaction-level, because the run spans several transactions |
| One transaction per migration, recording its version **in the same transaction** | A half-applied migration. A failure leaves no trace, and the next start retries it |
| Forward-only | Editing history. A mistake is fixed by a new migration |
| A database with a version the binary does not know is **refused** | An older binary running against a newer schema |

A file with several statements runs in one `Exec`. With no arguments, pgx uses Postgres's simple
query protocol, which accepts multiple statements; the extended protocol would not.

**The one gap:** Postgres refuses some statements inside a transaction, such as
`CREATE INDEX CONCURRENTLY`. None are used. If one is ever needed, the migrator grows a
`-- no-transaction` marker for that file rather than a migration library.

Evidence (`internal/db/migrate_test.go`, all against real Postgres): concurrent runs apply once;
a failed migration leaves neither its table nor its version; a newer schema is refused; gap,
duplicate, misnamed and empty sets are rejected before any database access. A live check:
two backend binaries started at the same instant against an empty database both became ready,
and `schema_migrations` held one row.

## Timestamps and clocks

Every time comparison involving stored data uses **Postgres's clock** (`now()`), including token
expiry computed at insert time. Go's clock is used only for JWTs, which Postgres never sees.
One clock per comparison means process-to-database skew cannot make a token valid in one place
and expired in another.

## Testing against the database

[`internal/db/dbtest`](../../internal/db/dbtest/dbtest.go) gives each test a fresh schema named
`test_<random>`. It sets the pool's `search_path` to that schema, applies every migration, and
drops the schema with `CASCADE` when the test ends. Tests share one database but not one another's
rows, so they can run in parallel, and uniqueness, row locks and transactions are the real
thing, not a mock's approximation.

It reads `TEST_DATABASE_URL` and **skips** when it is unset, so `go test ./...` passes on a
machine with no database. To run everything:

```bash
docker compose up -d
set -a; . ./.env; set +a          # exports TEST_DATABASE_URL (Compose Postgres on :5433)
go test -count=1 ./...
```

The race detector needs cgo, which is not set up on Windows, so it runs in a container on the
Compose network:

```bash
MSYS_NO_PATHCONV=1 docker run --rm --network ganymedserver_default \
  -v "D:/Projects/Go/GanymedServer:/src" -v gs-gomod:/go/pkg/mod -w /src \
  -e TEST_DATABASE_URL="postgres://ganymed:${POSTGRES_PASSWORD}@postgres:5432/ganymed?sslmode=disable" \
  golang:1.27 go test -race -count=1 ./...
```

## Local Postgres

Compose runs `postgres:18` and publishes it on **5433**, because the dev machine runs a native
PostgreSQL 17 on 5432. Inside the Compose network it is `postgres:5432`. Postgres 18's image
keeps its data in a versioned directory (`/var/lib/postgresql/18/docker`), so the volume mounts
`/var/lib/postgresql`, not the pre-18 `/var/lib/postgresql/data`.
