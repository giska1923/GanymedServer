package auth

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/db/dbtest"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(dbtest.New(t), Config{
		JWTSecret:       testSecret,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: time.Hour,
	}, slog.New(slog.DiscardHandler))
}

const deviceA = "8b1f3a52-0d7c-4b8e-9a51-2f6c7d1e0a93"

func TestLoginSameDeviceSameAccount(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	first, err := s.LoginDevice(ctx, deviceA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.LoginDevice(ctx, deviceA)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != second.AccountID {
		t.Fatalf("same device, two accounts: %s, %s", first.AccountID, second.AccountID)
	}
	other, _ := s.LoginDevice(ctx, "0c5e9d27-6a41-4f3b-8e2d-91b7c4a0f658")
	if other.AccountID == first.AccountID {
		t.Fatal("different devices share an account")
	}
}

// Many first logins of one device at the same instant must converge on one account, and leave no
// orphaned account rows from the losers.
func TestConcurrentFirstLoginCreatesOneAccount(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			sess, err := s.LoginDevice(ctx, deviceA)
			ids[i], errs[i] = sess.AccountID, err
		})
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("login %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("login %d got account %s, login 0 got %s", i, ids[i], ids[0])
		}
	}
	var accounts int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 {
		t.Fatalf("%d account rows, want 1: losers' accounts were not rolled back", accounts)
	}
}

func TestRefreshRotates(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	login, _ := s.LoginDevice(ctx, deviceA)
	next, err := s.Refresh(ctx, login.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if next.RefreshToken == login.RefreshToken {
		t.Fatal("refresh returned the same refresh token")
	}
	if next.AccountID != login.AccountID {
		t.Fatal("refresh changed the account")
	}
	if _, err := s.Refresh(ctx, next.RefreshToken); err != nil {
		t.Fatalf("the rotated token should work once: %v", err)
	}
}

// Presenting a consumed token revokes the family: the thief's token and the legitimate holder's
// newer token both stop working.
func TestRefreshReuseRevokesFamily(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	login, _ := s.LoginDevice(ctx, deviceA)
	legit, err := s.Refresh(ctx, login.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Refresh(ctx, login.RefreshToken); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("reuse: got %v, want ErrInvalidRefreshToken", err)
	}
	if _, err := s.Refresh(ctx, legit.RefreshToken); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("newest token after reuse: got %v, want ErrInvalidRefreshToken (family revoked)", err)
	}

	// A separate login is a separate family and is unaffected.
	other, _ := s.LoginDevice(ctx, deviceA)
	if _, err := s.Refresh(ctx, other.RefreshToken); err != nil {
		t.Fatalf("another login's family was revoked too: %v", err)
	}
}

// N concurrent refreshes of one token: the database lets exactly one consume it. The others see a
// consumed token, which is indistinguishable from theft, so the family is revoked and the
// winner's new token is dead too. This is why the engine client must serialize refreshes.
func TestConcurrentRefreshExactlyOneWinsThenFamilyRevoked(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	login, _ := s.LoginDevice(ctx, deviceA)

	const n = 10
	results := make([]Session, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results[i], errs[i] = s.Refresh(ctx, login.RefreshToken) })
	}
	wg.Wait()

	var winner *Session
	for i := range n {
		switch {
		case errs[i] == nil:
			if winner != nil {
				t.Fatal("two refreshes consumed the same token")
			}
			winner = &results[i]
		case !errors.Is(errs[i], ErrInvalidRefreshToken):
			t.Fatalf("refresh %d: unexpected error %v", i, errs[i])
		}
	}
	if winner == nil {
		t.Fatal("no refresh succeeded")
	}
	if _, err := s.Refresh(ctx, winner.RefreshToken); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("winner's token after the race: got %v, want revoked", err)
	}
}

func TestRefreshExpiredAndUnknown(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	login, _ := s.LoginDevice(ctx, deviceA)

	if _, err := s.pool.Exec(ctx, "UPDATE refresh_tokens SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, login.RefreshToken); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("expired: got %v", err)
	}
	if _, err := s.Refresh(ctx, "never-issued"); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("unknown: got %v", err)
	}
}

func TestSecretsAreStoredHashed(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	login, _ := s.LoginDevice(ctx, deviceA)

	var hits int
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM devices WHERE encode(device_hash, 'escape') LIKE '%' || $1 || '%')
		      + (SELECT count(*) FROM refresh_tokens WHERE encode(token_hash, 'escape') LIKE '%' || $2 || '%')`,
		deviceA, login.RefreshToken).Scan(&hits)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatal("a raw device ID or refresh token is stored in the database")
	}
}
