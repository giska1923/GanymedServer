// Package auth is identity: device login, access tokens, and rotating refresh tokens.
//
// It owns the accounts, devices, refresh_families and refresh_tokens tables. Nothing else reads
// them; other modules ask this package, or get the account ID from the request context.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidRefreshToken covers every refresh failure: unknown, expired, already used, or from a
// revoked login. The client's response is the same for all of them (log in again), and telling
// a caller which one it hit would tell a thief whether a stolen token was ever real.
var ErrInvalidRefreshToken = errors.New("invalid refresh token")

type Config struct {
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
}

type Service struct {
	pool       *pgxpool.Pool
	access     accessTokens
	refreshTTL time.Duration
	log        *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config, log *slog.Logger) *Service {
	return &Service{
		pool:       pool,
		access:     accessTokens{secret: cfg.JWTSecret, ttl: cfg.AccessTokenTTL, now: time.Now},
		refreshTTL: cfg.RefreshTokenTTL,
		log:        log,
	}
}

// Session is what a successful login or refresh hands back.
type Session struct {
	AccountID      string
	AccessToken    string
	AccessTokenTTL time.Duration
	RefreshToken   string
}

type Account struct {
	ID        string
	CreatedAt time.Time
}

// LoginDevice signs in with a device ID, creating the account the first time the device is seen.
// Each login starts a new refresh family, so two devices (or two logins) are independent sessions.
func (s *Service) LoginDevice(ctx context.Context, deviceID string) (Session, error) {
	accountID, err := s.findOrCreateAccount(ctx, hashSecret(deviceID))
	if err != nil {
		return Session{}, err
	}

	var refresh string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var familyID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO refresh_families (account_id) VALUES ($1) RETURNING id::text`,
			accountID).Scan(&familyID); err != nil {
			return fmt.Errorf("create refresh family: %w", err)
		}
		token, err := s.insertRefreshToken(ctx, tx, familyID, accountID)
		refresh = token
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.session(accountID, refresh)
}

// findOrCreateAccount is safe against two first logins with the same device racing each other.
//
// The fast path is a plain lookup. The slow path creates an account and claims the device with
// ON CONFLICT DO NOTHING: if another transaction claimed it first, Postgres makes this insert wait
// for that one to commit and then does nothing, so this transaction rolls back its now-orphaned
// account and reads the winner's. The unique key on devices is what makes it correct; the code
// only has to notice that it lost.
func (s *Service) findOrCreateAccount(ctx context.Context, deviceHash []byte) (string, error) {
	const lookup = `SELECT account_id::text FROM devices WHERE device_hash = $1`

	var accountID string
	err := s.pool.QueryRow(ctx, lookup, deviceHash).Scan(&accountID)
	if err == nil {
		return accountID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up device: %w", err)
	}

	lost := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO accounts DEFAULT VALUES RETURNING id::text`).Scan(&accountID); err != nil {
			return fmt.Errorf("create account: %w", err)
		}
		err := tx.QueryRow(ctx,
			`INSERT INTO devices (device_hash, account_id) VALUES ($1, $2)
			 ON CONFLICT (device_hash) DO NOTHING
			 RETURNING account_id::text`,
			deviceHash, accountID).Scan(&accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			lost = true
			return errLostRace
		}
		return err
	})
	if lost {
		if err := s.pool.QueryRow(ctx, lookup, deviceHash).Scan(&accountID); err != nil {
			return "", fmt.Errorf("look up device after losing creation race: %w", err)
		}
		return accountID, nil
	}
	if err != nil {
		return "", fmt.Errorf("create account: %w", err)
	}
	s.log.Info("account created", "account_id", accountID)
	return accountID, nil
}

// errLostRace only exists to make BeginFunc roll back.
var errLostRace = errors.New("device claimed by a concurrent login")

