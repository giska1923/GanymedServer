// Package party is groups of up to MaxSize players who intend to play together.
//
// It owns the Redis keys party:<id>, party:<id>:members, member:<account>, invites:<account> and
// the set "parties". Parties live in Redis, not Postgres: they are ephemeral by nature, and
// nothing about one needs to survive a Redis restart. Losing every party is an inconvenience;
// players re-form them.
//
// Presence comes from the realtime module, and pushes go out through it, via two interfaces this
// package declares. It never reads realtime's keys.
package party

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/realtime"
)

const (
	// MaxSize is the Proving Ground's co-op size.
	MaxSize = 4
	// InviteTTL is how long an invite can be accepted.
	InviteTTL = 5 * time.Minute
)

var (
	ErrAlreadyInParty = errors.New("already in a party")
	ErrNotInParty     = errors.New("not in a party")
	ErrNotLeader      = errors.New("only the party leader can do that")
	ErrPartyFull      = errors.New("the party is full")
	ErrNoInvite       = errors.New("no such invite, or it expired")
	ErrInvalidTarget  = errors.New("invalid target player")
)

// Presence is realtime's view of who is connected. Declared here, by the consumer.
type Presence interface {
	Status(ctx context.Context, accountIDs []string) (map[string]realtime.Status, error)
}

// Notifier sends a push to a player, wherever they are connected.
type Notifier interface {
	Notify(ctx context.Context, accountID, msgType string, payload any) error
}

// Names resolves display names (satisfied by profile.Service, as for the leaderboard).
type Names interface {
	DisplayNames(ctx context.Context, accountIDs []string) (map[string]string, error)
}

type Service struct {
	rdb      *redis.Client
	presence Presence
	notify   Notifier
	names    Names
	log      *slog.Logger
	now      func() time.Time
}

func NewService(rdb *redis.Client, presence Presence, notify Notifier, names Names, log *slog.Logger) *Service {
	return &Service{rdb: rdb, presence: presence, notify: notify, names: names, log: log, now: time.Now}
}

// Party is the state of record, as GET /v1/party returns it.
type Party struct {
	ID       string
	LeaderID string
	Members  []Member // in join order
}

type Member struct {
	AccountID   string
	DisplayName string
	Status      realtime.Status
}

type Invite struct {
	PartyID   string
	FromID    string
	FromName  string
	ExpiresAt time.Time
}

func partyKey(pid string) string    { return "party:" + pid }
func membersKey(pid string) string  { return "party:" + pid + ":members" }
func memberKey(acct string) string  { return "member:" + acct }
func invitesKey(acct string) string { return "invites:" + acct }

const partiesKey = "parties"

// Every mutation is a Lua script, because each one checks and changes several keys that must
// agree: "in at most one party" (member:<acct>), "at most MaxSize" (the members set), "exactly one
// leader". Redis runs a script atomically: no other command interleaves. The alternative,
// WATCH/MULTI optimistic transactions, retries on conflict and is harder to read.
//
// Scripts return a table whose first element is a status word, mapped to an error by result.
// Every key a script touches is passed in KEYS, never built inside the script. On one Redis node
// that does not matter, but Redis Cluster routes a script by its declared keys, and scripts that
// follow the rule stay portable. (Cluster would also need all of a party's keys in one hash slot,
// e.g. party:{id}. Not done: there is no cluster.)

var createScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return {'already-in-party'} end
redis.call('SET', KEYS[1], ARGV[2])
redis.call('HSET', KEYS[2], 'leader', ARGV[1])
redis.call('ZADD', KEYS[3], ARGV[3], ARGV[1])
redis.call('SADD', KEYS[4], ARGV[2])
return {'ok'}
`)

var inviteScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[2] then return {'not-in-party'} end
if redis.call('HGET', KEYS[2], 'leader') ~= ARGV[1] then return {'not-leader'} end
if redis.call('GET', KEYS[5]) == ARGV[2] then return {'already-in-party'} end
if redis.call('ZCARD', KEYS[3]) >= tonumber(ARGV[5]) then return {'party-full'} end
redis.call('HSET', KEYS[4], ARGV[2], ARGV[4])
return {'ok'}
`)

