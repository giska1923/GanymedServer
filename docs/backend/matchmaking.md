# Matchmaking: tickets, the match function, one director, allocation and results

[`internal/matchmaking`](../../internal/matchmaking) turns queued players into matches, gets each
match a game server, and records its result. It owns the `mm:*` Redis keys and the
`match_results` table. Party rosters, ratings, game servers and pushes come from other modules
through four interfaces it declares (`Parties`, `Ratings`, `Fleet`, `Notifier`).

| Route | Does |
|---|---|
| `POST /v1/matchmaking/tickets` `{mode}` | Queue alone, or queue your whole party if you lead it |
| `GET /v1/matchmaking/ticket` | Your latest ticket in any state, or `null`: what a client fetches on (re)connect |
| `GET /v1/matchmaking/tickets/{id}` | A ticket you are on |
| `DELETE /v1/matchmaking/tickets/{id}` | Cancel a queued ticket (any player on it) |
| `POST /v1/matches/{match_id}/result` `{outcome}` | The game server's result, authenticated by the match's result token, not a player's JWT |

Pushes: `ticket.updated` (queued, cancelled), `match.found`, `match.ready`, `match.finished`,
`ticket.failed` ([realtime.md](../api/realtime.md)).

| File | Holds |
|---|---|
| [match.go](../../internal/matchmaking/match.go) | `Mode`, `Policy`, the pure `Match` and `Expired` functions |
| [store.go](../../internal/matchmaking/store.go) | Keys, the three Lua scripts, pool reads |
| [service.go](../../internal/matchmaking/service.go) | `Deps`, Enqueue, Current, Ticket, Cancel, the ticket view |
| [director.go](../../internal/matchmaking/director.go) | The lease and the round |
| [allocation.go](../../internal/matchmaking/allocation.go) | Allocation, supervision of running matches, `ServerReady`, `ReportResult`, Elo |
| [http.go](../../internal/matchmaking/http.go) | Routes and error mapping |

## How production systems do it, and where this diverges

