# ToDo

Work that is **not done yet**. Everything here is open by definition. An item leaves this
folder only when the thing is built **and** documented in `docs/backend/` or `docs/api/`.

## Open items

| Document | Covers | Items |
|---|---|---|
| [BACKEND.md](BACKEND.md) | **Design + phase plan**: skeleton and identity, profiles and leaderboards, realtime gateway, matchmaking, fleet and connect tokens | B1–B5 done. Moves to `history/` once its execution notes are reviewed |

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
  command), serve `Mine` from `ZREVRANK`, and re-run the benchmark against this baseline. The
  lessons are cache-as-derived-index and what happens when the cache and the source disagree. It
  is not needed by the Proving Ground.
- **Shared idempotency helper, if a second module needs one.** Idempotency lives in the
  leaderboard today, because it must share the score's transaction. B4 did not need it: queueing
  is naturally idempotent per player (a second queue is `already-queued`). If another module needs
  it, extract a helper that claims a key on a *caller's* transaction against a *caller-owned*
  table.
- **Cancel a ticket when its party changes.** Tickets snapshot the party roster, so a member who
  leaves stays queued with the party. Fixing it means party calling into matchmaking on every
  membership change (a new dependency direction), or matchmaking validating rosters at commit
  time (one more read per round). Neither is worth it until it bites.
- **Pipeline the director's commits and pushes.** A round costs one `matchScript` and one
  `PUBLISH` per player, sequentially: 227 ms for 700 tickets
  ([matchmaking.md](../backend/matchmaking.md#measured-1000-tickets)), and a pool of 2,331 took
  1,024 ms: past the 1 s interval at about 3,000 queued tickets. Pipelining (or one script per
  round) is the fix, and `BenchmarkMatch` plus `gscli load` are the before/after. Since B5 the
  round also allocates (a few round trips per pending match), which is the same cost in a new
  place.
- **Results reach one replica.** `result_url` is `GS_PUBLIC_URL` + the path, and Compose gives
  both replicas `http://localhost:8080`. With replica A down for longer than the server's ~15 s
  of retries, the result is lost and the match ends `server_lost`
  ([matchmaking.md](../backend/matchmaking.md#what-it-does-not-do-yet)). The cheapest fix is to
  have the server report through its agent, which already fails over between replicas. That
  changes `server-lifecycle.md` (a contract change the engine's O5 must see), so decide it before
  `GanymedDedicated` implements the result call. A load balancer in Compose is the other answer.
- **Apply ratings for results nobody finished applying.** `ReportResult` records the result, then
  applies ratings, then ends the match, relying on the server's retry to finish after a crash.
  A server that stops retrying in between leaves a `match_results` row with no `rating_changes`.
  A reconciler (in the director's round, or at startup) that applies every such row closes it,
  and `ApplyRatingChange` is already idempotent. Rare, and it costs one query per sweep.
- **Drain the agent on shutdown.** Ctrl+C kills every server, matches in progress included, which
  then fail `server_lost`. A draining agent stops reporting ready servers, lets allocated ones
  finish, then exits: the difference between a deploy that players notice and one they do not.
- **Answer UDP before allocation.** A stub that is ready but unallocated reads nothing from its
  game port, so a `HELLO` there times out instead of getting `DENIED`. Read the socket from the
  start and answer `DENIED not allocated`; add the line to `server-lifecycle.md`'s replies so
  `GanymedDedicated` does the same.

## Known-stale entries in `docs/history/`

None yet.
