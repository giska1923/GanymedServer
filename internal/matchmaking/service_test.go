package matchmaking

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/party"
	"github.com/giska1923/GanymedServer/internal/redisdb/redistest"
)

// Fakes for the three interfaces matchmaking declares: these tests need Redis and nothing else.

type fakeParties struct {
	mu      sync.Mutex
	rosters map[string][]string // any member → the party's members, leader first
}

func (f *fakeParties) Roster(_ context.Context, acct string) (string, string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.rosters[acct]
	if !ok {
		return "", "", nil, party.ErrNotInParty
	}
	return "party-of-" + m[0], m[0], m, nil
}

func (f *fakeParties) form(members ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range members {
		f.rosters[m] = members
	}
}

type fakeRatings map[string]int

func (f fakeRatings) Ratings(_ context.Context, ids []string) (map[string]int, error) {
	out := map[string]int{}
	for _, a := range ids {
		out[a] = 1500
		if r, ok := f[a]; ok {
			out[a] = r
		}
	}
	return out, nil
}

type sent struct{ to, typ string }

type fakeNotifier struct {
	mu  sync.Mutex
	got []sent
}

func (f *fakeNotifier) Notify(_ context.Context, to, typ string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, sent{to, typ})
	return nil
}

func (f *fakeNotifier) count(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.got {
		if s.typ == typ {
			n++
		}
	}
	return n
}

type fixture struct {
	s       *Service
	rdb     *redis.Client
	parties *fakeParties
	ratings fakeRatings
	notes   *fakeNotifier
}

func newFixture(t *testing.T) fixture {
	rdb := redistest.New(t)
	return newFixtureOn(rdb)
}

// newFixtureOn makes a second service on the same Redis: a second replica.
func newFixtureOn(rdb *redis.Client) fixture {
	p, r, n := &fakeParties{rosters: map[string][]string{}}, fakeRatings{}, &fakeNotifier{}
	return fixture{NewService(rdb, p, r, n, slog.New(slog.DiscardHandler)), rdb, p, r, n}
}

// queueAt queues a solo player rated 1500 as if it happened `ago` before now.
func (f fixture) queueAt(t *testing.T, ago time.Duration) (string, TicketView) {
	t.Helper()
	return f.queueRatedAt(t, ago, 1500)
}

func (f fixture) queueRatedAt(t *testing.T, ago time.Duration, rating int) (string, TicketView) {
	t.Helper()
	player := id.New()
	f.ratings[player] = rating
	f.s.now = func() time.Time { return time.Now().Add(-ago) }
	v, err := f.s.Enqueue(context.Background(), player, "coop")
	f.s.now = time.Now
	if err != nil {
		t.Fatal(err)
	}
	return player, v
}

func TestEnqueueRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	leader, member, solo := id.New(), id.New(), id.New()
	f.parties.form(leader, member)

	if _, err := f.s.Enqueue(ctx, solo, "deathmatch"); !errors.Is(err, ErrUnknownMode) {
		t.Errorf("unknown mode: %v", err)
	}
	if _, err := f.s.Enqueue(ctx, member, "coop"); !errors.Is(err, ErrNotLeader) {
		t.Errorf("non-leader queueing the party: %v", err)
	}
	v, err := f.s.Enqueue(ctx, leader, "coop")
	if err != nil || len(v.Players) != 2 || v.State != StateQueued {
		t.Fatalf("party ticket: %+v %v", v, err)
	}
	if f.notes.count("ticket.updated") != 2 {
		t.Errorf("both party members should be told they are queued")
	}
	if _, err := f.s.Enqueue(ctx, leader, "coop"); !errors.Is(err, ErrAlreadyQueued) {
		t.Errorf("queue twice: %v", err)
	}
	if cur, err := f.s.Current(ctx, member); err != nil || cur == nil || cur.ID != v.ID {
		t.Errorf("the member's current ticket: %+v %v", cur, err)
	}
	if _, err := f.s.Ticket(ctx, solo, v.ID); !errors.Is(err, ErrNoTicket) {
		t.Errorf("a stranger reading the ticket: %v", err)
	}
}

