-- Owned by internal/profile. One row per (match, player): the rating change a match result
-- applied. The primary key is what makes applying a result idempotent: a retry, after a crash or
-- a game server repeating its report, inserts nothing and so changes no rating twice. This is the
-- "idempotent consumer" pattern: the consumer (profile) remembers what it has processed, keyed by
-- the producer's (matchmaking's) ID, so the producer may deliver more than once.
CREATE TABLE rating_changes (
    match_id   uuid        NOT NULL,
    account_id uuid        NOT NULL,
    delta      integer     NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (match_id, account_id)
);