// Refresh exchanges a refresh token for a new session, consuming the token.
//
// Rotation with reuse detection (RFC 9700, the OAuth 2.0 Security BCP): a refresh token works
// exactly once. Presenting one that was already consumed means two parties hold it, and the
// server cannot tell which is the thief, so it revokes the whole family. Both parties are logged
// out, and the legitimate one logs in again.
//
// A consequence clients must respect: two concurrent refreshes with the same token are, to the
// server, indistinguishable from theft. The first wins; the second finds the token consumed and
// revokes the family, which also kills the winner's new token. A client must serialize its
// refreshes. Some providers soften this with a short grace window for a just-rotated token; that
// is deliberately not done here, so the failure is visible rather than papered over.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	hash := hashSecret(refreshToken)

	var (
		accountID, familyID, next string
		reused                    bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The consuming UPDATE is the whole concurrency story. Under READ COMMITTED, a second
		// transaction updating the same row blocks on the first's row lock, then re-checks the
		// WHERE clause against the committed row, finds consumed_at set, and matches nothing.
		// Exactly one caller can consume a token, decided by the database, not by Go code.
		err := tx.QueryRow(ctx,
			`UPDATE refresh_tokens t SET consumed_at = now()
			 FROM refresh_families f
			 WHERE t.token_hash = $1
			   AND t.consumed_at IS NULL
			   AND t.expires_at > now()
			   AND f.id = t.family_id
			   AND f.revoked_at IS NULL
			 RETURNING t.family_id::text, t.account_id::text`,
			hash).Scan(&familyID, &accountID)
		if err == nil {
			next, err = s.insertRefreshToken(ctx, tx, familyID, accountID)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("consume refresh token: %w", err)
		}

		// Not consumable. Find out whether that is reuse, which must revoke the family and
		// commit that revocation even though this request fails.
		var consumed, revoked bool
		err = tx.QueryRow(ctx,
			`SELECT t.consumed_at IS NOT NULL, f.revoked_at IS NOT NULL, t.family_id::text, t.account_id::text
			 FROM refresh_tokens t JOIN refresh_families f ON f.id = t.family_id
			 WHERE t.token_hash = $1`,
			hash).Scan(&consumed, &revoked, &familyID, &accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // unknown token
		}
		if err != nil {
			return fmt.Errorf("inspect refresh token: %w", err)
		}
		if consumed && !revoked {
			if _, err := tx.Exec(ctx,
				`UPDATE refresh_families SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`,
				familyID); err != nil {
				return fmt.Errorf("revoke refresh family: %w", err)
			}
			reused = true
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if reused {
		s.log.Warn("refresh token reuse detected: family revoked", "account_id", accountID, "family_id", familyID)
	}
	if next == "" {
		return Session{}, ErrInvalidRefreshToken
	}
	return s.session(accountID, next)
}

// Account returns the account for an ID taken from a verified access token.
func (s *Service) Account(ctx context.Context, accountID string) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, created_at FROM accounts WHERE id = $1`, accountID).Scan(&a.ID, &a.CreatedAt)
	if err != nil {
		return Account{}, fmt.Errorf("load account: %w", err)
	}
	return a, nil
}

// VerifyAccessToken returns the account ID a valid access token names.
func (s *Service) VerifyAccessToken(token string) (string, error) {
	return s.access.verify(token)
}

// insertRefreshToken mints a token in family and stores only its hash. The expiry is computed by
// Postgres, so every time comparison in this package uses one clock: the database's.
func (s *Service) insertRefreshToken(ctx context.Context, tx pgx.Tx, familyID, accountID string) (string, error) {
	token, err := newRefreshToken()
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO refresh_tokens (token_hash, family_id, account_id, expires_at)
		 VALUES ($1, $2, $3, now() + $4::bigint * interval '1 second')`,
		hashSecret(token), familyID, accountID, int64(s.refreshTTL/time.Second))
	if err != nil {
		return "", fmt.Errorf("store refresh token: %w", err)
	}
	return token, nil
}

func (s *Service) session(accountID, refresh string) (Session, error) {
	access, err := s.access.issue(accountID)
	if err != nil {
		return Session{}, err
	}
	return Session{AccountID: accountID, AccessToken: access, AccessTokenTTL: s.access.ttl, RefreshToken: refresh}, nil
}
