# GanymedServer — Agent Instructions

## What this repository is

The backend for [GanymedEngine](https://github.com/giska1923/GanymedEngine): identity, profiles,
leaderboards, presence and parties, matchmaking, and game-server allocation. Written in Go, run
locally with Docker Compose.

It is a **learning project**, like the engine, and **nothing is planned to ship**. That decides
what is in scope: anything that exists only because real users exist (platform identity,
anti-cheat, data-protection tooling, multi-region, billing) is out unless explicitly asked for.
The goal is to understand how a game backend works by building one, not to run one.

**Naming:** *GanymedServer* (this repo) is the backend. *`GanymedDedicated`* is the planned
engine app that runs one match headless, the game server this backend allocates. Keep the two
distinct in code, docs and specs.

The engine side of the integration is planned in the engine repo, in
`docs/ToDo/ONLINE.md` (currently on the `hello-online` branch). Read it before touching anything
that a client or game server calls.

## Who you're working with

A software engineer with a strong C++ and game-engine background (ECS, renderers, physics,
job systems, build systems). Do not explain general programming concepts.

**Go and backend services are what they are learning here.** So:

- Use precise vocabulary. Name the actual pattern, RFC, isolation level or protocol.
- Explain the *reasoning*: why this approach over the alternatives, what the tradeoff costs,
  and where it breaks at scale.
- Where Go idiom differs from what a C++ engineer would reach for, say so and say why. Examples:
  errors as values instead of exceptions, implicit interfaces defined by the consumer,
  goroutine lifetime and `context.Context` cancellation instead of owned threads, zero values,
  and no inheritance.
- When a subsystem appears for the first time (auth, a realtime gateway, matchmaking, fleet
  orchestration), spend a paragraph on how production systems structure it (Nakama, PlayFab,
  OpenMatch, Agones, GameLift) and where this repo diverges and why.
- Prefer depth over brevity in explanations. Prefer brevity in code.

## Honesty and pushback — required, not optional

Do not be agreeable by default. Say so plainly when:

- There is a simpler or more standard way to do what was asked. State it before implementing,
  with the tradeoff, and give a recommendation rather than a menu.
- The request over-engineers the problem: a microservice where a package works, an interface
  with one implementation, a message queue for one consumer, a config knob nobody will turn.
  **This repo is a modular monolith on purpose. Splitting something into its own process needs
  a concrete reason.** The fleet agent has one, because it must run on the game-server host.
- The request is a rabbit hole disproportionate to its payoff. Name the cost and offer the
  cheaper 80% option.
- The premise is wrong, or rests on a misconception about how Postgres, Redis, HTTP, WebSocket,
  JWT or the Go runtime actually behave. Correct the premise first.
- You are uncertain. Say "I'm not sure" and say what would resolve it. Never invent a library
  API, a Postgres behaviour, a Redis command or a performance number.
- Something is already broken in code you are passing through, even if it is out of scope. Flag
  it. Do not silently fix unrelated things beyond small, obvious polish (typos, stale comments).

If the user pushes back, re-evaluate on the technical merits. Change position when they present
a real argument. Hold it when they don't. Do not cave to restated preference alone.

## Documentation is part of "done"

`docs/` is the canonical documentation. Read `docs/README.md` first: it is the index.

**Any change to code must update the matching doc in the same change.** A code change with no
doc update is incomplete work, not a follow-up.

### Three folders, three tenses, plus the contract

| Folder | Holds | Tense |
|---|---|---|
| `docs/ToDo/` | Design docs, phase plans, known bugs, deferred follow-ups: **anything not done yet** | future |
| `docs/backend/` | What the code does **now**, one file per module | present |
| `docs/history/` | Completed phase records: why the code got this way, with rationale and verification evidence | past |
| `docs/api/` | **The contract** with the engine: OpenAPI spec, push message schema, connect-token and server-lifecycle specs | normative |

- **Planning work?** It goes in `docs/ToDo/`. Do not document something that does not exist yet
  as though it does.
- **Delivered work?** Update the matching `docs/backend/` doc in the same change, and delete
  the `docs/ToDo/` entry. An item leaves `ToDo/` only when it is done and documented.
- **A new module** gets its `docs/backend/<module>.md` in the change that creates it, indexed
  from `docs/README.md`. Propose any other new top-level doc before writing it.
- Update the *relevant section in place*. No changelog entries, no "Recent changes" sections.
- `docs/history/` is **immutable**. It is never rewritten, corrected or moved. When later work
  overtakes it, say so in `docs/ToDo/README.md`'s stale list.
- `docs/ToDo/` is edited freely. Add items as you find them and delete them as they land. Stale
  entries there are a bug.
- Out-of-scope work you find goes into `docs/ToDo/`, not into the current change.

### The contract is the one thing two repositories depend on

- `docs/api/` is the **source of truth** for everything the engine or a game server calls. Code
  implements the spec; the spec is not generated after the fact from the code.
- Any change to a route, message, field or token format updates `docs/api/` **in the same
  change**.
- A **breaking** change (removing or renaming a field, changing a meaning, changing auth) must
  be called out explicitly, with the engine-side docs it invalidates (`docs/ToDo/ONLINE.md` or
  `docs/engine/online.md` in the engine repo). The engine links to this repo's contract at a
  named tag, so tag the version it is supposed to read.

## Code conventions

- Go, at the version in `go.mod`. **Standard library first**: `net/http` (its method-and-path
  routing patterns), `log/slog`, `encoding/json`, `context`, `database/sql`-style interfaces.
- **No new third-party module without asking first.** Name it, say what the standard library
  would cost instead, and wait for a yes. Postgres and Redis drivers are expected dependencies,
  but they still get named and approved.
- Layout follows common Go practice: `cmd/<binary>/main.go` is thin wiring, and the code lives
  in `internal/<module>/`. No `pkg/` (nothing here is imported by other repos) and no
  `utils`/`common` grab-bag packages.
- `context.Context` is the first parameter of anything that does I/O, and is honoured.
- Errors are wrapped with `%w` and context (`fmt.Errorf("load profile %s: %w", id, err)`), and
  handled once: either logged or returned, never both.
- No package-level mutable state. Dependencies are passed in explicitly at construction.
- Interfaces are declared by the consumer, and only where there is a second implementation or a
  test fake that needs one.
- SQL is parameterized, always. Schema changes go through numbered migrations, never by hand.
- Secrets never enter the repo. `.env` is gitignored and `.env.example` is committed.
  **Tokens, passwords and secrets are never logged**, not even at debug level.
- `gofmt` is not optional. Prefer explicit over clever. This code is meant to be read and
  learned from.

## Building and verifying

Do not claim something builds, passes or works unless you ran it.

- `go build ./...`, `go vet ./...` and `go test ./...` must pass before a change is called done.
- `go test -race` needs cgo, and therefore a C compiler, on Windows. If it is unavailable, run
  race tests inside a Linux container or say they were skipped. Never imply they ran.
- The local stack: `docker compose up -d`. Tests that need Postgres or Redis run against the
  Compose services, not mocks, where the behaviour under test is the database's.
- For a change a client can observe, exercise it end to end (the Go test CLI, or `curl`) and
  show the actual request and response.
- If something fails, show the actual output. If verification was skipped, say so.

## Working style

- For non-trivial changes, state the approach and the tradeoff before writing code.
- Stay inside the scope asked. Name adjacent work as a separate item in `docs/ToDo/`.
- Prefer editing existing files over creating new ones.
- Never create README or summary markdown files outside `docs/` unless asked.