func TestRoundMatchesAndPlayersCanRequeue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var players []string
	for i := 0; i < 4; i++ {
		p, _ := f.queueAt(t, 0)
		players = append(players, p)
	}
	if _, err := f.s.round(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.notes.count("match.found"); n != 4 {
		t.Fatalf("match.found sent %d times, want 4", n)
	}
	cur, err := f.s.Current(ctx, players[0])
	if err != nil || cur.State != StateMatched || len(cur.MatchPlayers) != 4 {
		t.Fatalf("after the round: %+v %v", cur, err)
	}
	if n := f.rdb.ZCard(ctx, poolKey("coop")).Val(); n != 0 {
		t.Fatalf("pool still holds %d tickets", n)
	}
	// Out of the queue, so free to queue again.
	if _, err := f.s.Enqueue(ctx, players[0], "coop"); err != nil {
		t.Fatalf("requeue after a match: %v", err)
	}
}

// One round, both policies: a pair queued 11 s ago is past the fill wait and matches; a player
// rated 1500 points away from everyone, queued 121 s ago, is past MaxWait and fails.
func TestFillWaitAndTimeout(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, _ := f.queueAt(t, 11*time.Second)
	f.queueAt(t, 11*time.Second)
	lonely, _ := f.queueRatedAt(t, 121*time.Second, 3000) // beyond the 1000 window cap

	if _, err := f.s.round(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := f.s.Current(ctx, a); cur.State != StateMatched || len(cur.MatchPlayers) != 2 {
		t.Fatalf("the pair: %+v", cur)
	}
	cur, err := f.s.Current(ctx, lonely)
	if err != nil || cur.State != StateFailed || cur.Reason != "timeout" {
		t.Fatalf("the lonely ticket: %+v %v", cur, err)
	}
	if f.notes.count("ticket.failed") != 1 || f.notes.count("match.found") != 2 {
		t.Fatalf("pushes: %d ticket.failed, %d match.found", f.notes.count("ticket.failed"), f.notes.count("match.found"))
	}
}

func TestCancel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	leader, member := id.New(), id.New()
	f.parties.form(leader, member)
	v, _ := f.s.Enqueue(ctx, leader, "coop")

	got, err := f.s.Cancel(ctx, member, v.ID) // any player on the ticket may cancel
	if err != nil || got.State != StateCancelled {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	var notQueued ErrNotQueued
	if _, err := f.s.Cancel(ctx, leader, v.ID); !errors.As(err, &notQueued) || notQueued.State != StateCancelled {
		t.Fatalf("cancel twice: %v", err)
	}
	if _, err := f.s.Enqueue(ctx, leader, "coop"); err != nil {
		t.Fatalf("requeue after cancelling: %v", err)
	}
}

// The plan's cancel race: a cancel and a round at the same instant. The two scripts are atomic
// and both check "still queued", so the outcome is cancelled OR matched, never both, never
// neither.
//
// Started together, the cancel always won: it is one script, while a round first reads the pool.
// That proved only one branch. So the cancel is delayed by a random 0–4 ms, which lands it before,
// during and after the round's commit across iterations, and the test requires seeing both
// outcomes. A race test that only ever exercises one side of the race is not testing the race.
func TestCancelRacesRound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(5, 6))
	outcomes := map[string]int{}
	for i := 0; i < 200; i++ {
		p1, t1 := f.queueAt(t, 11*time.Second)
		f.queueAt(t, 11*time.Second) // its partner, so the pair is matchable now
		delay := time.Duration(rng.IntN(4000)) * time.Microsecond

		var cancelErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			time.Sleep(delay)
			_, cancelErr = f.s.Cancel(ctx, p1, t1.ID)
		})
		wg.Go(func() {
			if _, err := f.s.round(ctx); err != nil {
				t.Error(err)
			}
		})
		wg.Wait()

		cur, err := f.s.Ticket(ctx, p1, t1.ID)
		if err != nil {
			t.Fatal(err)
		}
		var notQueued ErrNotQueued
		switch {
		case cancelErr == nil && cur.State == StateCancelled && cur.MatchID == "":
			outcomes["cancelled"]++
		case errors.As(cancelErr, &notQueued) && notQueued.State == StateMatched && cur.State == StateMatched:
			outcomes["matched"]++
		default:
			t.Fatalf("iteration %d: cancel returned %v, ticket is %s (match %q)", i, cancelErr, cur.State, cur.MatchID)
		}
		// Clear the pool for the next iteration: a cancelled ticket's partner is still queued.
		f.rdb.Del(ctx, poolKey("coop"))
	}
	t.Logf("outcomes over 200 races: %v", outcomes)
	if outcomes["cancelled"] == 0 || outcomes["matched"] == 0 {
		t.Fatalf("only one side of the race happened (%v): the test did not exercise the race", outcomes)
	}
}

