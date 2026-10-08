# gscli: the test client

[`cmd/gscli`](../../cmd/gscli/main.go) is a scriptable stand-in for the game. It is faster to
iterate with than launching the engine, and it is how phases are verified end to end.

```bash
go build -o bin/gscli.exe ./cmd/gscli
bin/gscli.exe [-server URL] [-profile NAME] [-v] login|me|refresh
```

| Command | Does |
|---|---|
| `login` | `POST /v1/auth/device` with the profile's device ID, then stores the session |
| `me` | `GET /v1/me` with the stored access token; prints the status and body |
| `refresh` | `POST /v1/auth/refresh` with the stored refresh token, then stores the new session |
| `profile` | `GET /v1/me/profile` |
| `rename NAME` | `PATCH /v1/me/profile` (quote names with spaces) |
| `submit BOARD SCORE [KEY]` | `POST /v1/leaderboards/BOARD/scores`. Prints the `Idempotency-Key` it used: a fresh UUID, or `KEY` if given. Pass a printed key back to retry that exact submission. `SCORE` is sent as typed, so `12.0` exercises the server's integer check |
| `top BOARD [N]` | `GET /v1/leaderboards/BOARD?limit=N` |
| `rank BOARD` | `GET /v1/leaderboards/BOARD/me` |
| `party` | `GET /v1/party` |
| `party-create` | `POST /v1/party` |
| `invite ACCOUNT_ID` / `kick ACCOUNT_ID` | `POST /v1/party/invites` / `POST /v1/party/kick` |
| `invites` | `GET /v1/party/invites` |
| `accept PARTY_ID` / `decline PARTY_ID` | `POST /v1/party/invites/PARTY_ID/accept` / `…/decline` |
| `leave` | `POST /v1/party/leave` |
| `listen` | Opens the realtime socket and prints every push with a timestamp until Ctrl+C (a normal close) or until the server closes it, printing the close code (`1001`, `4001`, …) |
| `queue MODE` | `POST /v1/matchmaking/tickets` (alone, or the whole party if you lead it) |
| `ticket` | `GET /v1/matchmaking/ticket`: your latest ticket in any state |
| `cancel TICKET_ID` | `DELETE /v1/matchmaking/tickets/TICKET_ID` |
| `load MODE N` | Load test: signs in N fresh players at once (no profile files), queues them all, polls until every ticket leaves the queue, and prints the drain time, final states and match sizes. Requests run on a pool of 32 goroutines, so the client is not the bottleneck being measured |

A replayed response prints `(replayed: the server had already processed this key)`.

## Profiles

A profile is **one simulated player**: a device ID (a v4 UUID, created on first use) plus the
tokens from its last login or refresh. It lives in
`<user config dir>/GanymedServer/gscli/<profile>.json` (`%APPDATA%` on Windows), written `0600`.
`-profile` mirrors the engine's `--profile=`: two profiles are two accounts.

Unlike the engine, which keeps tokens in memory only, gscli **stores its tokens on disk**. Every
invocation is a separate process, and a client that had to log in before every call could never
exercise refresh or expiry.

## `-v`

`-v` prints each exchange (request line, credential headers, bodies, response status and the
interesting headers) with **every device ID and token shortened to its first 8 characters**, so
the output is safe to paste into a doc or an issue. The access token is sent only on routes
outside `/v1/auth/`.

## Recipes

Access-token expiry without waiting 15 minutes: run the backend on the host, against the Compose
database, with a short TTL:

```bash
set -a; . ./.env; set +a
GS_DATABASE_URL="$TEST_DATABASE_URL" GS_HTTP_ADDR=127.0.0.1:8081 GS_ACCESS_TOKEN_TTL=2s go run ./cmd/backend
bin/gscli.exe -server http://127.0.0.1:8081 -profile exp login
sleep 3; bin/gscli.exe -server http://127.0.0.1:8081 -profile exp me        # 401 token-expired
bin/gscli.exe -server http://127.0.0.1:8081 -profile exp refresh             # new session
```

Idempotent retry, and a key reused for a different request:

```bash
bin/gscli.exe -profile ana submit proving-ground 4200            # prints Idempotency-Key: <K>
bin/gscli.exe -profile ana -v submit proving-ground 4200 <K>     # 200, Idempotent-Replayed: true
bin/gscli.exe -profile ana submit proving-ground 9999 <K>        # 422 idempotency-key-reused
```

Cross-replica push. Two players, each on a different replica (Compose: 8080 and 8082):

```bash
A="-server http://localhost:8080"; B="-server http://localhost:8082"
bin/gscli.exe $A -profile ana login; bin/gscli.exe $B -profile ben login
bin/gscli.exe $B -profile ben listen                   # leave running in its own terminal
bin/gscli.exe $A -profile ana party-create
bin/gscli.exe $A -profile ana invite <ben's account_id> # ben's listener prints party.invite
```

Then `docker kill ganymedserver-backend-b-1` and run `gscli $A -profile ana party` every few
seconds. Ben shows `online` until his presence expires (20–30 s), then `offline`, then he is gone
from the party within the next 5 s sweep. To test a dropped *client* instead, kill the listener
process. To test superseding, run a second `listen` for the same profile against the other
replica.

Matchmaker failover. Two solo players queue through replica B; they pair after the 10 s fill wait:

```bash
B="-server http://localhost:8082"
bin/gscli.exe $B -profile ana queue coop; bin/gscli.exe $B -profile ben queue coop
docker compose exec redis redis-cli GET mm:lease     # which replica is the director
docker kill ganymedserver-backend-1                  # if it is backend-a
bin/gscli.exe $B -profile ana ticket                 # matched, by backend-b, ~10 s after queueing
```

Load: `bin/gscli.exe -server http://localhost:8082 load coop 1000`.

Refresh reuse: copy the profile file, `refresh`, copy the old file back, then `refresh` again. The
replay gets 401 `invalid-refresh-token`, the newest token is revoked with it, and the backend logs
`refresh token reuse detected: family revoked`.
