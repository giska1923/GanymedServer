-- Owned by internal/auth. No other module reads or writes these tables.

CREATE TABLE accounts (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A device ID is the only credential (see docs/ToDo/BACKEND.md), so it is stored the way a
-- credential is: as a SHA-256 hash. A fast hash is correct here, where it would be wrong for a
-- password: slow hashes (bcrypt, argon2) exist to make guessing low-entropy human secrets
-- expensive, and a random 128-bit device ID cannot be guessed at any speed.
CREATE TABLE devices (
    device_hash bytea       PRIMARY KEY,
    account_id  uuid        NOT NULL REFERENCES accounts (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A family is one login's chain of refresh tokens. Reuse of a consumed token revokes the family,
-- which ends that login everywhere without touching the account's other logins.
CREATE TABLE refresh_families (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid        NOT NULL REFERENCES accounts (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE TABLE refresh_tokens (
    token_hash  bytea       PRIMARY KEY,
    family_id   uuid        NOT NULL REFERENCES refresh_families (id),
    account_id  uuid        NOT NULL REFERENCES accounts (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz
);

CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
