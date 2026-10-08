# Realtime: the WebSocket gateway and presence

[`internal/realtime`](../../internal/realtime) holds one WebSocket per signed-in player, tracks
presence, and delivers server pushes to whichever replica holds a player's socket. It owns the
Redis keys `presence:<account>` and `session:<account>`, and the pub/sub channels
`user:<account>` and `realtime:replica:<id>`. The contract is
[`docs/api/realtime.md`](../api/realtime.md).

| Piece | File |
|---|---|
| `Gateway`: the route, the receive loop, `Notify`, `Status`, shutdown | [gateway.go](../../internal/realtime/gateway.go) |
| `client`: one socket, its two goroutines, attach and detach | [client.go](../../internal/realtime/client.go) |
| Presence and its compare-and-set Lua scripts | [presence.go](../../internal/realtime/presence.go) |

## How production systems do it, and where this diverges

The realtime tier is the hard part of a game backend to scale, because a connection is *pinned*
to one process: the API tier can send any request to any replica, but a push for player X has to
reach the one process holding X's socket. The common answers:

- **A cluster of realtime nodes that know about each other** and route messages node to node.
  This is how clustered game servers such as Nakama's commercial edition work (its open-source
  edition is, as far as I know, single-node; unverified detail, not load-bearing here).
- **A message bus all nodes subscribe to.** Redis pub/sub, NATS, or Kafka for durable streams.
  Senders publish to the player's address, and whichever node holds the socket delivers. Many
  in-house game backends work this way.
- **A managed service** (AWS API Gateway WebSockets, Azure Web PubSub, Pusher): the provider
  holds the sockets, and the backend calls an HTTP API to push to a connection ID.

GanymedServer uses the bus approach, with Redis pub/sub and **one channel per player**
(`user:<account>`). Senders need no routing table; Redis delivers to whichever replica
subscribed. The cost is that pub/sub is **at most once**: no buffering, no retry, no ack. That
cost is accepted by contract: pushes are nudges, and HTTP is the state of record. Durable delivery
(Redis Streams, Kafka) would be the next step if something that must not be lost ever went over
this channel.

## The path of one push

```
party.Service ── Notify(account, "party.invite", …)
                   └─ PUBLISH user:<account> {"type","id","payload"}           any replica
                                │
                              Redis ─► every replica SUBSCRIBEd to user:<account>
                                        (only the one holding the socket is)
                                │
              Gateway.Run (the one receive goroutine) ─► dispatch
                   └─ clients[account].enqueue(msg)        non-blocking; full queue = slow client
                                │
              client.run (the socket's goroutine) ─► conn.Write, 5 s timeout
```

- **One subscription connection per replica**, not per socket. Every socket's channel is added
  to the replica's single `redis.PubSub` and removed when the socket goes. go-redis makes
  `Subscribe`/`Unsubscribe` safe from any goroutine, but *receiving* is not, so exactly one
  goroutine (`Gateway.Run`) receives and dispatches.
- **`enqueue` never blocks.** It runs on that one receive goroutine, and a blocking send to one
  slow socket would stall delivery to every socket on the replica. A full queue (32 messages)
  disconnects the slow client instead. That is the same principle as the engine's spawn cap: a
  guard, not a budget.

## One socket, two goroutines, two contexts

Each socket is served by the HTTP handler's own goroutine (`client.run`: writes, pings,
presence refresh) plus one reader goroutine (`client.readLoop`). Both end when the socket closes;
`TestNoGoroutineLeak` opens and closes 1000 sockets and the goroutine count returns to its
baseline (8 → 8).

There are two contexts, because of one property of coder/websocket: **cancelling the context
of an in-progress read or write closes the connection immediately, without a close frame.**

- `c.ctx` (control) is cancelled to *ask* `run` to close: a supersede, a slow client, a
  shutdown. `run` then sends the close frame with the right code.
- `c.peer` is cancelled by `readLoop` when the connection is really gone.

The first version read on the control context, and every close request killed the TCP
connection before its close code could be sent; the tests saw EOF instead of 4001, 1008 and 1001.
For the same reason, writes and pings use their own timeouts, never a context derived from
`c.ctx`.

**`readLoop` replaces the library's `websocket.CloseRead`.** `CloseRead` closes a socket that
receives a data message from inside its own reader goroutine. `Close` then waits for that same
goroutine to finish (`waitGoroutines`, a 15 s timer), so the socket hung for 15 s. Measured at
exactly 15.0 s, then replaced. `readLoop` asks `run` to close with 1008 and keeps reading, so the
closing handshake completes. It also caps incoming messages at 1 KiB, since clients send nothing.

## Superseding: one socket per account, ordered by generation

A new socket for an account, on any replica, replaces the old one (close `4001`):

