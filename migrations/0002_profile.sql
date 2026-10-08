-- Owned by internal/profile.
--
-- A row exists only once a player has changed something; until then the profile is derived from
-- the account ID (see profile.DefaultDisplayName). That is why there is no row created "at
-- account creation": doing so would put this module's write inside auth's transaction.
--
-- account_id is deliberately NOT a foreign key to accounts. That table belongs to another module,
-- and a cross-module foreign key is exactly the coupling that would stop the two ever being
-- split. The guarantee comes from elsewhere: account IDs only ever reach this module from a
-- verified access token.
CREATE TABLE profiles (
    account_id   uuid        PRIMARY KEY,
    display_name text        NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
