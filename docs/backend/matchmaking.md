# Matchmaking: tickets, the match function, and one director

[`internal/matchmaking`](../../internal/matchmaking) turns queued players into matches. It owns
the `mm:*` Redis keys. Party rosters, ratings and pushes come from other modules through three
interfaces it declares (`Parties`, `Ratings`, `Notifier`).

| Route | Does |
|---|---|
| `POST /v1/matchmaking/tickets` `{mode}` | Queue alone, or queue your whole party if you lead it |
| `GET /v1/matchmaking/ticket` | Your latest ticket in any state, or `null`: what a client fetches on (re)connect |
| `GET /v1/matchmaking/tickets/{id}` | A ticket you are on |
| `DELETE /v1/matchmaking/tickets/{id}` | Cancel a queued ticket (any player on it) |

Pushes: `ticket.updated` (queued, cancelled), `match.found`, `ticket.failed`
([realtime.md](../api/realtime.md)).

| File | Holds |
|---|---|
| [match.go](../../internal/matchmaking/match.go) | `Mode`, `Policy`, the pure `Match` and `Expired` functions |
| [store.go](../../internal/matchmaking/store.go) | Keys, the three Lua scripts, pool reads |
| [service.go](../../internal/matchmaking/service.go) | Enqueue, Current, Ticket, Cancel |
| [director.go](../../internal/matchmaking/director.go) | The lease and the round |
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

States: `queued` → `matched` | `cancelled` | `failed` (reason `timeout`). B5 will add
`allocating` and `ready` after `matched`.

### Keys

| Key | Holds |
|---|---|
| `mm:ticket:<id>` | hash: mode, state, players, rating, created_ms, match_id, reason |
| `mm:pool:<mode>` | sorted set of queued ticket IDs by creation time: the pool, oldest first |
| `mm:queued:<account>` | the ticket the player is queued on. **Exists only while queued**, which is how "one queued ticket per player" is enforced |
| `mm:last:<account>` | the player's latest ticket in any state, for `GET /v1/matchmaking/ticket` |
| `mm:match:<id>` | hash: mode, tickets, players, created_ms. B5 allocates from this |
| `mm:lease` | the replica running the director |

Queued tickets have no TTL. A ticket that leaves the queue, its players' `mm:last` keys, and its
match all expire after **10 minutes**. That's long enough for a client whose `match.found` was lost
(pushes are at most once) to reconnect and read `matched` from `GET /v1/matchmaking/ticket`.

### Atomic changes

As in the party module, every multi-key change is a Lua script, with every key passed in `KEYS`:

| Script | Does | Refuses when |
|---|---|---|
| `createScript` | queues a ticket: hash, pool entry, every player's `queued` and `last` keys | any player already has a `queued` key |
| `finishScript` | `queued` → `cancelled` or `failed` | the ticket is not `queued` (returns its actual state) |
| `matchScript` | a whole group `queued` → `matched`, all or nothing, plus the match record | **any** ticket in the group is not `queued` |

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

**A round costs Redis round trips, not CPU.** `Match` itself takes about 1 ms at this size. The
rest is one `matchScript` per match and one `PUBLISH` per player, sent one after another: about
1,000 round trips for 700 tickets. Extrapolating, a pool of roughly 10,000 tickets would make a
round longer than the 1 s interval, and long rounds are how a lease holder outlives its lease
(the fence still keeps that safe). The fix is to pipeline the commits and the pushes; it is in
[ToDo](../ToDo/README.md), and not needed at this project's scale.

## What it does not do yet

- **Allocation (B5).** A match ends at `matched`; nothing runs it. B5 allocates a game server and
  adds `allocating` and `ready`.
- **Latency-based matching.** There is one region, so it doesn't apply.
- Teams or roles. Co-op has none.
