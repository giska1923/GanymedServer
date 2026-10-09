package matchmaking

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
	"github.com/giska1923/GanymedServer/internal/fleet"
)

// AllocationPolicy is how patient the director is with game servers.
type AllocationPolicy struct {
	NoServerTimeout time.Duration // a match with no free server for this long fails: no_server
	AckTimeout      time.Duration // a claimed server that has not acknowledged by then is withdrawn
	MaxAttempts     int           // withdrawals before a match fails: allocation_failed
	MaxMatchLength  time.Duration // a ready match whose server never reports fails: server_lost
}

var DefaultAllocation = AllocationPolicy{
	NoServerTimeout: 30 * time.Second,
	AckTimeout:      5 * time.Second,
	MaxAttempts:     3,
	MaxMatchLength:  time.Hour,
}

// Co-op Elo: the team (its average rating) against a fixed opponent, the content itself.
const (
	contentRating = 1500
	eloK          = 32
)

var (
	// ErrUnauthorized is a result report whose credential does not match the match's current
	// allocation (wrong, withdrawn, or the match has expired).
	ErrUnauthorized = errors.New("not authorized to report this match")
	// ErrResultConflict is a report whose outcome differs from the one already recorded.
	ErrResultConflict = errors.New("a different result was already recorded")
)

