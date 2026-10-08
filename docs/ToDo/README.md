# ToDo

Work that is **not done yet**. Everything here is open by definition. An item leaves this
folder only when the thing is built **and** documented in `docs/backend/` or `docs/api/`.

## Open items

| Document | Covers | Items |
|---|---|---|
| [BACKEND.md](BACKEND.md) | **Design + phase plan**: skeleton and identity, profiles and leaderboards, realtime gateway, matchmaking, fleet and connect tokens | B1, B2 done; B3–B5 open |

## Follow-ups

- **`net/http/pprof` admin listener.** Named in BACKEND.md's conventions, not built in B1. Bind it
  to localhost on its own port (never on the public mux). Inside Docker, "localhost" is the
  container's own loopback, so reaching it from the host needs a deliberate choice: publish it on
  `127.0.0.1` only, or use `docker compose exec`. Worth doing before B3, where goroutine
  profiles are how leaks get found.
- **Redis leaderboard ranks, as a derived index.** B2 measured Postgres ranks at 13 ms (100k) and
  87 ms (1M) for last place ([leaderboard.md](../backend/leaderboard.md#measured-100k-and-1m-players)).
  Once Redis exists (B3), maintain a `ZSET` per board *derived* from `best_scores` (updated after
  each committed improvement, plus a rebuild command), serve `Mine` from `ZREVRANK`, and re-run the
  benchmark against this baseline. The lessons are cache-as-derived-index and what happens when
  the cache and the source disagree. It is not needed by the Proving Ground.
- **Shared idempotency helper, if B4 needs one.** Idempotency lives in the leaderboard today,
  because it must share the score's transaction. If matchmaking tickets need it, extract a helper
  that claims a key on a *caller's* transaction against a *caller-owned* table, and keep each
  module's keys in its own table.

## Known-stale entries in `docs/history/`

None yet.
