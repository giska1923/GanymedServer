package matchmaking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/id"
)

// DirectorConfig is how often the director runs and how long its lease lasts.
type DirectorConfig struct {
	Interval time.Duration // between rounds, and between lease renewals
	LeaseTTL time.Duration // how long a dead leader blocks failover
}

// DefaultDirector: a round every second; a crashed leader is replaced within 5 s.
var DefaultDirector = DirectorConfig{Interval: time.Second, LeaseTTL: 5 * time.Second}

// renewScript extends the lease only if this replica still holds it.
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// releaseScript gives the lease up, only if it is ours. A graceful shutdown releases it so the
// next replica takes over on its next tick, instead of waiting out the TTL.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// RunDirector runs matchmaking rounds on whichever replica holds the lease, until ctx is cancelled.
//
// Every replica runs this loop; at most one is the director at a time. Each tick, the holder
// renews the lease and runs a round, and everyone else tries to take it. A lease (SET NX PX) is
// how a single writer is chosen without a coordinator: whoever's SET lands first holds it for
// LeaseTTL, and must renew before it runs out. A replica that dies stops renewing, and after at
// most LeaseTTL another replica's SET succeeds.
//
// The textbook flaw: a holder can be paused (a long GC pause, a stopped VM, a laptop lid) for
// longer than LeaseTTL, lose the lease without knowing, and wake mid-round still believing it
// leads. Two directors then commit concurrently. That is safe here only because commits are
// fenced (see matchScript). Each commit re-checks every ticket's state atomically, so the
// zombie's proposals that conflict with the new leader's commit nothing. The lease makes double
// work rare; the fence makes it harmless.
func (s *Service) RunDirector(ctx context.Context, replica string, cfg DirectorConfig) {
	s.runDirector(ctx, replica, cfg, true)
}

// runDirector is RunDirector with release on stop optional, so a test can simulate a crash (the
// lease is left to expire) as well as a graceful stop.
func (s *Service) runDirector(ctx context.Context, replica string, cfg DirectorConfig, releaseOnStop bool) {
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	leader := false

	for {
		select {
		case <-ctx.Done():
			if leader && releaseOnStop {
				// A fresh context: ctx is already cancelled, and the release must still run.
				rctx, cancel := context.WithTimeout(context.Background(), time.Second)
				releaseScript.Run(rctx, s.rdb, []string{leaseKey}, replica)
				cancel()
				s.log.Info("matchmaker lease released")
			}
			return
		case <-t.C:
		}

		holding, err := s.holdLease(ctx, replica, cfg.LeaseTTL, leader)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("matchmaker lease", "err", err)
			}
			holding = false // cannot prove we hold it: do not act as if we do
		}
		if holding != leader {
			leader = holding
			if leader {
				s.log.Info("matchmaker lease acquired: this replica runs matchmaking")
			} else {
				s.log.Warn("matchmaker lease lost")
			}
		}
		if !leader {
			continue
		}
		if _, err := s.round(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("matchmaking round failed", "err", err)
		}
	}
}

// holdLease renews the lease if we held it, or tries to take it if we did not. It reports
// whether we hold it now.
func (s *Service) holdLease(ctx context.Context, replica string, ttl time.Duration, held bool) (bool, error) {
	if held {
		n, err := renewScript.Run(ctx, s.rdb, []string{leaseKey}, replica, ttl.Milliseconds()).Int()
		if err != nil {
			return false, fmt.Errorf("renew: %w", err)
		}
		if n == 1 {
			return true, nil
		}
		// Expired and possibly taken: fall through and try to take it like anyone else.
	}
	ok, err := s.rdb.SetNX(ctx, leaseKey, replica, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("acquire: %w", err)
	}
	return ok, nil
}

// roundStats is what one round did, summed over modes. Logged, and read by tests to prove that a
// contested path (the fence refusing a proposal) actually ran.
type roundStats struct {
	pool, matches, ticketsMatched, refused, timedOut int
}

