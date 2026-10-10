# Connect token contract (api-v0.6)

The token a player presents to a game server to be admitted to a match. Minted by the backend,
verified by the game server (`stubserver` today, `GanymedDedicated` later). Normative.

## Where a player gets one

In the `match.ready` push, and from `GET /v1/matchmaking/ticket` while the ticket is `ready`
(`ticket.server.connect_token`). Every read mints a **fresh** token. Tokens are never stored, so a
player who missed the push or let a token expire just reads the ticket again.

## Format

```
<payload>.<signature>
```

- `payload` is the claims JSON, **base64url without padding** (RFC 4648 §5).
- `signature` is the **Ed25519** signature (RFC 8032) over the ASCII bytes of `payload`, the
  encoded form exactly as received, base64url without padding. 64 bytes before encoding.

The signature covers the encoded text, so a verifier never re-serializes JSON; it checks the bytes
it was given. **Verify the signature before reading any claim.**

### Claims

| Claim | Type | Meaning |
|---|---|---|
| `v` | int | Format version. Currently `1`. Refuse anything else. |
| `match_id` | string (UUID) | The match this admits to |
| `account_id` | string (UUID) | The player admitted |
| `server_addr` | string `host:port` | The server this admits to |
| `iat` | int | Issued at, Unix seconds |
| `exp` | int | Expires, Unix seconds. `iat + 30` |
| `nonce` | string | 128 random bits, base64url. Unique per token |

## Verification rules

A server **admits** a player only if all of these hold, in this order:

1. The token splits into exactly two non-empty parts, and the signature decodes to 64 bytes.
2. The Ed25519 signature verifies with the backend's **public key** (below).
3. `v == 1`.
4. `now <= exp + 5 s` and `now >= iat - 5 s`. 5 s is the leeway for clock skew between the
   backend and the server.
5. `server_addr` equals this server's advertised address.
6. `match_id` equals the match this server was allocated.
7. `account_id` is one of the match's expected players (from the allocation).
8. `nonce` has not been seen before. Remember nonces at least until their token's `exp + 5 s`;
   after that, the token is refused as expired anyway. **This is the server's job:** the
   signature cannot stop the same valid token being presented twice.

**Retrying a join (since `api-v0.6`).** A client whose `HELLO` went unanswered reads the ticket
again for a **fresh** token. It never resends the same one: if the server admitted the player and
only the `WELCOME` was lost, the same token again is a replay under rule 8 and is refused. A fresh
token has a fresh nonce, and a server admits an already-admitted player again.

netcode.io answers this differently: the same token from the **same source address** is answered
again, and only a different address counts as a replay. That was considered on 2026-10-10 and
declined. The engine's client already retries with fresh tokens (its O5a), so remembering an
address with every nonce would buy nothing a client uses.

A token that fails any rule is refused, and the player is not admitted. The reason may be reported
back (see `server-lifecycle.md`, *Players*), but a client must not rely on its text.

## The public key

The game server receives the key from its fleet agent on its command line (`--public-key`,
base64url, 32 bytes). The agent receives it from the backend in every heartbeat response. The
backend holds the private key (`GS_CONNECT_TOKEN_KEY`, an Ed25519 seed). **Game servers never see
the private key.** A compromised server can admit players to its own match, but it can never mint a
token for any other server or match.

## Reference implementation

[`internal/connecttoken`](../../internal/connecttoken/connecttoken.go): `Mint` and `Verify`, with
tests for every rule above. `cmd/stubserver` uses `Verify` and implements rules 7 and 8.
