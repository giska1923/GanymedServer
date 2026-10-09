# stubserver: the reference game server

[`cmd/stubserver`](../../cmd/stubserver/main.go) stands in for `GanymedDedicated`. It implements
[server-lifecycle.md](../api/server-lifecycle.md) and [connect-token.md](../api/connect-token.md)
and nothing else; the game is a timer. It exists so that B5 can be tested end to end with no
engine involved. It is also the worked example the engine's O5 is written against: everything
a real game server owes the fleet, the backend and its players, in about 270 lines.

```
stubserver --server-id ID --agent URL --game-port N --advertise HOST:PORT --public-key KEY
           [-match-seconds 10s] [-outcome victory|defeat]
```

The first five flags are appended by [`fleetagent`](fleet.md#the-agent), which is the only
thing that should start it. The last two are for tests: put them before `--` in the agent's
command (`fleetagent -- bin/stubserver.exe -match-seconds 60s`).

## It is a client, never a server (except for UDP)

The stub opens no HTTP listener. It calls its agent (`ready`, `allocated`, `health`,
`shutdown`) and, once, the backend (the result). That is the Agones SDK model: the platform
never needs to reach into the game process, so the game needs no HTTP server, no port for the
platform, and no auth for inbound calls. For `GanymedDedicated` it means the engine needs only
the HTTP *client* from O1 plus a UDP socket.

## The sequence

| Step | Does | Why it is ordered this way |
|---|---|---|
| 1 | Bind the UDP game port | A server that cannot accept players must never report ready |
| 2 | Start a health goroutine: `POST /health` every 2 s until exit | A server busy running a match still reports health. A refused health call means the agent no longer knows this server, and it exits |
| 3 | Long-poll `POST /ready` until it returns an allocation | `204` means "nothing yet, ask again at once" |
| 4 | `POST /allocated` immediately | The backend withdraws an allocation not acknowledged in 5 s. A refused acknowledgement (`409`) means shut down without running the match |
| 5 | Admit players for `-match-seconds` | Below |
| 6 | Report the result to `result_url` | Retries network errors and `5xx` with exponential backoff (0.5 s doubling, 5 attempts); gives up on `4xx`. Safe because the endpoint is idempotent ([matchmaking.md](matchmaking.md#results)) |
| 7 | `POST /shutdown`, exit | The agent spawns a replacement with a new server ID |

Step 2 runs on its own goroutine, cancelled by a `context.WithCancel` that the whole run shares:
a refused health call cancels that context, and the long-poll or the match loop sees
`ctx.Err()` and unwinds. That's Go's answer to "stop the other thread": the goroutine is not killed
from outside, it is asked through the context and checks.

## Admitting players

A player sends one datagram, `HELLO <connect token>`, and gets one back: `WELCOME <account_id>`
or `DENIED <reason>`. connect-token.md's rules 1–6 are `connecttoken.Verify`:

- the shape, the signature, the version and the time window (`exp` + 5 s leeway);
- this server's advertised address;
- this match's ID.

The stub does the last two:

- **Rule 7, an expected player.** The account must be in the allocation's player list.
- **Rule 8, replay.** A map of nonce → expiry. A nonce seen before is refused. Entries are
  dropped once their token would be expired anyway, so the map holds at most the tokens of the
  last ~35 s and cannot grow without bound.

A token minted for one server is refused by another (rule 4), so a token leaked from a match
cannot be used to join a different one. The stub holds only the **public** key, so a compromised
server could verify tokens but never mint one. That's the reason for Ed25519 over HMAC in the
design.

### Measured: every rule, against live servers

Two matches running at once (60 s each) on `127.0.0.1:7001` (A) and `:7002` (B), tokens fetched
with `GET /v1/matchmaking/ticket` (a fresh token per read):

| Sent | Reply |
|---|---|
| a valid token to A | `WELCOME 10ec3298-…` |
| the same token again | `DENIED token already used` |
| one payload character changed | `DENIED bad connect token signature` |
| a fresh A token, to server B | `DENIED connect token is for another server` |
| a B player's token, to B | `WELCOME a26ab7e9-…` |
| `HELLO not-a-token` | `DENIED malformed connect token` |
| a token minted 36 s earlier, never used | `DENIED connect token expired` |
| a fresh token for that same player | `WELCOME baf66772-…` |

The last line is the recovery path the 30 s expiry assumes: a client that was too slow asks the
backend again, and the ticket mints a new token.

## What it does not do

- **It answers no UDP before it is allocated.** The socket is bound at start but only read in
  step 5, so a `HELLO` sent to a ready, unallocated server gets no reply at all (seen: a 3 s
  timeout). A real server should answer `DENIED not allocated`. In [ToDo](../ToDo/README.md),
  with the contract note it needs.
- **It reports its configured outcome whoever joined.** A match nobody joined still reports
  `victory`, and its players gain rating. That is fine for a stub; `GanymedDedicated` will report
  what happened.
- **No game traffic.** After `WELCOME` nothing else is exchanged. The connection, the protocol
  and the simulation are the engine's O5.
