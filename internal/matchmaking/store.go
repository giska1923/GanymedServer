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
//	mm:active:<acct>   the player's ticket while it is ACTIVE: from queueing until its match ends
//	                   (finished or failed) or it is cancelled. Its existence is what enforces "one
//	                   active ticket per player"; a matched player cannot queue again mid-match.
//	mm:last:<acct>     the player's most recent ticket in any state, for GET /v1/matchmaking/ticket
//	mm:match:<id>      hash: the match and its lifecycle (state, allocation, server, result)
//	mm:pending         sorted set of matches waiting for a server (matched, allocating), by created_ms
//	mm:running         sorted set of matches being played (ready), by ready_ms
//	mm:lease           the replica currently running the director
//
// A ticket keeps state "matched" from the moment it joins a match. Everything after that
// (allocating, ready, finished, failed) is the MATCH's state, held once in mm:match, not copied onto
// every ticket. The API shows a matched ticket with its match's state.
//
// Nothing active has a TTL. When a ticket stops being active (cancelled, failed, its match ended),
// it, its match, and its players' mm:last keys expire after terminalTTL: long enough for a client
// that missed the push to reconnect and read the outcome.
const terminalTTL = 10 * time.Minute

func ticketKey(id string) string   { return "mm:ticket:" + id }
func poolKey(mode string) string   { return "mm:pool:" + mode }
func activeKey(acct string) string { return "mm:active:" + acct }
func lastKey(acct string) string   { return "mm:last:" + acct }
func matchKey(id string) string    { return "mm:match:" + id }

const (
	leaseKey   = "mm:lease"
	pendingKey = "mm:pending"
	runningKey = "mm:running"
)

// Ticket and match states (docs/api/openapi.yaml).
const (
	StateQueued     = "queued"
	StateMatched    = "matched"
	StateAllocating = "allocating"
	StateReady      = "ready"
	StateFinished   = "finished"
	StateCancelled  = "cancelled"
	StateFailed     = "failed"
)

// As in the party module, every multi-key change is a Lua script, so its checks and its writes
// cannot be interleaved by another replica, and every key it touches is passed in KEYS.

// createScript queues a ticket, unless any of its players already has an active ticket.
//
// KEYS: ticket, pool, active[1..n], last[1..n]
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

// finishScript moves a QUEUED ticket to cancelled or failed. Anything other than queued is refused,
// and the current state returned: that is what makes "cancel" and "match" mutually exclusive.
// Whichever script runs first wins, and the other sees the result.
//
// KEYS: ticket, pool, active[1..n], last[1..n]
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

// matchScript commits one proposed match: every ticket queued → matched, all or nothing, and the
// match joins mm:pending to wait for a server.
//
// This script is the fence. The director that proposed the match may no longer hold the lease
// (it paused for longer than the lease TTL, and another replica took over): a "zombie leader".
// It does not matter, because the script re-checks every ticket's state at commit time. A zombie's
// stale proposal finds tickets that the new leader already matched, or a player already
// cancelled, and commits nothing. In lease terms: the store refuses a write that is based on
// stale state, which is what a fencing token is for. Here the tickets' own states play that role.
//
// The players' active keys are left alone: a matched player stays active until the match ends.
//
// KEYS: match, pool, pending, ticket[1..t]
// ARGV: matchID, t, mode, created_ms, tickets, players, playerTickets, ticketID[1..t]
var matchScript = redis.NewScript(`
local t = tonumber(ARGV[2])
for i = 1, t do
  if redis.call('HGET', KEYS[3 + i], 'state') ~= 'queued' then return ARGV[7 + i] end
end
for i = 1, t do
  redis.call('HSET', KEYS[3 + i], 'state', 'matched', 'match_id', ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[7 + i])
end
redis.call('HSET', KEYS[1], 'mode', ARGV[3], 'created_ms', ARGV[4], 'tickets', ARGV[5],
           'players', ARGV[6], 'player_tickets', ARGV[7], 'state', 'matched', 'attempts', 0)
redis.call('ZADD', KEYS[3], ARGV[4], ARGV[1])
return 'ok'
`)

// allocatingScript records a claimed server: matched → allocating. The result token's hash, not
// the token, is kept: the token is a credential, and Redis is not where credentials live in clear.
//
// KEYS: match
// ARGV: allocID, serverID, serverAddr, resultHash, now_ms
var allocatingScript = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'state')
if state ~= 'matched' then return state or 'missing' end
redis.call('HSET', KEYS[1], 'state', 'allocating', 'alloc_id', ARGV[1], 'server_id', ARGV[2],
           'server_addr', ARGV[3], 'result_hash', ARGV[4], 'alloc_ms', ARGV[5])
return 'ok'
`)

// retryScript gives up on an unacknowledged allocation: allocating → matched (to try another
// server), or "exhausted" after maxAttempts. Only for the allocation it names, so it cannot undo a
// newer one.
//
// KEYS: match
// ARGV: allocID, maxAttempts
var retryScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'allocating' or redis.call('HGET', KEYS[1], 'alloc_id') ~= ARGV[1] then
  return 'stale'
end
local attempts = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
if attempts >= tonumber(ARGV[2]) then return 'exhausted' end
redis.call('HSET', KEYS[1], 'state', 'matched', 'alloc_id', '', 'result_hash', '')
return 'retry'
`)

// readyScript records a server's acknowledgement: allocating → ready, if the allocation named is
// still the current one. A late acknowledgement for a withdrawn allocation is refused here.
//
// KEYS: match, pending, running
// ARGV: matchID, allocID, now_ms
var readyScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'allocating' or redis.call('HGET', KEYS[1], 'alloc_id') ~= ARGV[2] then
  return 'withdrawn'
