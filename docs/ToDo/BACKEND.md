# Design — the GanymedServer backend

**Status: designed, not started.** Phases B1–B5 below. B1 can start now.

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
| **B5** fleet, allocation, connect tokens, results | O5 `GanymedDedicated` hooks |

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
        │                                         │                  │ (B3+)           │ WebSocket, dials out
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
  server/        http.Server, ServeMux routes, middleware (request id, logging, recover, auth)
  config/        env → typed config, validated at startup
  db/            pgx pool, the migrator, transaction helper
  auth/          device login, access tokens, refresh rotation
  profile/       display name, skill rating
  leaderboard/   scores, ranks, idempotency
  realtime/      WebSocket gateway, presence, parties, cross-instance fan-out
  matchmaking/   tickets, the pool, match function, director loop
  fleet/         agent registry, warm-pool view, allocation (backend side)
  connecttoken/  sign (backend) and verify (stubserver; the spec for the engine)
  agent/         the fleet agent's process supervision (used only by cmd/fleetagent)
migrations/      0001_init.sql, 0002_… — numbered, embedded, forward-only
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
| **Postgres** | accounts, devices, refresh tokens, profiles, scores, idempotency keys, matches and results | Anything that must survive a restart and be correct under concurrency. The source of truth. |
| **Redis** (from B3) | presence (TTL keys), pub/sub channels, matchmaking tickets, the matchmaker's lease | Ephemeral, or coordination between replicas. **Nothing in Redis is the only copy of something that matters.** |

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
- **IDs**: UUIDs generated by Postgres (`gen_random_uuid()`, built in since Postgres 13).
  Random secrets (refresh tokens, server credentials) come from `crypto/rand`.

### Testing

- **Pure logic**, such as the match function, window widening, token sign/verify and refresh
  rotation rules, gets table-driven unit tests.
- **Anything whose correctness is the database's** (uniqueness, idempotency, rotation races)
  runs against the Compose Postgres, never a mock. Each test creates its own schema and sets
  `search_path` on its connection, so tests run in parallel without interfering. If
  `TEST_DATABASE_URL` is unset, these tests skip with a message rather than fail.
- **Time-dependent logic** (heartbeats, TTLs, window widening, disconnect grace periods) uses
  `testing/synctest` (stable since Go 1.25). Time is virtual inside the test bubble, so a 30-second
  grace period tests in microseconds, deterministically.
- **`-race`** needs cgo on Windows. It runs in a `golang` container:
  `docker run --rm -v ${PWD}:/src -w /src golang:1.27 go test -race ./...`. Any concurrency
  change (B3–B5 especially) runs it, and says so if it did not.

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

## Phase B1 — skeleton and identity

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

---

## Phase B2 — profiles and leaderboards

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

---

## Phase B3 — the realtime gateway

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

---

## Phase B4 — matchmaking

### Goal

Players or parties submit tickets. The backend forms matches of the right size from compatible
tickets, widening its tolerance the longer a ticket waits, and exactly one replica runs the
matchmaker at a time. Without a fleet (B5), a formed match ends in state `matched`.

### Steps

1. Contract first: `POST /v1/matchmaking/tickets` `{mode}` (a party leader submits for the party),
   `GET /v1/matchmaking/tickets/{id}`, `DELETE …/{id}`, and pushes `match.found` and
   `ticket.failed`.
2. **Ticket store in Redis**: a hash per ticket and a sorted set per mode keyed by enqueue time.
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

---

## Phase B5 — fleet, allocation, connect tokens, results

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
| B1 | `openapi.yaml` (auth, `/me`) | `server.md` (mux, middleware, timeouts, shutdown), `db.md` (pool, migrator), `auth.md`, `gscli.md` |
| B2 | `openapi.yaml` (+profile, leaderboards) | `profile.md`, `leaderboard.md` (with the measured rank timings) |
| B3 | `realtime.md` | `realtime.md` |
| B4 | `openapi.yaml` (+tickets), ticket state machine | `matchmaking.md` |
| B5 | `connect-token.md`, `server-lifecycle.md`, `openapi.yaml` (+results) | `fleet.md`, `stubserver.md` |

When all five have landed, this file moves to `docs/history/`, with each phase's execution
notes and measurements appended first, the same lifecycle the engine uses.
