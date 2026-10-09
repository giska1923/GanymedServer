# Design — the GanymedServer backend

**Status: complete.** B1–B5 are done, each tagged `api-v0.1` to `api-v0.5`, and this file
moved from `docs/ToDo/` to `docs/history/` on 2026-10-09. Live behaviour is in `docs/backend/`
([server](../backend/server.md), [db](../backend/db.md), [auth](../backend/auth.md),
[profile](../backend/profile.md), [leaderboard](../backend/leaderboard.md),
[realtime](../backend/realtime.md), [party](../backend/party.md),
[matchmaking](../backend/matchmaking.md), [fleet](../backend/fleet.md),
[stubserver](../backend/stubserver.md), [gscli](../backend/gscli.md)), and the contract is in
[`docs/api/`](../api/README.md). Each phase's execution notes are under it below: where the plan
was wrong is recorded there, not corrected in the plan. Follow-ups found along the way are in
[ToDo](../ToDo/README.md).

The backend for GanymedEngine: device identity and sessions, profiles and leaderboards, a
realtime gateway for presence and parties, ticket-based matchmaking, and a fleet agent that keeps
game-server processes warm and hands them out to matches with signed connect tokens.

The engine's half is planned in the engine repo at `docs/ToDo/ONLINE.md` (O0–O5). The phases
here line up with it:

| Backend phase | Unblocks engine phase |
|---|---|
| **B1** skeleton + identity | O2 identity |
| **B2** profiles + leaderboards | O3 leaderboards in the Proving Ground |
| **B3** realtime gateway | O4 push channel |
| **B4** matchmaking | nothing on its own; it is tested with the Go CLI |
| **B5** fleet, allocation, connect tokens, results | O5: the client's queue-and-join half (with B4), and `GanymedDedicated`'s hooks |

---

## Why a backend, for a game that is single-player

It is a learning project, and so is the engine. Nothing ships. So the question for every piece
here is not "does a player need this" but "does building it teach how game backends actually
work". The answer is yes for five things, and they are the five phases:

1. **Identity and sessions.** Stateless versus stateful tokens, refresh rotation and reuse
   detection, and why a monolith still wants a JWT.
2. **A transactional write path.** Idempotency keys, uniqueness constraints doing correctness
   work, and why a leaderboard does not need Redis at this size.
3. **A stateful service.** Long-lived connections, presence with a TTL, what a party does when
   a connection drops, and fan-out between two backend instances.
4. **Matchmaking.** A ticket pool, a match function, widening skill windows, and a single-writer
   loop guarded by a lease.
5. **Orchestration.** A warm pool of game servers, a lifecycle state machine, an agent that dials
   out, and connect tokens that let a game server admit a player without calling home.

## What it deliberately is not