var acceptScript = redis.NewScript(`
local inv = redis.call('HGET', KEYS[1], ARGV[2])
if not inv or tonumber(string.match(inv, '^(%d+)')) < tonumber(ARGV[3]) then
  redis.call('HDEL', KEYS[1], ARGV[2])
  return {'no-invite'}
end
if redis.call('EXISTS', KEYS[2]) == 1 then return {'already-in-party'} end
if redis.call('EXISTS', KEYS[3]) == 0 then
  redis.call('HDEL', KEYS[1], ARGV[2])
  return {'no-invite'}
end
if redis.call('ZCARD', KEYS[4]) >= tonumber(ARGV[4]) then return {'party-full'} end
redis.call('HDEL', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[2])
redis.call('ZADD', KEYS[4], ARGV[3], ARGV[1])
local out = {'ok'}
for _, m in ipairs(redis.call('ZRANGE', KEYS[4], 0, -1)) do out[#out + 1] = m end
return out
`)

// removeScript takes a member out, for three reasons: they left, the leader kicked them, or the
// sweeper found them offline. If the leader goes, the longest-standing remaining member is
// promoted. The last member out deletes the party.
var removeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[2] then return {'not-in-party'} end
if ARGV[3] == 'kick' and redis.call('HGET', KEYS[2], 'leader') ~= ARGV[4] then return {'not-leader'} end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[3], ARGV[1])
local remaining = redis.call('ZRANGE', KEYS[3], 0, -1)
if #remaining == 0 then
  redis.call('DEL', KEYS[2], KEYS[3])
  redis.call('SREM', KEYS[4], ARGV[2])
  return {'disbanded'}
end
if redis.call('HGET', KEYS[2], 'leader') == ARGV[1] then
  redis.call('HSET', KEYS[2], 'leader', remaining[1])
