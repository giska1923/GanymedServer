// Package fleet is the backend's view of the game servers: which agents are alive, which of their
// servers are ready, and the allocation of a ready server to a match.
//
// Agents run next to their game servers (cmd/fleetagent) and dial out to the backend over HTTP:
// a heartbeat every second reporting their servers, and a long-poll for commands. Neither needs
// the agent to reach a particular replica. State lives in Redis, and commands wait in a Redis
// list, so whichever replica an agent's request lands on can answer it. (A WebSocket per agent
// would pin each agent to one replica, and an allocation decided on another replica would then
// need B3's cross-replica routing to reach it.)
//
// It owns the Redis keys fleet:agent:*, fleet:server:*, fleet:ready and fleet:cmds:*.
package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
	"github.com/giska1923/GanymedServer/internal/id"
)

// Liveness: an agent heartbeats every second; anything it has not reported for LivenessTTL is
// gone. Its servers expire with it, so a dead agent's servers are never allocated.
const LivenessTTL = 5 * time.Second

// Server states. The agent reports starting, ready and shutdown. allocating and allocated are
// the backend's own and a heartbeat never overwrites them (see heartbeatScript).
const (
	StateStarting   = "starting"
	StateReady      = "ready"
	StateAllocating = "allocating"
	StateAllocated  = "allocated"
	StateShutdown   = "shutdown"
	StateWithdrawn  = "withdrawn"
)

var (
	ErrNoServer  = errors.New("no ready game server")
	ErrWithdrawn = errors.New("allocation withdrawn")
)

func agentKey(agent string) string { return "fleet:agent:" + agent }
func serverKey(sid string) string  { return "fleet:server:" + sid }
func cmdsKey(agent string) string  { return "fleet:cmds:" + agent }

const readyKey = "fleet:ready"

// ReadyHandler is told when a server has acknowledged its allocation. Matchmaking implements it.
// It is a function, set after construction (SetReadyHandler), because matchmaking also depends on
// fleet (to allocate): construction cannot pass each to the other. Neither package imports the
// other; main connects them.
type ReadyHandler func(ctx context.Context, matchID, allocID, serverAddr string) error

type Service struct {
	rdb     *redis.Client
	secret  []byte
	pubKey  string
	log     *slog.Logger
	onReady ReadyHandler
}

func NewService(rdb *redis.Client, agentSecret []byte, tokenKey ed25519.PrivateKey, log *slog.Logger) *Service {
	return &Service{rdb: rdb, secret: agentSecret, log: log,
		pubKey: connecttoken.EncodePublicKey(tokenKey.Public().(ed25519.PublicKey))}
}

func (s *Service) SetReadyHandler(h ReadyHandler) { s.onReady = h }

// ReportedServer is one server in a heartbeat.
type ReportedServer struct {
	ServerID string `json:"server_id"`
	Address  string `json:"address"`
	State    string `json:"state"`
}

// heartbeatScript records an agent's report.
//
// The rule that matters: a heartbeat never downgrades allocating/allocated/withdrawn to ready. The
// backend claims a server between two heartbeats, and the agent's next report still says "ready"
// because the allocate command has not reached it yet. Taking that report at its word would put
// the server back in the ready set, and it would be allocated twice. Server IDs are per process,
// so a server, once claimed, never genuinely becomes ready again. Its replacement has a new ID.
//
// It returns the servers to retire: withdrawn ones the agent still reports as alive. An allocate
// command is delivered at most once (BLPOP removes it before the HTTP answer is written), so a
// command lost on the way leaves the agent holding a server it believes is ready and the backend
// will never allocate again. Without retirement, every lost command shrinks the warm pool by one.
//
// KEYS: agent, ready, server[1..n]
// ARGV: ttl_ms, agentID, n, then (serverID, address, state) per server
var heartbeatScript = redis.NewScript(`
local ttl, n = ARGV[1], tonumber(ARGV[3])
local retire = {}
redis.call('SET', KEYS[1], 'alive', 'PX', ttl)
for i = 1, n do
  local key = KEYS[2 + i]
  local sid, addr, reported = ARGV[1 + 3 * i], ARGV[2 + 3 * i], ARGV[3 + 3 * i]
  local cur = redis.call('HGET', key, 'state')
  if cur == 'allocating' or cur == 'allocated' or cur == 'withdrawn' then
    if reported == 'shutdown' then
      redis.call('HSET', key, 'state', 'shutdown')
    elseif cur == 'withdrawn' then
      table.insert(retire, sid)
    end
  else
    redis.call('HSET', key, 'agent', ARGV[2], 'address', addr, 'state', reported)
    if reported == 'ready' then
      redis.call('SADD', KEYS[2], sid)
    else
      redis.call('SREM', KEYS[2], sid)
    end
  end
  redis.call('PEXPIRE', key, ttl)
end
return retire
`)

