package matchmaking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
	"github.com/giska1923/GanymedServer/internal/fleet"
)

// matchFour queues four solo players, runs a round, and returns them with their match ID.
func (f fixture) matchFour(t *testing.T) ([]string, string) {
	t.Helper()
	var players []string
	for i := 0; i < 4; i++ {
		p, _ := f.queueAt(t, 0)
		players = append(players, p)
	}
	if _, err := f.s.round(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur, err := f.s.Current(context.Background(), players[0])
	if err != nil || cur.MatchID == "" {
		t.Fatalf("not matched: %+v %v", cur, err)
	}
	return players, cur.MatchID
}

// later moves the service's clock forward.
func (f fixture) later(d time.Duration) {
	at := time.Now().Add(d)
	f.s.now = func() time.Time { return at }
}

// The whole happy path short of the result: matched, a server claimed in the same round,
// acknowledged, ready, and every player holding a connect token the server will accept.
func TestAllocationToReady(t *testing.T) {
	f := newFixture(t)
	f.fleet.free = []string{"127.0.0.1:7001"}
	ctx := context.Background()
	players, matchID := f.matchFour(t)

	if cur, _ := f.s.Current(ctx, players[0]); cur.State != StateAllocating {
		t.Fatalf("after the round: %s, want allocating (claimed in the same round it was matched)", cur.State)
	}
	a := f.fleet.lastClaim()
	if err := f.s.ServerReady(ctx, matchID, a.AllocID, a.Address); err != nil {
		t.Fatal(err)
	}
	if f.notes.count("match.ready") != 4 {
		t.Fatalf("match.ready sent %d times, want 4", f.notes.count("match.ready"))
	}

	for _, p := range players {
		cur, err := f.s.Current(ctx, p)
		if err != nil || cur.State != StateReady || cur.ServerAddr != a.Address {
			t.Fatalf("player %s: %+v %v", p, cur, err)
		}
		// The token is the caller's own, and passes the server's checks.
		c, err := connecttoken.Verify(f.pub, cur.ConnectToken,
			connecttoken.Expect{ServerAddr: a.Address, MatchID: matchID}, time.Now())
		if err != nil || c.AccountID != p {
			t.Fatalf("player %s's token: %+v %v", p, c, err)
		}
	}
	if f.rdb.ZScore(ctx, runningKey, matchID).Err() != nil {
		t.Fatal("a ready match is not in mm:running")
	}
}

// A server that never acknowledges is withdrawn after AckTimeout, and the match tries another.
// After MaxAttempts withdrawals the match fails, and its players are free to queue again.
func TestAckTimeoutRetriesThenFails(t *testing.T) {
	f := newFixture(t)
	f.fleet.free = []string{"a:1", "b:2", "c:3", "d:4"}
	ctx := context.Background()
	players, matchID := f.matchFour(t)
	first := f.fleet.lastClaim()

	for attempt := 1; attempt <= DefaultAllocation.MaxAttempts; attempt++ {
		f.later(time.Duration(attempt) * (DefaultAllocation.AckTimeout + time.Second))
		if _, err := f.s.round(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !f.fleet.withdrawn[first.AllocID] {
		t.Fatal("the unacknowledged allocation was not withdrawn on the fleet side")
	}
	if len(f.fleet.claims) != DefaultAllocation.MaxAttempts {
		t.Fatalf("%d servers claimed, want %d attempts", len(f.fleet.claims), DefaultAllocation.MaxAttempts)
	}
	cur, _ := f.s.Current(ctx, players[0])
	if cur.State != StateFailed || cur.Reason != "allocation_failed" {
		t.Fatalf("after %d silent servers: %+v", DefaultAllocation.MaxAttempts, cur)
	}
	if f.notes.count("ticket.failed") != 4 {
		t.Fatalf("ticket.failed sent %d times, want 4", f.notes.count("ticket.failed"))
	}

	// The first server's acknowledgement, arriving late, must be refused.
	if err := f.s.ServerReady(ctx, matchID, first.AllocID, first.Address); !errors.Is(err, fleet.ErrWithdrawn) {
		t.Fatalf("late acknowledgement: %v, want ErrWithdrawn", err)
	}
	f.s.now = time.Now
	if _, err := f.s.Enqueue(ctx, players[0], "coop"); err != nil {
		t.Fatalf("requeue after a failed match: %v", err)
	}
}

func TestNoServerTimeout(t *testing.T) {
	f := newFixture(t) // no servers at all
	ctx := context.Background()
	players, _ := f.matchFour(t)

	f.later(DefaultAllocation.NoServerTimeout - time.Second)
	f.s.round(ctx)
	if cur, _ := f.s.Current(ctx, players[0]); cur.State != StateMatched {
		t.Fatalf("before the timeout: %s", cur.State)
	}
	f.later(DefaultAllocation.NoServerTimeout + time.Second)
	f.s.round(ctx)
	if cur, _ := f.s.Current(ctx, players[0]); cur.State != StateFailed || cur.Reason != "no_server" {
		t.Fatalf("after the timeout: %+v", cur)
	}
}

// A server that vanishes mid-match (its agent stops reporting it) fails the match, releasing the
// players. Without this they would stay active, and unable to queue, forever.
func TestServerLostMidMatch(t *testing.T) {
	f := newFixture(t)
	f.fleet.free = []string{"127.0.0.1:7001"}
	ctx := context.Background()
	players, matchID := f.matchFour(t)
	a := f.fleet.lastClaim()
	f.s.ServerReady(ctx, matchID, a.AllocID, a.Address)

	f.s.round(ctx)
	if cur, _ := f.s.Current(ctx, players[0]); cur.State != StateReady {
		t.Fatalf("with the server alive: %s", cur.State)
	}
	f.fleet.dead[a.ServerID] = true
	f.s.round(ctx)
	if cur, _ := f.s.Current(ctx, players[0]); cur.State != StateFailed || cur.Reason != "server_lost" {
		t.Fatalf("after the server vanished: %+v", cur)
	}
}

// The result endpoint's whole contract: the credential, idempotence, conflicting outcomes, one
// set of pushes, one rating change, tickets finished and players released.
func TestReportResult(t *testing.T) {
	f := newFixture(t).withPostgres(t)
	f.fleet.free = []string{"127.0.0.1:7001"}
	ctx := context.Background()
	players, matchID := f.matchFour(t)
	a := f.fleet.lastClaim()

	if _, err := f.s.ReportResult(ctx, matchID, a.ResultToken, "victory"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a result before the server was ready: %v, want ErrUnauthorized", err)
	}
	f.s.ServerReady(ctx, matchID, a.AllocID, a.Address)

	if _, err := f.s.ReportResult(ctx, matchID, "not-the-token", "victory"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong credential: %v", err)
	}

	first, err := f.s.ReportResult(ctx, matchID, a.ResultToken, "victory")
	if err != nil || first.RatingChange != 16 {
		t.Fatalf("first report: %+v %v; want +16 for even odds", first, err)
	}
	again, err := f.s.ReportResult(ctx, matchID, a.ResultToken, "victory")
	if err != nil || again != first {
		t.Fatalf("repeated report: %+v %v; want the same response", again, err)
	}
	if _, err := f.s.ReportResult(ctx, matchID, a.ResultToken, "defeat"); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("a different outcome: %v, want ErrResultConflict", err)
	}

	if n := f.notes.count("match.finished"); n != 4 {
		t.Fatalf("match.finished sent %d times over two reports, want 4 (once per player)", n)
	}
	got, _ := f.ratings.Ratings(ctx, players)
	for _, p := range players {
		if got[p] != 1516 {
			t.Fatalf("rating of %s = %d after one victory reported twice, want 1516", p, got[p])
		}
	}
	cur, _ := f.s.Current(ctx, players[0])
	if cur.State != StateFinished || cur.Outcome != "victory" || cur.RatingChange != 16 {
		t.Fatalf("the ticket after the result: %+v", cur)
	}
	if _, err := f.s.Enqueue(ctx, players[0], "coop"); err != nil {
		t.Fatalf("requeue after a finished match: %v", err)
	}
	var rows int
	f.s.pool.QueryRow(ctx, "SELECT count(*) FROM match_results WHERE match_id = $1", matchID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d result rows, want 1", rows)
	}
}

func TestEloChange(t *testing.T) {
	for _, c := range []struct {
		team    float64
		outcome string
		want    int
	}{
		{1500, "victory", 16}, {1500, "defeat", -16},
		{1700, "victory", 8}, {1700, "defeat", -24}, // expected to win: little gained, much lost
		{1300, "victory", 24}, {1300, "defeat", -8},
	} {
		if got := eloChange(c.team, c.outcome); got != c.want {
			t.Errorf("team %v %s: %d, want %d", c.team, c.outcome, got, c.want)
		}
	}
}