func (a *roundStats) add(b roundStats) {
	a.pool += b.pool
	a.matches += b.matches
	a.ticketsMatched += b.ticketsMatched
	a.refused += b.refused
	a.timedOut += b.timedOut
}

// round is one pass over every mode: propose matches, commit them, fail tickets that waited too
// long.
func (s *Service) round(ctx context.Context) (roundStats, error) {
	names := make([]string, 0, len(Modes))
	for name := range Modes {
		names = append(names, name)
	}
	sort.Strings(names) // map order is random; rounds should not be
	var total roundStats
	for _, name := range names {
		st, err := s.roundMode(ctx, Modes[name])
		total.add(st)
		if err != nil {
			return total, fmt.Errorf("mode %s: %w", name, err)
		}
	}
	// Then the matches' lifecycle: get servers for new and withdrawn matches (in the same round, so
	// a match made now usually has a server claimed before the round ends), and fail any whose
	// server vanished mid-match.
	if s.fleet != nil {
		if err := s.allocate(ctx); err != nil {
			return total, fmt.Errorf("allocate: %w", err)
		}
		if err := s.superviseRunning(ctx); err != nil {
			return total, fmt.Errorf("supervise: %w", err)
		}
	}
	return total, nil
}

func (s *Service) roundMode(ctx context.Context, mode Mode) (roundStats, error) {
	start := time.Now()
	pool, err := s.loadPool(ctx, mode.Name)
	if err != nil || len(pool) == 0 {
		return roundStats{}, err
	}
	now := s.now()

	byID := make(map[string]storedTicket, len(pool))
	in := make([]Ticket, len(pool))
	for i, t := range pool {
		byID[t.ID] = t
		in[i] = Ticket{ID: t.ID, Players: len(t.Players), Rating: t.Rating, Created: t.Created}
	}

	matched := map[string]bool{}
	formed, refused := 0, 0
	for _, group := range Match(mode, s.policy, in, now) {
		tickets := make([]storedTicket, len(group))
		for i, tid := range group {
			tickets[i] = byID[tid]
		}
		matchID := id.New()
		ok, players, err := s.commitMatch(ctx, matchID, mode.Name, tickets, now)
		if err != nil {
			return roundStats{}, err
		}
		if !ok {
			// The fence: a ticket in this proposal left the queue since the pool was read
			// (cancelled, or matched by another director). Nothing was committed. Its partners
			// stay queued for the next round.
			refused++
			continue
		}
		formed++
		for _, t := range tickets {
			matched[t.ID] = true
			s.pushAll(ctx, t.Players, "match.found", map[string]any{
				"ticket_id": t.ID, "match_id": matchID, "players": players,
			})
		}
	}

	failed := 0
	for _, tid := range Expired(s.policy, in, now) {
		if matched[tid] {
			continue
		}
		t := byID[tid]
		res, err := s.finish(ctx, t, StateFailed, "timeout")
		if err != nil {
			return roundStats{}, err
		}
		if res == "ok" {
			failed++
			s.pushAll(ctx, t.Players, "ticket.failed", map[string]string{"ticket_id": t.ID, "reason": "timeout"})
		}
	}

	st := roundStats{pool: len(pool), matches: formed, ticketsMatched: len(matched), refused: refused, timedOut: failed}
	if formed+refused+failed > 0 {
		s.log.Info("matchmaking round", "mode", mode.Name, "pool", st.pool, "matches", st.matches,
			"tickets_matched", st.ticketsMatched, "refused_by_fence", st.refused, "timed_out", st.timedOut,
			"duration_ms", float64(time.Since(start).Microseconds())/1000)
	}
	return st, nil
}

// leaseHolder reports which replica holds the lease, or "" (for tests and diagnostics).
func (s *Service) leaseHolder(ctx context.Context) (string, error) {
	v, err := s.rdb.Get(ctx, leaseKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}
