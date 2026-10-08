package party

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/realtime"
	"github.com/giska1923/GanymedServer/internal/redisdb/redistest"
)

// Fakes for the three interfaces this package declares. Party's own logic needs only Redis:
// presence and pushes are realtime's business, names are profile's. That is the payoff of
// consumer-declared interfaces: these tests need neither Postgres nor a socket.

type fakePresence struct {
	mu      sync.Mutex
	offline map[string]bool
}

func (f *fakePresence) Status(_ context.Context, ids []string) (map[string]realtime.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]realtime.Status{}
	for _, a := range ids {
		out[a] = realtime.StatusOnline
		if f.offline[a] {
			out[a] = realtime.StatusOffline
		}
	}
	return out, nil
}

func (f *fakePresence) setOffline(a string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offline[a] = true
}

type push struct{ to, typ string }

type fakeNotifier struct {
	mu   sync.Mutex
	sent []push
}

func (f *fakeNotifier) Notify(_ context.Context, to, typ string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, push{to, typ})
	return nil
}

func (f *fakeNotifier) take() []push {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

type fakeNames struct{}

func (fakeNames) DisplayNames(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range ids {
		out[a] = "name-" + a[:4]
	}
	return out, nil
}

type fixture struct {
	s        *Service
	presence *fakePresence
	notes    *fakeNotifier
}

func newFixture(t *testing.T) fixture {
	p, n := &fakePresence{offline: map[string]bool{}}, &fakeNotifier{}
	return fixture{NewService(redistest.New(t), p, n, fakeNames{}, slog.New(slog.DiscardHandler)), p, n}
}

// party builds a party led by the first player with the rest invited and joined.
func (f fixture) party(t *testing.T, players ...string) string {
	t.Helper()
	ctx := context.Background()
	p, err := f.s.Create(ctx, players[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range players[1:] {
		if err := f.s.Invite(ctx, players[0], m); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.Accept(ctx, m, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.notes.take()
	return p.ID
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = id.New()
	}
	return out
}

func TestCreateInviteAccept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(2)
	leader, guest := p[0], p[1]

	party, err := f.s.Create(ctx, leader)
	if err != nil || party.LeaderID != leader || len(party.Members) != 1 {
		t.Fatalf("create: %+v %v", party, err)
	}
	if _, err := f.s.Create(ctx, leader); !errors.Is(err, ErrAlreadyInParty) {
		t.Fatalf("second create: %v", err)
	}

	if err := f.s.Invite(ctx, leader, guest); err != nil {
		t.Fatal(err)
	}
	if got := f.notes.take(); len(got) != 1 || got[0] != (push{guest, "party.invite"}) {
		t.Fatalf("invite pushes: %v", got)
	}
	invites, err := f.s.Invites(ctx, guest)
	if err != nil || len(invites) != 1 || invites[0].PartyID != party.ID || invites[0].FromID != leader {
		t.Fatalf("invites: %+v %v", invites, err)
	}

	joined, err := f.s.Accept(ctx, guest, party.ID)
	if err != nil || len(joined.Members) != 2 || joined.Members[1].AccountID != guest {
		t.Fatalf("accept: %+v %v", joined, err)
	}
	if got := f.notes.take(); len(got) != 2 {
		t.Fatalf("accept should nudge both members, got %v", got)
	}
	if inv, _ := f.s.Invites(ctx, guest); len(inv) != 0 {
		t.Fatalf("an accepted invite is still listed: %+v", inv)
	}
}

func TestInviteRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(6)
	pid := f.party(t, p[0], p[1])

	if err := f.s.Invite(ctx, p[1], p[2]); !errors.Is(err, ErrNotLeader) {
		t.Errorf("non-leader invite: %v", err)
	}
	if err := f.s.Invite(ctx, p[0], p[1]); !errors.Is(err, ErrAlreadyInParty) {
		t.Errorf("invite an existing member: %v", err)
	}
	if err := f.s.Invite(ctx, p[0], p[0]); !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("invite yourself: %v", err)
	}
	if err := f.s.Invite(ctx, p[5], p[0]); !errors.Is(err, ErrNotInParty) {
		t.Errorf("invite with no party: %v", err)
	}

	// Fill to MaxSize, then the party is full both for inviting and for accepting.
	for _, m := range p[2:4] {
		f.s.Invite(ctx, p[0], m)
		if _, err := f.s.Accept(ctx, m, pid); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.Invite(ctx, p[0], p[4]); !errors.Is(err, ErrPartyFull) {
		t.Errorf("invite into a full party: %v", err)
	}
}

func TestInviteExpires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(2)
	pid := f.party(t, p[0])
	if err := f.s.Invite(ctx, p[0], p[1]); err != nil {
		t.Fatal(err)
	}

	f.s.now = func() time.Time { return time.Now().Add(InviteTTL + time.Second) }
	if inv, _ := f.s.Invites(ctx, p[1]); len(inv) != 0 {
		t.Errorf("expired invite listed: %+v", inv)
	}
	if _, err := f.s.Accept(ctx, p[1], pid); !errors.Is(err, ErrNoInvite) {
		t.Errorf("accept an expired invite: %v", err)
	}
}

// The last seat, contested: two invitees accept at the same instant. The accept script checks
// the size and adds the member atomically, so exactly one gets in.
func TestLastSeatRace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(5)
	pid := f.party(t, p[0], p[1], p[2])
	f.s.Invite(ctx, p[0], p[3])
	f.s.Invite(ctx, p[0], p[4])

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, m := range p[3:5] {
		wg.Go(func() { _, errs[i] = f.s.Accept(ctx, m, pid) })
	}
	wg.Wait()

	ok, full := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrPartyFull):
			full++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	got, _ := f.s.Get(ctx, p[0])
	if ok != 1 || full != 1 || len(got.Members) != MaxSize {
		t.Fatalf("ok=%d full=%d members=%d; want exactly one winner and %d members", ok, full, len(got.Members), MaxSize)
	}
}

