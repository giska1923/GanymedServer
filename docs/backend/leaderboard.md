# Leaderboard: ranks and idempotent submission

[`internal/leaderboard`](../../internal/leaderboard) owns `leaderboards`, `score_submissions`,
`best_scores` and `score_idempotency_keys`
([`0003_leaderboard.sql`](../../migrations/0003_leaderboard.sql)). Display names come from the
profile module through the `Names` interface. This package never reads `profiles`.

| Route | Does |
|---|---|
| `POST /v1/leaderboards/{board}/scores` | Submit a score under an `Idempotency-Key`; returns `{rank, best}` |
| `GET /v1/leaderboards/{board}?limit=N` | The first N entries (default 10, max 100), best first |
| `GET /v1/leaderboards/{board}/me` | This player's `{rank, best}`, both null if they have no score |

All three require an access token. Exact shapes are in [`openapi.yaml`](../api/openapi.yaml).

## How production systems do it, and where this diverges

PlayFab calls them *statistics*: a named, server-declared value per player, with an aggregation
rule (last, min, max, sum) and an optional reset schedule. Leaderboards are views over a
statistic. Nakama declares leaderboards server-side with a sort order, an operator (best, set,
increment) and a reset cron. Both keep scores in a database and rank them in a purpose-built
structure. "Leaderboards are a Redis sorted set" is the received wisdom, because `ZADD` and
`ZREVRANK` are O(log N) in memory.

This module diverges deliberately: it ranks **in Postgres**, measures where that stops being
enough, and keeps the one shape every one of those systems shares, which is boards **declared
by the server**. A board is a row in `leaderboards`, added by migration (`proving-ground` today).
Submitting to an unknown board is a 404, so a typo in a client is an error rather than a new
board. There is only one operator (best, higher wins), because the Proving Ground needs nothing
else.

## Data: an event log and a projection

- **`score_submissions`** is the log: every accepted submission, including ones that changed
  nothing. Kept for history and anti-cheat (an impossible jump between two submissions is
  visible here).
- **`best_scores`** is the projection: one row per player per board. It is updated by an upsert
  whose `DO UPDATE` only fires on improvement:
  ```sql
  ON CONFLICT (board, account_id) DO UPDATE
    SET score = excluded.score, achieved_at = excluded.achieved_at
    WHERE excluded.score > best_scores.score
  ```
  A lower score leaves the row untouched, `achieved_at` included, so "who got here first" stays
  true. The projection could be rebuilt from the log at any time.

