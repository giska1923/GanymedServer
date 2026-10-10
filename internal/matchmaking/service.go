// Package matchmaking turns queued players into played matches.
//
// A ticket is one player, or one party queued together by its leader. Tickets wait in a pool per
// mode, and once a second a director (one replica, chosen by a lease) runs the pure Match function
// over the pool, commits the matches it proposes, and gets each match a game server from the fleet.
// When the server acknowledges, players get its address and a connect token; when it reports the
// result, ratings change. Players learn each step by push (match.found, match.ready,
// match.finished, ticket.failed) and, authoritatively, from GET /v1/matchmaking/ticket.
//
// It owns the mm:* Redis keys (see store.go) and the match_results table. Parties, ratings, game
// servers and pushes come from other modules through interfaces it declares.
package matchmaking

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
	"github.com/giska1923/GanymedServer/internal/fleet"
	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/party"
)

var (
	ErrUnknownMode   = errors.New("no such mode")
	ErrNotLeader     = errors.New("only the party leader can queue the party")
	ErrAlreadyQueued = errors.New("a player on this ticket is already queued")
	ErrNoTicket      = errors.New("no such ticket")
	// ErrPartyTooLarge cannot happen with today's sizes (parties and coop both cap at 4); it
	// guards a future mode smaller than the largest party.
	ErrPartyTooLarge = errors.New("the party is larger than this mode allows")
)

// ErrNotQueued is returned when cancelling a ticket that has already left the queue. State says
// where it went.
type ErrNotQueued struct{ State string }

func (e ErrNotQueued) Error() string { return "ticket is no longer queued: " + e.State }

// Parties is the party module's view of membership.
type Parties interface {
	Roster(ctx context.Context, accountID string) (partyID, leaderID string, memberIDs []string, err error)
}

// Ratings is the profile module's view of skill: read for matching, written by results.
type Ratings interface {
	Ratings(ctx context.Context, accountIDs []string) (map[string]int, error)
	ApplyRatingChange(ctx context.Context, matchID string, accountIDs []string, delta int) error
}

// Fleet is the fleet module's view of game servers.
type Fleet interface {
	Claim(ctx context.Context, matchID string, players []string) (fleet.Allocation, error)
	Withdraw(ctx context.Context, serverID, allocID string) error
	ServerAlive(ctx context.Context, serverID string) (bool, error)
}

// Notifier sends pushes (the realtime gateway).
type Notifier interface {
	Notify(ctx context.Context, accountID, msgType string, payload any) error
}

// Deps is everything the service is built from: other modules, through the interfaces above, and
// its own stores. A struct rather than a parameter list, because there are now enough of them that
// positional arguments would be easy to swap by mistake.
type Deps struct {
	Redis    *redis.Client
	Postgres *pgxpool.Pool
	Parties  Parties
	Ratings  Ratings
	Fleet    Fleet
	Notifier Notifier
	TokenKey ed25519.PrivateKey // signs connect tokens
	Log      *slog.Logger
}

type Service struct {
	rdb      *redis.Client
	pool     *pgxpool.Pool
	parties  Parties
	ratings  Ratings
	fleet    Fleet
	notify   Notifier
	tokenKey ed25519.PrivateKey
	log      *slog.Logger
	policy   Policy
	alloc    AllocationPolicy
	now      func() time.Time
}

func NewService(d Deps) *Service {
	return &Service{rdb: d.Redis, pool: d.Postgres, parties: d.Parties, ratings: d.Ratings,
		fleet: d.Fleet, notify: d.Notifier, tokenKey: d.TokenKey, log: d.Log,
		policy: DefaultPolicy, alloc: DefaultAllocation, now: time.Now}
}

// TicketView is a ticket as the API shows it, to one caller (the connect token is theirs).
type TicketView struct {
	ID           string
	Mode         string
	State        string
	Players      []string
	Created      time.Time
	Reason       string
	MatchID      string
	MatchPlayers []string
	ServerAddr   string // set when ready
	ConnectToken string // set when ready: minted for the caller on every read
	Outcome      string // set when finished
	RatingChange int
}

// Enqueue queues the caller, or the caller's whole party if they lead one.
func (s *Service) Enqueue(ctx context.Context, accountID, modeName string) (TicketView, error) {
	mode, ok := Modes[modeName]
	if !ok {
		return TicketView{}, ErrUnknownMode
	}

	players := []string{accountID}
	_, leader, members, err := s.parties.Roster(ctx, accountID)
	switch {
	case errors.Is(err, party.ErrNotInParty):
		// solo
	case err != nil:
		return TicketView{}, err
	case leader != accountID:
		return TicketView{}, ErrNotLeader
	default:
		// The roster at this instant. A member who leaves the party later stays on the ticket;
		// see docs/backend/matchmaking.md.
		players = members
	}
	if len(players) > mode.MaxPlayers {
		return TicketView{}, ErrPartyTooLarge
	}

	ratings, err := s.ratings.Ratings(ctx, players)
	if err != nil {
		return TicketView{}, err
	}
	// The party's average. The alternative, the strongest member's rating, protects opponents in
	// competitive modes from a strong player carrying weak friends. In co-op, average is fair.
	rating := math.Round(averageRating(ratings, players))

	t := storedTicket{ID: id.New(), Mode: mode.Name, State: StateQueued, Players: players,
		Rating: rating, Created: s.now().UTC().Truncate(time.Millisecond)}
	keys := []string{ticketKey(t.ID), poolKey(mode.Name)}
	keys = append(keys, mapKeys(players, activeKey)...)
	keys = append(keys, mapKeys(players, lastKey)...)
	res, err := createScript.Run(ctx, s.rdb, keys,
		t.ID, mode.Name, strings.Join(players, ","), int64(rating), t.Created.UnixMilli(), len(players)).Text()
	if err != nil {
		return TicketView{}, fmt.Errorf("queue ticket: %w", err)
	}
	if res == "already-queued" {
		return TicketView{}, ErrAlreadyQueued
	}

	s.pushAll(ctx, players, "ticket.updated", map[string]string{"ticket_id": t.ID, "state": StateQueued})
	return s.view(ctx, t, accountID)
}

