# The contract

Everything outside this repository depends on the files in this folder, and nothing else here.
The code implements these specs; the specs are not generated from the code. Rules are in
[AGENTS.md](../../AGENTS.md#the-contract-is-the-one-thing-two-repositories-depend-on).

| File | Specifies | Consumers | Written in |
|---|---|---|---|
| `openapi.yaml` | Every HTTP route: paths, JSON shapes, problem-detail error types | engine O1–O3, `gscli` | B1, extended each phase |
| `realtime.md` | The WebSocket envelope, message types, close codes | engine O4 | B3 |
| `connect-token.md` | Token payload, encoding, Ed25519 signature, validation rules | `stubserver`, engine O5 | B5 |
| `server-lifecycle.md` | Agent ↔ game server: states, health, allocation, result credential | `stubserver`, engine O5 | B5 |

**Written so far:**

- [`openapi.yaml`](openapi.yaml), version 0.4.0: B1 (health, device login, refresh, `/me`, problem
  types, refresh rotation rules), B2 (profile, leaderboards, `Idempotency-Key`, integer scores),
  B3 (parties, the `/v1/realtime` upgrade) and B4 (matchmaking tickets and their states, the
  profile's `rating`).
- [`realtime.md`](realtime.md) (api-v0.4): the push socket. Messages (party and, since B4,
  `ticket.updated`, `match.found`, `ticket.failed`), presence, close codes, and the rule that
  pushes are nudges and HTTP is the state of record.

The others are written at the start of their phase, before the code. See
[BACKEND.md](../ToDo/BACKEND.md).

Versions: when a phase closes, the repo is tagged `api-v0.<phase>`. The engine links to a tag,
never to `main`.