**OpenMatch** (Google's open-source matchmaking framework) is the reference design, and it splits
the problem into services:

- a **frontend** that accepts tickets;
- a **state store** (Redis) holding them;
- **match functions**, the game's own code, which read pools of tickets and *propose* matches;
- a **director**, which calls the match functions, takes their proposals, allocates servers
  and assigns players.

AWS GameLift FlexMatch and Unity's matchmaker expose the same concepts as configuration: rules
over ticket attributes, latency, team composition, and relaxation of the rules as tickets wait.

GanymedServer keeps OpenMatch's *split* and drops its *services*. The match function is a pure Go
function, the director is a loop, the store is Redis, and all three live in one package. The split
that matters for correctness, **propose (pure) versus commit (atomic)**, is kept exactly.

## Tickets

A ticket is one player, or a **party queued by its leader**. Its players are the party roster
**at the moment of queueing** (`party.Service.Roster`). A member who leaves the party later stays
on the ticket: making a party change cancel the ticket would need party to call into matchmaking,
a dependency it does not have. That's in [ToDo](../ToDo/README.md). A non-leader member trying
to queue gets `not-party-leader`. A party is never split across matches.

The ticket's **rating** is the average of its players' ratings, read through `Ratings` (the
profile module; [profile.md](profile.md#ratings)). In a competitive mode the strongest member's
rating is the safer choice, to stop a strong player from carrying weak friends into easy games.
In co-op, average is fair.

States, as the API shows them:

```
queued ──► matched ──► allocating ──► ready ──► finished
  │           │  ▲          │           │
  │           │  └─(retry)──┘           │
  ▼           ▼             ▼           ▼
cancelled,  failed:       failed:       failed:
or failed:  no_server     allocation_   server_lost
timeout                   failed
```

A ticket's own hash stops at `matched`. Everything after that is the **match's** state, held
once in `mm:match:<id>` and shown on every ticket in it, so four tickets never have to be
updated in step.

### Keys

| Key | Holds |
|---|---|
| `mm:ticket:<id>` | hash: mode, state, players, rating, created_ms, match_id, reason |
| `mm:pool:<mode>` | sorted set of queued ticket IDs by creation time: the pool, oldest first |
| `mm:active:<account>` | the player's ticket **while it is active**: from queueing until it is cancelled or fails, or its match ends. Its existence is what enforces "one active ticket per player" |
| `mm:last:<account>` | the player's latest ticket in any state, for `GET /v1/matchmaking/ticket` |
| `mm:match:<id>` | hash: mode, tickets, players, created_ms, **state**, attempts; once allocated, alloc_id, server_id, server_addr, result_hash, alloc_ms, ready_ms; at the end, reason or outcome and rating_change |
| `mm:pending` | sorted set of matches waiting for a server (`matched`, `allocating`), by creation time |
| `mm:running` | sorted set of `ready` matches, by ready time, for supervision |
| `mm:lease` | the replica running the director |

**A player stays active until their match ends**, not just while queued. In B4 the guard key was
`mm:queued`, cleared at `matched`, which was right while `matched` was terminal. With allocation
after it, a player could otherwise queue again while their server was still being set up, and
end up in two matches. Queueing during a match gets `409 already-queued`.

Nothing active has a TTL. A ticket that stops being active, its players' `mm:last` keys, and its
match all expire after **10 minutes**. That's long enough for a client whose push was lost (pushes
are at most once) to reconnect and read the outcome from `GET /v1/matchmaking/ticket`.

### Atomic changes

As in the party module, every multi-key change is a Lua script, with every key passed in `KEYS`:

| Script | Does | Refuses when |
|---|---|---|
| `createScript` | queues a ticket: hash, pool entry, every player's `active` and `last` keys | any player already has an `active` key |
| `finishScript` | `queued` → `cancelled` or `failed` | the ticket is not `queued` (returns its actual state) |
| `matchScript` | a whole group `queued` → `matched`, all or nothing, plus the match record, into `mm:pending` | **any** ticket in the group is not `queued` |
| `allocatingScript` | match `matched` → `allocating`, recording the server and the result token's hash | the match is not `matched` |
| `retryScript` | an unacknowledged allocation: back to `matched`, or `exhausted` after 3 attempts | the allocation is no longer the current one (`stale`: acknowledged meanwhile) |
| `readyScript` | `allocating` → `ready`, from `mm:pending` to `mm:running` | the allocation named is not the current one (it was withdrawn) |
| `endScript` | → `finished` (only from `ready`) or `failed` (from any live state): expire the tickets and the match, delete the players' `active` keys | already ended: returns `already` and changes nothing |

`finishScript` and `matchScript` both start with "is it still queued?", and Redis runs one script
at a time, so a cancel and a match of the same ticket cannot both happen. Whichever runs second
sees the first one's result. `TestCancelRacesRound` runs 200 such races with the cancel delayed by
a random 0–4 ms, and requires seeing both outcomes. Typical result: about 25 cancelled and 175
matched, never both, never neither.

## The match function

`Match(mode, policy, pool, now) [][]string` is **pure**: no Redis, no goroutines, no clock. `now`
is a parameter. That is the whole point of OpenMatch's split: all the interesting rules are tested
as tables of inputs and outputs ([match_test.go](../../internal/matchmaking/match_test.go)),
including the exact second a rating gap closes.

**Greedy, oldest first.** Take the oldest unmatched ticket as an anchor. Add the oldest tickets
that fit (total ≤ `MaxPlayers`) and are compatible with **every** ticket already in the group.
Stop at `MaxPlayers`. The group becomes a match when:

- it is **full**, immediately; or
- it has at least `MinPlayers` and its oldest ticket has waited **`FillWait`** (10 s).

Without the fill wait, two players queueing a second apart would always be matched as a pair,
and a 2-player party would match alone the instant it queued.

**Compatibility.** Each ticket accepts partners within ±*window* of its rating, where the window
is `100 + 10 × seconds waited`, capped at 1000. Two tickets are compatible when each is inside the
other's window, so the narrower window (the newer ticket) decides. A strong player queueing at an
empty hour still gets a game, just a less even one, and a newcomer is never forced on a veteran
before both have waited.

**Greedy is not optimal**, deliberately. Maximising matched players is an assignment problem. With
sizes 3, 2, 2 and a maximum of 4, the 3 blocks the 2s, and a solver would pair the 2s. Shipped
matchmakers are mostly greedy by age, because wait time is what players feel.

**Timeouts.** `Expired` returns tickets that have waited `MaxWait` (2 minutes). The director fails
them *after* the round's matching, so a ticket at the edge still gets its last chance.

Cost: `BenchmarkMatch`, 1.1 ms at 1,000 tickets and 54 ms at 10,000. That's O(n²) in the worst case,
since every candidate is checked against every anchor.

The policy numbers live in one struct (`DefaultPolicy`), not in configuration. They are product
decisions, and no deployment is going to tune them per environment.

## The director, the lease, and the fence

Every replica runs `RunDirector`. **One** holds the lease and runs rounds:

- Each second, the holder renews `mm:lease` (a compare-and-set Lua script) and runs a round.
- Everyone else tries `SET mm:lease <replica> NX PX 5000`.
- A dead holder stops renewing; within 5 s another replica's `SET` wins.
- A graceful shutdown releases the lease, so the next replica takes over at its next tick.

Measured in the tests (400 ms TTL): crash failover in **400 ms** and graceful handover in **53 ms**.
End to end in Compose (5 s TTL): `docker kill` of the lease holder while two players waited out
their fill time, and the other replica took the lease **about 4.6 s later** and matched them on
schedule.

**Why a lease is not enough on its own, and what the fence is.** A lease holder can be paused for
longer than the TTL (a long GC pause, a stopped VM) and wake up mid-round still believing it
leads. Two directors then commit at once: the textbook "zombie leader". The textbook defence is a
**fencing token**, an increasing number that the store checks on every write so that a stale
writer is refused. Here, `matchScript` plays that role without a separate token. It re-checks
every ticket's state at commit time, so a proposal based on a stale read of the pool commits
nothing. **The lease makes double work rare; the fence makes it harmless.**
`TestTwoDirectorsNeverDoubleMatch` runs two rounds concurrently over the same 40 tickets: every
ticket ends in exactly one match, and the test requires that the fence actually refused something.
It refuses 3–10 proposals per run.

Contrast with the party sweeper ([party.md](party.md#the-sweeper-is-the-grace)), which runs on
every replica with no lease at all. Its removals are idempotent, so duplicates do nothing. A
matchmaker is not idempotent in the same way: two directors would propose *different* groupings
of the same tickets, and the players would get conflicting matches. That's why it needs a single
writer, and the fence for when single-writer fails.

## Measured: 1,000 tickets

`gscli load coop 1000` signs in 1,000 fresh players, queues them (354 ms), and polls until the
queue drains. With every rating at the default 1500:

| | |
|---|---|
| Queue drained | **1.66 s** after the first ticket |
| Outcome | 1,000 matched, 250 matches of 4, none failed |
| Director rounds | pool 300 in **167 ms**; pool 700 in **227 ms** |

And with `gscli load coop 3000`: drained in 2.51 s, 750 matches of 4, but one round saw a pool of
**2,331 tickets and took 1,024 ms**, longer than the 1 s interval.

**A round costs Redis round trips, not CPU.** `Match` itself takes about 1 ms at 1,000 tickets.
The rest is one `matchScript` per match and one `PUBLISH` per player, sent one after another:
about **0.3 ms per queued ticket** on this machine, so the ceiling for a 1 s round is about **3,000
queued tickets**. Long rounds are how a lease holder outlives its lease (the fence keeps that
safe, but the work is wasted). The fix is to pipeline the commits and the pushes; it is in
[ToDo](../ToDo/README.md), and not needed at this project's scale. (B4's first write-up
extrapolated the ceiling as ~10,000 from the 700-ticket round. That was wrong by 3×, and the
3,000-ticket run corrected it.)

## Allocation

After matching, each round drives every match in `mm:pending` one step, then checks every match
in `mm:running`. Only the lease holder does this, for the same reason only it matches: one
writer, with the scripts as the fence.

| Match state | The round |
|---|---|
| `matched` | `Fleet.Claim` a ready server ([fleet.md](fleet.md#claiming-a-server)) and record it (`allocatingScript`). If no server is free, try again next round, and fail the match **`no_server` once it has waited 30 s** |
| `allocating`, acknowledged | Nothing: the acknowledgement moved it to `ready` the moment it arrived (`ServerReady`, below) |
| `allocating` for > 5 s | `retryScript`, `Fleet.Withdraw` (so a late acknowledgement is refused), and **claim another server in the same round**. After 3 attempts: **`allocation_failed`** |
| `ready` | `Fleet.ServerAlive`. A server no longer heartbeated, or a match running for over an hour: **`server_lost`** |

Every failure is `endScript` plus a `ticket.failed` per player, and the players are free to queue
again. The numbers are `AllocationPolicy` (`DefaultAllocation`), a struct for the same reason as
`Policy`: they are decisions, not deployment settings.

Re-claiming in the same round matters: a withdrawn allocation has already cost the players 5 s
of loading screen, and waiting for the next round would add up to another second.
`TestAckTimeoutRetriesThenFails` caught the version that waited, by counting claims.

**`ServerReady`** is the fleet's ready handler, wired in `main`
([fleet.md](fleet.md#acknowledgement-withdrawal-and-the-cycle-with-matchmaking)). It runs
`readyScript`, logs `matched_to_ready_ms` (9.8–18.2 ms measured with a warm pool), and pushes
`match.ready` to each player with **their own connect token**.

Measured end to end: a mid-match server crash failed the match `server_lost` 4.5 s later; with
no agent running, a match failed `no_server` 30 s after matching
([fleet.md](fleet.md#measured)).

### Connect tokens

Minted with `connecttoken.Mint` and the private key from `GS_CONNECT_TOKEN_KEY`, to the format in
[connect-token.md](../api/connect-token.md): Ed25519 over `{v, match_id, account_id,
server_addr, iat, exp, nonce}`, valid 30 s. Tokens are **minted on read, never stored**:
`match.ready` carries one, and every `GET` of a ready ticket mints a fresh one. A client that
took longer than 30 s to connect simply reads its ticket again. Storing tokens would mean storing
credentials and expiring them; minting is one signature, microseconds.
[stubserver.md](stubserver.md#measured-every-rule-against-live-servers) shows every rejection
rule against live servers.

## Results

`POST /v1/matches/{match_id}/result` with `Authorization: Bearer <result_token>` and
`{"outcome": "victory" | "defeat"}`.

**The credential is per match.** The result token is minted by the fleet for one allocation and
handed only to the server that got it. Matchmaking stores its SHA-256 and compares hashes in
constant time. A server cannot report a match it was not given, and a server whose allocation was
withdrawn holds a token that no longer matches anything. An unknown match, a wrong token, and a
match not `ready` all get the same `401` (checked with curl), so the endpoint does not reveal
which matches exist.

`ReportResult` is three steps, **each idempotent on its own**:

1. `INSERT INTO match_results … ON CONFLICT (match_id) DO NOTHING`, then read the stored row. The
   first report wins. A retry reuses the stored rating change rather than recomputing it from
   ratings that may have moved since. A report with a *different* outcome gets
   `409 result-conflict`.
2. `Ratings.ApplyRatingChange` ([profile.md](profile.md#ratings)), which applies each
   (match, player) pair once.
3. `endScript` → `finished`. Only the call that actually ends the match pushes `match.finished`.

No transaction spans the three, and none can: two modules, two stores. This is the
**idempotent-consumer** pattern. When an operation can't be atomic, make every step safe to repeat
and have the producer retry until it sees success. A crash between any two steps is repaired by
the game server's retry, which is why [server-lifecycle.md](../api/server-lifecycle.md) tells servers to
retry network errors and 5xx with backoff.

The hole that remains: a server that gives up retrying after step 1 leaves a recorded result
whose ratings were never applied, and the match then ends `server_lost` once the server exits.
A reconciler that applies `match_results` rows lacking `rating_changes` would close it. It's in
[ToDo](../ToDo/README.md).

`TestReportResult` reports twice (same response, ratings changed once, one push), then once with
the other outcome (`409`). Live, in Postgres after one victory: both players at 1516, one
`match_results` row, two `rating_changes` rows.

### Co-op Elo

Co-op has no opposing team, so the team plays **the content**, which has a fixed rating of 1500.
The standard Elo update, with the team's average rating:

```
E = 1 / (1 + 10^((1500 − R_team) / 400))      expected score
Δ = round(32 × (S − E))                        S = 1 for victory, 0 for defeat
```

Every player in the match gets the same Δ. At even odds that is ±16. A 1700 team gains 8 for a
win and loses 24 for a loss; a 1300 team gains 24 and loses 8 (`TestEloChange`). Beating what
you were expected to beat earns little. Against a fixed opponent, ratings still drift upwards
for players who mostly win, with no ceiling. A real co-op game would rate each piece of content
by difficulty, which is a one-constant change here.

## What it does not do yet

- **Latency-based matching.** There is one region, so it doesn't apply.
- Teams or roles. Co-op has none.
- **Results reach one replica.** `result_url` is built from `GS_PUBLIC_URL`, the same for every
  replica, so a game server reports to `backend` on 8080 whichever replica allocated it. If that
  replica is down for longer than the server's retries (about 15 s), the result is lost and the
  match ends `server_lost`. In [ToDo](../ToDo/README.md).
