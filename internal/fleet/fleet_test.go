package fleet

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/redisdb/redistest"
)

func newTestService(t *testing.T) *Service {
	_, priv, _ := ed25519.GenerateKey(nil)
	return NewService(redistest.New(t), []byte("0123456789abcdef0123456789abcdef"), priv, slog.New(slog.DiscardHandler))
}

func ready(sid, addr string) ReportedServer {
	return ReportedServer{ServerID: sid, Address: addr, State: StateReady}
}

func TestHeartbeatReturnsPublicKey(t *testing.T) {
	s := newTestService(t)
	pub, _, err := s.Heartbeat(context.Background(), "agent-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connecttoken.ParsePublicKey(pub); err != nil {
		t.Fatalf("heartbeat returned an unusable public key %q: %v", pub, err)
	}
}

func TestClaimSendsCommandToTheRightAgent(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	sid := id.New()
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(sid, "127.0.0.1:7001")})

	a, err := s.Claim(ctx, "match-1", []string{"p1", "p2"})
	if err != nil || a.ServerID != sid || a.Address != "127.0.0.1:7001" || a.ResultToken == "" {
		t.Fatalf("claim: %+v %v", a, err)
	}
	if _, err := s.Claim(ctx, "match-2", nil); !errors.Is(err, ErrNoServer) {
		t.Fatalf("the only server was claimed twice: %v", err)
	}

	cmd, err := s.NextCommand(ctx, "agent-1", time.Second)
	if err != nil || cmd == nil || cmd.MatchID != "match-1" || cmd.ServerID != sid || cmd.ResultToken != a.ResultToken {
		t.Fatalf("agent-1's command: %+v %v", cmd, err)
	}
	if cmd, _ := s.NextCommand(ctx, "agent-2", 100*time.Millisecond); cmd != nil {
		t.Fatalf("another agent received a command: %+v", cmd)
	}
}

// The double-allocation guard. The backend claims a server between two heartbeats, and the agent's
// next heartbeat still reports it ready (the command has not reached it yet). That report must not
// put the server back in the ready set.
func TestHeartbeatDoesNotUndoAClaim(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	sid := id.New()
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(sid, "a:1")})
	if _, err := s.Claim(ctx, "match-1", nil); err != nil {
		t.Fatal(err)
	}
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(sid, "a:1")}) // stale "ready"
	if _, err := s.Claim(ctx, "match-2", nil); !errors.Is(err, ErrNoServer) {
		t.Fatalf("a stale heartbeat made a claimed server allocatable again: %v", err)
	}
}

// A long-poll is answered the moment a command arrives, not at its timeout.
func TestLongPollWakesOnCommand(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(id.New(), "a:1")})

	got := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		s.NextCommand(ctx, "agent-1", 10*time.Second)
		got <- time.Since(start)
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := s.Claim(ctx, "match-1", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-got:
		if d > time.Second {
			t.Fatalf("the long-poll returned after %v: it waited instead of waking", d)
		}
		t.Logf("long-poll answered %v after it started (command sent at ~200 ms)", d)
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poll never returned")
	}
}

func TestAcknowledge(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	sid := id.New()
	var readied []string
	s.SetReadyHandler(func(_ context.Context, matchID, allocID, addr string) error {
		readied = append(readied, matchID+"|"+allocID+"|"+addr)
		return nil
	})
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(sid, "a:1")})
	a, _ := s.Claim(ctx, "match-1", nil)

	if err := s.Acknowledge(ctx, "agent-2", sid, "match-1"); !errors.Is(err, ErrWithdrawn) {
		t.Fatalf("acknowledged by the wrong agent: %v", err)
	}
	if err := s.Acknowledge(ctx, "agent-1", sid, "match-other"); !errors.Is(err, ErrWithdrawn) {
		t.Fatalf("acknowledged for the wrong match: %v", err)
	}
	if err := s.Acknowledge(ctx, "agent-1", sid, "match-1"); err != nil {
		t.Fatal(err)
	}
	if len(readied) != 1 || readied[0] != "match-1|"+a.AllocID+"|a:1" {
		t.Fatalf("ready handler calls: %v", readied)
	}
	if err := s.Acknowledge(ctx, "agent-1", sid, "match-1"); !errors.Is(err, ErrWithdrawn) {
		t.Fatalf("acknowledged twice: %v", err)
	}
}

func TestWithdrawRefusesLateAcknowledge(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	sid := id.New()
	s.SetReadyHandler(func(context.Context, string, string, string) error { return nil })
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(sid, "a:1")})
	a, _ := s.Claim(ctx, "match-1", nil)

	if err := s.Withdraw(ctx, sid, a.AllocID); err != nil {
		t.Fatal(err)
	}
	if err := s.Acknowledge(ctx, "agent-1", sid, "match-1"); !errors.Is(err, ErrWithdrawn) {
		t.Fatalf("late acknowledgement after a withdrawal: %v", err)
	}
}

// A withdrawn server whose allocate command never reached its agent is still reported as ready.
// The heartbeat answer tells the agent to kill it, or the warm pool would keep a server nothing
// will ever allocate.
func TestHeartbeatRetiresWithdrawnServers(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	lost, other := id.New(), id.New()
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(lost, "a:1")})
	a, _ := s.Claim(ctx, "match-1", nil)
	s.rdb.Del(ctx, cmdsKey("agent-1")) // the command is lost on its way to the agent
	s.Withdraw(ctx, lost, a.AllocID)

	_, retire, err := s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(lost, "a:1"), ready(other, "a:2")})
	if err != nil {
		t.Fatal(err)
	}
	if len(retire) != 1 || retire[0] != lost {
		t.Fatalf("retire = %v, want only the withdrawn server %s", retire, lost)
	}
	if _, retire, _ := s.Heartbeat(ctx, "agent-1", []ReportedServer{{ServerID: lost, Address: "a:1", State: StateShutdown}}); len(retire) != 0 {
		t.Fatalf("a server reporting shutdown is retired again: %v", retire)
	}
}

// An agent that stops heartbeating takes its servers with it: they expire, and are neither
// allocated nor reported alive.
func TestDeadAgentsServersAreNotAllocated(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	sid := id.New()
	s.Heartbeat(ctx, "agent-1", []ReportedServer{ready(sid, "a:1")})
	// Fast-forward LivenessTTL: expire the agent and its server as if 5 s of silence had passed.
	s.rdb.PExpire(ctx, agentKey("agent-1"), time.Millisecond)
	s.rdb.PExpire(ctx, serverKey(sid), time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	if _, err := s.Claim(ctx, "match-1", nil); !errors.Is(err, ErrNoServer) {
		t.Fatalf("a dead agent's server was claimed: %v", err)
	}
	if alive, _ := s.ServerAlive(ctx, sid); alive {
		t.Fatal("a dead agent's server is reported alive")
	}
	if s.rdb.SIsMember(ctx, readyKey, sid).Val() {
		t.Fatal("the stale server was not cleaned out of the ready set")
	}
}
