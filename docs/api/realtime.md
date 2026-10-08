# Realtime contract (api-v0.3)

The push channel between the backend and a signed-in client. Normative, like
[`openapi.yaml`](openapi.yaml): the code implements this file.

## The one rule

**Pushes are nudges, not state.** A push tells the client that something changed ("you have an
invite", "your party changed"). The HTTP API says *what* the state is (`GET /v1/party`,
`GET /v1/party/invites`). Delivery is **at most once**: a push sent while the client is
disconnected, reconnecting or slow is **lost, never replayed**. So a client:

- re-fetches the state it shows over HTTP **every time it (re)connects**, and
- treats every push as "re-fetch this", never as the state itself.

A client written this way is correct whether or not pushes arrive. Pushes only make it prompt.

## Connecting

```
GET /v1/realtime
Authorization: Bearer <access token>
Upgrade: websocket
```

An ordinary RFC 6455 WebSocket upgrade, on the same host and port as the HTTP API.

- **Authentication happens once, at the upgrade.** A missing or invalid token is a normal HTTP
  401 with a problem body (`unauthorized` or `token-expired`), and no upgrade. Once open, the
  socket stays authenticated for its lifetime, even past the access token's expiry. Refreshing
  the token does not require reconnecting.
- **One socket per account.** A new connection for the same account, to any replica,
  **replaces** the old one, which is closed with code `4001`.
- **The client sends no data messages.** The socket is server → client only; every action is an
  HTTP call. A client data message closes the socket with `1008`. The client must answer
  WebSocket **pings** with pongs, which every WebSocket library does automatically. The server
  pings every 10 s.
- No `Origin` header is expected. A native client sends none. A browser-style cross-origin
  request is refused.

## Presence

While connected, a player is **online**. When their socket closes they are **away** for 30
seconds, then **offline**. If the replica holding the socket dies without closing it, the player
stays online for up to 30 s, then goes straight to offline. Status appears in `GET /v1/party`
(`members[].status`): `online`, `away` or `offline`.

**Away is the reconnection grace.** A party member who reconnects while away keeps their seat. A
member who reaches offline is removed from their party (see `party.removed`, `party.updated`).
Expect removal within about 35 s of a disconnect: the 30 s grace, plus the backend's 5 s sweep.

## Messages (server → client)

Every message is one JSON text frame:

```json
{ "type": "party.invite", "id": "6f2a…", "payload": { … } }
```

| Field | |
|---|---|
| `type` | What happened. Clients **ignore types they do not know**: new types are added without a version bump. |
| `id` | A UUID unique to this message, for logs and de-duplication. |
| `payload` | Type-specific, below. May be omitted. |

| `type` | Sent to | `payload` | Client should |
|---|---|---|---|
| `party.invite` | the invited player | `{ "party_id", "from": { "account_id", "display_name" }, "expires_at" }` | show the invite; `GET /v1/party/invites` is the list of record |
| `party.updated` | every member of a party whose membership or leader changed | `{ "party_id" }` | `GET /v1/party` |
| `party.removed` | a player taken out of their party by someone else or by the grace expiring | `{ "party_id", "reason": "kicked" \| "disconnected" }` | drop the party from the UI; `GET /v1/party` returns `null` |

A player who leaves on their own (`POST /v1/party/leave`) gets no push: they made the call.

## Close codes

| Code | Meaning | Client should |
|---|---|---|
| `1000` | Normal closure | |
| `1001` | Going away: the replica is shutting down | reconnect (it lands on a live replica), then re-fetch state |
| `1008` | Policy violation: the client sent a data message, or **could not keep up** and its send queue filled. A client that has stopped reading usually never sees this frame (it cannot be delivered past a full TCP window), so the connection simply drops (`1006`) | fix the client; reconnect, then re-fetch |
| `4001` | Replaced by a newer connection for the same account | **do not** reconnect automatically: another session of this account is active |
| none (`1006`) | The connection dropped | reconnect with backoff and jitter, then re-fetch |
