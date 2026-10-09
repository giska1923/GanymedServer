# Game server lifecycle contract (api-v0.5)

How a game server process (`stubserver` today, `GanymedDedicated` later) lives under a fleet
agent, receives a match, admits players and reports the result. Normative.

The game server is an HTTP **client** only. It calls its agent on localhost and the backend for
the result, and needs no HTTP server of its own. This is the shape of Agones' game server SDK
(`Ready`, `Health`, `Shutdown`, watch for allocation), and it means the engine's existing HTTP
client is all `GanymedDedicated` needs.

```
                 agent (localhost HTTP)                         backend
   spawn ──► POST …/ready  (long-poll) ◄── 200 allocation ◄── allocate command
             POST …/allocated ──────────────────────────────► "server allocated" → tickets ready
             POST …/health   every 2 s
   players ─► UDP "HELLO <token>"                 (match runs)
             POST {result_url} ─────────────────────────────► result → ratings
             POST …/shutdown, then exit ──► agent spawns a replacement
```

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
  "players": ["a1…", "b2…"],
  "result_url": "http://localhost:8080/v1/matches/6f…/result",
  "result_token": "…"
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

Verification follows `connect-token.md` exactly, rules 1–8. A real game would continue with its
own protocol after `WELCOME`. The stub has none.

## Server → backend: the result

```
POST {result_url}
Authorization: Bearer <result_token>
{"outcome": "victory" | "defeat"}
```

- `result_token` is a credential for **this match only**, valid for this one allocation. A server
  cannot report a match it was not given, and a server whose allocation was withdrawn holds a
  dead credential.
- **Idempotent**: repeating the call (same match) returns the same response and changes nothing
  further. A server should retry on a network error or a 5xx, with backoff.
- `200` with `{"match_id", "outcome", "rating_change"}`; `401` for a wrong or expired
  credential; `409` if a *different* outcome was already recorded.

## Timing summary

| | |
|---|---|
| `ready` long-poll | ≤ 20 s per call |
| ack deadline after allocation | 5 s |
| health interval / kill after | 2 s / 6 s silent |
| connect token lifetime | 30 s (+5 s leeway) |
