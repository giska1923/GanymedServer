# Party: groups that play together

[`internal/party`](../../internal/party) owns the Redis keys `party:<id>` (hash: `leader`),
`party:<id>:members` (sorted set, scored by join time), `member:<account>` (the account's party
ID), `invites:<account>` (hash: party ID → `"<expires ms>:<inviter>"`) and the set `parties`.

| Route | Does |
|---|---|
| `GET /v1/party` | The caller's party with members' names and presence, or `{"party": null}` |
| `POST /v1/party` | Create one, led by the caller (201) |
| `POST /v1/party/invites` | Leader invites `{account_id}`; the invitee gets `party.invite` |
| `GET /v1/party/invites` | The caller's unexpired invites |
| `POST /v1/party/invites/{party_id}/accept` | Join; every member gets `party.updated` |
| `POST /v1/party/invites/{party_id}/decline` | Drop an invite (idempotent) |
| `POST /v1/party/leave` | Leave; the rest get `party.updated` |
| `POST /v1/party/kick` | Leader removes `{account_id}`; they get `party.removed` |

Rules: at most **4 members** (`MaxSize`), one party per player, only the leader invites and
kicks, invites expire after **5 minutes**. When the leader leaves, the longest-standing member
(lowest join score) becomes leader, and the last one out deletes the party.

## How production systems do it, and where this diverges

Parties (Steam lobbies, Epic's EOS lobbies, Nakama's parties) are the social layer that feeds
matchmaking: a party queues as one ticket. They are short-lived, chatty, and lost without real
harm, which is why they are typically kept in memory or a fast store rather than a durable
database. Steam lobbies, for example, are server-side objects with a member list and key/value
metadata. GanymedServer stores them in **Redis, not Postgres**, for that reason. Nothing about
a party needs to survive a restart, and Compose runs Redis with no volume to keep that claim
honest. The divergence is scale and features: no metadata, no chat, no public lobbies, no
"join in progress".

## Atomic changes: Lua scripts

Every mutation checks and changes several keys that must agree: "in at most one party"
(`member:<account>`), "at most 4" (the members set), "exactly one leader". Done as separate
commands, two players accepting the last seat at once could both see 3 members and both join.

Each mutation is a **Lua script** (`createScript`, `inviteScript`, `acceptScript`,
`removeScript`). Redis runs a script atomically, with no other command interleaving, so the check and
the change cannot be separated. `TestLastSeatRace` has two invitees accept the last seat
simultaneously: exactly one joins, and the other gets `party-full`.

The alternative is `WATCH`/`MULTI`/`EXEC`, optimistic locking. Watch the keys, read, queue the
writes, and `EXEC` fails if a watched key changed, so you retry. It works without server-side
code, but the retry loop and the "read outside, write inside" split are harder to read than a
script that says exactly what it checks.

Two conventions the scripts follow:

- **Every key is passed in `KEYS`, never built inside the script.** On one Redis node it would not
  matter, but Redis Cluster routes a script by its declared keys. (Cluster would also need all of
  a party's keys in one hash slot, `party:{id}`. Not done: there is no cluster.)
- **A party ID read before a script is re-checked inside it** (`member:<account> == pid`), so a
  player who changed parties in between is refused, not removed from the wrong one.

Reading a party (`Get`) uses `MULTI`/`EXEC` (`TxPipelined`) to read the leader and the members
from the same instant, so a leave that promotes a new leader cannot land between the two reads.

## Pushes are nudges

Every change nudges the players it affects through `Notifier` (the realtime gateway). Pushes are
**fire-and-forget**: a failed push is logged, never returned. The change is already committed,
and the contract makes clients re-fetch on reconnect, so a lost nudge costs promptness, never
correctness.

## The sweeper is the grace

`RunSweeper` runs `Sweep` every 5 s on **every replica**. It lists the parties, reads members'
presence through `Presence.Status`, and removes every member who is **offline**, meaning their
30 s reconnection grace ([realtime.md](realtime.md#presence)) has run out. Removed players get
`party.removed {reason: "disconnected"}`, almost certainly lost since they are offline, and the
rest get `party.updated`.

No coordination is needed between replicas. `removeScript` checks membership first, so when two
replicas sweep the same member, exactly one removal succeeds, and only that replica sends the
nudges (`TestSweepRemovesOfflineOnce` sweeps concurrently twice: one removal, one push).

It is also why a **crashed replica** cannot strand its players in parties: the crashed process runs
no timers, but its players' presence expires on its own, and any surviving replica's sweep then
finds them offline. Measured: removed 30 s after `docker kill`.

**One race is accepted rather than closed:** a player who reconnects between the sweep's presence
read and its removal script is removed anyway. They were at the very edge of a 30 s grace, and
closing it would mean moving the presence check into the script, which would read realtime's keys
inside party's script, a cross-module transaction. Not worth it.

## Module boundaries

Party declares three interfaces and is handed their implementations in `main`:

| Interface | Implemented by | Used for |
|---|---|---|
| `Presence` | `realtime.Gateway` | member status, the sweeper |
| `Notifier` | `realtime.Gateway` | pushes |
| `Names` | `profile.Service` | display names |

It imports `realtime` only for the `Status` type. It never reads realtime's keys or profile's
table. The tests show what that buys: [`party_test.go`](../../internal/party/party_test.go) runs
on Redis alone, with three ten-line fakes. No Postgres, no sockets. Only the HTTP contract test
adds Postgres, for real tokens.

Invite targets are not checked against the account list: that would mean reading auth's tables.
An invite to an ID that is no account simply expires.

In the other direction, party offers `Roster(ctx, account) (partyID, leaderID, memberIDs, err)`:
the bare membership, read in one `MULTI`/`EXEC`, without the presence and name lookups `Get`
adds. Matchmaking queues a party through it ([matchmaking.md](matchmaking.md#tickets)), and `Get`
is built on it.
