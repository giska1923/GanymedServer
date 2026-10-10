# Server: the HTTP host

`cmd/backend` is wiring only. Load config, open the pool, migrate, register routes, serve.
The behaviour lives in the packages below.

| Package | Owns |
|---|---|
| [`internal/config`](../../internal/config/config.go) | Environment → typed `Config`, validated once at startup |
| [`internal/server`](../../internal/server) | `http.Server` construction, graceful shutdown, middleware, health probes |
| [`internal/problem`](../../internal/problem/problem.go) | RFC 9457 error responses |
| [`internal/httpjson`](../../internal/httpjson/httpjson.go) | Bounded JSON request decoding and JSON responses |

## Boot order

```
config.FromEnv            every problem reported at once; a bad config never starts
signal.NotifyContext      SIGINT (Ctrl+C) and SIGTERM (docker stop) cancel ctx
db.Open                   pool + one Ping, so a bad URL fails here, not on the first request
db.Migrate                advisory-locked; see db.md
redisdb.Open              client + one Ping
services                  auth, profile, leaderboard (handed profile as its Names),
                          realtime.Gateway (subscribes this replica at once, so a broken Redis
                          fails here), party (handed the gateway as Presence and Notifier,
                          profile as Names), fleet (the connect-token key, for its public
                          half), matchmaking (party as Parties, profile as Ratings, fleet as
                          Fleet, the gateway as Notifier, the connect-token key), then
                          fleet.SetReadyHandler(matchmaking.ServerReady): the one link the
                          other way (fleet.md)
routes                    RegisterHealth, then each module's Register; every module but auth
                          and fleet takes auth.RequireAuth as plain middleware (fleet's routes
                          take the agent secret)
net.Listen                bind before serving, so a port conflict is a startup error
background goroutines     gateway.Run, party.RunSweeper (5 s), leaderboard.ExpireKeys (1 h),
                          matchmaking.RunDirector (1 s; only the lease holder matches,
                          allocates and supervises),
                          tracked by one sync.WaitGroup
server.Run                serve until ctx is cancelled, then drain
```

**Shutdown order is set by the defers**, which run last-in first-out: cancel `ctx` → wait for the
background goroutines → close Redis, then Postgres. Cancelling explicitly matters. If
`server.Run` returns because serving *failed*, no signal ever cancels `ctx`, and waiting on
goroutines that watch it would hang forever. Closing a pool before they finish would pull it out
from under a query.

