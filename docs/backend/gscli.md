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

Refresh reuse: copy the profile file, `refresh`, copy the old file back, then `refresh` again. The
replay gets 401 `invalid-refresh-token`, the newest token is revoked with it, and the backend logs
`refresh token reuse detected: family revoked`.
