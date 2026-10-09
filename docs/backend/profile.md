# Profile: display names and ratings

[`internal/profile`](../../internal/profile) owns the `profiles` and `rating_changes` tables
([`0002_profile.sql`](../../migrations/0002_profile.sql),
[`0004_profile_rating.sql`](../../migrations/0004_profile_rating.sql),
[`0005_profile_rating_changes.sql`](../../migrations/0005_profile_rating_changes.sql)).

| Route | Does |
|---|---|
| `GET /v1/me/profile` | `{account_id, display_name, rating}` |
| `PATCH /v1/me/profile` | Change `display_name`; returns the stored profile |

## Lazy profiles

**A profile row exists only once it differs from the defaults**: the player renamed, or a match
result changed their rating. Until then the profile is derived: the
display name is `Player-` followed by the first six hex digits of the account ID, uppercased
(`Player-7F3A9C`). It is deterministic, so every reader computes the same default, and it needs
no stored row.

The original plan created a profile "at account creation". That would put this module's write
inside auth's account transaction, and a transaction never spans two modules. A post-commit
hook in auth could lose the profile if the process died between the two commits, and then a
lazy fallback would be needed anyway. Making lazy the *only* path removes the hook, the
failure mode and the coupling. The first rename, or the first rating change, is an upsert, which
is also the first time the row exists.

Display names are **labels, not identities**: they are not unique, and nothing looks a player up
by name. The defaults can collide (six hex digits is 16.7M values), and that is fine.

## Validation

Trimmed, then 3–24 characters: ASCII letters, digits, space, `-`, `_` and `.`, starting and
ending with a letter or digit. **ASCII only**, because the engine's HUD renders through RmlUi
with a Latin font, and a name it cannot draw is worse than one that was refused. Widening the
set is a contract change.

## `DisplayNames`: how other modules get names

```go
func (s *Service) DisplayNames(ctx context.Context, accountIDs []string) (map[string]string, error)
```

One query (`WHERE account_id = ANY($1::uuid[])`). Every requested ID is in the result, with a
default where there is no row. The leaderboard declares an interface with exactly this method
and is handed a `*profile.Service` in `main`. This is the module rule in code: other modules
*ask* for names, and never join against `profiles`. The cost is two queries where a join would
be one. The benefit is that the two modules could be split apart without rewriting either.

## Ratings

`rating` is the skill rating matchmaking groups players by ([matchmaking.md](matchmaking.md)).
It defaults to **1500** (`DefaultRating`, the conventional Elo starting point), and the column
default in `0004_profile_rating.sql` is the same value, so a row created by a rename starts where
a row-less player already was. Match results change it (below).

```go
func (s *Service) Ratings(ctx context.Context, accountIDs []string) (map[string]int, error)
```

It is the same shape as `DisplayNames` and runs the same single query (`load`): every requested ID
is present, defaulted where there is no row. Matchmaking declares an interface with this method
and the next one.

```go
func (s *Service) ApplyRatingChange(ctx context.Context, matchID string, accountIDs []string, delta int) error
```

Adds `delta` to each player's rating **once per (match, player), however often it is called**.
Matchmaking calls it after recording a result, and calls it again when a game server repeats its
report after a crash or a lost response ([matchmaking.md](matchmaking.md#results)). One
transaction, per player:

1. `INSERT INTO rating_changes (match_id, account_id, delta) … ON CONFLICT DO NOTHING`.
2. Only if that inserted a row: upsert the profile with `rating = profiles.rating + delta`, or,
   for a player with no row yet, insert one with the default name and `1500 + delta`.

`rating_changes` is the **idempotency record**: its primary key `(match_id, account_id)` is what
makes a repeat a no-op, and the rows double as a per-player rating history. Recording "this
change was applied" in the same transaction as the change is the whole trick. If the record were
written separately, a crash between the two writes would either lose the change or apply it
twice on retry. This is the inbox half of the transactional-outbox/inbox pair; the result is not
transactional across modules, but each module's half is.

`rating = profiles.rating + $delta` is an increment computed inside Postgres, so two results for
the same player committing at once both land: the row lock serialises them and the second reads
the first's value. Computing the new rating in Go (`read, add, write`) would lose one of them,
the classic lost update.

`TestApplyRatingChangeIsIdempotent` applies the same match three times and a second match once, for a
player with a row and one without.

## No cross-module foreign keys

`profiles.account_id`, `best_scores.account_id` and `score_submissions.account_id` are plain `uuid`
columns, **not** `REFERENCES accounts`. A foreign key across modules is a schema-level
dependency: once it exists, the two modules' tables can never live in separate databases, and
it is exactly the coupling the module rule exists to prevent.

What replaces it: account IDs reach these modules **only** from a verified access token
(`auth.AccountID(ctx)`), so a write can never name an account that was not real when the token
was issued. Accounts are never deleted today. If deletion is ever added, it becomes a
cross-module operation that each module handles for its own tables, which is how
service-oriented systems do it (a "user deleted" event, not `ON DELETE CASCADE`).

Within a module, foreign keys stay: `best_scores.board REFERENCES leaderboards` is a leaderboard
table pointing at a leaderboard table.

## Wiring

Handlers take auth's middleware as a plain `func(http.Handler) http.Handler`, the standard Go
middleware shape, and read the account with `auth.AccountID`. That imports `auth`, and there is
no cycle: `auth` imports neither `profile` nor `leaderboard`.
