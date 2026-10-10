# ToDo

Work that is **not done yet**. Everything here is open by definition. An item leaves this
folder only when the thing is built **and** documented in `docs/backend/` or `docs/api/`.

## Open items

No phase plan is open. The design and its phases B1–B5 are complete and recorded in
[history/BACKEND.md](../history/BACKEND.md), and the `api-v0.6` contract change that followed
(results through the agent, `DENIED not allocated`) is live in
[server-lifecycle.md](../api/server-lifecycle.md). What is left is the follow-ups below.

## Follow-ups

- **A dead upgrade can evict a live socket (`4001`).** Seen during the engine's O4 checks: while
  replica b restarted, an upgrade for the engine's account reached b 1.7 s after it came back, at a
  moment the engine sent nothing. It upgraded in 4 ms on a connection already closed at the far end,
  and the gateway's newest-wins rule closed the engine's live socket `4001`. The likeliest source is
  Docker Desktop's port forwarder delivering an upgrade request the client had given up on while the
  replica was down; it did not reproduce on demand. The engine now retries once after a `4001`, and
  realtime.md allows that. The fix at the source: replace an account's socket only once the new one
  proves alive (its first pong, or its first successful write), so a connection that dies in its
  first milliseconds never evicts one that works. That changes the "one socket per account" path in
  `realtime.Gateway` and its superseding tests (docs/backend/realtime.md).

- **`net/http/pprof` admin listener.** Named in
  [BACKEND.md](../history/BACKEND.md#cross-cutting-conventions)'s conventions, not built yet. Bind it
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
- **Apply ratings for results nobody finished applying.** `ReportResult` records the result, then
  applies ratings, then ends the match, relying on the server's retry to finish after a crash.
  A server that stops retrying in between leaves a `match_results` row with no `rating_changes`.
  A reconciler (in the director's round, or at startup) that applies every such row closes it,
  and `ApplyRatingChange` is already idempotent. Rare, and it costs one query per sweep.
- **Drain the agent on shutdown.** Ctrl+C kills every server, matches in progress included, which
  then fail `server_lost`. A draining agent stops reporting ready servers, lets allocated ones
  finish, then exits: the difference between a deploy that players notice and one they do not.

- **`go.mod` is not tidy.** `go-redis` and `coder/websocket` are imported directly but marked
  `// indirect`, and `go mod tidy -diff` also wants to refresh `go.sum` (a newer `testify`, among
  others, pulled in by go-redis's tests). Harmless today: builds and tests pass. Found during
  api-v0.6, when a container run with `-mod=mod` rewrote the go-redis line on its own. Run
  `go mod tidy` as its own change, and review the `go.sum` diff before committing it.

## Known-stale entries in `docs/history/`

- **[BACKEND.md](../history/BACKEND.md), B5's execution notes, "Left open, in ToDo":** two of the
  four items listed there are closed by `api-v0.6`. An unallocated stub now answers
  `DENIED not allocated`, and results go through the agent, so no URL names one replica (and
  `GS_PUBLIC_URL` is gone). Its descriptions of the allocation carrying `result_url` and
  `result_token`, and of the stub posting the result to the backend, describe api-v0.5.