func averageRating(ratings map[string]int, players []string) float64 {
	sum := 0
	for _, p := range players {
		sum += ratings[p]
	}
	return float64(sum) / float64(len(players))
}

// Current is the caller's most recent ticket in any state, or nil: what a reconnecting client
// fetches, so a push lost while it was disconnected is never lost for good.
func (s *Service) Current(ctx context.Context, accountID string) (*TicketView, error) {
	tid, err := s.rdb.Get(ctx, lastKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up ticket: %w", err)
	}
	t, err := s.loadTicket(ctx, tid)
	if errors.Is(err, errNoTicket) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := s.view(ctx, t, accountID)
	return &v, err
}

// Ticket returns a ticket the caller is on. Someone else's ticket is ErrNoTicket, not "forbidden":
// whether a ticket ID exists is nobody else's business.
func (s *Service) Ticket(ctx context.Context, accountID, ticketID string) (TicketView, error) {
	t, err := s.ticketFor(ctx, accountID, ticketID)
	if err != nil {
		return TicketView{}, err
	}
	return s.view(ctx, t, accountID)
}

// Cancel takes a queued ticket out of the queue. Any player on it may.
func (s *Service) Cancel(ctx context.Context, accountID, ticketID string) (TicketView, error) {
	t, err := s.ticketFor(ctx, accountID, ticketID)
	if err != nil {
		return TicketView{}, err
	}
	res, err := s.finish(ctx, t, StateCancelled, "")
	if err != nil {
		return TicketView{}, err
	}
	if res != "ok" {
		return TicketView{}, ErrNotQueued{State: res}
	}
	t.State = StateCancelled

	var others []string
	for _, p := range t.Players {
		if p != accountID {
			others = append(others, p)
		}
	}
	s.pushAll(ctx, others, "ticket.updated", map[string]string{"ticket_id": t.ID, "state": StateCancelled})
	return s.view(ctx, t, accountID)
}

func (s *Service) ticketFor(ctx context.Context, accountID, ticketID string) (storedTicket, error) {
	t, err := s.loadTicket(ctx, ticketID)
	if errors.Is(err, errNoTicket) {
		return storedTicket{}, ErrNoTicket
	}
	if err != nil {
		return storedTicket{}, err
	}
	for _, p := range t.Players {
		if p == accountID {
			return t, nil
		}
	}
	return storedTicket{}, ErrNoTicket
}

// view shows a ticket to caller. A matched ticket shows its match's state (allocating, ready,
// finished, failed), and a ready one carries a connect token minted for the caller right now, so
// a client that missed match.ready, or let its token expire, gets a valid one by reading again.
// Tokens are never stored: there is nothing to leak at rest, and nothing to keep in sync.
func (s *Service) view(ctx context.Context, t storedTicket, caller string) (TicketView, error) {
	v := TicketView{ID: t.ID, Mode: t.Mode, State: t.State, Players: t.Players, Created: t.Created,
		Reason: t.Reason, MatchID: t.MatchID}
	if t.MatchID == "" {
		return v, nil
	}
	m, err := s.loadMatch(ctx, t.MatchID)
	if errors.Is(err, errNoMatch) {
		return v, nil // expired with its tickets a moment ago
	}
	if err != nil {
		return TicketView{}, err
	}
	v.MatchPlayers = m.Players
	v.State = m.State
	switch m.State {
	case StateReady:
		tok, err := connecttoken.Mint(s.tokenKey, m.ID, caller, m.ServerAddr, s.now())
		if err != nil {
			return TicketView{}, fmt.Errorf("mint connect token: %w", err)
		}
		v.ServerAddr, v.ConnectToken = m.ServerAddr, tok
	case StateFinished:
		v.Outcome, v.RatingChange = m.Outcome, m.RatingChange
	case StateFailed:
		v.Reason = m.Reason
	}
	return v, nil
}

// pushAll is fire-and-forget, as in the party module: the state is committed, and pushes are
// nudges.
func (s *Service) pushAll(ctx context.Context, accountIDs []string, msgType string, payload any) {
	for _, a := range accountIDs {
		if err := s.notify.Notify(ctx, a, msgType, payload); err != nil {
			s.log.Warn("push failed", "type", msgType, "account_id", a, "err", err)
		}
	}
}
