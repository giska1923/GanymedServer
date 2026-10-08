# ToDo

Work that is **not done yet**. Everything here is open by definition. An item leaves this
folder only when the thing is built **and** documented in `docs/backend/` or `docs/api/`.

## Open items

| Document | Covers | Items |
|---|---|---|
| [BACKEND.md](BACKEND.md) | **Design + phase plan**: skeleton and identity, profiles and leaderboards, realtime gateway, matchmaking, fleet and connect tokens | B1–B3 done; B4–B5 open |

## Follow-ups

- **`net/http/pprof` admin listener.** Named in BACKEND.md's conventions, not built yet. Bind it
  to localhost on its own port (never on the public mux). Inside Docker, "localhost" is the
  container's own loopback, so reaching it from the host needs a deliberate choice: publish it on
  `127.0.0.1` only, or use `docker compose exec`. B3's leak hunt used an in-test
  `runtime/pprof` goroutine dump instead; a live endpoint would let the same dump be taken from a
  running replica.
- **Report coder/websocket's `CloseRead` hang upstream.** On a client data message, `CloseRead`'s
  goroutine calls `Close`, which waits (`waitGoroutines`, 15 s timer) for that same goroutine.
  Measured at 15.0 s in v1.8.15; worked around with an own reader loop
  ([realtime.md](../backend/realtime.md)). A minimal reproduction is: accept, `CloseRead`, the
  client writes one text message, time how long the server's handler takes to see the close.
- **Close open sockets when a session is revoked.** A socket is authenticated once, at the upgrade,
  so refresh-token reuse detection (B1) revokes the session but leaves its socket open. Fix: auth
  publishes "session revoked" and the gateway closes the account's socket. Small, but it crosses
  the auth/realtime boundary, so it waits until it matters.
- **Redis leaderboard ranks, as a derived index.** Unblocked: Redis exists since B3. B2 measured
  Postgres ranks at 13 ms (100k) and 87 ms (1M) for last place
  ([leaderboard.md](../backend/leaderboard.md#measured-100k-and-1m-players)). Maintain a `ZSET`
  per board *derived* from `best_scores` (updated after each committed improvement, plus a rebuild
  command), serve `Mine` from `ZREVRANK`, and re-run the benchmark against this baseline. The lessons are cache-as-derived-index and what happens when
  the cache and the source disagree. It is not needed by the Proving Ground.
- **Engine-side: align `ONLINE.md` O4 with `api-v0.3`** (engine repo, `hello-online`). Points the
  realtime contract decides that O4 does not say yet:
  - Re-fetch party state over HTTP **on every (re)connect**; pushes are lost while disconnected.
  - Close `4001` (replaced by another session) means **do not** reconnect automatically. `1001`
    means reconnect at once, since another replica takes it. Anything else, back off with jitter.
  - The socket is receive-only: the engine sends no data messages (a data message gets 1008).
  - Auth is checked once, at the upgrade: no re-auth is needed when the access token expires
    mid-socket.
  - `Backend.Subscribe` types today: `party.invite`, `party.updated`, `party.removed`. Unknown
    types must be ignored.
- **Shared idempotency helper, if B4 needs one.** Idempotency lives in the leaderboard today,
  because it must share the score's transaction. If matchmaking tickets need it, extract a helper
  that claims a key on a *caller's* transaction against a *caller-owned* table, and keep each
  module's keys in its own table.

## Known-stale entries in `docs/history/`

None yet.