// allocate drives every match waiting for a server one step: claim a server, give up on one that
// did not acknowledge, or fail the match. Called by the director each round, after matching.
func (s *Service) allocate(ctx context.Context) error {
	ids, err := s.rdb.ZRange(ctx, pendingKey, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("read pending matches: %w", err)
	}
	now := s.now()
	for _, mid := range ids {
		m, err := s.loadMatch(ctx, mid)
		if errors.Is(err, errNoMatch) {
			s.rdb.ZRem(ctx, pendingKey, mid)
			continue
		}
		if err != nil {
			return err
		}

		switch m.State {
		case StateMatched:
			if err := s.claimFor(ctx, m, now); err != nil {
				return err
			}

		case StateAllocating:
			if now.Sub(m.AllocAt) <= s.alloc.AckTimeout {
				continue
			}
			res, err := retryScript.Run(ctx, s.rdb, []string{matchKey(m.ID)}, m.AllocID, s.alloc.MaxAttempts).Text()
			if err != nil {
				return fmt.Errorf("withdraw allocation: %w", err)
			}
			if res == "stale" {
				continue // acknowledged in the meantime
			}
			// Withdrawn on the fleet side too, so the server's late acknowledgement is refused and
			// its agent shuts it down.
			s.fleet.Withdraw(ctx, m.ServerID, m.AllocID)
			s.log.Warn("allocation not acknowledged in time: withdrawn", "match_id", m.ID, "server_id", m.ServerID)
			if res == "exhausted" {
				s.failMatch(ctx, m, "allocation_failed")
				continue
			}
			// Try the next server now, not next round: every second here is a second the players
			// spend looking at a loading screen.
			m.State, m.Attempts = StateMatched, m.Attempts+1
			if err := s.claimFor(ctx, m, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// claimFor claims a server for a matched match, or fails it once NoServerTimeout has passed.
func (s *Service) claimFor(ctx context.Context, m storedMatch, now time.Time) error {
	if now.Sub(m.Created) > s.alloc.NoServerTimeout {
		s.failMatch(ctx, m, "no_server")
		return nil
	}
	a, err := s.fleet.Claim(ctx, m.ID, m.Players, s.publicURL+"/v1/matches/"+m.ID+"/result")
	if errors.Is(err, fleet.ErrNoServer) {
		return nil // try again next round, until NoServerTimeout
	}
	if err != nil {
		return err
	}
	res, err := allocatingScript.Run(ctx, s.rdb, []string{matchKey(m.ID)},
		a.AllocID, a.ServerID, a.Address, hashToken(a.ResultToken), now.UnixMilli()).Text()
	if err != nil {
		return fmt.Errorf("record allocation: %w", err)
	}
	if res != "ok" {
		// The match moved on (failed) between reading it and claiming: give the server back.
		s.fleet.Withdraw(ctx, a.ServerID, a.AllocID)
		return nil
	}
	s.log.Info("server claimed", "match_id", m.ID, "server_id", a.ServerID, "attempt", m.Attempts+1)
	return nil
}

// superviseRunning fails ready matches whose server has disappeared (the agent stopped reporting
// it: the process died) or that never reported a result. Without this, a crashed server would
// leave its players marked active forever, unable to queue again.
func (s *Service) superviseRunning(ctx context.Context) error {
	ids, err := s.rdb.ZRange(ctx, runningKey, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("read running matches: %w", err)
	}
	for _, mid := range ids {
		m, err := s.loadMatch(ctx, mid)
		if errors.Is(err, errNoMatch) {
			s.rdb.ZRem(ctx, runningKey, mid)
			continue
		}
		if err != nil {
			return err
		}
		alive, err := s.fleet.ServerAlive(ctx, m.ServerID)
		if err != nil {
			return err
		}
		if !alive || s.now().Sub(m.ReadyAt) > s.alloc.MaxMatchLength {
			s.log.Warn("match lost its server", "match_id", m.ID, "server_id", m.ServerID, "server_alive", alive)
			s.failMatch(ctx, m, "server_lost")
		}
	}
	return nil
}

func (s *Service) failMatch(ctx context.Context, m storedMatch, reason string) {
	ok, err := s.endMatch(ctx, m, StateFailed, reason, "", 0)
	if err != nil {
		s.log.Error("fail match", "match_id", m.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	s.log.Info("match failed", "match_id", m.ID, "reason", reason)
	for _, p := range m.Players {
		s.pushAll(ctx, []string{p}, "ticket.failed", map[string]string{"ticket_id": m.ticketOf(p), "reason": reason})
	}
}

// ServerReady is fleet's ReadyHandler: a server acknowledged its allocation. The match goes ready,
// and every player gets match.ready with a connect token of their own.
func (s *Service) ServerReady(ctx context.Context, matchID, allocID, serverAddr string) error {
	res, err := readyScript.Run(ctx, s.rdb, []string{matchKey(matchID), pendingKey, runningKey},
		matchID, allocID, s.now().UnixMilli()).Text()
	if err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	if res != "ok" {
		return fleet.ErrWithdrawn
	}
	m, err := s.loadMatch(ctx, matchID)
	if err != nil {
		return err
	}
	if since := s.now().Sub(m.Created); since >= 0 {
		s.log.Info("match ready", "match_id", matchID, "server_addr", serverAddr,
			"matched_to_ready_ms", float64(since.Microseconds())/1000)
	}
	for _, p := range m.Players {
		tok, err := connecttoken.Mint(s.tokenKey, matchID, p, serverAddr, s.now())
		if err != nil {
			return err
		}
		s.pushAll(ctx, []string{p}, "match.ready", map[string]string{
			"ticket_id": m.ticketOf(p), "match_id": matchID, "server_addr": serverAddr, "connect_token": tok,
		})
	}
	return nil
}

// Result is a recorded match result.
type Result struct {
	MatchID      string
	Outcome      string
	RatingChange int
}

// ReportResult records a game server's result, once, however often it is reported.
//
// Three steps, each idempotent on its own, so a crash between any two is repaired by the server's
// retry:
//
//  1. INSERT the result into match_results (first report wins; a retry reads it back and reuses
//     the stored rating change, rather than recomputing from ratings that may have moved).
//  2. Apply the rating change through the profile module, which remembers (match, player) pairs
//     and applies each once.
//  3. End the match in Redis: finished, tickets released. Only the call that actually ends it
//     sends match.finished.
//
// No transaction spans the three, and none can: they are two modules and two stores. Idempotence
// is what replaces the transaction.
func (s *Service) ReportResult(ctx context.Context, matchID, resultToken, outcome string) (Result, error) {
	m, err := s.loadMatch(ctx, matchID)
	if errors.Is(err, errNoMatch) {
		return Result{}, ErrUnauthorized
	}
	if err != nil {
		return Result{}, err
	}
	// Constant-time comparison of the hashes: see fleet.requireAgent for why == is not enough.
	if m.ResultHash == "" || subtle.ConstantTimeCompare([]byte(hashToken(resultToken)), []byte(m.ResultHash)) != 1 {
		return Result{}, ErrUnauthorized
	}
	if m.State != StateReady && m.State != StateFinished {
		return Result{}, ErrUnauthorized
	}

	ratings, err := s.ratings.Ratings(ctx, m.Players)
	if err != nil {
		return Result{}, err
	}
	change := eloChange(averageRating(ratings, m.Players), outcome)

	var stored Result
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO match_results (match_id, mode, outcome, players, rating_change)
			 VALUES ($1, $2, $3, $4::uuid[], $5)
			 ON CONFLICT (match_id) DO NOTHING`,
			m.ID, m.Mode, outcome, m.Players, change); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT match_id::text, outcome, rating_change FROM match_results WHERE match_id = $1`,
			m.ID).Scan(&stored.MatchID, &stored.Outcome, &stored.RatingChange)
	})
	if err != nil {
		return Result{}, fmt.Errorf("record result: %w", err)
	}
	if stored.Outcome != outcome {
		return Result{}, ErrResultConflict
	}

	if err := s.ratings.ApplyRatingChange(ctx, m.ID, m.Players, stored.RatingChange); err != nil {
		return Result{}, err
	}

	ended, err := s.endMatch(ctx, m, StateFinished, "", stored.Outcome, stored.RatingChange)
	if err != nil {
		return Result{}, err
	}
	if ended {
		s.log.Info("match finished", "match_id", m.ID, "outcome", stored.Outcome, "rating_change", stored.RatingChange)
		s.pushAll(ctx, m.Players, "match.finished", map[string]any{
			"match_id": m.ID, "outcome": stored.Outcome, "rating_change": stored.RatingChange,
		})
	}
	return stored, nil
}

// eloChange is every player's rating change for a co-op result: the standard Elo update for the
// team against the content's fixed rating. Expected score E = 1 / (1 + 10^((Rc − Rteam)/400)),
// change = K × (S − E), with S = 1 for victory and 0 for defeat. At even odds that is ±16. Beating
// content the team was expected to beat earns less; losing to it costs more.
func eloChange(teamRating float64, outcome string) int {
	expected := 1 / (1 + math.Pow(10, (contentRating-teamRating)/400))
	score := 0.0
	if outcome == "victory" {
		score = 1
	}
	return int(math.Round(eloK * (score - expected)))
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