1. `attach` takes a **generation** with `INCR session:<account>`.
2. It replaces any local socket for the account directly.
3. It publishes an internal `session.replaced {gen}` on `user:<account>`. The replica holding the
   old socket closes it **only if the announced generation is newer** than its socket's own.

Generations, not connection IDs, because the announcement can still be in flight when an even
newer socket attaches. Compared by ID, a late announcement from socket N would close socket N+1
("replaced by someone who isn't me"). That was a real bug, found by the leak test failing about
half the time. The pub/sub message is at most once, so there is a backstop: every ping refreshes
presence with a compare-and-set, and a socket that finds the presence owned by someone else closes
itself with 4001 within one ping interval.

`session:<account>` has no TTL: one small counter per account that ever connected.

## Presence

`presence:<account>` holds one of three states:

| Value | Meaning | TTL |
|---|---|---|
| `conn:<connID>` | online; this socket owns presence | 30 s, refreshed by every ping (10 s) |
| `away` | the socket closed (client, network or graceful server shutdown) | 30 s |
| (missing) | offline | |

**The away state is the reconnection grace.** A player who reconnects while away is online again,
and nothing else noticed. There is no separate grace timer, because a timer on a replica that
crashed never fires. A crashed replica's players are never marked away: their `conn:` values just
stop being refreshed and expire, 20–30 s after the crash, going straight to offline. Every
transition that "frees" a player is a TTL, so none depends on the process that held them still
being alive. The party sweeper ([party.md](party.md#the-sweeper-is-the-grace)) acts on offline.

Writes to presence are **compare-and-set Lua scripts**: refresh only if the value is still our
`conn:<id>`, mark away only if it is still ours. A Lua script runs atomically in Redis, with no
command in between its `GET` and its `SET`, so a socket on another replica that has since taken
over is never overwritten. `Status(ids)` reads many players in one `MGET`.

Measured end to end, with two replicas in Compose:

| Event | What happened |
|---|---|
| `docker kill` of the replica holding Ben | Ben `online` until **t+24 s**, then `offline`; removed from his party at **t+30 s** by the *other* replica's sweeper; the leader got `party.updated` |
| Ben's client killed, reconnected after 9 s | `away` → `online`; seat kept; **no** push to the party |
| Ben's client killed and gone | `away`; removed at **t+31 s**; one `party.updated` |
| Ben connects to replica A while on B | B's socket closed `4001`; A's receives his pushes |
| `docker compose stop` of Ben's replica | Ben's socket closed `1001`; Ben `away`, not offline |

## Server timeouts, and why the gateway clears them

The HTTP server's `ReadTimeout` (10 s) and `WriteTimeout` (15 s) are **deadlines on the
connection**, set when the request arrives. Hijacking the connection for the WebSocket does not
clear them; `net/http`'s documentation says the hijacker is responsible for them. Left alone,
every socket would die 10 s after opening. `handle` clears both with `http.ResponseController`
before `websocket.Accept`. That reaches the real connection through the logging middleware's
`Unwrap`, added in B1 for exactly this. `TestSocketOutlivesServerTimeouts` runs a server with
200 ms timeouts and checks a socket still works after a second.

Hijacking itself works through the same `Unwrap`: coder/websocket follows `Unwrap` chains to find
an `http.Hijacker`, so no middleware change was needed.

## Shutdown

`http.Server.Shutdown` **neither closes nor waits for hijacked connections**; its documentation
says the caller must. So `Gateway.Run`, when its context ends, cancels the gateway context
(every socket's control context derives from it), waits for every socket's goroutine, then closes
the subscription. Each socket closes with `1001` and marks its player **away**, so a deploy gets
the same reconnection grace as a dropped connection. `main` waits for `Run` before closing Redis.
A connection that arrives during shutdown gets a 503.

## Authentication

The token is checked once, by `auth.RequireAuth` on the upgrade request. A socket stays
authenticated for its lifetime, even past the access token's expiry. That keeps the engine
simple (no re-auth protocol over the socket), at the cost that revoking a session (refresh
reuse, B1) does not close an open socket. Accepted, and stated in the contract. Closing sockets
on revocation would mean auth publishing a "revoked" event that the gateway consumes.

No `Origin` header is expected; coder/websocket refuses cross-origin upgrades by default, which is
right for a native client.

## Tests

[`gateway_test.go`](../../internal/realtime/gateway_test.go) runs real gateways, often two at once
on one Redis, with real sockets: cross-replica push, presence through all three states, supersede
across replicas, slow client, server timeouts, shutdown (1001 + away), client data (1008), no
token (401), and the 1000-socket leak check. A crash is approximated by asserting that the
`conn:` value carries a TTL, since a test cannot stop a process mid-flight; the real crash was
checked with `docker kill`, above.