// Heartbeat records an agent's servers. It returns the connect-token public key for the agent to
// pass on, and the IDs of servers the agent must kill (see heartbeatScript).
func (s *Service) Heartbeat(ctx context.Context, agentID string, servers []ReportedServer) (pubKey string, retire []string, err error) {
	keys := []string{agentKey(agentID), readyKey}
	args := []any{LivenessTTL.Milliseconds(), agentID, len(servers)}
	for _, sv := range servers {
		keys = append(keys, serverKey(sv.ServerID))
		args = append(args, sv.ServerID, sv.Address, sv.State)
	}
	retire, err = heartbeatScript.Run(ctx, s.rdb, keys, args...).StringSlice()
	if err != nil {
		return "", nil, fmt.Errorf("record heartbeat: %w", err)
	}
	return s.pubKey, retire, nil
}

// Command is what an agent's long-poll receives.
type Command struct {
	Type        string   `json:"type"` // "allocate"
	ServerID    string   `json:"server_id"`
	MatchID     string   `json:"match_id"`
	Players     []string `json:"players"`
	ResultURL   string   `json:"result_url"`
	ResultToken string   `json:"result_token"`
}

// NextCommand blocks up to wait for a command for agentID (BLPOP), or returns nil.
//
// BLPOP is Redis' blocking pop: the call parks on the server until an element arrives in the list
// or the timeout passes. That makes a list a work queue with instant delivery and no polling
// loop. go-redis gives a blocking command a read timeout of its own timeout plus 10 s, so the
// client does not abandon the wait early.
func (s *Service) NextCommand(ctx context.Context, agentID string, wait time.Duration) (*Command, error) {
	res, err := s.rdb.BLPop(ctx, wait, cmdsKey(agentID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wait for command: %w", err)
	}
	var c Command
	if err := json.Unmarshal([]byte(res[1]), &c); err != nil {
		return nil, fmt.Errorf("decode command: %w", err)
	}
	return &c, nil
}

// claimScript allocates one server to a match: only if it is still ready and its agent alive.
//
// KEYS: ready, server, agent, cmds
// ARGV: serverID, matchID, allocID, command JSON, ttl_ms
var claimScript = redis.NewScript(`
if redis.call('HGET', KEYS[2], 'state') ~= 'ready' or redis.call('EXISTS', KEYS[3]) == 0 then
  redis.call('SREM', KEYS[1], ARGV[1])
  return 'stale'
end
redis.call('HSET', KEYS[2], 'state', 'allocating', 'match_id', ARGV[2], 'alloc_id', ARGV[3])
redis.call('SREM', KEYS[1], ARGV[1])
redis.call('RPUSH', KEYS[4], ARGV[4])
redis.call('PEXPIRE', KEYS[4], ARGV[5])
return 'ok'
`)

// Allocation is a claimed server.
type Allocation struct {
	AllocID     string // identifies this attempt; an acknowledgement must name it
	ServerID    string
	Address     string
	ResultToken string // handed to the server; the caller stores only its hash
}

// Claim allocates a ready server to a match and queues the allocate command for its agent.
//
// The candidate list is read outside the script, and each candidate is then claimed by a script
// that re-checks it. That keeps every key a script touches declared in KEYS (the server's agent
// is only known after reading the server). A candidate that went stale in between (claimed,
// shut down, its agent gone) is dropped from the ready set and the next one is tried.
func (s *Service) Claim(ctx context.Context, matchID string, players []string, resultURL string) (Allocation, error) {
	candidates, err := s.rdb.SMembers(ctx, readyKey).Result()
	if err != nil {
		return Allocation{}, fmt.Errorf("list ready servers: %w", err)
	}
	for _, sid := range candidates {
		h, err := s.rdb.HMGet(ctx, serverKey(sid), "agent", "address").Result()
		if err != nil {
			return Allocation{}, fmt.Errorf("read server: %w", err)
		}
		agent, _ := h[0].(string)
		addr, _ := h[1].(string)
		if agent == "" {
			s.rdb.SRem(ctx, readyKey, sid) // expired with its agent
			continue
		}

		var b [32]byte
		rand.Read(b[:])
		a := Allocation{AllocID: id.New(), ServerID: sid, Address: addr,
			ResultToken: base64.RawURLEncoding.EncodeToString(b[:])}
		cmd, _ := json.Marshal(Command{Type: "allocate", ServerID: sid, MatchID: matchID, Players: players,
			ResultURL: resultURL, ResultToken: a.ResultToken})

		res, err := claimScript.Run(ctx, s.rdb,
			[]string{readyKey, serverKey(sid), agentKey(agent), cmdsKey(agent)},
			sid, matchID, a.AllocID, string(cmd), (time.Minute).Milliseconds()).Text()
		if err != nil {
			return Allocation{}, fmt.Errorf("claim server: %w", err)
		}
		if res == "ok" {
			return a, nil
		}
	}
	return Allocation{}, ErrNoServer
}

// ackScript records a server's acknowledgement, if the allocation is still the current one.
//
// KEYS: server
// ARGV: agentID, matchID
var ackScript = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'agent', 'state', 'match_id', 'alloc_id', 'address')
if h[1] ~= ARGV[1] or h[2] ~= 'allocating' or h[3] ~= ARGV[2] then return {'withdrawn'} end
redis.call('HSET', KEYS[1], 'state', 'allocated')
return {'ok', h[4], h[5]}
`)

// Acknowledge handles an agent's "server allocated" report and tells the ready handler, which
// moves the match's players to ready. ErrWithdrawn means the allocation timed out and the match
// went elsewhere: the agent must shut the server down.
func (s *Service) Acknowledge(ctx context.Context, agentID, serverID, matchID string) error {
	res, err := ackScript.Run(ctx, s.rdb, []string{serverKey(serverID)}, agentID, matchID).StringSlice()
	if err != nil {
		return fmt.Errorf("acknowledge: %w", err)
	}
	if res[0] != "ok" {
		return ErrWithdrawn
	}
	if s.onReady == nil {
		return errors.New("fleet: no ready handler")
	}
	if err := s.onReady(ctx, matchID, res[1], res[2]); err != nil {
		// The match side refused (it withdrew the allocation in the same instant). Mark the
		// server so it is not left looking allocated, and let the agent shut it down.
		s.rdb.HSet(ctx, serverKey(serverID), "state", StateWithdrawn)
		return err
	}
	return nil
}

// withdrawScript gives up on an allocation that was never acknowledged.
var withdrawScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'alloc_id') == ARGV[1] and redis.call('HGET', KEYS[1], 'state') == 'allocating' then
  redis.call('HSET', KEYS[1], 'state', 'withdrawn')
  return 1
end
return 0
`)

// Withdraw marks an unacknowledged allocation dead, so a late acknowledgement is refused.
func (s *Service) Withdraw(ctx context.Context, serverID, allocID string) error {
	return withdrawScript.Run(ctx, s.rdb, []string{serverKey(serverID)}, allocID).Err()
}

// ServerAlive reports whether a server is still being heartbeated: false once its agent stops
// reporting it, which means the process (or its agent) is gone.
func (s *Service) ServerAlive(ctx context.Context, serverID string) (bool, error) {
	n, err := s.rdb.Exists(ctx, serverKey(serverID)).Result()
	return n == 1, err
}
