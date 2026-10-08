-- Owned by internal/profile. The skill rating matchmaking groups players by (B4), changed by match
-- results (B5). Profiles stay lazy: a player with no row has the default, DefaultRating in Go,
-- which this column default matches so that a row created by a rename starts at the same value.
ALTER TABLE profiles ADD COLUMN rating integer NOT NULL DEFAULT 1500;
