# Auth: device identity and sessions

[`internal/auth`](../../internal/auth) is identity. It owns the `accounts`, `devices`,
`refresh_families` and `refresh_tokens` tables
([`0001_auth.sql`](../../migrations/0001_auth.sql)). Other modules never read them. They get the
caller's account ID from the request context (`auth.AccountID(ctx)`), set by `RequireAuth`.

Routes (normative shapes in [`openapi.yaml`](../api/openapi.yaml)):

| Route | Does |
|---|---|
| `POST /v1/auth/device` | Signs in with a device ID; creates the account on first sight; starts a new session |
| `POST /v1/auth/refresh` | Exchanges a refresh token for a new session, consuming it |
| `GET /v1/me` | The signed-in account (the first authenticated route) |

## How production systems do this, and where this diverges

A production game backend rarely authenticates the human itself. Steam, PSN or Epic does, and
the backend verifies a platform ticket, then issues **its own** short-lived session token. It
almost never stores a password. What it does own is the session model: access tokens,
refresh tokens, revocation. That part is what this module builds.

The divergence is the first credential. Instead of a platform ticket, the client presents a
**device ID**: a random UUID it generated once. Anyone holding it *is* that player. That is
acceptable for a learning project with no users, and it is the first thing to replace if this
ever faces the internet. Nakama offers the same "device authentication" as its lowest tier, for
the same reason: zero-friction guest accounts.

## The session model: JWT access token plus opaque refresh token

| | Access token | Refresh token |
|---|---|---|
| Form | JWT, HS256 | 256 random bits, base64url, **opaque** |
| Lifetime | 15 min | 30 days |
| Verified by | signature alone, **no database hit** | database lookup |
| Revocable before expiry | **no** | yes |
| Stored server-side | not at all | SHA-256 hash only |

This is the standard hybrid, and the reason is the tradeoff. A JWT can be verified by any
process holding the key, without a lookup. That becomes a real property once there are two
replicas (B3) and an agent (B5). The price is that **a JWT cannot be revoked**: it is valid
until it expires. So it is kept short-lived, and the long-lived credential is the refresh token,
which is stateful and therefore revocable.

**HS256, one shared secret**, because the issuer and the verifier are the same binary.
Asymmetric signing is for when something verifies without being allowed to mint; that is B5's
connect tokens.

**The algorithm is pinned by the server** (`jwt.WithValidMethods`). Without that, the token's own
`alg` header picks how it is verified. `alg: none` skips the signature, and algorithm confusion
tricks an RSA verifier into using its public key as an HMAC secret. `tokens_test.go` proves that
`none`, HS512 with the right secret, a wrong secret, a wrong issuer and a missing expiry are all
rejected.

Claims are `iss` (`ganymedserver`), `sub` (account ID), `iat` and `exp`. **Clients must treat the
token as opaque**, and the contract says so. A client that reads its own claims starts
trusting them.

## Refresh rotation and reuse detection

This follows RFC 9700 (the OAuth 2.0 Security Best Current Practice):

- Each login starts a **family**. Every refresh consumes the presented token and issues the next
  one in the same family.
- **A token works once.** Presenting a consumed token means two parties hold it, and the server
  cannot tell which one is the thief. So it **revokes the whole family**: the replayed token, and
  every newer one, including the legitimate holder's. Both are logged out; the legitimate one
  signs in again. A `WARN` log records the account and family, never the token.
- Every refresh failure is the same 401 `invalid-refresh-token`, whether the token is unknown,
  expired, consumed or revoked. The remedy is identical, and distinguishing them would tell a
  thief whether a stolen token was ever real.
- Other logins (other families) are unaffected.

### The concurrency is the database's job

The consuming statement is:

```sql
UPDATE refresh_tokens t SET consumed_at = now()
FROM refresh_families f
WHERE t.token_hash = $1 AND t.consumed_at IS NULL AND t.expires_at > now()
  AND f.id = t.family_id AND f.revoked_at IS NULL
RETURNING ...
```

Under `READ COMMITTED`, a second transaction updating the same row **blocks on the first's row
lock**. When the first commits, the second re-evaluates its `WHERE` against the committed row,
sees `consumed_at` set, and matches nothing. Exactly one caller consumes a token, and no Go code
decides it.

**The consequence clients must respect:** two concurrent refreshes with one token are, to the
server, indistinguishable from theft. The loser finds the token consumed and revokes the family,
which kills the winner's new token too. `TestConcurrentRefreshExactlyOneWinsThenFamilyRevoked`
pins that down with 10 concurrent refreshes: one success, then the winner's token is revoked. Some
providers (Auth0, Okta) soften this with a grace window for a just-rotated token. That is
deliberately not done here, so a client bug is visible rather than masked. The engine plan
(`docs/ToDo/ONLINE.md`, O2) requires serialized refreshes because of it.

## Device login, and the first-login race

`findOrCreateAccount` looks the device up first. On a miss, one transaction creates an account
and claims the device with `INSERT … ON CONFLICT (device_hash) DO NOTHING RETURNING`. If another
login claimed it first, Postgres makes this insert **wait for that transaction to commit**, then
does nothing. The loser rolls back its now-orphaned account and reads the winner's.
`TestConcurrentFirstLoginCreatesOneAccount` runs 16 simultaneous first logins: one account ID
and one `accounts` row.

## Secrets at rest

Device IDs and refresh tokens are stored as **SHA-256 hashes**, never raw. A fast hash is right
here, where it would be wrong for passwords. Slow hashes (bcrypt, argon2) exist to make
guessing low-entropy human secrets expensive; a random 128- or 256-bit value cannot be guessed
at any speed, so the only threat is a database leak, and any one-way hash answers it.

Nothing secret is logged. Request logs carry only the route pattern, never the path or body. A
rejected access token's reason is logged at `debug`, and the reason never contains the token.
Checked: a debug-level run through login, `/me`, refresh, reuse and an `alg: none` token, scanned
for every device ID, access token and refresh token issued during it, found none in the logs.

## Timestamps

pgx returns `timestamptz` in the **process's** local zone, so the same instant serialized
differently from the container (UTC) and a developer's machine (`+02:00`). Responses convert to
UTC before encoding. Any new handler that returns a stored time must do the same.

## Validation

`device_id` must be 32–128 characters. The floor is an entropy floor (a UUID is 36), since the
ID is the credential; the ceiling just bounds input.
