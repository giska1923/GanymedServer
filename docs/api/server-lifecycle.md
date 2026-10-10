# Game server lifecycle contract (api-v0.6)

How a game server process (`stubserver` today, `GanymedDedicated` later) lives under a fleet
agent, receives a match, admits players and reports the result. Normative.

The game server is an HTTP **client** only, and **its agent on localhost is its only HTTP peer**.
It never calls the backend: the agent relays everything, the result included. This is the shape of
Agones' game server SDK (`Ready`, `Health`, `Shutdown`, watch for allocation), and it means the
engine's existing HTTP client is all `GanymedDedicated` needs.

```
                 agent (localhost HTTP)                         backend (any replica)
   spawn ──► POST …/ready  (long-poll) ◄── 200 allocation ◄── allocate command
             POST …/allocated ──────────────────────────────► "server allocated" → tickets ready
             POST …/health   every 2 s
   players ─► UDP "HELLO <token>"                 (match runs)
             POST …/result ─────────────────────────────────► result → ratings
             POST …/shutdown, then exit ──► agent spawns a replacement
```

**Changed in `api-v0.6`** (breaking, for game servers): the result goes to the agent
(`POST …/result`), and the allocation no longer carries `result_url` or `result_token`. Before,
the server posted to `result_url` itself, which named one backend replica, so a result reported
while that replica was down was lost and the match ended `server_lost`. The agent fails over
between replicas on every call. Also new: a server answers `HELLO` before its allocation
(*Players*, below).

## Spawning

The agent starts the server with these flags. The server must refuse to start without them.

| Flag | Meaning |
|---|---|
| `--server-id` | A UUID for this process's life. A replacement process gets a new one |
| `--agent` | The agent's base URL, e.g. `http://127.0.0.1:7600` |
| `--game-port` | The UDP port to listen on for players |
| `--advertise` | `host:port` players connect to. Connect tokens name exactly this (`server_addr`) |
| `--public-key` | The backend's Ed25519 public key, base64url (see `connect-token.md`) |

## Server → agent

All `POST`, JSON bodies, `Content-Type: application/json`, on the agent's base URL. A non-2xx answer
from the agent means "stop and exit": the agent no longer considers this server alive.

### `POST /v1/servers/{server_id}/ready`: become available, and wait for a match

A **long-poll**. The agent holds the request for up to **20 s**:

- `204 No Content`: no match yet. Call `ready` again at once.
- `200 OK` with an allocation: this server now belongs to that match.

```json
{
  "match_id": "6f…",
  "players": ["a1…", "b2…"]
}
```

While a `ready` call is outstanding, or within 2 s of one ending, the agent counts the server as
`ready` and may allocate it. A server that stops calling `ready` is never allocated.

### `POST /v1/servers/{server_id}/allocated`: acknowledge

`{"match_id": "6f…"}`, sent **as soon as the allocation is received**, before anything slow
(loading a map). The agent tells the backend, and the backend tells the players. **An allocation
not acknowledged within 5 s is withdrawn**, and the match goes to another server. A server whose
acknowledgement is refused (4xx) must not run the match; it shuts down instead. A server whose
allocation was withdrawn before it ever arrived is killed by the agent, so a game server needs no
handling for that case.

### `POST /v1/servers/{server_id}/health`: still alive

Empty body, every **2 s** from start to exit. A server that misses 3 in a row (6 s) is killed and
replaced. Health is separate from `ready` on purpose: a server busy running a match still
reports health.

### `POST /v1/servers/{server_id}/result`: the match's result

`{"outcome": "victory" | "defeat"}`, once the match has ended. The agent forwards it to the
backend with the match's result credential, which only the agent holds, trying each backend
replica in turn. Its answer is the backend's, relayed unchanged:

- `200` with `{"match_id", "outcome", "rating_change"}`: recorded.
- `4xx`: refused, and retrying will not help. `400` is a malformed body or an unknown outcome;
  `409` (`result-conflict`) means a *different* outcome was already recorded. A `409` that is the
  agent's own (plain text) means this server has no acknowledged allocation.
- `5xx`, including the agent's `502` when no backend replica answered: **retry, with backoff.**
  The backend's result endpoint is idempotent: repeating the call (same match) returns the same
  response and changes nothing further, so a retry after an unseen success is harmless.

### `POST /v1/servers/{server_id}/shutdown`: done

Sent after the result is reported (or when giving up), immediately before exiting. The agent
then spawns a replacement. A server that exits without it is treated as crashed, and is also
replaced.

## Players (UDP)

On `--game-port`, one request datagram per join attempt:

```
HELLO <connect token>
```

Answered with one datagram:

```
WELCOME <account_id>      admitted
DENIED <reason>           refused; the reason is for logs, not for clients to parse
```

**A server answers from the moment its port is bound**, not only during a match (since
`api-v0.6`):

| When | Reply |
|---|---|
| before its allocation is acknowledged | `DENIED not allocated` |
| during the match | verification by `connect-token.md`, rules 1–8: `WELCOME`, or `DENIED` with the failed rule |
| after the match | `DENIED match over` (optional: a server may also simply have exited) |

A ticket is `ready` only after its server acknowledged the allocation, so a client holding a ready
ticket that gets `DENIED not allocated` is talking to a different process on that port: the
match's server is gone, and the ticket is about to fail `server_lost`. Silence there would cost
the client its whole timeout for the same answer.

A real game would continue with its own protocol after `WELCOME`. The stub has none.

## Agent → backend: the result, on the server's behalf

The agent receives a per-match `result_token` with each allocate command, keeps it, and uses it
for that server's `POST …/result`:

```
POST /v1/matches/{match_id}/result
Authorization: Bearer <result_token>
{"outcome": "victory" | "defeat"}
```

- `result_token` is a credential for **this match only**, valid for this one allocation. It never
  reaches the game server, so a game process holds no backend credential at all. An agent cannot
  report a match it was not given, and an allocation that was withdrawn leaves a dead credential.
- The route is in `openapi.yaml`. Like every agent call, it goes to whichever backend replica
  answers.

## Timing summary

| | |
|---|---|
| `ready` long-poll | ≤ 20 s per call |
| ack deadline after allocation | 5 s |
| health interval / kill after | 2 s / 6 s silent |
| connect token lifetime | 30 s (+5 s leeway) |
