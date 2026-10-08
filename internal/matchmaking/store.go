package matchmaking

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Keys, all owned by this package:
//
//	mm:ticket:<id>     hash: mode, state, players (comma-separated), rating, created_ms, match_id, reason
//	mm:pool:<mode>     sorted set of queued ticket IDs, scored by created_ms: the pool, oldest first
//	mm:queued:<acct>   the ticket the player is queued on; exists ONLY while it is queued, which
//	                   is what enforces "one queued ticket per player"
//	mm:last:<acct>     the player's most recent ticket in any state, for GET /v1/matchmaking/ticket
//	mm:match:<id>      hash: mode, tickets, players, created_ms (B5 allocates from this)
//	mm:lease           the replica currently running the director
//
// Queued tickets have no TTL. Once a ticket leaves the queue (matched, cancelled, failed), it, its
// players' mm:last keys and its match expire after terminalTTL: long enough for a client that
// missed the push to reconnect and read the outcome.
const terminalTTL = 10 * time.Minute

func ticketKey(id string) string   { return "mm:ticket:" + id }
func poolKey(mode string) string   { return "mm:pool:" + mode }
func queuedKey(acct string) string { return "mm:queued:" + acct }
func lastKey(acct string) string   { return "mm:last:" + acct }
func matchKey(id string) string    { return "mm:match:" + id }

const leaseKey = "mm:lease"

// Ticket states (docs/api/openapi.yaml). B5 adds allocating and ready after matched.
const (
	StateQueued    = "queued"
	StateMatched   = "matched"
	StateCancelled = "cancelled"
	StateFailed    = "failed"
)

// As in the party module, every multi-key change is a Lua script, so its checks and its writes
// cannot be interleaved by another replica, and every key it touches is passed in KEYS.

// createScript queues a ticket, unless any of its players is already queued.
//
// KEYS: ticket, pool, queued[1..n], last[1..n]
// ARGV: ticketID, mode, players, rating, created_ms, n
var createScript = redis.NewScript(`
local n = tonumber(ARGV[6])
for i = 1, n do
  if redis.call('EXISTS', KEYS[2 + i]) == 1 then return 'already-queued' end
end
redis.call('HSET', KEYS[1], 'mode', ARGV[2], 'state', 'queued', 'players', ARGV[3],
           'rating', ARGV[4], 'created_ms', ARGV[5])
redis.call('ZADD', KEYS[2], ARGV[5], ARGV[1])
for i = 1, n do
  redis.call('SET', KEYS[2 + i], ARGV[1])
  redis.call('SET', KEYS[2 + n + i], ARGV[1])
end
return 'ok'
`)

