-- Owned by internal/matchmaking. A match lives in Redis while it is formed, allocated and played;
-- its result is durable, so it lands here. The first report wins (primary key); a repeated report
-- reads this row back, which is what makes the result endpoint idempotent, and stores the rating
-- change it computed so that a retry applies the same change, not one recomputed from ratings that
-- may already have moved.
CREATE TABLE match_results (
    match_id      uuid        PRIMARY KEY,
    mode          text        NOT NULL,
    outcome       text        NOT NULL CHECK (outcome IN ('victory', 'defeat')),
    players       uuid[]      NOT NULL,
    rating_change integer     NOT NULL,
    reported_at   timestamptz NOT NULL DEFAULT now()
);