end
redis.call('HSET', KEYS[1], 'state', 'ready', 'ready_ms', ARGV[3])
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('ZADD', KEYS[3], ARGV[3], ARGV[1])
return 'ok'
`)

// endScript ends a match: → finished (only from ready: a result needs a server that was given the
// match) or → failed (from any live state). Idempotent: ending an ended match changes nothing and
// returns "already", so a repeated result report sends no second round of pushes.
//
// KEYS: match, pending, running, ticket[1..t], active[1..p], last[1..p]
// ARGV: matchID, newState, reason, outcome, rating_change, ttl_ms, t, p, ticketID[1..t], ticketOfPlayer[1..p]
var endScript = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'state')
if state == 'finished' or state == 'failed' then return 'already' end
if not state then return 'missing' end
if ARGV[2] == 'finished' and state ~= 'ready' then return state end
local t, p, ttl = tonumber(ARGV[7]), tonumber(ARGV[8]), ARGV[6]
redis.call('HSET', KEYS[1], 'state', ARGV[2], 'reason', ARGV[3], 'outcome', ARGV[4], 'rating_change', ARGV[5])
redis.call('PEXPIRE', KEYS[1], ttl)
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('ZREM', KEYS[3], ARGV[1])
for i = 1, t do redis.call('PEXPIRE', KEYS[3 + i], ttl) end
for j = 1, p do
  local tid = ARGV[8 + t + j]
  if redis.call('GET', KEYS[3 + t + j]) == tid then redis.call('DEL', KEYS[3 + t + j]) end
  if redis.call('GET', KEYS[3 + t + p + j]) == tid then redis.call('PEXPIRE', KEYS[3 + t + p + j], ttl) end
end
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

// storedMatch is a match as the hash holds it.
type storedMatch struct {
	ID            string
	Mode          string
	State         string
	Tickets       []string
	Players       []string
	PlayerTickets []string // parallel to Players: which ticket each player is on
	Created       time.Time
	Attempts      int
	AllocID       string
	ServerID      string
	ServerAddr    string
	ResultHash    string
	AllocAt       time.Time
	ReadyAt       time.Time
	Reason        string
	Outcome       string
	RatingChange  int
}

func (m storedMatch) ticketOf(player string) string {
	for i, p := range m.Players {
		if p == player {
			return m.PlayerTickets[i]
		}
	}
	return ""
}

var (
	errNoTicket = errors.New("no such ticket")
	errNoMatch  = errors.New("no such match")
)

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

func (s *Service) loadMatch(ctx context.Context, id string) (storedMatch, error) {
	h, err := s.rdb.HGetAll(ctx, matchKey(id)).Result()
	if err != nil {
		return storedMatch{}, fmt.Errorf("load match: %w", err)
	}
	if len(h) == 0 {
		return storedMatch{}, errNoMatch
	}
	ms := func(k string) time.Time {
		v, _ := strconv.ParseInt(h[k], 10, 64)
		if v == 0 {
			return time.Time{}
		}
		return time.UnixMilli(v).UTC()
	}
	attempts, _ := strconv.Atoi(h["attempts"])
	change, _ := strconv.Atoi(h["rating_change"])
	return storedMatch{
		ID: id, Mode: h["mode"], State: h["state"], Tickets: splitList(h["tickets"]),
		Players: splitList(h["players"]), PlayerTickets: splitList(h["player_tickets"]),
		Created: ms("created_ms"), Attempts: attempts, AllocID: h["alloc_id"], ServerID: h["server_id"],
		ServerAddr: h["server_addr"], ResultHash: h["result_hash"], AllocAt: ms("alloc_ms"),
		ReadyAt: ms("ready_ms"), Reason: h["reason"], Outcome: h["outcome"], RatingChange: change,
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
	keys = append(keys, mapKeys(t.Players, activeKey)...)
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

	keys := []string{matchKey(matchID), poolKey(mode), pendingKey}
	keys = append(keys, mapKeys(ticketIDs, ticketKey)...)
	args := []any{matchID, len(ticketIDs), mode, created.UnixMilli(), strings.Join(ticketIDs, ","),
		strings.Join(players, ","), strings.Join(playerTickets, ",")}
	for _, id := range ticketIDs {
		args = append(args, id)
	}

	res, err := matchScript.Run(ctx, s.rdb, keys, args...).Text()
	if err != nil {
		return false, nil, fmt.Errorf("commit match: %w", err)
	}
	return res == "ok", players, nil
}

// endMatch runs endScript. transitioned is false when the match had already ended (or, for
// finished, was not ready): the caller then sends no pushes, which keeps retries silent.
func (s *Service) endMatch(ctx context.Context, m storedMatch, state, reason, outcome string, ratingChange int) (transitioned bool, err error) {
	keys := []string{matchKey(m.ID), pendingKey, runningKey}
	keys = append(keys, mapKeys(m.Tickets, ticketKey)...)
	keys = append(keys, mapKeys(m.Players, activeKey)...)
	keys = append(keys, mapKeys(m.Players, lastKey)...)
	args := []any{m.ID, state, reason, outcome, ratingChange, terminalTTL.Milliseconds(), len(m.Tickets), len(m.Players)}
	for _, t := range m.Tickets {
		args = append(args, t)
	}
	for _, t := range m.PlayerTickets {
		args = append(args, t)
	}
	res, err := endScript.Run(ctx, s.rdb, keys, args...).Text()
	if err != nil {
		return false, fmt.Errorf("end match: %w", err)
	}
	return res == "ok", nil
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
