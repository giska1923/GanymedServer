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
| [db.md](backend/db.md) | The pool, the advisory-locked migrator, one clock for stored times, per-test schemas, running `-race` |
| [auth.md](backend/auth.md) | Device login, JWT access + rotating refresh tokens, reuse detection and its concurrency, secrets at rest |
| [profile.md](backend/profile.md) | Display names, lazy profiles, `DisplayNames` for other modules, why there are no cross-module foreign keys |
| [leaderboard.md](backend/leaderboard.md) | Declared boards, the submission log and best-score projection, competition ranking with measured 100k/1M timings and plans, idempotent submission |
| [gscli.md](backend/gscli.md) | The test client: profiles, `-v` redaction, recipes for expiry and reuse |

## Running it

```bash
cp .env.example .env              # then set both secrets
docker compose up -d --build      # Postgres on :5433, backend on :8080
go build -o bin/gscli.exe ./cmd/gscli && bin/gscli.exe login && bin/gscli.exe me
set -a; . ./.env; set +a; go test -count=1 ./...
```

### Contract

See [api/README.md](api/README.md).

## The engine side

The engine's half of this integration is `docs/ToDo/ONLINE.md` in the
[GanymedEngine](https://github.com/giska1923/GanymedEngine) repo (phases O0–O5). It links to
this repo's `docs/api/` at a tagged version.
