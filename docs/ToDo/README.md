# ToDo

Work that is **not done yet**. Everything here is open by definition. An item leaves this
folder only when the thing is built **and** documented in `docs/backend/` or `docs/api/`.

## Open items

| Document | Covers | Items |
|---|---|---|
| [BACKEND.md](BACKEND.md) | **Design + phase plan**: skeleton and identity, profiles and leaderboards, realtime gateway, matchmaking, fleet and connect tokens | B1 done; B2–B5 open |

## Follow-ups

- **`net/http/pprof` admin listener.** Named in BACKEND.md's conventions, not built in B1. Bind it
  to localhost on its own port (never on the public mux). Inside Docker, "localhost" is the
  container's own loopback, so reaching it from the host needs a deliberate choice: publish it on
  `127.0.0.1` only, or use `docker compose exec`. Worth doing before B3, where goroutine
  profiles are how leaks get found.
- **Tag `api-v0.1`** once B1 is committed, so the engine's O2 has a contract version to link to.

## Known-stale entries in `docs/history/`

None yet.
