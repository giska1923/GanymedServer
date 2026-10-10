# Fleet: agents, the warm pool, and allocation

Two halves, one on each side of the network:

- [`internal/fleet`](../../internal/fleet) is the backend's view of the game servers: which
  agents are alive, which of their servers are ready, and the claim of one ready server for one
  match. It owns the `fleet:*` Redis keys.
- [`cmd/fleetagent`](../../cmd/fleetagent) runs **on the machine that hosts game servers**. It
  keeps a warm pool of server processes, supervises them, and connects them to the backend.
  It is the one binary in this repo that is a separate process for a structural reason: it has
  to be where the processes are.

The game server itself (today [`stubserver`](stubserver.md), later `GanymedDedicated`) talks only
to its agent ([server-lifecycle.md](../api/server-lifecycle.md)) and to players over UDP
([connect-token.md](../api/connect-token.md)). Matchmaking decides *when* to allocate
([matchmaking.md](matchmaking.md#allocation)). This module decides *which server*.

| Route (backend) | Does |
|---|---|
| `POST /v1/fleet/agents/{agent_id}/heartbeat` | Every second: the agent's servers and their states. Answers with the connect-token public key and the servers to retire |
| `GET /v1/fleet/agents/{agent_id}/commands` | Long-poll, up to 20 s: the next command (`allocate`), or `204` |
| `POST /v1/fleet/agents/{agent_id}/servers/{server_id}/allocated` | The server acknowledged; `409 allocation-withdrawn` if it was too late |

All three authenticate with `Authorization: Bearer <GS_FLEET_AGENT_SECRET>`: agents are
infrastructure, not accounts, so they get a shared secret and not a player's JWT. The comparison
is `subtle.ConstantTimeCompare`, because `==` on a secret returns at the first differing byte and
its timing leaks how much of a guess was right.

| File | Holds |
|---|---|
| [fleet.go](../../internal/fleet/fleet.go) | Keys, the four Lua scripts, `Heartbeat`, `NextCommand`, `Claim`, `Acknowledge`, `Withdraw`, `ServerAlive` |
| [http.go](../../internal/fleet/http.go) | The agent routes and `requireAgent` |
| [fleetagent/agent.go](../../cmd/fleetagent/agent.go) | The pool, the lifecycle API the servers call, the heartbeat and command loops |

## How production systems do it, and where this diverges

**Agones** (Kubernetes' game-server operator, by Google and Ubisoft) is the closest model and the
source of most of the vocabulary here:

- A `GameServer` is a pod with a state machine: `Scheduled → RequestReady → Ready → Allocated →
  Shutdown`.
- A `Fleet` keeps N of them `Ready`: a **warm pool**.
- A `GameServerAllocation` atomically picks one `Ready` server and marks it `Allocated`. That is
  the only way a server leaves the pool, so two matches can never get the same one.
- The game server never listens for the platform. It runs the **Agones SDK**, which talks to a
  sidecar in its pod (`Ready()`, `Health()`, `Shutdown()`, and a watch for its own allocation).

**AWS GameLift** has the same shape with different names: an agent on each fleet instance, game
processes that call `ProcessReady` and `ProcessEnding` through the server SDK, and game sessions
placed onto ready processes. **Unity's Multiplay** is the same again.

GanymedServer keeps the shape and drops Kubernetes:

| Agones | Here |
|---|---|
| Fleet controller, reconciling to N ready pods | `fleetagent`'s supervise loop, topping the pool up to `-pool` every 500 ms |
| SDK sidecar in the pod, over gRPC | `fleetagent`'s lifecycle API on `127.0.0.1:7600`, over HTTP |
| `GameServerAllocation`, against the Kubernetes API | `fleet.Claim`, a Lua script against Redis |
| Pod IP and host port | `-host` and a port from `-port-base` upwards |
| Many nodes, a scheduler | One machine, one agent. More agents work (each has an ID), but nothing chooses *between* machines |

What is lost by having one machine: there is no network between the agent and its servers to
partition, and nothing chooses which machine a match runs on (in production that is latency to
the players, cost and packing). Agent disconnect is the one place the distributed case shows,
and it is measured below.

## Why the agent dials out over HTTP, not a WebSocket

[BACKEND.md](../history/BACKEND.md#phase-b5--fleet-allocation-connect-tokens-results--done) planned a WebSocket from the agent, reusing B3's gateway. It was built as two plain
HTTP calls instead:

- a **heartbeat** every second, carrying the agent's full state, and
- a **long-poll** for commands, held up to 20 s and answered the instant a command exists.

The reason is replicas. A WebSocket pins the agent to whichever replica accepted it, while the
director that allocates can be any replica (whoever holds the lease). The allocate command would
then have to cross replicas, which is B3's pub/sub, and **pub/sub is at most once**. That's fine
for a nudge but not for "this server now belongs to that match". Here, commands wait in a Redis
list (`fleet:cmds:<agent>`) and any replica's long-poll pops them with `BLPOP`, so a command
reaches the agent whichever replica it happens to be talking to, and the agent fails over
between replicas with a loop rather than a protocol ([`call`](../../cmd/fleetagent/agent.go)).

**`BLPOP`** is Redis' blocking pop: the client's connection parks on the server until an element
arrives or the timeout passes. A list plus `BLPOP` is a work queue with instant delivery and no
polling interval. Two details matter:

- go-redis gives a blocking command a read timeout of *its own timeout + 10 s*, so the client
  does not give up on a wait that is supposed to be long.
- The handler's 20 s wait outlives the server's 15 s `WriteTimeout` ([server.md](server.md#timeouts)),
  so it extends its own write deadline with `http.ResponseController`. It's the same `net/http`
  fact B3 hit with WebSockets: request timeouts are connection deadlines, and a handler that
  legitimately runs long has to move its own.

The cost: up to 1 s of staleness in the backend's view of the agent (the heartbeat interval),
and one held HTTP request per agent. Both are irrelevant at this scale. gRPC bidirectional
streaming is what production agents use; it would remove the polling shape but bring back the
pinning problem, which they solve with a routing layer.

## Keys

| Key | Holds | TTL |
|---|---|---|
| `fleet:agent:<id>` | `alive` | 5 s, renewed by each heartbeat |
| `fleet:server:<sid>` | hash: agent, address, state, match_id, alloc_id | 5 s, renewed by each heartbeat that reports it |
| `fleet:ready` | set of ready server IDs: the allocation candidates | none (cleaned lazily, below) |
| `fleet:cmds:<agent>` | list of JSON commands waiting for the agent | 1 min |

**Liveness is a TTL.** Nothing marks an agent dead. It stops renewing, and 5 s later its key and
its servers' keys are gone. That needs no failure detector and no cleanup job, and it's why the
backend does not need to know the agent's address.

## Server states and the heartbeat

The agent reports `starting`, `ready`, `allocated` and `shutdown`. The backend adds two of its
own: `allocating` (claimed, command sent, not yet acknowledged) and `withdrawn` (claimed, then
given up on).

The agent reports a server as `ready` only while it would accept an allocation *right now*: a
`ready` long-poll from the server is outstanding, or ended less than 2 s ago and is about to be
repeated. A server busy loading, or one that has stopped asking, is reported as `starting`.

**`heartbeatScript`'s one rule: a heartbeat never downgrades `allocating`, `allocated` or
`withdrawn` back to `ready`.** The backend claims a server between two heartbeats, and the
agent's next report still says `ready` because the command has not reached it yet. Taking that
report at its word would put the server back in `fleet:ready`, and it would be allocated twice.
Server IDs are per process, so a claimed server never genuinely becomes ready again; its
replacement has a new ID. Only `shutdown` overrides a backend state.
`TestHeartbeatDoesNotUndoAClaim` is this case.

**Retirement.** The heartbeat's answer lists servers the agent must kill: `withdrawn` ones it
still reports alive. They exist because a command is delivered **at most once**: `BLPOP` removes
it before the HTTP answer is written, so an agent that drops the long-poll at that moment never
sees it. The backend withdraws the allocation 5 s later and moves the match on, but the agent
still holds that server as ready, and the backend will never allocate it again. Without
retirement, every lost command would shrink the warm pool by one, permanently and silently.
Found while documenting this module. Verified by `TestHeartbeatRetiresWithdrawnServers`, and
live: withdrawing a ready server by hand in Redis, the agent killed it at the next heartbeat and
had a replacement 0.5 s later.

## Claiming a server

`Claim(ctx, matchID, players)` reads `fleet:ready` and tries each candidate with
`claimScript`, which, atomically:

1. re-checks that the server is still `ready` **and its agent's key still exists**;
2. sets the server `allocating` with the match ID and a fresh allocation ID;
3. removes it from `fleet:ready`;
4. `RPUSH`es the allocate command (`match_id`, `players`, `result_token`) onto its agent's list.

A candidate that fails the re-check is removed from the ready set and the next one is tried.
The candidate list is read *outside* the script on purpose: the script must declare every key it
touches in `KEYS`, and the agent's key is only known after reading the server's hash.

**The ready set is cleaned lazily.** A server that dies, or whose agent dies, stays in
`fleet:ready` until a claim trips over it. Seen live: after an agent was killed, `SCARD
fleet:ready` stayed at 3 with no live servers, and the next match's first claim emptied it. A
correct but untidy design. The cost is one wasted script per stale entry, once.

The **result token** is 32 random bytes, generated per allocation and returned to matchmaking,
which stores only its SHA-256 ([matchmaking.md](matchmaking.md#results)). The agent keeps it and
reports the result for its server ([below](#the-agent)); the game server never receives it. It
does pass through Redis in clear inside the command, for the milliseconds between `RPUSH` and
`BLPOP`. Redis is not
exposed outside the Compose network here; on a shared Redis, that would be the argument for
encrypting command payloads or moving commands off Redis.

## Acknowledgement, withdrawal, and the cycle with matchmaking

The server acknowledges through its agent (`…/allocated`). `ackScript` checks that the server
is still `allocating`, **for the same agent and match**, and marks it `allocated`. `Acknowledge`
then calls the `ReadyHandler`, which is `matchmaking.Service.ServerReady`: the match goes `ready`
and the players get `match.ready`.

If matchmaking gave up first (no acknowledgement within 5 s), it has already called `Withdraw`,
which marks the server `withdrawn` *only if that allocation ID is still current*. The late
acknowledgement then fails `ackScript`, the agent gets `409 allocation-withdrawn`, and the server
shuts down without running the match.

**Why a setter, `SetReadyHandler`.** Matchmaking calls fleet (to claim), and fleet calls
matchmaking (when a server is ready). Go forbids import cycles outright, and even without them,
two values that each need the other at construction cannot both be constructed first. So:

- fleet declares `ReadyHandler` as a **function type**;
- `main` builds fleet, then matchmaking (handed fleet), then calls
  `fleetSvc.SetReadyHandler(matcher.ServerReady)`.

Neither package imports the other. A method value (`matcher.ServerReady`) is a closure over its
receiver, so it satisfies the function type with no adapter. A C++ engineer would reach for an
abstract listener class here; in Go a function value is the lighter idiom when the "interface"
has one method and one caller.

## The agent

```
fleetagent [-backend URLs] [-id ID] [-pool K] [-host H] [-port-base P] [-listen ADDR] -- <server command> [args]
```

`GS_FLEET_AGENT_SECRET` comes from the environment, never a flag (flags show up in process
listings). The server command gets the lifecycle flags appended (`--server-id`, `--agent`,
`--game-port`, `--advertise`, `--public-key`), so any binary implementing
[server-lifecycle.md](../api/server-lifecycle.md) can be run: stubserver today, `GanymedDedicated`
later.

Three goroutines, all ended by one `context` (Ctrl+C):

| Loop | Every | Does |
|---|---|---|
| supervise | 500 ms | reaps exited servers, kills servers silent for 6 s (3 missed health calls), spawns up to K live servers once the public key is known |
| heartbeat | 1 s | reports every server, learns the public key, kills retired servers |
| commands | long-poll | hands an `allocate` to its server's outstanding `ready` call, or parks it for the next one; keeps the command's result token |

**Handing an allocation to its server.** If the server's `ready` call is waiting, the command
goes straight to it through a one-slot channel. Otherwise it is parked (`pending`) and returned by
the next `ready` call. A parked allocation stays until the server acknowledges, so a `ready`
answer lost on the way is simply delivered again. A command that lands in the same instant the
`ready` call times out is taken back out of the channel and parked, not dropped. That case was
found by reading the code, not by seeing it happen, and retirement would have recovered it
anyway, 5 s later.

**Reporting the result for its server.** A server posts `{"outcome": …}` to `POST
/v1/servers/{id}/result` on the agent. The agent forwards the body unread to `POST
/v1/matches/{match}/result` with that match's result token, and relays the backend's status and
body back unchanged, so the backend stays the one place a result is validated. The request goes
through the same loop as every agent call: the first backend URL that answers below 500 wins, and
a network error or a 5xx moves on to the next replica. If none answers, the server gets `502` and
retries.

Two reasons it goes through the agent rather than straight to the backend
([server-lifecycle.md](../api/server-lifecycle.md), changed in api-v0.6):

- **Failover.** A server used to post to a `result_url` naming one replica. While that replica
  was down, the result was lost and a finished match ended `server_lost`. The agent already
  fails over on every call.
- **The game process holds no backend credential and knows no backend URL.** Its only HTTP peer
  is its agent on localhost. That is the Agones shape: a game server talks to its SDK sidecar.

`do` is the one request function: the agent's own calls go through `call`, which adds the agent
secret and decodes JSON; the result goes through `do` directly with the match's token. Neither
token is ever logged. `agent_test.go` checks the forwarding with fake backends: failover past a
`503` replica with the match's token (not the agent secret) and the body passed through, a `409`
relayed and not retried on another replica, `502` with no replica up, and `409` for a server
with no acknowledged allocation.

Each spawned process gets one goroutine that does nothing but `cmd.Wait()`. That's how a child is
reaped in Go: there is no `SIGCHLD` handler to write, and the goroutine ends when the process
does.

**Shutdown is not a drain.** On Ctrl+C the agent stops its loops and kills its servers, matches
in progress included; the backend notices within 5 s and fails those matches `server_lost`. A
production agent drains instead: it stops offering servers, lets running matches finish, then
exits. That's in [ToDo](../ToDo/README.md).

**Killing the agent hard** (`taskkill /F`) leaves its children running: Windows has no
parent-death signal. They stop themselves: their next health call is refused, and a server whose
health is refused exits. Measured: all servers gone within 5 s of the kill.

## Measured

With `fleetagent -pool 3 -- bin/stubserver.exe`, against both Compose replicas:

| Check | Result |
|---|---|
| Warm pool | agent started → first heartbeat answered in 45 ms → 3 servers spawned at the next supervise tick (480 ms later) → all 3 `ready` 60 ms after that |
| Allocation latency | matched → ready (claim, command, long-poll wake, server ack, relay) **9.8–18.2 ms** over 5 matches |
| A crash, idle | `taskkill` of a ready stub: reaped and a replacement spawned in the same tick (26 ms apart in the log) |
| A crash, mid-match | `taskkill` of an allocated stub: the match failed `server_lost` **4.5 s** later, and its players could queue again at once |
| Agent disconnect | agent killed: its key expired in ≤ 5 s, a pair queued after that failed `no_server` **30 s after matching**, and the first claim cleared the 3 stale ready entries. Agent restarted: the next pair was `ready` on schedule |
| Retirement | above |
| More matches than servers | `gscli load coop 20` (5 matches) against `-pool 3` with 5 s matches: 3 matches `ready` at ~0.5 s; the other 2 waited for a server to finish and be replaced, `ready` at 6.6 s; all 20 players `WELCOME`. This is the warm-pool sizing problem in miniature: K servers absorb a burst of K matches, and the next ones wait one match length |
| Port reuse | the same run, joining only after *all* tickets were ready (`gscli load`'s first version): the first matches had ended and their ports had been reused by the later matches' servers, so 8 old tokens reached a live server and got `DENIED connect token is for another match`. The address check alone could not tell the two servers apart; the token's `match_id` did |

`TestLongPollWakesOnCommand` measures the long-poll's wake-up: a command pushed while an agent
waits is answered in about 11 ms.

### Measured: the result through the agent (api-v0.6)

`fleetagent` with stubservers, both Compose replicas rebuilt with the change:

| Check | Result |
|---|---|
| `gscli load coop 4` | queue drained in 434 ms, `WELCOME:4`, `finished:4`, `victory +16:4`; the agent logged `result forwarded … status=200` on the stub's first attempt |
| Replica A down | `docker compose stop backend` before the match was allocated (on B), down through the result: `result forwarded … status=200` on the first attempt, `victory +16:2`. Only B was up to answer. The agent's heartbeat had already moved to B, so this shows the result reaching B, not the failover within one result call; that path is the unit test's |
| A refused result | a stub run with `-outcome draw`: the backend's `400` relayed, the stub stopped retrying at once (`result refused: 400`), shut down, and the match ended `server_lost`, as a server that never reports should |
