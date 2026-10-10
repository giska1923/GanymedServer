# The contract

Everything outside this repository depends on the files in this folder, and nothing else here.
The code implements these specs; the specs are not generated from the code. Rules are in
[AGENTS.md](../../AGENTS.md#the-contract-is-the-one-thing-two-repositories-depend-on).

| File | Specifies | Consumers | Written in |
|---|---|---|---|
| `openapi.yaml` | Every HTTP route: paths, JSON shapes, problem-detail error types | engine O1–O3, `gscli` | B1, extended each phase |
| `realtime.md` | The WebSocket envelope, message types, close codes | engine O4 | B3 |
| `connect-token.md` | Token payload, encoding, Ed25519 signature, validation rules | `stubserver`, engine O5 | B5 |
| `server-lifecycle.md` | Agent ↔ game server: states, health, allocation, the result, UDP replies | `stubserver`, engine O5 and netcode N1 | B5; changed in api-v0.6 |

**Written so far:**

- [`openapi.yaml`](openapi.yaml), version 0.6.0: B1 (health, device login, refresh, `/me`, problem
  types, refresh rotation rules), B2 (profile, leaderboards, `Idempotency-Key`, integer scores),
  B3 (parties, the `/v1/realtime` upgrade), B4 (matchmaking tickets and their states, the
  profile's `rating`) and B5 (the states after `matched`, the ticket's `server` and `result`,
  match results, the fleet agent routes). 0.6.0: the allocate command lost `result_url`, and the
  result route is called by the agent.
- [`realtime.md`](realtime.md) (api-v0.5): the push socket. Messages (party; since B4,
  `ticket.updated`, `match.found`, `ticket.failed`; since B5, `match.ready` and
  `match.finished`), presence, close codes, and the rule that pushes are nudges and HTTP is the
  state of record.
- [`connect-token.md`](connect-token.md) (api-v0.6): the token a player presents to a game server.
  Format, Ed25519 signature, claims, the eight verification rules, and (new in api-v0.6, no
  behaviour change) how a client retries a join.
- [`server-lifecycle.md`](server-lifecycle.md) (api-v0.6): what a game server does, from spawn to
  exit. The agent's localhost API, the UDP join, and the result report. **api-v0.6 is breaking
  for game servers**: the result goes to the agent, the allocation carries no result URL or
  token, and a server answers `HELLO` before its allocation (`DENIED not allocated`). It changes
  nothing a player client calls.

All four are written. A change to any of them follows the rules in AGENTS.md.

The fleet agent routes (`/v1/fleet/…`) are in `openapi.yaml` too, but their only client is this
repo's `fleetagent`: the engine never calls them, and changing them breaks nothing outside this
repository.

Versions: when a phase closes, the repo is tagged `api-v0.<phase>`. The engine links to a tag,
never to `main`.
