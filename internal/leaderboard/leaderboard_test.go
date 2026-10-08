package leaderboard

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/db/dbtest"
	"github.com/giska1923/GanymedServer/internal/profile"
)

const board = "proving-ground"

func newTestService(t testing.TB) *Service {
	t.Helper()
	pool := dbtest.New(t)
	log := slog.New(slog.DiscardHandler)
	return NewService(pool, profile.NewService(pool, log), log)
}

// newID returns a random v4 UUID. Leaderboard tests need no real accounts: there is no foreign
// key to the auth module's tables, which is the point of the module rule.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *Service) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSubmitKeepsBestAndLogsEverything(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	player := newID()

	st, _, err := s.Submit(ctx, player, board, newID(), 500)
	if err != nil || st != (Standing{Rank: 1, Best: 500}) {
		t.Fatalf("first: %+v, %v", st, err)
	}
	st, _, err = s.Submit(ctx, player, board, newID(), 300)
	if err != nil || st.Best != 500 {
		t.Fatalf("lower score changed best: %+v, %v", st, err)
	}
	st, _, err = s.Submit(ctx, player, board, newID(), 900)
	if err != nil || st.Best != 900 {
		t.Fatalf("higher score not kept: %+v, %v", st, err)
	}
	if n := s.count(t, "SELECT count(*) FROM score_submissions WHERE account_id = $1", player); n != 3 {
		t.Fatalf("%d submissions logged, want 3", n)
	}
}

func TestSubmitSameKeyReplays(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	player, key := newID(), newID()

	first, replayed, err := s.Submit(ctx, player, board, key, 700)
	if err != nil || replayed {
		t.Fatalf("first: replayed=%v err=%v", replayed, err)
	}
	// Someone else overtakes the player before the retry arrives. The replay must still answer
	// with the original standing: same request, same response.
	if _, _, err := s.Submit(ctx, newID(), board, newID(), 800); err != nil {
		t.Fatal(err)
	}
	again, replayed, err := s.Submit(ctx, player, board, key, 700)
	if err != nil || !replayed || again != first {
		t.Fatalf("retry: %+v replayed=%v err=%v; first was %+v", again, replayed, err, first)
	}
	if n := s.count(t, "SELECT count(*) FROM score_submissions WHERE account_id = $1", player); n != 1 {
		t.Fatalf("%d submissions, want 1", n)
	}
}

// Ten identical requests at the same instant: the key's unique index serializes them, so exactly
// one does the work and nine replay its response.
func TestSubmitSameKeyConcurrently(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	player, key := newID(), newID()

	const n = 10
	standings := make([]Standing, n)
	replays := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { standings[i], replays[i], errs[i] = s.Submit(ctx, player, board, key, 4242) })
	}
	wg.Wait()

	fresh := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("submit %d: %v", i, errs[i])
		}
		if standings[i] != standings[0] {
			t.Fatalf("submit %d answered %+v, submit 0 answered %+v", i, standings[i], standings[0])
		}
		if !replays[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d submissions did the work, want exactly 1", fresh)
	}
	if c := s.count(t, "SELECT count(*) FROM score_submissions WHERE account_id = $1", player); c != 1 {
		t.Fatalf("%d submission rows, want 1", c)
	}
}

func TestSubmitKeyReusedForDifferentRequest(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "INSERT INTO leaderboards (id) VALUES ('other')"); err != nil {
		t.Fatal(err)
	}
	player, key := newID(), newID()
	if _, _, err := s.Submit(ctx, player, board, key, 100); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Submit(ctx, player, board, key, 101); !errors.Is(err, ErrKeyReused) {
		t.Errorf("different score: got %v", err)
	}
	if _, _, err := s.Submit(ctx, player, "other", key, 100); !errors.Is(err, ErrKeyReused) {
		t.Errorf("different board: got %v", err)
	}
	// Keys are scoped per account: another player may use the same key value.
	if _, replayed, err := s.Submit(ctx, newID(), board, key, 100); err != nil || replayed {
		t.Errorf("another account, same key: replayed=%v err=%v", replayed, err)
	}
}

func TestUnknownBoard(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if _, _, err := s.Submit(ctx, newID(), "no-such-board", newID(), 1); !errors.Is(err, ErrUnknownBoard) {
		t.Errorf("submit: %v", err)
	}
	if _, err := s.Top(ctx, "no-such-board", 5); !errors.Is(err, ErrUnknownBoard) {
		t.Errorf("top: %v", err)
	}
	if _, _, err := s.Mine(ctx, "no-such-board", newID()); !errors.Is(err, ErrUnknownBoard) {
		t.Errorf("mine: %v", err)
	}
}

// Competition ranking: 100, 90, 90, 80 rank 1, 2, 2, 4. Tied players list in the order they
// reached the score.
func TestRanksAndTies(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	players := []string{newID(), newID(), newID(), newID()}
	scores := []int64{90, 100, 90, 80} // players[0] reaches 90 before players[2]
	for i, p := range players {
		if _, _, err := s.Submit(ctx, p, board, newID(), scores[i]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct achieved_at for the tie-break
	}
	if _, err := profile.NewService(s.pool, s.log).SetDisplayName(ctx, players[1], "Champion"); err != nil {
		t.Fatal(err)
	}

	top, err := s.Top(ctx, board, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		rank   int64
		player string
		score  int64
	}{{1, players[1], 100}, {2, players[0], 90}, {2, players[2], 90}, {4, players[3], 80}}
	if len(top) != len(want) {
		t.Fatalf("got %d entries", len(top))
	}
	for i, w := range want {
		if top[i].Rank != w.rank || top[i].AccountID != w.player || top[i].Score != w.score {
			t.Errorf("entry %d: got %+v, want rank %d score %d", i, top[i], w.rank, w.score)
		}
	}
	if top[0].DisplayName != "Champion" || top[3].DisplayName != profile.DefaultDisplayName(players[3]) {
		t.Errorf("names: %q, %q", top[0].DisplayName, top[3].DisplayName)
	}

	if st, found, err := s.Mine(ctx, board, players[2]); err != nil || !found || st != (Standing{Rank: 2, Best: 90}) {
		t.Errorf("mine for a tied player: %+v found=%v err=%v", st, found, err)
	}
	if _, found, err := s.Mine(ctx, board, newID()); err != nil || found {
		t.Errorf("player with no score: found=%v err=%v", found, err)
	}
	if two, _ := s.Top(ctx, board, 2); len(two) != 2 {
		t.Errorf("limit 2 returned %d", len(two))
	}
}

func TestExpireKeys(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	old, fresh := newID(), newID()
	for _, key := range []string{old, fresh} {
		if _, _, err := s.Submit(ctx, newID(), board, key, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx,
		"UPDATE score_idempotency_keys SET created_at = now() - interval '25 hours' WHERE key = $1", old); err != nil {
		t.Fatal(err)
	}

	n, err := s.expireOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, err %v; want 1", n, err)
	}
	if c := s.count(t, "SELECT count(*) FROM score_idempotency_keys WHERE key = $1", fresh); c != 1 {
		t.Fatal("a fresh key was expired")
	}

	// The loop must return promptly once its context is cancelled.
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.ExpireKeys(loopCtx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ExpireKeys did not return after cancellation")
	}
}