func TestLeavePromotesAndDisbands(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(3)
	f.party(t, p...)

	if err := f.s.Leave(ctx, p[0]); err != nil { // the leader
		t.Fatal(err)
	}
	got, _ := f.s.Get(ctx, p[1])
	if got.LeaderID != p[1] {
		t.Fatalf("leader after the leader left: %s, want the longest-standing member %s", got.LeaderID, p[1])
	}
	if pushes := f.notes.take(); len(pushes) != 2 {
		t.Fatalf("the two who remain should be nudged, got %v", pushes)
	}
	if _, err := f.s.Get(ctx, p[0]); !errors.Is(err, ErrNotInParty) {
		t.Fatalf("leaver still has a party: %v", err)
	}

	f.s.Leave(ctx, p[1])
	f.s.Leave(ctx, p[2])
	if n := f.s.rdb.Exists(ctx, partyKey(got.ID), membersKey(got.ID)).Val(); n != 0 {
		t.Fatalf("an empty party left %d keys behind", n)
	}
	if f.s.rdb.SIsMember(ctx, partiesKey, got.ID).Val() {
		t.Fatal("an empty party is still in the parties set")
	}
}

func TestKick(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(4)
	f.party(t, p[0], p[1], p[2])

	if err := f.s.Kick(ctx, p[1], p[2]); !errors.Is(err, ErrNotLeader) {
		t.Errorf("non-leader kick: %v", err)
	}
	if err := f.s.Kick(ctx, p[0], p[3]); !errors.Is(err, ErrNotInParty) {
		t.Errorf("kick a stranger: %v", err)
	}
	if err := f.s.Kick(ctx, p[0], p[2]); err != nil {
		t.Fatal(err)
	}
	pushes := f.notes.take()
	removed := false
	for _, x := range pushes {
		if x == (push{p[2], "party.removed"}) {
			removed = true
		}
	}
	if !removed || len(pushes) != 3 { // party.removed to the kicked, party.updated to the two left
		t.Fatalf("pushes: %v", pushes)
	}
}

// The sweeper is the disconnect grace: offline members are removed. Two replicas sweeping at
// once must remove each member exactly once, and nudge exactly once.
func TestSweepRemovesOfflineOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := ids(3)
	f.party(t, p...)
	f.presence.setOffline(p[0]) // the leader's grace ran out

	var wg sync.WaitGroup
	counts := make([]int, 2)
	for i := range counts {
		wg.Go(func() {
			n, err := f.s.Sweep(ctx)
			if err != nil {
				t.Error(err)
			}
			counts[i] = n
		})
	}
	wg.Wait()

	if counts[0]+counts[1] != 1 {
		t.Fatalf("removals %v: want exactly one across both sweeps", counts)
	}
	got, _ := f.s.Get(ctx, p[1])
	if len(got.Members) != 2 || got.LeaderID != p[1] {
		t.Fatalf("after sweep: %+v", got)
	}
	removedPushes := 0
	for _, x := range f.notes.take() {
		if x.typ == "party.removed" {
			removedPushes++
		}
	}
	if removedPushes != 1 {
		t.Fatalf("party.removed sent %d times, want once", removedPushes)
	}
}