- **Not microservices.** One Go binary with internal modules, plus two small binaries that have
  a real reason to be separate (see [Processes](#processes)).
- **Not production identity.** Device-ID login only. **Anyone who knows a device ID is that
  player.** Accepted, because there are no users to protect. No passwords, no email, no OAuth,
  no platform tickets.
- **No TLS.** Everything is on localhost. The engine plan records the same decision and what
  reopens it.
- **No rate limiting, abuse protection, GDPR tooling, multi-region or billing.**
- **No Kubernetes.** The fleet agent is Agones' model at the scale of one machine, not Agones.
- **No admin UI.** `psql`, `redis-cli`, the CLI and logs.

---

## How production game backends are shaped, and where this one diverges

Commercial game backends converge on the same parts, whether bought (PlayFab, Epic Online
Services, Steamworks) or self-hosted (Nakama, Pragma, AccelByte):

- **An identity service** that trusts a platform (Steam, PSN, Epic) to authenticate the human,
  and issues its own short-lived session token. The backend almost never holds passwords.
- **A stateless API tier** for request/response work (profiles, inventories, leaderboards,
  store), horizontally scaled behind a load balancer, with state in a database.
- **A stateful realtime tier**, WebSockets or a custom TCP protocol, for presence, chat,
  parties and notifications. It is the hard tier to scale, because a connection is pinned to
  one process. Nakama clusters its realtime nodes; others route through Redis or NATS pub/sub.
- **Matchmaking** as a separate pipeline. OpenMatch is the reference design: a *frontend* that
  accepts tickets, a *store* holding them, *match functions* that propose matches, and a
  *director* that takes proposals, allocates servers and tells players.
- **Fleet orchestration**, meaning Agones on Kubernetes, AWS GameLift or Unity Multiplay. It
  keeps a *fleet* of game-server processes, most of them `Ready` (booted, idle, waiting), and
  *allocates* one per match. A small SDK inside the game server reports lifecycle to a sidecar
  or agent.
- **Connect tokens.** The matchmaker's allocation result includes a token the client presents to
  the game server. netcode.io's design is the clearest public reference.

**Where GanymedServer diverges:**

- **The API tier and the realtime tier are one process.** They are separate packages with
  separate responsibilities, so they *could* be split, but at one developer's scale the split
  costs deployment complexity and teaches only networking between services. B3 runs **two
  replicas** of the one binary to learn the realtime tier's actual scaling problem (which
  process holds which connection) without splitting anything.
- **Matchmaking is OpenMatch's pipeline collapsed into one package**: frontend, pool, match
  function and director are functions and a loop, not services.
- **The fleet is one agent on the dev machine**, not a cluster. The lifecycle states,
  warm-pool logic and allocation protocol are Agones', so the model transfers.
- **Connect tokens are signed, not encrypted.** netcode.io encrypts a private portion so the
  client cannot read it. Ours contains nothing secret, so a signature is enough, and Ed25519 is in
  Go's standard library.

---

## Architecture

### Processes

```
 ┌──────────────┐  HTTP/JSON + WebSocket   ┌──────────────────────────── backend ───────────────────────────┐
 │ gscli (Go)   ├─────────────────────────▶│ server  (routing, middleware, auth check)                      │
 │ GanymedRuntime│                          │ auth · profile · leaderboard · realtime · matchmaking · fleet │
 └──────┬───────┘                          └──────┬──────────────────┬─────────────────▲──────────────────┘
        │                                         │                  │ (B3+)           │ HTTP poll, dials out
        │                                    Postgres              Redis                │
        │                                  (source of truth)   (presence, pub/sub,      │
        │                                                        tickets, leases)       │
        │                                                                     ┌─────────┴────────┐
        │                                                                     │ fleetagent (Go)  │ native, on the host
        │                                                                     └─────────┬────────┘
        │                                                                               │ spawns; localhost HTTP
        │                   UDP, connect token                                ┌─────────▼────────┐
        └────────────────────────────────────────────────────────────────────▶│ stubserver (Go)  │ → later GanymedDedicated
                                                                              └──────────────────┘
```

| Binary | Why it is its own process |
|---|---|
| `cmd/backend` | The service. Everything that is not below. |
| `cmd/fleetagent` | It must run **on the machine that hosts game servers**, because it spawns and watches them. The one real reason to split. |
| `cmd/stubserver` | Stands in for `GanymedDedicated` until the engine has one. It lets B5 be tested end to end with no engine involvement. |
| `cmd/gscli` | The test client. Faster to iterate with than launching the runtime, and scriptable for verification. |

### Packages

```
cmd/
  backend/  fleetagent/  stubserver/  gscli/      thin main.go: config, wiring, signal handling
internal/
  server/        http.Server, middleware (request id, logging, recover), health probes
  config/        env → typed config, validated at startup
  problem/       RFC 9457 problem details
  httpjson/      bounded JSON decoding and responses
  id/            random UUIDs for things Postgres never sees
  db/            pgx pool and the migrator
  redisdb/       the Redis client
  auth/          device login, access tokens, refresh rotation
  profile/       display name, skill rating, rating changes
  leaderboard/   scores, ranks, idempotency
  realtime/      WebSocket gateway, presence, cross-replica fan-out
  party/         parties
  matchmaking/   tickets, match function, director, allocation, results
  fleet/         agent registry, ready servers, claims (backend side)
  connecttoken/  sign (backend) and verify (stubserver; the reference for the engine)
migrations/      0001_auth.sql, 0002_… — numbered, embedded, forward-only
```

As built. The plan had parties inside `realtime/` (B3 split them out) and an `agent/` package
for `cmd/fleetagent` (B5 kept that code in `cmd/fleetagent`, its only user).

```
docs/api/        the contract (see below)
```

### The module rule: every module owns its tables

This is the rule that makes a monolith *modular* rather than just big.

- A module's tables are read and written **only by that module's package**. `leaderboard` never
  `JOIN`s `accounts`. It asks `auth` (or `profile`) through a Go function.
- Cross-module calls go through a small interface **declared by the caller** (the Go idiom: the
  consumer owns the interface). For example, `leaderboard` declares
  `type Names interface { DisplayNames(ctx, ids) (map[ID]string, error) }` and `profile`
  satisfies it without knowing it does.
- A transaction never spans two modules. If an operation needs two modules to agree, that is a
  design smell to write down, not to paper over with a shared `*pgx.Tx`.

**Why:** it is the difference between "could be split into services later" and "could not,
because 40 queries join across every table". It is also exactly the boundary a C++ engineer
already knows as "module owns its data": `PhysicsScene` does not reach into the renderer's
buffers.

**Cost:** a leaderboard page is one query for scores and one for names, instead of one join.
At this scale that is noise. At real scale the extra round trip is the normal price of service
boundaries, paid once here in miniature.

### Storage

| Store | Holds | Why there |
|---|---|---|
| **Postgres** | accounts, devices, refresh tokens, profiles and rating changes, scores, idempotency keys, match results | Anything that must survive a restart and be correct under concurrency. The source of truth. |
| **Redis** (from B3) | presence (TTL keys), pub/sub channels, parties, matchmaking tickets and live matches, the matchmaker's lease, the fleet's view of its servers | Ephemeral, or coordination between replicas. **Nothing in Redis is the only copy of something that matters.** |

Redis arrives in B3, not B1. See B2's decision on leaderboards.

### Contract files (`docs/api/`)

| File | Written in | Consumed by |
|---|---|---|
| `openapi.yaml` (OpenAPI 3.1) | B1, extended every phase | engine O1–O3, `gscli` |
| `realtime.md` (WebSocket envelope + message types) | B3 | engine O4 |
| `connect-token.md` | B5 | `stubserver`, engine O5 |
| `server-lifecycle.md` (agent ↔ game server) | B5 | `stubserver`, engine O5 |

**Each phase writes its contract before its code.** The spec is reviewed as a design artifact.
Then the handlers are written to it, and `gscli` is the first client to prove it. When a phase
closes, tag the repo `api-v0.<phase>` so the engine can link to an exact version.

### Cross-cutting conventions

- **HTTP**: Go's `net/http` `ServeMux` with method-and-path patterns (`"POST /v1/auth/device"`),
  no router library. Every route is under `/v1`.
- **Errors**: RFC 9457 problem details (`application/problem+json`, with `type`, `title`, `status`,
  `detail`), with a stable machine-readable `type` per error. The engine switches on `type`,
  never on the `title` string.
- **Server hygiene**: explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` and
  `IdleTimeout` on `http.Server`. A zero-valued `http.Server` has **no timeouts**, which is the
  classic Go footgun. Graceful shutdown via `signal.NotifyContext` → `Server.Shutdown(ctx)`, with
  a deadline.
- **Logging**: `log/slog`, JSON handler, one request ID per request carried in `context`.
  Tokens, device IDs and secrets are never logged (AGENTS.md).
- **Profiling**: `net/http/pprof` on a **separate admin listener** bound to localhost, never on
  the public mux.
- **Config**: environment variables, parsed once into a typed struct and validated at startup.
  A missing required value is a startup error, not a nil at first use.
- **IDs**: UUIDs generated by Postgres (`gen_random_uuid()`, built in since Postgres 13) for its
  own rows, and by `internal/id` (`crypto/rand`, v4) for things that never reach Postgres:
  sockets, parties, tickets, matches, servers. Random secrets (refresh tokens, result tokens)
  come from `crypto/rand`.

### Testing

- **Pure logic**, such as the match function, window widening, token sign/verify and refresh
  rotation rules, gets table-driven unit tests.
- **Anything whose correctness is the database's** (uniqueness, idempotency, rotation races)
  runs against the Compose Postgres, never a mock. Each test creates its own schema and sets
  `search_path` on its connection, so tests run in parallel without interfering. If
  `TEST_DATABASE_URL` is unset, these tests skip with a message rather than fail.
- **Time-dependent logic** was planned on `testing/synctest`. **None of it ended up there.**
  Pure logic takes `now` as a parameter (the match function, token verification), so time is
  just an input. Everything else waits on Redis TTLs and network I/O, which `synctest`'s
  virtual clock does not advance, so those tests use short real durations (a 400 ms lease, a
  1 ms `PEXPIRE`). See B4's execution notes.
- **`-race`** needs cgo on Windows. It runs in a `golang` container on the Compose network
  (the exact command is in [db.md](../backend/db.md)). Every phase ran it.

### Dependencies, all needing sign-off

| Module | Phase | For | Standard-library alternative, and why not |
|---|---|---|---|
| `github.com/jackc/pgx/v5` | B1 | Postgres driver + pool | none: `database/sql` needs a driver anyway. pgx's native API exposes Postgres types and `COPY`, and it is the de facto choice |
| `github.com/golang-jwt/jwt/v5` | B1 | access tokens | hand-rolled HS256 is ~60 lines, but JWT verification is a famous source of bugs (`alg: none`, algorithm confusion). Worth reading the library, not reimplementing it |
| `github.com/redis/go-redis/v9` | B3 | Redis client | none reasonable |
| `github.com/coder/websocket` | B3 | WebSocket server and client | the standard library has no WebSocket server. This is nhooyr's `context`-aware library, now maintained by Coder. `gorilla/websocket` is the older alternative |

**Not taken**: an ORM (hand-written SQL is the point), a migration library (the migrator is
~100 lines and worth writing; see B1), a router, `google/uuid` (Postgres generates IDs), and any
config library.

---

## Phase B1 — skeleton and identity — **DONE**

### Goal

`docker compose up` brings up Postgres and the backend. A client can sign in with a device ID,
receive an access token and a refresh token, call an authenticated route, and refresh. `gscli`
does all of it.

### Steps

1. `go mod init github.com/giska1923/GanymedServer`. `.gitignore`, `.env.example`, and
   `compose.yaml` with Postgres on a pinned major version (check the current one at B1; 18 was
   current when this was written) and a named volume.
2. `Dockerfile`: multi-stage. `golang:1.27` builds with `CGO_ENABLED=0`, and the result is copied
   into a minimal base (`gcr.io/distroless/static` or `scratch`).
3. `internal/config`, `internal/server` (mux, middleware, timeouts, graceful shutdown),
   `GET /healthz` (process up) and `GET /readyz` (database reachable). Two endpoints because an
   orchestrator treats "restart me" and "don't send me traffic" differently.
4. **The migrator** (`internal/db`): `migrations/*.sql` embedded with `embed.FS`, applied in
   order, each in its own transaction, recorded in a `schema_migrations` table, and the whole run
   serialized by `pg_advisory_lock` so two replicas starting at once cannot both migrate.
   Forward-only. A mistake is fixed by a new migration.
5. `docs/api/openapi.yaml`: the auth routes, **written before the handlers**.
6. `internal/auth`:
   - `POST /v1/auth/device` `{device_id}` → creates the account on first sight →
     `{access_token, refresh_token, expires_in, account_id}`.
   - `POST /v1/auth/refresh` `{refresh_token}` → a new pair. **The old refresh token is
     consumed.**
   - Middleware that validates the access token and puts the account ID in `context`.
   - `GET /v1/me` as the first authenticated route.
7. `cmd/gscli`: `gscli login --profile a`, `gscli me`, `gscli refresh`. The profile file mirrors
   the engine's `--profile=` (a device ID per profile).

### Decisions, with reasoning

**JWT access token plus an opaque refresh token: the standard hybrid.** In a monolith with a
database, a plain opaque session token looked up per request would work and be revocable
instantly. JWTs earn their place as **short-lived** access tokens (15 minutes), verified with
no database hit. Once B3 has two replicas and B5 has an agent, "any process can verify this
token alone" is a real property. The price of statelessness is that **a JWT cannot be revoked
before it expires**, which is why it is short-lived, and why the long-lived credential is
the refresh token, which *is* stateful and revocable. Learning that tradeoff is the reason this
phase exists.

- HS256 with one server secret. Asymmetric signing (RS256/EdDSA) matters when the verifier is not
  the issuer. Here both are this binary. Connect tokens (B5) are where asymmetric signing
  earns its keep.
- The verifier pins the algorithm. It never trusts the token's `alg` header.

**Refresh tokens rotate, with reuse detection.** This follows the OAuth 2.0 Security Best
Current Practice (RFC 9700). Each refresh token is single-use and belongs to a *family* started
at login. Presenting an already-consumed token means it was stolen or replayed, so **the whole
family is revoked** and the client must log in again. Stored as a SHA-256 hash, never raw, so a
database leak does not leak live tokens. The race to get right: two concurrent refreshes with the
same token. The consuming `UPDATE … WHERE token_hash = $1 AND consumed_at IS NULL` decides it.
Exactly one wins, and the database does the correctness work.

**Device ID in the request body, and never logged.** It is effectively a password here (see
[What it deliberately is not](#what-it-deliberately-is-not)).

### Risks

- **Clock skew** between processes is irrelevant now, because one process issues and verifies.
  It becomes relevant for connect tokens in B5, where a small leeway will be needed.
- **The migrator is hand-written.** Its failure mode is a half-applied migration. Per-migration
  transactions prevent it for everything except statements Postgres refuses to run inside a
  transaction (`CREATE INDEX CONCURRENTLY`). None are planned. If one is ever needed, the
  migrator grows a `-- no-transaction` marker rather than a new library.

### Verification

| Check | Pass when |
|---|---|
| Cold start | `docker compose up` from nothing → migrations applied, `/readyz` 200 |
| Two replicas start at once | exactly one applies migrations; the other waits on the advisory lock, then sees them applied |
| Login twice, same device | same `account_id` |
| Two `gscli` profiles | two accounts |
| Expired access token | 401 with a problem `type`; `gscli refresh` then succeeds |
| **Refresh reuse** | using a consumed refresh token revokes the family; the newest token of that family then fails too |
| **Concurrent refresh race** (test: N goroutines, one token) | exactly one success |
| Graceful shutdown | `SIGTERM` during a slow request lets it finish within the deadline; new connections are refused |
| Logs | grep the logs for a device ID and a token after a session: nothing |

### Execution notes

**Results.** Go 1.27.1, Postgres 18, 2026-09-26. `go vet` clean, `gofmt` clean. 20 test functions
(plus their table-driven subtests) pass against the Compose database, and **`-race` is clean** (run in a `golang:1.27` container on the
Compose network).

| Check | Result | Evidence |
|---|---|---|
| Cold start | **pass** | `docker compose down -v && up`: migration 0001 applied, `/readyz` 200 |
| Two replicas at once | **pass** | `TestMigrateConcurrentRunsApplyOnce` (deterministic). Live: two binaries started together on an empty database both became ready, and `schema_migrations` held one row. The live run cannot show that one *waited*, only that the result is right; the test is the proof |
| Same device twice | **pass** | same account (unit test and `gscli`) |
| Two profiles | **pass** | two accounts |
| Expired access token | **pass** | a host backend with `GS_ACCESS_TOKEN_TTL=2s`: `me` → 401 `token-expired` → `refresh` → `me` 200 |
| Refresh reuse | **pass** | `gscli`: the replayed token → 401, the legitimate newest → 401, a `WARN` naming the account and family. A new login works |
| Concurrent refresh race | **pass, with a consequence the plan missed**; see below | 10 goroutines, one token: one success |
| Concurrent first login | **pass** (added) | 16 goroutines, one device: one account ID, one `accounts` row |
| Failed migration | **pass** (added) | neither its table nor its version survives |
| Graceful shutdown | **pass** | `TestRunDrainsInFlightRequests`; `docker compose stop` → `shutting down` → `shutdown complete`, exit 0 |
| Logs | **pass** | debug-level run through login, `me`, refresh, reuse and `alg: none`; 6 issued secrets scanned for in 1.7 KB of logs; 0 found |

**Where the plan was wrong or incomplete, kept visible:**

- **"Concurrent refresh race: exactly one success" was true but not the whole story.** Strict
  reuse detection means the losers see a consumed token, which is indistinguishable from theft,
  and they **revoke the family**, including the winner's fresh token. So a client that refreshes
  twice at once logs itself out. That is RFC 9700's behaviour, and it was kept (no grace window) so
  that a client bug is visible. The engine plan (`ONLINE.md` O2) already required serialized
  refreshes; the test now pins down the reason.
- **Timestamps were not in the plan, and were wrong on first run.** pgx returns `timestamptz` in
  the process's local zone, so `/me` returned `Z` from the container and `+02:00` from a host
  binary: the same contract, two encodings. Responses now convert to UTC, and the contract says
  so.
- **The environment was different from what the plan assumed.** The dev machine runs a native
  PostgreSQL 17 on 5432, so Compose publishes on **5433**. Postgres 18's image moved its data
  directory (`/var/lib/postgresql/18/docker`), so the volume mounts `/var/lib/postgresql`.
- **The migrator is ~150 lines, not ~100**: the gap/duplicate checks and the newer-schema
  refusal were not in the estimate, and each earns its place.
- The first migration is `0001_auth.sql`, not `0001_init.sql`. Migrations are named for their
  module, which makes the module rule visible in the file list.
- **Not built:** the `net/http/pprof` admin listener from the conventions. Moved to
  [ToDo/README.md](../ToDo/README.md).

---

## Phase B2 — profiles and leaderboards — **DONE**

### Goal

A player has a display name. Scores are submitted idempotently and ranked, and a client can read
the top N and its own rank. This is what the engine's O3 needs.

### Steps

1. Contract first: add the profile and leaderboard routes to `openapi.yaml`.
2. `internal/profile`: a generated display name at account creation (`Player-7F3A`), and
   `PATCH /v1/me/profile` to change it. The skill rating column exists from here, unused until
   B4/B5.
3. `internal/leaderboard`:
   - `POST /v1/leaderboards/{board}/scores` `{score}` with header `Idempotency-Key: <uuid>` →
     `{rank, best}`.
   - `GET /v1/leaderboards/{board}?limit=N` → `[{rank, name, score}]`.
   - `GET /v1/leaderboards/{board}/me` → `{rank, best}`.
4. Idempotency: the key, account, route and stored response in one table with a unique
   constraint, written **in the same transaction** as the score. A repeat returns the stored
   response. Keys expire after 24 hours (a periodic `DELETE`, run by the backend itself).

### Decisions, with reasoning

**Leaderboards in Postgres, not Redis. This is the deliberate one.** "Leaderboards are a Redis
sorted set" is received wisdom, and it is right at scale: `ZADD`/`ZREVRANK` are O(log N), with no
disk. But:

- At this scale, one best-score row per player per board with an index on `(board, score DESC)`
  gives the top N as an index scan, and a player's rank as a `count(*)` of higher scores. That is
  O(N) in principle and microseconds at thousands of rows.
- Redis would add a second store to keep consistent with Postgres (which one is the truth after a
  crash?) for a performance problem that does not exist.

So B2 uses Postgres and **measures**: seed 100k and 1M scores, and time the rank query. That
number is the evidence. **Optional follow-up once Redis exists (B3):** move ranks to a `ZSET`
*derived* from Postgres, with a rebuild command, and compare. That teaches the "cache as a
derived index" pattern properly, with a measured reason, instead of by default.

**Best score per player, not every score.** A table of every submission is an event log, and it
is useful for anti-cheat and history. The board is a *projection* of it. Keep both: an insert
into `score_submissions`, and an upsert of `best_scores` with
`ON CONFLICT … DO UPDATE … WHERE excluded.score > best_scores.score`. The projection is then
rebuildable from the log.

**Idempotency lives in the database, not in memory.** An in-memory "seen keys" set breaks the
moment there are two replicas (B3), or a restart. The unique constraint makes a retry
structurally unable to double-count.

### Verification

| Check | Pass when |
|---|---|
| Same `Idempotency-Key` sent twice, including concurrently | one submission row; both responses identical |
| Same key, different body | 422 with a problem `type`: a key reused for a different request is a client bug |
| Lower score after a higher one | `best` unchanged; submission logged |
| **Rank query at 100k and 1M rows** | timings recorded here; if the 1M rank query is above a few milliseconds, that is the measured case for the Redis follow-up |
| Engine O3 against this | the Proving Ground's HUD shows the top five (engine repo verification) |

### Execution notes

**Results.** 2026-10-08. `go vet` and `gofmt` clean; 31 test functions pass against the Compose
database (11 new for B2: 8 leaderboard, 3 profile); **`-race` clean** in the container. Exercised end to
end with three `gscli` players.

| Check | Result | Evidence |
|---|---|---|
| Same key twice | **pass** | `TestSubmitSameKeyReplays`: one submission row; the retry answers the *original* standing even after another player overtook; `gscli -v` shows `Idempotent-Replayed: true` |
| Same key, concurrently | **pass** | `TestSubmitSameKeyConcurrently`: 10 at once; exactly one does the work, nine replay; one row |
| Same key, different body | **pass** | 422 `idempotency-key-reused`, for a different score and for a different board; another account may use the same key value |
| Lower score after higher | **pass** | `best` unchanged; three submissions logged for three requests |
| Ties | **pass** (added) | 100, 90, 90, 80 rank 1, 2, 2, 4; tied players listed by who got there first |
| **Rank at 100k / 1M** | **measured: 13 ms / 87 ms for last place** | the full table and plans are in [leaderboard.md](../backend/leaderboard.md#measured-100k-and-1m-players). Top 10 is constant (0.17 ms of server time at 1M) |
| Engine O3 | **not run**: the engine side (O0–O3) does not exist yet | |

**Where the plan was wrong or incomplete, kept visible:**

- **"Microseconds at thousands of rows" was right; the extrapolation was not.** The plan said the
  rank count is "O(N) in principle and microseconds" in practice. The plan used is exactly the
  intended one (`Index Only Scan`, `Heap Fetches: 0`), and it still costs about 0.1 µs per row
  above the player: 13 ms at 100k, 87 ms at 1M. Above the plan's own "a few milliseconds"
  threshold, so the Redis follow-up now has a measured reason. It is still a learning exercise,
  not a fix: the Proving Ground has a handful of players.
- **"A generated display name at account creation" would have broken the module rule.** The
  profile write would sit inside auth's transaction. Profiles are lazy instead: the default name
  is derived from the account ID, and a row exists only after a rename. See
  [profile.md](../backend/profile.md#lazy-profiles).
- **The skill-rating column was not added.** The plan put it here, "unused until B4/B5". A column
  nobody reads is the knob nobody turns, so it moved to B4, where it is first read.
- **Not in the plan, decided here:**
  - No foreign keys across modules ([profile.md](../backend/profile.md#no-cross-module-foreign-keys)).
  - Boards declared by migration, so an unknown board is a 404.
  - `MaxScore` = 2^53−1, and scores must be written as integers: the engine's Lua numbers are
    doubles, and `1234.0` is a 400. That is a constraint on the engine's JSON writer, recorded in
    the contract.
  - A concurrent duplicate *waits* rather than getting a 409.
  - `GET …/me` returns nulls instead of 404 for "no score yet".
- **Found in passing:** `encoding/json`'s type errors leaked Go type names to clients
  (`Go struct field .score of type int64`). `httpjson.Decode` now reports them in JSON terms.

---

## Phase B3 — the realtime gateway — **DONE**

### Goal

A signed-in client holds one WebSocket. The backend knows who is online, players can form a
party, and server-initiated messages reach a player **regardless of which replica holds their
connection**.

### Steps

1. Contract first: `docs/api/realtime.md`. It specifies an envelope
   `{ "type": "...", "id": "...", "payload": {...} }`, the list of message types, and the close
   codes.
2. Add Redis to Compose. Add `go-redis` and `coder/websocket` (sign-off).
3. `GET /v1/realtime`: an upgrade authenticated with `Authorization: Bearer <access token>`. The
   engine's client is native, so a header works (browsers cannot set one, which is why web
   clients put the token in a query string or first message).
4. Per connection: a read goroutine and a write goroutine, with a **bounded send queue**. A client
   that cannot keep up is disconnected, never allowed to grow memory. Ping/pong keepalive.
5. **Presence**: `SET presence:<account> <replica> EX 30`, refreshed on each pong. Online means
   the key exists. A crash of the replica expires everything it held within 30 seconds, with no
   cleanup code, which is why TTLs are the idiom.
6. **Fan-out**: each replica `SUBSCRIBE`s to `user:<account>` for every account it holds a
   connection for. Sending to a player is `PUBLISH user:<account> <msg>`, from any replica.
7. **Parties**: create, invite (push `party.invite`), accept, leave, kick, with the leader
   promoted on leave. State in a Redis hash per party. Parties are ephemeral by nature, so they
   live in Redis, not Postgres. `GET /v1/party` is the source of truth; pushes are nudges (see
   Decisions).
8. **Disconnect grace**: a dropped connection keeps its party seat for 30 seconds. Reconnecting
   inside the window resumes. Outside it, the player leaves and the party is notified.
9. **Two replicas**: Compose runs `backend-a` and `backend-b` on different ports. `gscli` connects
   clients to different replicas, so every cross-replica path is exercised without adding a
   load balancer.

### Decisions, with reasoning

**Redis pub/sub is at-most-once, and that decides the protocol.** A published message reaches
only subscribers connected *at that instant*; there is no buffer and no retry. So pushes are
**nudges** ("you have an invite") and the HTTP API is the **source of truth** ("list my
invites"). A client that reconnects re-fetches state over HTTP. The engine plan's O4 records
the same rule from the client side. The alternative is Redis Streams or a real broker with
acknowledgements. That is at-least-once delivery, idempotent consumers, consumer groups: a
bigger system, needed when a lost message is a lost purchase, not a lost "you were invited".

**Per-user channels over a routing table.** The alternative is a Redis map of
account → replica, plus one channel per replica, so a sender looks up the replica and publishes
to it. That is fewer channels, but it adds a lookup and a race: the player moves between the
lookup and the publish. Per-user channels let Redis do the routing. Redis handles channel counts
far beyond this project's.

**A bounded send queue, and slow clients are cut.** The classic realtime-server failure is one
stalled client whose unbounded queue exhausts memory for everyone. The same principle as the
engine's spawn cap: a guard, not a budget.

**Goroutine lifetime is owned by the connection's `context`.** Every goroutine a connection
starts exits when its context is cancelled: close, error or shutdown. The Go counterpart of the
engine's "a dropped Future cancels and waits". A goroutine leak is the Go equivalent of a
detached thread writing into freed memory, just slower to notice. Verified in B3 by counting
goroutines (`runtime.NumGoroutine`) before and after 1000 connect/disconnect cycles.

### Verification

| Check | Pass when |
|---|---|
| Presence | `gscli` online → key exists; kill `gscli` → gone within the TTL |
| **Cross-replica push** | A on `backend-a` invites B on `backend-b`; B receives `party.invite` |
| Replica crash | `docker kill backend-a` → its players' presence expires within 30 s; their parties see them leave after the grace |
| Reconnect inside grace | party seat kept, no leave broadcast |
| Slow client (test that never reads) | disconnected when its queue fills; other clients unaffected |
| **Goroutine leak** | the count returns to baseline after 1000 connect/disconnect cycles |
| `-race` | clean, in the container |

### Execution notes

**Results.** 2026-10-08. `go vet` and `gofmt` clean; 50 test functions pass against the Compose
stores (19 new: 11 realtime, 8 party); **`-race` clean** in the container. Exercised end to end
against the two Compose replicas with `gscli listen`.

| Check | Result | Evidence |
|---|---|---|
| Presence | **pass** | online → `away` the moment the client process is killed → offline 30 s later; `TestPresenceOnlineAwayOffline` |
| **Cross-replica push** | **pass** | Ana on A invites Ben listening on B: Ben gets `party.invite`. Ben accepts on B: Ana's socket on A gets `party.updated`. `TestCrossReplicaPush` |
| Replica crash | **pass** | `docker kill` of B with Ben's socket on it: Ben `online` to **t+24 s**, then `offline`; removed from the party at **t+30 s** by replica **A**'s sweeper; Ana nudged |
| Reconnect inside grace | **pass** | client killed, back after 9 s: `away` → `online`, seat kept, **0** pushes to the party; when left gone: removed at t+31 s, 1 push |
| Slow client | **pass**, after correcting the expectation (below) | 5000 × 4 KB pushes to a client that never reads: the server lets it go; the other socket still receives |
| **Goroutine leak** | **pass** | 8 goroutines before, **8** after 1000 socket cycles, five repeated runs |
| Supersede (added) | **pass** | Ben connects to A while on B: B's socket closed **4001**; A's gets his pushes |
| Graceful shutdown (added) | **pass** | `docker compose stop` of B: Ben's socket closed **1001**, Ben `away` (not offline), exit 0 |
| Server timeouts on sockets (added) | **pass** | `TestSocketOutlivesServerTimeouts`: 200 ms server timeouts, socket alive after 1 s |
| `-race` | **pass** | all 8 packages, in a `golang:1.27` container on the Compose network |

**Where the plan was wrong or incomplete, kept visible:**

- **"Grace" was described as its own mechanism; it cannot be.** The plan had presence (step 5)
  and a disconnect grace (step 8) as separate things. A grace *timer* lives in the replica that
  held the socket, and a crashed replica runs no timers. Grace is instead the presence key's
  third state (`away`, 30 s TTL), and a sweeper on **every** replica removes offline members. A
  crash and a clean disconnect then follow the same path. The verification row "parties see them
  leave after the grace" only passes because of this.
- **Two `net/http` facts the plan did not know about would have broken every socket:**
  - The server's `ReadTimeout`/`WriteTimeout` are connection deadlines that survive hijacking, so
    sockets would have died at 10 s. The gateway clears them.
  - `Shutdown` neither closes nor waits for hijacked connections. The gateway does both itself.
- **coder/websocket, three findings:**
  - Cancelling the context of a read or write closes the connection with no close frame. The
    first version tied the reader to the control context, and close codes became EOF.
  - Its `CloseRead` hangs **15 s** when a client sends data, because `Close` waits on the
    goroutine that called it. It was replaced by an own reader loop, and that close now takes
    0.15 s.
  - Hijacking works through `Unwrap` chains, so B1's `statusRecorder.Unwrap` was enough.
- **Superseding by connection ID was wrong; it needs a generation.** A late `session.replaced`
  from socket N closed the *newer* socket N+1. That surfaced as the leak test failing about
  half the time, and a goroutine dump showed a map entry with no goroutine behind it. Fixed with a
  per-account `INCR` generation. In the same bug, an attach interrupted part-way skipped cleanup
  (`detach` was deferred too late).
- **"Disconnected when its queue fills" is only half observable.** A client that has stopped
  reading has a full TCP window, so the 1008 close frame cannot reach it either. The write
  timeout drops the connection (1006). The contract and the test now say exactly that.
- **A data race found by reading, before `-race` ran:** `finish` wrote the close reason outside
  the `sync.Once` that `close` uses from the receive goroutine. Both now go through the `Once`.
- **Not in the plan, decided here:**
  - One socket per account; clients send no data; auth once, at upgrade.
  - Party mutations as Lua scripts; party size 4; invites expire after 5 min.
  - Compose keeps the service name `backend` (as `backend-a`) and adds `backend-b` on 8082,
    rather than renaming, so as not to churn every doc.
  - `/readyz` checks Redis too, through `server.PingFunc`.

---

## Phase B4 — matchmaking — **DONE**

### Goal

Players or parties submit tickets. The backend forms matches of the right size from compatible
tickets, widening its tolerance the longer a ticket waits, and exactly one replica runs the
matchmaker at a time. Without a fleet (B5), a formed match ends in state `matched`.

### Steps

1. Contract first: `POST /v1/matchmaking/tickets` `{mode}` (a party leader submits for the party),
   `GET /v1/matchmaking/tickets/{id}`, `DELETE …/{id}`, and pushes `match.found` and
   `ticket.failed`.
2. **Ticket store in Redis**: a hash per ticket and a sorted set per mode keyed by enqueue time.
   **Skill rating** arrives here, not in B2 as first planned: a rating column owned by the profile
   module, read by matchmaking through a profile method (never a join), written by B5's results.
3. **Ticket states**: `queued → matched → allocating → ready | failed`, plus `cancelled` from
   `queued`. The state machine is written into `docs/api/` because the client renders it.
4. **The match function**, which is pure: given the tickets in a mode's pool and "now", return
   the proposed matches. Rules for co-op, 2–4 players: parties stay whole, and a ticket's skill
   window is `base + rate × wait`, capped. Two tickets are compatible when each is inside the
   other's window.
5. **The director loop**: every second, take the pool, run the match function, and atomically
   move the matched tickets `queued → matched`. The atomic move is a Lua script or `WATCH`/`MULTI`
   in Redis, so a ticket cancelled mid-run cannot also be matched. Then (B5) allocate.
6. **Single writer via a lease**: the director runs only while holding
   `SET matchmaker:lease <replica> NX PX 5000`, renewed every second. If `backend-a` dies,
   `backend-b` takes over within 5 seconds.

### Decisions, with reasoning

**Why a lease, not "run the matchmaker in one replica by config".** A config flag makes the
second replica correct only as long as nobody sets it on both, and it gives no failover. The lease is how
real systems pick a single writer without a coordinator. Its known flaw is worth learning: a
paused process (a GC pause, a stopped VM) can believe it still holds a lease that has expired.
The textbook fix is a **fencing token**: an increasing number checked by the store on every
write. Here, the atomic `queued → matched` move is itself the fence: a stale director's move
finds the tickets no longer `queued` and does nothing. Say that in the code.

**The match function is pure and separate from the loop.** That is OpenMatch's split, and the
reason is testing. All the interesting logic (windows, parties, fairness) is tested as
`f(tickets, now) → matches`, with no Redis, no goroutines and no clock. The loop is dumb plumbing.

**Oldest-first and greedy, not optimal.** Globally optimal matching is an assignment problem.
Greedy from the oldest ticket is what most shipped matchmakers do, because wait time is what
players feel. Recorded so nobody "fixes" it into Hungarian-algorithm territory.

### Verification

| Check | Pass when |
|---|---|
| Match function table tests | parties never split; windows widen as specified; no ticket in two matches |
| Window widening | with `synctest`, a lone outlier ticket matches after the computed wait, not before |
| **Lease failover** | kill the lease-holding replica mid-queue; the other takes over within 5 s; no ticket matched twice |
| Cancel race | cancelling in the same second as the director runs results in cancelled **or** matched, never both |
| Load | 1000 tickets from `gscli` in one mode drain into matches of valid size; time recorded |

### Execution notes

**Results.** 2026-10-08. `go vet` and `gofmt` clean; 64 test functions pass (14 new: 13
matchmaking, 1 profile); **`-race` clean**. Exercised end to end against both Compose replicas.

| Check | Result | Evidence |
|---|---|---|
| Match function table tests | **pass** | full groups at once, partial after the fill wait, parties never split, oldest first, pairwise compatibility; plus 200 random pools checked for the invariants (no ticket twice, legal sizes, every pair compatible) |
| Window widening | **pass**, without `synctest` (below) | players 300 apart, queued 10 s apart: no match at t=29.9 s, match at t=30 s, exactly where the newer ticket's window reaches 300 |
| **Lease failover** | **pass** | tests (400 ms TTL): crash failover **400 ms**, graceful handover **53 ms**. Compose (5 s TTL): `docker kill` of the leader while a pair waited out its fill time; backend-b took the lease **~4.6 s** later and matched them on schedule |
| Cancel race | **pass**, after making the test prove it raced (below) | 200 races with a random 0–4 ms cancel delay: ~25 cancelled, ~175 matched, never both, never neither |
| No ticket matched twice (fence) | **pass** | two concurrent rounds over 40 tickets: each in exactly one match; the fence refused 3–10 proposals per run |
| Load | **pass** | `gscli load coop 1000`: queued in 354 ms, **drained in 1.66 s**, 250 matches of 4, none failed. Rounds: pool 300 in 167 ms, pool 700 in 227 ms |
| Party queueing (added) | **pass** | a member queueing: 403 `not-party-leader`; the leader queues both; queueing twice: 409 `already-queued`; a cancel nudges the other member; cancelling after the match: 409 `ticket-not-queued` |

**Where the plan was wrong or incomplete, kept visible:**

- **"With `synctest`, a lone outlier matches after the computed wait" did not need `synctest`.**
  The plan's own design made the match function pure with `now` as a parameter, so widening is
  tested by passing different `now` values, deterministically and with no fake clock. Where time
  *does* drive goroutines (the director loop), the loop does network I/O against Redis, which
  `synctest`'s virtual time does not cover well. That is tested with short real intervals
  instead.
- **Two race tests passed without racing.** The first cancel-race run was 100 of 100 "cancelled":
  the cancel always beat the round's pool read, so the "matched" branch never ran. The first
  two-director run never showed whether the fence *refused* anything. Both tests now require the
  contested path to have happened (both outcomes seen; at least one refusal), with jitter to make
  it happen. A race test that only exercises one side of the race is not testing the race.
- **"Match whatever is compatible now" makes queueing pointless for parties.** A 2-player party
  would match alone the instant it queued. Added a fill wait (10 s): full groups at once, partial
  groups only after their oldest ticket has waited.
- **A round costs Redis round trips, not CPU.** `Match` takes ~1 ms at 1,000 tickets
  (`BenchmarkMatch`; 54 ms at 10,000). The round's 167–227 ms is one script per match and one
  `PUBLISH` per player, sent one after another. Pipelining both is in ToDo, as the measured limit
  of this design: a 2,331-ticket round took 1,024 ms, so the 1 s interval is exceeded at about
  3,000 queued tickets. (First written here as "~10,000", a careless extrapolation corrected by
  a 3,000-ticket run.)
- **Not in the plan, decided here:**
  - Tickets snapshot the party roster.
  - A ticket's rating is the members' average.
  - Tickets time out after 2 minutes (which gives `ticket.failed` a reason in B4).
  - `GET /v1/matchmaking/ticket` returns the latest ticket in any state, so a lost `match.found`
    is recoverable.
  - Two keys per player: `mm:queued` (enforces one queue) and `mm:last` (lookup).
  - Ratings live on `profiles` (migration 0004), default 1500.

---

## Phase B5 — fleet, allocation, connect tokens, results — **DONE**

### Goal

A match formed by B4 gets a running game server within about a second. Each player receives
that server's address and a signed connect token. The game server admits only valid tokens,
and reports the result, which updates skill ratings. Everything is proven end to end with
`stubserver`; `GanymedDedicated` later implements the same two specs.

### Steps

1. Contract first: `docs/api/connect-token.md` and `docs/api/server-lifecycle.md`. The engine's
   O5 plans against these.
2. **`cmd/fleetagent`**, native on the host:
   - It **dials out** to the backend (a WebSocket at `/v1/fleet/agent`, authenticated with an
     agent secret from its environment) and holds that connection. It reports capacity and
     server states; the backend sends allocation commands down the same socket.
   - It keeps a **warm pool** of K server processes in `Ready`. Each is spawned with a port, a
     server credential and the connect-token public key on its command line.
   - It supervises them: restarts crashes, reaps exits, and tops the pool back up to K.
3. **The lifecycle** (agent ↔ game server over localhost HTTP):
   `Starting → Ready → Allocated → Shutdown`, plus a health ping. A server that misses pings is
   killed and replaced. Allocation is a message **to** the server: the match ID, the expected
   players, and a per-match result credential.
4. **Allocation** (backend, in `internal/fleet`): the director asks for a server, `fleet` picks a
   `Ready` one from an agent's report and sends the allocate command. The server acknowledges
   `Allocated`, and tickets move `allocating → ready` with `{address, connect_token}` pushed to
   each player.
5. **Connect tokens** (`internal/connecttoken`): Ed25519 from Go's `crypto/ed25519`. The payload is
   `{v, match_id, account_id, server_addr, iat, exp, nonce}`, encoded base64url, then a signature.
   The expiry is short (about 30 s): a token is for *connecting*, not for the session.
6. **`cmd/stubserver`**: a UDP listener that accepts `HELLO <token>`, verifies the signature,
   expiry, match and address, rejects replayed nonces, runs a fake match for N seconds, then
   `POST /v1/matches/{id}/result` with its result credential.
7. **Results**: idempotent per match ID. They update each player's skill rating (simple Elo to
   start), and the server goes `Shutdown`, then the agent spawns a replacement.

### Decisions, with reasoning

**A warm pool, not spawn-on-demand.** Spawning at allocation time puts process start, asset
loading and scene load on every player's wait. For `GanymedDedicated` that will be seconds.
Keeping K servers already `Ready` makes allocation a message, not a boot. This is Agones'
`Fleet` of `Ready` `GameServer`s, exactly. The cost is K idle processes, and sizing K against
the arrival rate is the real operational problem that production fleets autoscale on.

**The agent dials out.** A backend that calls into agents needs to reach them, which means
knowing their addresses and punching through whatever network sits in between (here: from
inside Docker to the host). An agent that dials out only needs the backend's address, which is
how GameLift's agent and Agones' SDK sidecar both relate to their control planes. gRPC
bidirectional streaming is the production-typical transport for this. A WebSocket reuses B3's
code. **Recommended: WebSocket, with gRPC as an optional exercise** if you want to learn it with
both ends in Go.

**Asymmetric signatures for connect tokens.** Game servers hold only the **public** key. A
compromised game server can verify tokens but never mint one. HMAC would put the minting secret
on every server. This is the case B1 said asymmetric signing earns its keep.

**The result credential is per match, not per server.** A server posting a result for a match
it was not allocated is refused structurally, not by a policy check.

**Where `GanymedDedicated` meets this.** The two specs are the entire interface. The engine
needs to verify an Ed25519 signature (its O5 decides the library), speak the lifecycle over
localhost HTTP (its O0 transport), and send one result. Nothing else in this repo is visible to
it.

### Verification

| Check | Pass when |
|---|---|
| Warm pool | the agent starts K stub servers; killing one → replaced; the pool returns to K |
| **End to end** | 4 `gscli` players queue → match → allocation → each receives address + token → each connects to the stub over UDP → result posted → ratings updated → the server replaced |
| Allocation latency | ticket `matched → ready` time recorded (target: well under a second with a warm pool) |
| Forged token | a changed payload byte → rejected |
| Expired token | rejected past `exp` + leeway |
| Replay | the same token twice → the second rejected |
| Wrong server | a token for server A presented to B → rejected |
| Duplicate result | a second `POST` for the same match → same response, ratings unchanged |
| Agent disconnect | its servers are marked unavailable; the director stops allocating to them |

### Execution notes

**Results.** 2026-10-09. `go vet` and `gofmt` clean; 82 test functions pass (18 new: 8 fleet,
6 allocation and results, 3 connect token, 1 profile); **`-race` clean** (in the `golang:1.27`
container). Exercised end to end with `fleetagent` and `stubserver` on the host against both
Compose replicas.

| Check | Result | Evidence |
|---|---|---|
| Warm pool | **pass** | `-pool 3`: three stubs `ready` 60 ms after the first supervise tick; a killed idle stub reaped and replaced in the same tick |
| **End to end** | **pass** | `gscli load coop 4`: queue drained in 1.06 s, one match of 4, `UDP joins: WELCOME:4`, ended `victory +16` ×4 at 9.1 s; the server exited and a replacement with a new ID took its port |
| Allocation latency | **pass** | `matched_to_ready_ms` **9.8–18.2 ms** over 5 matches (claim, command, long-poll wake, ack, relay) |
| Forged token | **pass** | one payload character changed: `DENIED bad connect token signature` |
| Expired token | **pass** | minted 36 s before use: `DENIED connect token expired`; a fresh read for the same player: `WELCOME` |
| Replay | **pass** | the same token twice: `WELCOME`, then `DENIED token already used` |
| Wrong server | **pass**, against a second *allocated* server | A's token at B: `DENIED connect token is for another server`. An unallocated stub does not answer at all (below) |
| Duplicate result | **pass** | `TestReportResult`: same response, ratings changed once, one `match.finished`; a different outcome `409 result-conflict`. Live: both players 1516, one `match_results` row, two `rating_changes` rows. Wrong or missing token and unknown match: `401` (curl) |
| Agent disconnect | **pass** | agent killed: its key gone within 5 s, its stubs exited on refused health, a pair matched afterwards failed **`no_server` 30 s after matching**; agent restarted: next pair `ready` on schedule |
| Server lost (added) | **pass** | an allocated stub killed mid-match: `failed: server_lost` **4.5 s** later; its players could queue again at once |
| Ack timeout (added) | **pass**, tests only | `TestAckTimeoutRetriesThenFails`: withdraw, re-claim, `allocation_failed` after 3. Not seen live: the stub always acknowledges |

**Where the plan was wrong or incomplete, kept visible:**

- **The agent does not hold a WebSocket.** It sends a heartbeat every second and long-polls for
  commands, which wait in a Redis list and are popped with `BLPOP`. A socket pins the agent to
  one replica while the allocating director can be any replica, and getting the command across
  would have meant B3's pub/sub, which is at most once: fine for a nudge, wrong for "this server
  belongs to that match". The list makes every replica able to answer every agent call
  ([fleet.md](../backend/fleet.md#why-the-agent-dials-out-over-http-not-a-websocket)).
- **Allocation is not a message *to* the server.** The server long-polls its agent (`ready`) and
  the allocation is the answer: the Agones SDK model. A game server then needs no listener for
  the platform, and `GanymedDedicated` needs only an HTTP client.
- **Crashed servers are replaced, not restarted.** Server IDs are per process, and that is what
  makes the heartbeat's "never downgrade a claimed server" rule sound: a claimed ID never
  becomes ready again.
- **"Marked unavailable" is a TTL.** Nothing marks an agent's servers. Their keys stop being
  renewed and expire in 5 s. The ready set is cleaned lazily, at the next claim: seen live as
  `SCARD fleet:ready` = 3 with no live servers until a match needed one.
- **The plan missed a lost command.** `BLPOP` delivers at most once; an agent that drops the
  long-poll as a command is popped never sees it. The match recovers (withdrawn after 5 s), but
  the agent kept that server as ready forever and the backend never allocated it again: a pool
  shrinking silently. Found while writing fleet.md. Heartbeat answers now list servers to retire
  (`TestHeartbeatRetiresWithdrawnServers`; live: killed at the next heartbeat, replaced 0.5 s
  later).
- **B4's `mm:queued` had to become `mm:active`**, held until the match ends, so a matched player
  cannot queue into a second match while the first is being set up (the B4 ToDo item).
- **The first withdrawal waited a round to re-claim.** `TestAckTimeoutRetriesThenFails` counted 2
  claims instead of 3. A withdrawn allocation has already cost 5 s, so the re-claim now happens in
  the same round.
- **"Idempotent per match ID" was half of it.** Recording the result once is not enough when the
  ratings live in another module: the profile side needs its own record (`rating_changes`) so
  that a retry after a crash between the two applies the change exactly once. No transaction can
  span the two modules; two idempotent halves and a retrying producer replace it.
- **Not in the plan, decided here:**
  - Connect tokens are minted on read and never stored.
  - The result token is stored as a SHA-256 and compared in constant time.
  - An unknown match, a wrong token and a match not `ready` all get the same `401`.
  - Co-op Elo is played against the content at a fixed 1500, with K = 32.
  - `no_server` after 30 s, 5 s to acknowledge, 3 attempts, and `server_lost` for a match
    running over an hour.
- **Left open, in ToDo:** an unallocated stub does not answer UDP; `result_url` names one
  replica; the agent kills its servers on shutdown instead of draining; a result recorded by a
  server that then stops retrying has its ratings applied by nobody.

---

## Explicitly not doing

- Production identity of any kind, TLS, rate limiting, abuse handling.
- Kubernetes, Agones itself, autoscaling. The warm pool size is a constant.
- Region selection and latency-based matchmaking (one machine, one region).
- Chat, friends lists, inventories, a store.
- An admin panel or dashboards.
- Encrypting connect tokens (see the netcode.io divergence above).

## Design tensions, recorded

- **The modular monolith's boundaries are enforced by convention, not by the compiler.** Go's
  `internal/` stops other *repositories* importing these packages, but not one module reaching
  into another's tables. If that starts happening, the fix is a lint check (a test that greps
  SQL for foreign table names), not a split into services.
- **Device ID as the only credential** means a leaked device file is a stolen account. That is fine
  for learning, and it is the first thing to replace if this ever faces the internet.
- **Postgres leaderboards** are a bet on scale that B2's measurements either confirm or retire.
- **pub/sub's at-most-once delivery** is accepted on the grounds that pushes are nudges. The day
  something that must not be lost goes over the push channel, this is wrong, and Streams or a
  broker is the answer.
- **A single-machine fleet** makes allocation look easier than it is: no network partitions
  between agent and backend, and no agent on a machine that has gone quiet. The agent-disconnect
  check is the only place that shows up.

## Docs this design must produce

| Phase | Contract (`docs/api/`) | Present-tense docs (`docs/backend/`), in the change that builds them |
|---|---|---|
| B1 | `openapi.yaml` (auth, `/me`), **done** | `server.md`, `db.md`, `auth.md`, `gscli.md`, **done** |
| B2 | `openapi.yaml` (+profile, leaderboards), **done** | `profile.md`, `leaderboard.md` (with the measured rank timings), **done** |
| B3 | `realtime.md`, `openapi.yaml` (+party, `/v1/realtime`), **done** | `realtime.md`, `party.md`, Redis in `db.md`, **done** |
| B4 | `openapi.yaml` (+tickets, states, `rating`), `realtime.md` (+ticket pushes), **done** | `matchmaking.md`, ratings in `profile.md`, `Roster` in `party.md`, keys in `db.md`, **done** |
| B5 | `connect-token.md`, `server-lifecycle.md`, `openapi.yaml` (+results, fleet routes), `realtime.md` (+`match.ready`, `match.finished`), **done** | `fleet.md`, `stubserver.md`, allocation and results in `matchmaking.md`, rating changes in `profile.md`, keys in `db.md`, config in `server.md`, `connect` and the match recipe in `gscli.md`, **done** |

All five landed, and this file moved to `docs/history/` with each phase's execution notes and
measurements in place, the same lifecycle the engine uses.
