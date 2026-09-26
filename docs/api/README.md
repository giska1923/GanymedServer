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

**None are written yet.** Each is written at the start of its phase, before the code. See
[BACKEND.md](../ToDo/BACKEND.md).

Versions: when a phase closes, the repo is tagged `api-v0.<phase>`. The engine links to a tag,
never to `main`.