`gateway.Run` is in that wait group for a reason specific to WebSockets: `http.Server.Shutdown`
neither closes nor waits for hijacked connections. The gateway closes its own sockets (1001)
when `ctx` ends, and waiting for `Run` is what waits for them
([realtime.md](realtime.md#shutdown)).

Every log line carries `replica` (`GS_REPLICA_ID`), so two replicas' logs can be told apart.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `GS_HTTP_ADDR` | `:8080` | |
| `GS_DATABASE_URL` | required | Never logged; parse errors are not wrapped with it, because it carries the password |
| `GS_REDIS_URL` | required | `redis://host:port/db`; same no-logging rule |
| `GS_REPLICA_ID` | the hostname | Names this process in every log line and in its pub/sub channel. Compose sets `backend-a` and `backend-b` |
| `GS_JWT_SECRET` | required | At least 32 bytes. Changing it invalidates every access token |
| `GS_ACCESS_TOKEN_TTL` | `15m` | Must be shorter than the refresh TTL |
| `GS_REFRESH_TOKEN_TTL` | `720h` | 30 days |
| `GS_CONNECT_TOKEN_KEY` | required | The Ed25519 private key that signs connect tokens: a base64 32-byte seed. Game servers get only the public half, through the fleet agent. Generate with `openssl rand -base64 32`. Changing it invalidates tokens in flight (30 s worth) |
| `GS_FLEET_AGENT_SECRET` | required | At least 32 bytes. Fleet agents send it as a Bearer token; the agent reads the same variable |
| `GS_SHUTDOWN_TIMEOUT` | `20s` | How long in-flight requests get to finish |
| `GS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

`compose.yaml` runs two replicas, `backend` (`GS_REPLICA_ID=backend-a`, port 8080) and
`backend-b` (port 8082), from one YAML anchor, and passes them `GS_DATABASE_URL`, `GS_REDIS_URL`,
`GS_JWT_SECRET`, `GS_CONNECT_TOKEN_KEY`, `GS_FLEET_AGENT_SECRET`, `GS_LOG_LEVEL` and the replica
ID. (`GS_PUBLIC_URL` existed until api-v0.6, to build the result URL game servers posted to; the
agent reports results now, to whichever replica answers, so the backend no longer needs to know
its own public address.) The rest take their defaults in Compose. To try
a short token TTL, run the binary on the host against the Compose stores;
[gscli.md](gscli.md) shows how.

**Nothing in the code reads `.env`.** The binary sees only its process environment. Compose
reads `.env` for the containers, and builds the store URLs itself with the in-network hostnames
`postgres` and `redis`. A process on the host needs its own URLs, through `localhost:5433` and
`localhost:6379`, which are the `GS_DATABASE_URL` and `GS_REDIS_URL` lines in `.env` (Compose
ignores them).

### Debugging in VS Code

`.vscode/launch.json` has two Delve configurations:

- **`backend`** hands `.env` to the process (`envFile`) and overrides two values: it listens on
  `127.0.0.1:8081`, so it can run beside the Compose replicas on 8080 and 8082 against the same
  stores (it is effectively a third replica, named by your hostname), and logs at `debug`. Start
  the stores first with `docker compose up -d postgres redis`.
- **`gscli login`** runs the client with `-v` against that debugged backend, so a breakpoint in a
  handler can be hit from a second debug session.

## Timeouts

`server.New` sets all four. **A zero-valued `http.Server` has none**, and that is the classic Go
footgun. A client that trickles its headers (Slowloris) then holds a goroutine and a file
descriptor forever.

| Timeout | Value | Bounds |
|---|---|---|
| `ReadHeaderTimeout` | 5 s | Slow headers |
| `ReadTimeout` | 10 s | Slow request bodies |
| `WriteTimeout` | 15 s | Slow readers, **and the total time a handler may take** |
| `IdleTimeout` | 60 s | Idle keep-alive connections |

`WriteTimeout` is a whole-response deadline set when the request is read. B3's WebSocket route
must lift it per connection with `http.ResponseController`. The logging middleware's
`statusRecorder` implements `Unwrap` so that the controller can reach the real writer through it.

## Graceful shutdown

`server.Run` serves on a listener it is handed (tests bind port 0). When `ctx` is cancelled it
calls `Shutdown` with a fresh context bounded by `GS_SHUTDOWN_TIMEOUT`. `Shutdown` closes the
listener at once, lets in-flight requests finish, closes idle connections, and returns. Run then
returns nil and `main` exits 0. Verified with `docker compose stop`: `shutting down` →
`http server stopped` → (the gateway closes its sockets, see [realtime.md](realtime.md#shutdown)) → exit code 0. `TestRunDrainsInFlightRequests` proves a request in flight
at cancel time completes, and a request after it is refused.

## Middleware

Order, outermost first:

1. **Request ID.** Eight random bytes, hex, set as `X-Request-Id` on every response and carried
   in `context`. An incoming `X-Request-Id` is **ignored**: trusting it would let a client write
   arbitrary text into every log line of its request.
2. **Logging.** One `request` line per request: `request_id`, `route`, `status`, `duration_ms`.
   `route` is `r.Pattern`, the matched mux pattern (`GET /v1/me`), not the raw path, so logs
   group by route and never contain an ID or anything a client typed. Unmatched requests log
   `unmatched`.
3. **Recover.** A panicking handler becomes a 500 `internal` problem, and the panic and stack go
   to the log. `http.ErrAbortHandler` is re-panicked, because it is `net/http`'s own abort
   mechanism, not a bug.

## Errors: RFC 9457 problem details

Every error response is `application/problem+json` with `type`, `title`, `status` and optional
`detail`. `type` is a URN (`urn:ganymed:problem:token-expired`): an absolute URI, as the RFC
asks, that nobody will try to fetch. **Clients switch on `type` only.** The full list, with the
client action for each, is in [`docs/api/openapi.yaml`](../api/openapi.yaml). A 401 also carries
`WWW-Authenticate: Bearer` (RFC 6750). A 5xx never carries internal error text; the real error
is logged server-side.

## Request and response bodies

`httpjson.Decode` caps bodies at 64 KiB (`http.MaxBytesReader`), requires exactly one JSON value,
and **ignores unknown fields**. That is the robustness principle, chosen because the engine and
the backend are deployed separately, so a newer client must be able to talk to an older server.
A field of the wrong JSON type is reported in JSON terms (`field "score": got number 12.0, want
an integer`), never with the Go type names `encoding/json` puts in its own message.
Responses are written with HTML escaping off, so `<` stays `<`. Timestamps are always
serialized in UTC; see [auth.md](auth.md#timestamps).

## Health probes

| Route | Question | Checks | Failing means |
|---|---|---|---|
| `GET /healthz` | Can the process serve? | nothing external | restart me |
| `GET /readyz` | Can it do useful work? | pings Postgres and Redis, 2 s timeout; the body names the one that failed | send me no traffic |

Liveness deliberately does not touch the database. If it did, a database outage would make an
orchestrator restart every replica in a loop, which fixes nothing and adds load to a database
that is already struggling.

`RegisterHealth` takes a map of named `server.Pinger`s, an interface this package declares.
`*pgxpool.Pool` satisfies it as it is. go-redis's `Ping` returns a command object, not an error, so
Redis is adapted with `server.PingFunc`, a function type with a `Ping` method. It is the same trick
as `http.HandlerFunc`.

## Not built yet

The `net/http/pprof` admin listener named in the design's conventions. Tracked in
[ToDo/README.md](../ToDo/README.md).