// finishScript moves a queued ticket to cancelled or failed. Anything other than queued is
// refused, and the current state returned: that is what makes "cancel" and "match" mutually
// exclusive. Whichever script runs first wins, and the other sees the result.
//
// KEYS: ticket, pool, queued[1..n], last[1..n]
// ARGV: ticketID, newState, reason, ttl_ms, n
var finishScript = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'state')
if state ~= 'queued' then return state or 'missing' end
local n = tonumber(ARGV[5])
redis.call('HSET', KEYS[1], 'state', ARGV[2], 'reason', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('ZREM', KEYS[2], ARGV[1])
for i = 1, n do
  if redis.call('GET', KEYS[2 + i]) == ARGV[1] then redis.call('DEL', KEYS[2 + i]) end
  if redis.call('GET', KEYS[2 + n + i]) == ARGV[1] then redis.call('PEXPIRE', KEYS[2 + n + i], ARGV[4]) end
end
return 'ok'
`)

// matchScript commits one proposed match: every ticket queued → matched, all or nothing.
//
// This script is the fence. The director that proposed the match may no longer hold the lease
// (it paused for longer than the lease TTL, and another replica took over): a "zombie leader".
// It does not matter, because the script re-checks every ticket's state at commit time. A zombie's
// stale proposal finds tickets that the new leader already matched, or a player already
// cancelled, and commits nothing. In lease terms: the store refuses a write that is based on
// stale state, which is what a fencing token is for. Here the tickets' own states play that role.
//
// KEYS: match, pool, ticket[1..t], queued[1..p], last[1..p]
// ARGV: matchID, t, p, ttl_ms, mode, created_ms, tickets, players, ticketID[1..t], ticketOfPlayer[1..p]
var matchScript = redis.NewScript(`
local t, p = tonumber(ARGV[2]), tonumber(ARGV[3])
for i = 1, t do
  if redis.call('HGET', KEYS[2 + i], 'state') ~= 'queued' then return ARGV[8 + i] end
end
for i = 1, t do
  redis.call('HSET', KEYS[2 + i], 'state', 'matched', 'match_id', ARGV[1])
  redis.call('PEXPIRE', KEYS[2 + i], ARGV[4])
  redis.call('ZREM', KEYS[2], ARGV[8 + i])
end
for j = 1, p do
  local tid = ARGV[8 + t + j]
  if redis.call('GET', KEYS[2 + t + j]) == tid then redis.call('DEL', KEYS[2 + t + j]) end
  if redis.call('GET', KEYS[2 + t + p + j]) == tid then redis.call('PEXPIRE', KEYS[2 + t + p + j], ARGV[4]) end
end
redis.call('HSET', KEYS[1], 'mode', ARGV[5], 'created_ms', ARGV[6], 'tickets', ARGV[7], 'players', ARGV[8])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 'ok'
`)

// storedTicket is a ticket as the hash holds it.
type storedTicket struct {
	ID      string
	Mode    string
	State   string
	Players []string
	Rating  float64
	Created time.Time
	MatchID string
	Reason  string
}

var errNoTicket = errors.New("no such ticket")

func (s *Service) loadTicket(ctx context.Context, id string) (storedTicket, error) {
	h, err := s.rdb.HGetAll(ctx, ticketKey(id)).Result()
	if err != nil {
		return storedTicket{}, fmt.Errorf("load ticket: %w", err)
	}
	if len(h) == 0 {
		return storedTicket{}, errNoTicket
	}
	return parseTicket(id, h)
}

func parseTicket(id string, h map[string]string) (storedTicket, error) {
	ms, err := strconv.ParseInt(h["created_ms"], 10, 64)
	if err != nil {
		return storedTicket{}, fmt.Errorf("ticket %s: bad created_ms: %w", id, err)
	}
	rating, _ := strconv.ParseFloat(h["rating"], 64)
	return storedTicket{
		ID: id, Mode: h["mode"], State: h["state"], Players: splitList(h["players"]),
		Rating: rating, Created: time.UnixMilli(ms).UTC(), MatchID: h["match_id"], Reason: h["reason"],
	}, nil
}

// loadPool reads a mode's queued tickets, oldest first, in two round trips however large the
// pool: one ZRANGE, then every ticket's hash in one pipeline.
func (s *Service) loadPool(ctx context.Context, mode string) ([]storedTicket, error) {
	ids, err := s.rdb.ZRange(ctx, poolKey(mode), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("read pool: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			cmds[i] = p.HGetAll(ctx, ticketKey(id))
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read pool tickets: %w", err)
	}
	out := make([]storedTicket, 0, len(ids))
	for i, id := range ids {
		h := cmds[i].Val()
		if len(h) == 0 || h["state"] != StateQueued {
			continue // left the queue between the two reads; the next round won't see it
		}
		t, err := parseTicket(id, h)
		if err != nil {
			s.log.Warn("unreadable ticket skipped", "ticket_id", id, "err", err)
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// finish runs finishScript. It returns "ok", or the state that prevented it.
func (s *Service) finish(ctx context.Context, t storedTicket, state, reason string) (string, error) {
	keys := []string{ticketKey(t.ID), poolKey(t.Mode)}
	keys = append(keys, mapKeys(t.Players, queuedKey)...)
	keys = append(keys, mapKeys(t.Players, lastKey)...)
	res, err := finishScript.Run(ctx, s.rdb, keys,
		t.ID, state, reason, terminalTTL.Milliseconds(), len(t.Players)).Text()
	if err != nil {
		return "", fmt.Errorf("finish ticket: %w", err)
	}
	return res, nil
}

// commitMatch runs matchScript for one group. ok is false when the fence refused it.
func (s *Service) commitMatch(ctx context.Context, matchID, mode string, group []storedTicket, created time.Time) (ok bool, players []string, err error) {
	var ticketIDs, playerTickets []string
	for _, t := range group {
		ticketIDs = append(ticketIDs, t.ID)
		for _, p := range t.Players {
			players = append(players, p)
			playerTickets = append(playerTickets, t.ID)
		}
	}

	keys := []string{matchKey(matchID), poolKey(mode)}
	keys = append(keys, mapKeys(ticketIDs, ticketKey)...)
	keys = append(keys, mapKeys(players, queuedKey)...)
	keys = append(keys, mapKeys(players, lastKey)...)

	args := []any{matchID, len(ticketIDs), len(players), terminalTTL.Milliseconds(), mode,
		created.UnixMilli(), strings.Join(ticketIDs, ","), strings.Join(players, ",")}
	for _, id := range ticketIDs {
		args = append(args, id)
	}
	for _, id := range playerTickets {
		args = append(args, id)
	}

	res, err := matchScript.Run(ctx, s.rdb, keys, args...).Text()
	if err != nil {
		return false, nil, fmt.Errorf("commit match: %w", err)
	}
	return res == "ok", players, nil
}

func (s *Service) loadMatchPlayers(ctx context.Context, matchID string) ([]string, error) {
	v, err := s.rdb.HGet(ctx, matchKey(matchID), "players").Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil // expired with its tickets
	}
	if err != nil {
		return nil, fmt.Errorf("load match: %w", err)
	}
	return splitList(v), nil
}

func mapKeys(ids []string, key func(string) string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = key(id)
	}
	return out
}

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}
