// Package profile is the player-facing identity: display names, for now.
//
// It owns the profiles table. A row exists only once a player renames; until then the profile is
// derived from the account ID, so no row has to be created when the account is (which would put
// this module's write inside auth's transaction).
package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidDisplayName is returned for a name that fails validation; its text is safe to show.
var ErrInvalidDisplayName = errors.New("display name must be 3 to 24 characters: letters, digits, space, '-', '_' or '.', starting and ending with a letter or digit")

// ASCII only, on purpose: the engine's HUD renders through RmlUi with a Latin font, and a name it
// cannot draw is worse than a name it refused. Widening this is a contract change.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.\-]{1,22}[A-Za-z0-9]$`)

type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

type Profile struct {
	AccountID   string
	DisplayName string
	Rating      int
}

// DefaultRating is every player's skill rating until match results change it (B5). 1500 is the
// conventional Elo starting point. It matches the column default in 0004_profile_rating.sql.
const DefaultRating = 1500

// DefaultDisplayName is the name of an account that never chose one: "Player-" and the first six
// hex digits of its ID. Deterministic, so it needs no stored row and is the same everywhere.
// Not unique, and does not need to be: a display name is a label, never an identity.
func DefaultDisplayName(accountID string) string {
	hex := strings.ReplaceAll(accountID, "-", "")
	if len(hex) > 6 {
		hex = hex[:6]
	}
	return "Player-" + strings.ToUpper(hex)
}

// Get returns the profile, falling back to the defaults when no row exists.
func (s *Service) Get(ctx context.Context, accountID string) (Profile, error) {
	profiles, err := s.load(ctx, []string{accountID})
	if err != nil {
		return Profile{}, err
	}
	return profiles[accountID], nil
}

// SetDisplayName validates and stores a new name: an upsert, because the first rename is also
// the first time the row exists.
func (s *Service) SetDisplayName(ctx context.Context, accountID, name string) (Profile, error) {
	name = strings.TrimSpace(name)
	if !validName.MatchString(name) {
		return Profile{}, ErrInvalidDisplayName
	}
	// RETURNING the rating, so the profile returned is the stored one, not a half-built value.
	p := Profile{AccountID: accountID, DisplayName: name}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO profiles (account_id, display_name) VALUES ($1, $2)
		 ON CONFLICT (account_id) DO UPDATE SET display_name = excluded.display_name, updated_at = now()
		 RETURNING rating`,
		accountID, name).Scan(&p.Rating)
	if err != nil {
		return Profile{}, fmt.Errorf("store display name: %w", err)
	}
	return p, nil
}

// DisplayNames resolves many accounts in one query, defaulting the ones with no row. Every ID in
// the input is present in the result.
//
// This is the method other modules call instead of joining against profiles. The leaderboard
// declares an interface with exactly this method and never learns this package exists.
func (s *Service) DisplayNames(ctx context.Context, accountIDs []string) (map[string]string, error) {
	profiles, err := s.load(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(profiles))
	for id, p := range profiles {
		out[id] = p.DisplayName
	}
	return out, nil
}

// Ratings is DisplayNames for skill ratings: one query, every ID present, DefaultRating where no
// row exists. Matchmaking reads ratings through this, never from the table.
func (s *Service) Ratings(ctx context.Context, accountIDs []string) (map[string]int, error) {
	profiles, err := s.load(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(profiles))
	for id, p := range profiles {
		out[id] = p.Rating
	}
	return out, nil
}

// ApplyRatingChange adds delta to every listed player's rating, once per (match, player), however
// many times it is called. Matchmaking calls it after recording a result, and may call it again
// after a crash or a repeated report. The rating_changes primary key makes the repeats no-ops.
//
// It is one transaction, but only over this module's tables. Matchmaking records the result in
// its own table, in its own transaction, before calling this. The two are not atomic together,
// and they do not need to be: a crash between them leaves a recorded result whose ratings are not
// applied yet, and the next retry applies them exactly once. That is how two modules stay
// consistent without a transaction spanning both: an idempotent consumer and a retrying producer.
func (s *Service) ApplyRatingChange(ctx context.Context, matchID string, accountIDs []string, delta int) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, id := range accountIDs {
			tag, err := tx.Exec(ctx,
				`INSERT INTO rating_changes (match_id, account_id, delta) VALUES ($1, $2, $3)
				 ON CONFLICT (match_id, account_id) DO NOTHING`,
				matchID, id, delta)
			if err != nil {
				return fmt.Errorf("record rating change: %w", err)
			}
			if tag.RowsAffected() == 0 {
				continue // already applied for this match: the whole point
			}
			// A player with no profile row gets one now, carrying the default name explicitly
			// and the default rating plus the change.
			if _, err := tx.Exec(ctx,
				`INSERT INTO profiles (account_id, display_name, rating) VALUES ($1, $2, $3)
				 ON CONFLICT (account_id) DO UPDATE SET rating = profiles.rating + $4, updated_at = now()`,
				id, DefaultDisplayName(id), DefaultRating+delta, delta); err != nil {
				return fmt.Errorf("apply rating change: %w", err)
			}
		}
		return nil
	})
}

// load reads the profiles that exist and fills in the defaults for the rest.
func (s *Service) load(ctx context.Context, accountIDs []string) (map[string]Profile, error) {
	out := make(map[string]Profile, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = Profile{AccountID: id, DisplayName: DefaultDisplayName(id), Rating: DefaultRating}
	}
	if len(accountIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT account_id::text, display_name, rating FROM profiles WHERE account_id = ANY($1::uuid[])`,
		accountIDs)
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Profile])
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	for _, p := range stored {
		out[p.AccountID] = p
	}
	return out, nil
}