// The fence: two directors run a round over the same pool at the same moment, as a zombie leader
// and its successor would. Each ticket must end in exactly one match.
func TestTwoDirectorsNeverDoubleMatch(t *testing.T) {
	a := newFixture(t)
	b := newFixtureOn(a.rdb)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		a.queueAt(t, 0)
	}

	var wg sync.WaitGroup
	var sa, sb roundStats
	wg.Go(func() { sa, _ = a.s.round(ctx) })
	wg.Go(func() { sb, _ = b.s.round(ctx) })
	wg.Wait()
	t.Logf("director a: %d matches, %d refused; director b: %d matches, %d refused",
		sa.matches, sa.refused, sb.matches, sb.refused)

	matchOf := map[string]string{}
	keys, _ := a.rdb.Keys(ctx, "mm:match:*").Result()
	for _, k := range keys {
		tickets := splitList(a.rdb.HGet(ctx, k, "tickets").Val())
		for _, tid := range tickets {
			if prev, dup := matchOf[tid]; dup {
				t.Fatalf("ticket %s is in two matches: %s and %s", tid, prev, k)
			}
			matchOf[tid] = k
		}
	}
	if len(matchOf) != 40 {
		t.Fatalf("%d of 40 tickets matched, want all (10 full matches)", len(matchOf))
	}
	// The fence was exercised only if the directors' proposals actually collided. If both rounds
	// read the pool before either committed, they propose the same groups, so every commit after
	// the first is refused.
	if sa.refused+sb.refused == 0 {
		t.Fatalf("no proposal was refused: the two rounds did not overlap, so the fence was not tested")
	}
}

// The lease, end to end with three replicas' director loops:
//
//	a leads; b starts and must NOT take over from a healthy leader;
//	a crashes (stops without releasing): b takes over once the lease expires;
//	c starts; b stops gracefully (releases): c takes over within a tick or two.
func TestLeaseFailover(t *testing.T) {
	a := newFixture(t)
	cfg := DirectorConfig{Interval: 50 * time.Millisecond, LeaseTTL: 400 * time.Millisecond}
	ctx := context.Background()

	type loop struct {
		stop context.CancelFunc
		done chan struct{}
	}
	start := func(replica string, release bool) loop {
		f := newFixtureOn(a.rdb)
		c, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { f.s.runDirector(c, replica, cfg, release); close(done) }()
		return loop{cancel, done}
	}
	holder := func() string { h, _ := a.s.leaseHolder(ctx); return h }
	waitFor := func(want string, within time.Duration) time.Duration {
		t.Helper()
		begin := time.Now()
		for time.Since(begin) < within {
			if holder() == want {
				return time.Since(begin)
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("lease holder %q after %v, want %q", holder(), within, want)
		return 0
	}

	la := start("a", false) // a will crash: it never releases
	waitFor("a", time.Second)

	lb := start("b", true)
	defer func() { lb.stop(); <-lb.done }()
	time.Sleep(3 * cfg.LeaseTTL)
	if h := holder(); h != "a" {
		t.Fatalf("a healthy leader lost the lease to %q", h)
	}

	la.stop()
	<-la.done
	crash := waitFor("b", 3*cfg.LeaseTTL)
	t.Logf("crash failover: b led %v after a stopped renewing (lease TTL %v)", crash, cfg.LeaseTTL)
	if crash > cfg.LeaseTTL+3*cfg.Interval {
		t.Fatalf("crash failover took %v, more than the TTL plus a few ticks", crash)
	}

	lc := start("c", true)
	defer func() { lc.stop(); <-lc.done }()
	time.Sleep(3 * cfg.Interval) // c is now polling; b still leads
	lb.stop()
	<-lb.done
	graceful := waitFor("c", 3*cfg.LeaseTTL)
	t.Logf("graceful handover: c led %v after b released", graceful)
	if graceful > 3*cfg.Interval {
		t.Fatalf("graceful handover took %v: the release did not shorten it", graceful)
	}
}
