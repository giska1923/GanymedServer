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
- **Tag the contract.** Neither `api-v0.1` (B1) nor `api-v0.2` (B2) exists yet. Tag the B1 commit
  `api-v0.1` and the B2 commit `api-v0.2`, so the engine's O2 and O3 have exact versions to link
  to.
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
- **Engine-side doc fixes**, in the engine repo's `docs/ToDo/ONLINE.md` (branch `hello-online`):
  - O3 step 2 says "a fresh UUID per call" for the idempotency key. It must be fresh per
    **logical submission** and **reused on every retry** of that submission, or it protects
    nothing.
  - O3's Lua tables use `name`, but the contract's field is `display_name`: the binding must
    map it. It must also handle `rank`/`best` being `null` from `GET …/me` ("no score yet").
  - O1's hand-written JSON writer must emit whole numbers without a fraction (`1234`, not
    `1234.0`): Lua numbers are doubles, and the backend's integer fields reject `1234.0`.

## Known-stale entries in `docs/history/`

None yet.
