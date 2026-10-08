-- Owned by internal/leaderboard. account_id columns are not foreign keys; see 0002_profile.sql.

-- Boards are declared, not created by the first score: a client typo must be a 404, not a new
-- board. Adding a board is a migration.
CREATE TABLE leaderboards (
    id         text        PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO leaderboards (id) VALUES ('proving-ground');

-- The event log: every accepted submission, kept for history and anti-cheat. best_scores is a
-- projection of it and could be rebuilt from it.
CREATE TABLE score_submissions (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    board        text        NOT NULL REFERENCES leaderboards (id),
    account_id   uuid        NOT NULL,
    score        bigint      NOT NULL,
    submitted_at timestamptz NOT NULL DEFAULT now()
);

-- The projection: one row per player per board, holding their best.
CREATE TABLE best_scores (
    board       text        NOT NULL REFERENCES leaderboards (id),
    account_id  uuid        NOT NULL,
    score       bigint      NOT NULL,
    achieved_at timestamptz NOT NULL,
    PRIMARY KEY (board, account_id)
);

-- Serves both leaderboard queries:
--   top N:  WHERE board = $1 ORDER BY score DESC, achieved_at   → an index scan that stops at N
--   rank:   WHERE board = $1 AND score > $2                     → an index-only count
-- achieved_at breaks ties in listing order: whoever reached a score first is listed first.
CREATE INDEX best_scores_rank_idx ON best_scores (board, score DESC, achieved_at);

-- Idempotency keys for score submission (see leaderboard.Service.Submit). Scoped per account,
-- so one client's keys can never collide with, or reveal, another's. The request (board, score)
-- is stored to detect a key reused for a different request, and the response to replay it.
--
-- The response columns are nullable because the row is inserted FIRST, before the work: that
-- insert is what makes a concurrent duplicate wait. They are filled in later in the same
-- transaction, so no committed row ever has them NULL.
CREATE TABLE score_idempotency_keys (
    account_id    uuid        NOT NULL,
    key           uuid        NOT NULL,
    board         text        NOT NULL,
    score         bigint      NOT NULL,
    response_rank bigint,
    response_best bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, key)
);

CREATE INDEX score_idempotency_keys_created_idx ON score_idempotency_keys (created_at);
