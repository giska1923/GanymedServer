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
}

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

// Get returns the profile, falling back to the default name when no row exists.
func (s *Service) Get(ctx context.Context, accountID string) (Profile, error) {
	names, err := s.DisplayNames(ctx, []string{accountID})
	if err != nil {
		return Profile{}, err
	}
	return Profile{AccountID: accountID, DisplayName: names[accountID]}, nil
}

// SetDisplayName validates and stores a new name: an upsert, because the first rename is also
// the first time the row exists.
func (s *Service) SetDisplayName(ctx context.Context, accountID, name string) (Profile, error) {
	name = strings.TrimSpace(name)
	if !validName.MatchString(name) {
		return Profile{}, ErrInvalidDisplayName
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO profiles (account_id, display_name) VALUES ($1, $2)
		 ON CONFLICT (account_id) DO UPDATE SET display_name = excluded.display_name, updated_at = now()`,
		accountID, name)
	if err != nil {
		return Profile{}, fmt.Errorf("store display name: %w", err)
	}
	return Profile{AccountID: accountID, DisplayName: name}, nil
}

// DisplayNames resolves many accounts in one query, defaulting the ones with no row. Every ID in
// the input is present in the result.
//
// This is the method other modules call instead of joining against profiles. The leaderboard
// declares an interface with exactly this method and never learns this package exists.
func (s *Service) DisplayNames(ctx context.Context, accountIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = DefaultDisplayName(id)
	}
	if len(accountIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT account_id::text, display_name FROM profiles WHERE account_id = ANY($1::uuid[])`,
		accountIDs)
	if err != nil {
		return nil, fmt.Errorf("load display names: %w", err)
	}
	type row struct {
		AccountID   string
		DisplayName string
	}
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, fmt.Errorf("load display names: %w", err)
	}
	for _, r := range stored {
		out[r.AccountID] = r.DisplayName
	}
	return out, nil
}
