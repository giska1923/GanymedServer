package realtime

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// presence:<account> holds one of:
//
//	"conn:<connID>"  online: this socket owns the account's presence; TTL refreshed every ping
//	"away"           the socket closed cleanly within the last PresenceTTL: the reconnection grace
//	(missing)        offline
//
// A crashed replica cannot mark its players away. Their "conn:" values simply stop being
// refreshed and expire, which is why presence is a TTL and not a flag someone has to clear.
const (
	presenceConnPrefix = "conn:"
	presenceAway       = "away"
)

func presenceKey(accountID string) string { return "presence:" + accountID }

// Compare-and-set scripts. A Lua script runs atomically inside Redis: no other command runs
// between its GET and its write. That is what makes "only if it is still mine" safe. In plain
// commands, another replica could write between the check and the set.

// refreshScript keeps our presence alive. It returns 1 if the key is ours (or vanished, e.g. after
// a Redis restart, and is reclaimed), 0 if another socket owns it now.
var refreshScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == false or v == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
return 0
`)

// awayScript marks us away, but only if the presence is still ours.
var awayScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
  return 1
end
return 0
`)

func (g *Gateway) setPresence(ctx context.Context, c *client) error {
	// Unconditional: a new socket always takes over, whatever the old value.
	if err := g.rdb.Set(ctx, presenceKey(c.accountID), presenceConnPrefix+c.connID, PresenceTTL).Err(); err != nil {
		return fmt.Errorf("set presence: %w", err)
	}
	return nil
}

func (g *Gateway) refreshPresence(ctx context.Context, c *client) (current bool, err error) {
	n, err := refreshScript.Run(ctx, g.rdb, []string{presenceKey(c.accountID)},
		presenceConnPrefix+c.connID, PresenceTTL.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("refresh presence: %w", err)
	}
	return n == 1, nil
}

func (g *Gateway) leavePresence(ctx context.Context, c *client) error {
	err := awayScript.Run(ctx, g.rdb, []string{presenceKey(c.accountID)},
		presenceConnPrefix+c.connID, presenceAway, PresenceTTL.Milliseconds()).Err()
	if err != nil {
		return fmt.Errorf("mark away: %w", err)
	}
	return nil
}
