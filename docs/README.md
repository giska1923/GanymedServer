# GanymedServer Documentation

The backend for GanymedEngine. Not to be confused with `GanymedDedicated`, the engine app that
runs one match headless and that this backend allocates. See [AGENTS.md](../AGENTS.md).

## Layout

| Folder | Holds | Tense |
|---|---|---|
| [`ToDo/`](ToDo/README.md) | Design docs, phase plans, known bugs, deferred follow-ups: anything not done yet | future |
| `backend/` | What the code does now, one file per module. Created as modules land | present |
| `history/` | Completed phase records, with rationale and verification evidence. Immutable | past |
| [`api/`](api/README.md) | The contract with the engine and game servers | normative |

## Documents

### Design and plans

| Document | Covers |
|---|---|
| [BACKEND.md](ToDo/BACKEND.md) | The design: architecture, the module rule, storage, contract, testing, dependencies, and phases B1–B5 |

### Modules

| Document | Covers |
|---|---|
| [server.md](backend/server.md) | Boot order, configuration, timeouts, graceful shutdown, middleware, problem details, JSON bodies, health probes |
| [db.md](backend/db.md) | Postgres (the pool, the advisory-locked migrator, one clock for stored times, per-test schemas) and Redis (what lives there, who owns which keys, per-test databases), running `-race` |
| [auth.md](backend/auth.md) | Device login, JWT access + rotating refresh tokens, reuse detection and its concurrency, secrets at rest |
| [profile.md](backend/profile.md) | Display names and skill ratings, lazy profiles, `DisplayNames` for other modules, idempotent rating changes, why there are no cross-module foreign keys |
| [leaderboard.md](backend/leaderboard.md) | Declared boards, the submission log and best-score projection, competition ranking with measured 100k/1M timings and plans, idempotent submission |
| [realtime.md](backend/realtime.md) | The WebSocket gateway: pushes across replicas over Redis pub/sub, one socket per account ordered by generation, presence and the reconnection grace, server timeouts and shutdown for hijacked connections, measured crash behaviour |
| [party.md](backend/party.md) | Parties in Redis: Lua scripts for atomic changes, pushes as nudges, the sweeper that enforces the grace on every replica |
| [matchmaking.md](backend/matchmaking.md) | Tickets (solo or party), the pure greedy match function with widening rating windows and a fill wait, the lease-elected director and the fence that makes a zombie leader harmless, measured failover and a 1,000-ticket drain; allocation and its failure modes, connect tokens minted on read, idempotent results, co-op Elo |
| [fleet.md](backend/fleet.md) | Game servers: the agent and its warm pool, why it dials out over HTTP long-polls, heartbeats as liveness, the claim script, retirement of servers whose command was lost, the fleet/matchmaking cycle, measured allocation latency and failures |
| [stubserver.md](backend/stubserver.md) | The reference game server: the lifecycle as a client, admitting players by connect token, every rejection measured against live servers |
| [gscli.md](backend/gscli.md) | The test client: profiles, `-v` redaction, party and queue commands, `listen`, `connect`, `load`, recipes for expiry, reuse, cross-replica pushes, matchmaker failover and a whole match with game servers |

## Running it

```bash
cp .env.example .env              # then set the secrets (the file says how to generate each)
docker compose up -d --build      # Postgres :5433, Redis :6379, replicas on :8080 and :8082
go build -o bin/gscli.exe ./cmd/gscli && bin/gscli.exe login && bin/gscli.exe me
set -a; . ./.env; set +a; go test -count=1 ./...
```

### Contract

See [api/README.md](api/README.md).

## The engine side

The engine's half of this integration is `docs/ToDo/ONLINE.md` in the
[GanymedEngine](https://github.com/giska1923/GanymedEngine) repo (phases O0–O5). It links to
this repo's `docs/api/` at a tagged version.