end
local out = {'ok'}
for _, m in ipairs(remaining) do out[#out + 1] = m end
return out
`)

// run executes a script and splits its reply into the status word and the member list.
func (s *Service) run(ctx context.Context, script *redis.Script, keys []string, args ...any) (string, []string, error) {
	reply, err := script.Run(ctx, s.rdb, keys, args...).StringSlice()
	if err != nil {
		return "", nil, fmt.Errorf("party script: %w", err)
	}
	return reply[0], reply[1:], nil
}

var statusErrors = map[string]error{
	"already-in-party": ErrAlreadyInParty,
	"not-in-party":     ErrNotInParty,
	"not-leader":       ErrNotLeader,
	"party-full":       ErrPartyFull,
	"no-invite":        ErrNoInvite,
}

// Create starts a party led by the caller.
func (s *Service) Create(ctx context.Context, accountID string) (Party, error) {
	pid := id.New()
	status, _, err := s.run(ctx, createScript,
		[]string{memberKey(accountID), partyKey(pid), membersKey(pid), partiesKey},
		accountID, pid, s.now().UnixMilli())
	if err != nil {
		return Party{}, err
	}
	if e := statusErrors[status]; e != nil {
		return Party{}, e
	}
	return s.Get(ctx, accountID)
}

// Get returns the caller's party, or ErrNotInParty.
func (s *Service) Get(ctx context.Context, accountID string) (Party, error) {
	pid, err := s.rdb.Get(ctx, memberKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		return Party{}, ErrNotInParty
	}
	if err != nil {
		return Party{}, fmt.Errorf("look up party: %w", err)
	}

	// Leader and members in one MULTI/EXEC, so they are read from the same instant: a leave that
	// promotes a new leader cannot land between the two reads.
	var leader *redis.StringCmd
	var members *redis.StringSliceCmd
	if _, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		leader = p.HGet(ctx, partyKey(pid), "leader")
		members = p.ZRange(ctx, membersKey(pid), 0, -1)
		return nil
	}); err != nil && !errors.Is(err, redis.Nil) {
		return Party{}, fmt.Errorf("load party: %w", err)
	}
	ids := members.Val()
	if len(ids) == 0 {
		return Party{}, ErrNotInParty // disbanded between the two steps
	}

	statuses, err := s.presence.Status(ctx, ids)
	if err != nil {
		return Party{}, err
	}
	names, err := s.names.DisplayNames(ctx, ids)
	if err != nil {
		return Party{}, err
	}
	p := Party{ID: pid, LeaderID: leader.Val()}
	for _, m := range ids {
		p.Members = append(p.Members, Member{AccountID: m, DisplayName: names[m], Status: statuses[m]})
	}
	return p, nil
}

// Invite lets the leader invite a player. The invitee is nudged with party.invite.
func (s *Service) Invite(ctx context.Context, leaderID, targetID string) error {
	if targetID == leaderID {
		return ErrInvalidTarget
	}
	pid, err := s.rdb.Get(ctx, memberKey(leaderID)).Result()
	if errors.Is(err, redis.Nil) {
		return ErrNotInParty
	}
	if err != nil {
		return fmt.Errorf("look up party: %w", err)
	}

	expires := s.now().Add(InviteTTL)
	// The invite stores "<expires unix ms>:<inviter>", which is everything the invitee's list needs.
	status, _, err := s.run(ctx, inviteScript,
		[]string{memberKey(leaderID), partyKey(pid), membersKey(pid), invitesKey(targetID), memberKey(targetID)},
		leaderID, pid, targetID, fmt.Sprintf("%d:%s", expires.UnixMilli(), leaderID), MaxSize)
	if err != nil {
		return err
	}
	if e := statusErrors[status]; e != nil {
		return e
	}

	names, err := s.names.DisplayNames(ctx, []string{leaderID})
	if err != nil {
		return err
	}
	s.push(ctx, targetID, "party.invite", map[string]any{
		"party_id":   pid,
		"from":       map[string]string{"account_id": leaderID, "display_name": names[leaderID]},
		"expires_at": expires.UTC(),
	})
	return nil
}

// Invites lists the caller's unexpired invites, pruning expired ones as it goes.
func (s *Service) Invites(ctx context.Context, accountID string) ([]Invite, error) {
	raw, err := s.rdb.HGetAll(ctx, invitesKey(accountID)).Result()
	if err != nil {
		return nil, fmt.Errorf("load invites: %w", err)
	}
	now := s.now()
	var out []Invite
	var expired []string
	for pid, v := range raw {
		msStr, from, ok := strings.Cut(v, ":")
		ms, err := strconv.ParseInt(msStr, 10, 64)
		if !ok || err != nil || time.UnixMilli(ms).Before(now) {
			expired = append(expired, pid)
			continue
		}
		out = append(out, Invite{PartyID: pid, FromID: from, ExpiresAt: time.UnixMilli(ms).UTC()})
	}
	if len(expired) > 0 {
		s.rdb.HDel(ctx, invitesKey(accountID), expired...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })

	ids := make([]string, len(out))
	for i, inv := range out {
		ids[i] = inv.FromID
	}
	names, err := s.names.DisplayNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].FromName = names[out[i].FromID]
	}
	return out, nil
}

// Accept joins the party the invite is for. Every member, the newcomer included, is nudged.
func (s *Service) Accept(ctx context.Context, accountID, pid string) (Party, error) {
	status, members, err := s.run(ctx, acceptScript,
		[]string{invitesKey(accountID), memberKey(accountID), partyKey(pid), membersKey(pid)},
		accountID, pid, s.now().UnixMilli(), MaxSize)
	if err != nil {
		return Party{}, err
	}
	if e := statusErrors[status]; e != nil {
		return Party{}, e
	}
	s.pushAll(ctx, members, "party.updated", map[string]string{"party_id": pid})
	return s.Get(ctx, accountID)
}

// Decline drops an invite. Declining one that does not exist is not an error: the end state is
// the same.
func (s *Service) Decline(ctx context.Context, accountID, pid string) error {
	if err := s.rdb.HDel(ctx, invitesKey(accountID), pid).Err(); err != nil {
		return fmt.Errorf("decline invite: %w", err)
	}
	return nil
}

// Leave takes the caller out of their party.
func (s *Service) Leave(ctx context.Context, accountID string) error {
	_, err := s.remove(ctx, accountID, "leave", "")
	return err
}

// Kick lets the leader remove a member, who is nudged with party.removed.
func (s *Service) Kick(ctx context.Context, leaderID, targetID string) error {
	if targetID == leaderID {
		return ErrInvalidTarget
	}
	pid, err := s.rdb.Get(ctx, memberKey(leaderID)).Result()
	if errors.Is(err, redis.Nil) {
		return ErrNotInParty
	}
	if err != nil {
		return fmt.Errorf("look up party: %w", err)
	}
	// The target must be in the leader's party: remove checks member:<target> == pid.
	removed, err := s.removeFrom(ctx, targetID, pid, "kick", leaderID)
	if err != nil {
		return err
	}
	if removed {
		s.push(ctx, targetID, "party.removed", map[string]string{"party_id": pid, "reason": "kicked"})
	}
	return nil
}

func (s *Service) remove(ctx context.Context, accountID, mode, requester string) (bool, error) {
	pid, err := s.rdb.Get(ctx, memberKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		return false, ErrNotInParty
	}
	if err != nil {
		return false, fmt.Errorf("look up party: %w", err)
	}
	return s.removeFrom(ctx, accountID, pid, mode, requester)
}

// removeFrom runs the remove script for one member of party pid and nudges whoever remains. The
// party ID is read before the script and re-checked inside it (member:<acct> == pid), so a member
// who moved parties in between is refused, not removed from the wrong one.
func (s *Service) removeFrom(ctx context.Context, accountID, pid, mode, requester string) (bool, error) {
	status, remaining, err := s.run(ctx, removeScript,
		[]string{memberKey(accountID), partyKey(pid), membersKey(pid), partiesKey},
		accountID, pid, mode, requester)
	if err != nil {
		return false, err
	}
	if status == "disbanded" {
		return true, nil
	}
	if e := statusErrors[status]; e != nil {
		return false, e
	}
	s.pushAll(ctx, remaining, "party.updated", map[string]string{"party_id": pid})
	return true, nil
}

// Sweep removes every party member who is offline, meaning their reconnection grace has run out.
//
// It runs on every replica (RunSweeper). That is safe without coordination: the remove script is
// atomic and checks membership first, so when two replicas sweep the same member, exactly one
// removes them and only that one sends the nudges.
//
// It is also what makes grace work when a replica crashes. The crashed replica cannot run a
// timer for its players, but their presence keys expire on their own, and any surviving replica's
// sweep then finds them offline.
//
// One race is accepted rather than closed: a player who reconnects between the presence read and
// the removal is removed anyway, right at the edge of their 30 s grace.
func (s *Service) Sweep(ctx context.Context) (removed int, err error) {
	pids, err := s.rdb.SMembers(ctx, partiesKey).Result()
	if err != nil {
		return 0, fmt.Errorf("list parties: %w", err)
	}
	for _, pid := range pids {
		members, err := s.rdb.ZRange(ctx, membersKey(pid), 0, -1).Result()
		if err != nil {
			return removed, fmt.Errorf("list members: %w", err)
		}
		statuses, err := s.presence.Status(ctx, members)
		if err != nil {
			return removed, err
		}
		for _, m := range members {
			if statuses[m] != realtime.StatusOffline {
				continue
			}
			ok, err := s.removeFrom(ctx, m, pid, "sweep", "")
			if errors.Is(err, ErrNotInParty) {
				continue // another replica's sweep got there first
			}
			if err != nil {
				return removed, err
			}
			if ok {
				removed++
				s.log.Info("party member removed: grace expired", "party_id", pid, "account_id", m)
				// They are offline, so this push is almost certainly lost. Sent anyway: a
				// reconnecting client that catches it saves an HTTP round trip.
				s.push(ctx, m, "party.removed", map[string]string{"party_id": pid, "reason": "disconnected"})
			}
		}
	}
	return removed, nil
}

// RunSweeper sweeps every interval until ctx is cancelled.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("party sweep failed", "err", err)
			}
		}
	}
}

// push is fire-and-forget: a push is a nudge, and a failed nudge must not fail the action that
// caused it. The state is already committed, and clients re-fetch it.
func (s *Service) push(ctx context.Context, accountID, msgType string, payload any) {
	if err := s.notify.Notify(ctx, accountID, msgType, payload); err != nil {
		s.log.Warn("push failed", "type", msgType, "account_id", accountID, "err", err)
	}
}

func (s *Service) pushAll(ctx context.Context, accountIDs []string, msgType string, payload any) {
	for _, a := range accountIDs {
		s.push(ctx, a, msgType, payload)
	}
}
