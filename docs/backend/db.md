# Databases: Postgres and Redis

[`internal/db`](../../internal/db) owns the Postgres connection pool and the schema migrator.
[`internal/redisdb`](../../internal/redisdb/redisdb.go) owns the Redis client. Neither **knows
anything about any module's tables or keys**: modules receive the pool or the client and own
their own data. See the module rule in
[BACKEND.md](../history/BACKEND.md#the-module-rule-every-module-owns-its-tables), and
[Redis](#redis) below for which module owns which keys.

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
machine with no database. Redis tests do the same with `TEST_REDIS_URL` ([below](#testing-against-redis)).
To run everything:

```bash
docker compose up -d
set -a; . ./.env; set +a          # exports TEST_DATABASE_URL (:5433) and TEST_REDIS_URL (:6379)
go test -count=1 ./...
```

The race detector needs cgo, which is not set up on Windows, so it runs in a container on the
Compose network:

```bash
MSYS_NO_PATHCONV=1 docker run --rm --network ganymedserver_default \
  -v "D:/Projects/Go/GanymedServer:/src" -v gs-gomod:/go/pkg/mod -w /src \
  -e TEST_DATABASE_URL="postgres://ganymed:${POSTGRES_PASSWORD}@postgres:5432/ganymed?sslmode=disable" \
  -e TEST_REDIS_URL="redis://redis:6379/0" \
  golang:1.27 go test -race -count=1 ./...
```

## Local Postgres

Compose runs `postgres:18` and publishes it on **5433**, because the dev machine runs a native
PostgreSQL 17 on 5432. Inside the Compose network it is `postgres:5432`. Postgres 18's image
keeps its data in a versioned directory (`/var/lib/postgresql/18/docker`), so the volume mounts
`/var/lib/postgresql`, not the pre-18 `/var/lib/postgresql/data`.

## Redis

`redisdb.Open` parses `GS_REDIS_URL`, connects and pings once, like `db.Open`. The returned
`*redis.Client` (go-redis v9) is a connection pool, safe for concurrent use.

**What lives in Redis is ephemeral by design**: presence, parties, matchmaking tickets and
matches, the fleet's view of its servers, pub/sub. Nothing in it is the only copy of something
that must survive. A match *result* is the one thing in the match lifecycle that must, and it
goes to Postgres (`match_results`) before anything else happens to it. Compose runs `redis:8` with **no volume**, so
`docker compose down` loses it all, which is the honest test of that claim. Postgres remains
the source of truth for anything durable.

### Who owns which keys

| Key or channel | Owner | Holds |
|---|---|---|
| `presence:<account>` | realtime | `conn:<id>` / `away`, with a TTL ([realtime.md](realtime.md#presence)) |
| `session:<account>` | realtime | socket generation counter |
| channel `user:<account>` | realtime | pushes to that player |
| channel `realtime:replica:<id>` | realtime | keeps each replica's subscription alive |
| `party:<id>`, `party:<id>:members` | party | leader; members by join time |
| `member:<account>` | party | the account's party ID |
| `invites:<account>` | party | pending invites |
| `parties` | party | every live party ID, for the sweeper |
| `mm:ticket:*`, `mm:pool:*`, `mm:active:*`, `mm:last:*`, `mm:match:*`, `mm:pending`, `mm:running`, `mm:lease` | matchmaking | tickets, pools, the one-active-ticket-per-player guard, latest ticket, matches and their lifecycle, the director's lease ([matchmaking.md](matchmaking.md#keys)) |
| `fleet:agent:*`, `fleet:server:*`, `fleet:ready`, `fleet:cmds:*` | fleet | agent liveness, game servers and their states, allocation candidates, commands waiting for each agent ([fleet.md](fleet.md#keys)) |

The module rule applies to keys exactly as to tables: party never reads `presence:*`; it asks
realtime through an interface.

### Testing against Redis

[`internal/redisdb/redistest`](../../internal/redisdb/redistest/redistest.go) is the counterpart
of `dbtest`. Redis has no schemas, but it has 16 numbered databases. A test **claims** one of
1–15 with a lock key in database 0 (`SET NX` with a TTL, so a crashed test cannot hold it), flushes
it, and releases it at cleanup. A lock rather than an in-process counter, because `go test` runs
each package in its own process, in parallel.

One trap: **pub/sub ignores database numbers.** A channel is global to the server, so tests do
not collide on channels only because every account ID they use is a fresh random UUID.

### Local Redis

Compose publishes Redis on **6379** for host-side tests and the debugger
(`GS_REDIS_URL` / `TEST_REDIS_URL` in `.env`). Inside the network it is `redis:6379`.