`account_id` is not a foreign key to `accounts`; see [profile.md](profile.md#no-cross-module-foreign-keys).
Leaderboard tests rely on that: they use random UUIDs as players and never create an account.

## Ranking

**Competition ranking** ("1224"): tied scores share a rank, and the next rank skips. Tied
players are *listed* in the order they reached the score (`achieved_at`).

- **Top N** uses `rank() OVER (ORDER BY score DESC)` with `ORDER BY score DESC, achieved_at LIMIT N`.
- **A player's rank** is `1 + count(*)` of strictly better scores on the board.

Both are served by one index, `best_scores_rank_idx (board, score DESC, achieved_at)`.

### Measured: 100k and 1M players

`GS_BENCH=1 go test -run '^$' -bench Leaderboard -benchtime 2s ./internal/leaderboard/`. The
benchmark seeds random scores, places three known players at the top, middle and bottom, runs
`VACUUM ANALYZE`, then times the service methods. i7-11800H, Postgres 18 in Docker Desktop,
2026-10-08:

| Path | 100k | 1M |
|---|---|---|
| `Top(10)` | 2.1 ms | 2.1 ms |
| rank of the 1st player | 1.2 ms | 0.7 ms |
| rank of the median player | 7.2 ms | 47 ms |
| **rank of the last player** | **13 ms** | **87 ms** |

What the plans (`EXPLAIN ANALYZE BUFFERS`, logged by the benchmark) show:

- **Top N is constant.** `Limit → Incremental Sort → WindowAgg → Index Scan`: the window function
  streams, so the scan stops after N+1 index rows (11 rows, 14 buffers, **0.17 ms** of server
  time at 1M). The ~2 ms the benchmark sees is three round trips through Docker Desktop's
  network (board check, the query, the name lookup), not query work. The Incremental Sort exists
  because the window's output is known to be ordered by `score` only, not by the `achieved_at`
  tie-break. It re-sorts groups of equal scores, which costs nothing measurable.
- **Rank is O(rank).** `Index Only Scan … Heap Fetches: 0`: exactly the intended plan, with no
  table access. It still counts every index entry above the player, at roughly 0.1 µs each
  (13 ms for 100k entries, 87 ms for 1M; per-entry cost falls a little with size as the scan
  warms the CPU caches): **on the order of 10 ms per 100k players ahead of you.** The last of
  1M players pays for 1M entries.

Why `VACUUM` matters for that plan: an index-only scan can skip the table only for pages the
visibility map marks all-visible, and only `VACUUM` sets those bits. On a busy board,
autovacuum keeps up. Right after a bulk insert it may not, and the same query would then fetch
heap pages.

**Reading the numbers.** The design said a 1M rank above "a few milliseconds" is the measured
case for Redis. 87 ms is well above that, so the case is made **in principle**. In practice the
Proving Ground has a handful of players, where every query is sub-millisecond. The follow-up,
a Redis `ZSET` as a *derived* index rebuilt from `best_scores`, is in
[ToDo/README.md](../ToDo/README.md) as a B3-or-later learning exercise with this measurement as
its baseline. It is not a fix for a real problem. Cheaper production answers exist too: exact
ranks only for the top N, and a percentile ("top 12%") from a periodically computed histogram
for everyone else, which is what many shipped games show.

## Idempotent submission

`POST …/scores` requires an `Idempotency-Key` header holding a UUID. The key names **one logical
submission**: a fresh UUID per new score, and the **same** key on every retry of that score. The
model is Stripe's API and the IETF draft *The Idempotency-Key HTTP Header Field*:

| Situation | Result |
|---|---|
| New key | The score is applied; the response is stored with the key |
| Key already processed, same request | Nothing applied; the **stored** response, with `Idempotent-Replayed: true` |
| Key already processed, different board or score | 422 `idempotency-key-reused` (a client bug) |
| Same key in flight concurrently | The second request **waits**, then gets the replay |

The replayed standing is the one from the first request, not the current one. Same request,
same response: that is the definition. Keys are scoped per account, so one client's keys can
never collide with or reveal another's. They are kept for at least 24 hours, deleted hourly by
`ExpireKeys`.

### How it works: the unique index does the locking

Everything happens in one transaction, and the **first** statement claims the key:

```sql
INSERT INTO score_idempotency_keys (account_id, key, board, score)
VALUES ($1, $2, $3, $4)
ON CONFLICT (account_id, key) DO NOTHING
```

- **Inserted**: this transaction owns the key. Log the submission, upsert the best score,
  compute the standing, write it into the key's row, commit.
- **Nothing inserted, key committed earlier**: read the stored request, compare, and replay or
  refuse.
- **Nothing inserted *yet*, another transaction holds the key uncommitted**: Postgres makes this
  `INSERT` **wait** on the unique index until that transaction ends. If it commits, this becomes
  the previous case. If it rolls back, this insert succeeds and this request does the work.

So duplicates serialize on the index, across replicas and restarts, with no in-memory lock.
`TestSubmitSameKeyConcurrently` fires ten identical requests at once: one does the work, nine
replay it, and there is one submission row.

The tradeoff: a waiting duplicate holds a pool connection while it waits. The IETF draft
describes the alternative, answering a concurrent duplicate with **409** at once and letting
the client retry, which frees resources at the cost of a client-visible state. Waiting is
right at this scale and simpler for the engine. Under heavy load, fail-fast would be the
better choice.

The response columns are nullable in the schema because the row is inserted before the
response exists. They are filled in the same transaction, so no committed row has them null.

### Why it lives in this package

Idempotency has to share the score's transaction, and a transaction never spans two modules.
So for now the mechanism and its table belong to the leaderboard. If B4's matchmaking tickets
need the same thing, that is the moment to extract a helper that runs on a caller's
transaction against a caller-owned table. See [ToDo/README.md](../ToDo/README.md).

## Validation

- `score`: an integer from 0 to 2^53−1 (`MaxScore`), the largest integer a double holds exactly.
  Lua numbers are doubles, and so are many JSON parsers, so a larger score could arrive as a
  different number. The field decodes into `int64`, so `1234.0` is a 400
  (`field "score": got number 12.0, want an integer`). A client with float numbers must format
  whole numbers without a fraction.
- `Idempotency-Key`: required, a UUID. The column is `uuid`, so a malformed key is a 400 here
  rather than a database error later.
- `limit`: 1–100.

## Background work: `ExpireKeys`

`main` runs `ExpireKeys(ctx, time.Hour)` in a goroutine tracked by a `sync.WaitGroup`. It deletes
keys older than 24 hours and returns as soon as `ctx` is cancelled. Shutdown order is enforced by
the defers in [`cmd/backend/main.go`](../../cmd/backend/main.go): cancel the context, wait for
the goroutine, then close the pool it uses. With two replicas both run it, which is harmless,
because deleting already-deleted rows does nothing.
