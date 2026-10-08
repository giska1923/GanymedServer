// Package realtime is the WebSocket gateway: one socket per signed-in player, presence, and
// delivery of server pushes to whichever replica holds a player's socket.
//
// It owns the Redis keys presence:<account> and session:<account>, and the pub/sub channels
// user:<account> and realtime:replica:<id>. Other modules never touch them; they call Notify and Status.
//
// How a push reaches a player, from any replica:
//
//	Notify(account, …) ── PUBLISH user:<account> ──► Redis ──► every replica SUBSCRIBEd to it
//	                                                            (only the one holding the socket is)
//	                                                            └─► receive loop ─► client queue ─► socket
package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/problem"
)

const (
	// PingInterval is how often the server pings each socket, and refreshes its presence.
	PingInterval = 10 * time.Second
	// PresenceTTL is how long presence outlives its last refresh. It is also the reconnection
	// grace: a closed socket leaves its player "away" for this long, a dead replica's players
	// "online" for at most this long. Three missed pings before a crashed replica's players
	// go offline.
	PresenceTTL = 30 * time.Second
	// SendQueue bounds the pushes waiting for one socket. A client that falls this far behind
	// is disconnected rather than allowed to grow server memory.
	SendQueue = 32

	writeTimeout = 5 * time.Second
	pingTimeout  = 5 * time.Second

	// CloseSuperseded closes a socket replaced by a newer one for the same account. 4000-4999 is
	// the application range of RFC 6455 close codes.
	CloseSuperseded websocket.StatusCode = 4001

	userChannelPrefix = "user:"
	// typeSessionReplaced is internal: a replica tells every other replica that it now holds
	// this account's socket. It is never forwarded to a client.
	typeSessionReplaced = "session.replaced"
)

// Message is the envelope of every push (docs/api/realtime.md).
type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Status is a player's presence.
type Status string

const (
	StatusOnline  Status = "online"
	StatusAway    Status = "away"
	StatusOffline Status = "offline"
)

type Gateway struct {
	rdb     *redis.Client
	replica string
	log     *slog.Logger

	// One subscription connection per replica, shared by all of its sockets. Channels are added
	// and removed as sockets come and go. go-redis makes Subscribe and Unsubscribe safe to call
	// from any goroutine; receiving is not, so exactly one goroutine (Run) receives.
	pubsub *redis.PubSub

	mu      sync.Mutex
	clients map[string]*client // account → this replica's socket for it

	// ctx is the gateway's lifetime. Every socket's context derives from it, so cancelling it
	// (Run returning) closes every socket. conns counts their handler goroutines.
	ctx    context.Context
	cancel context.CancelFunc
	conns  sync.WaitGroup
}

// NewGateway subscribes to this replica's own channel, so a broken Redis fails at startup, and so
// the subscription connection exists before the first socket needs it. Call Run to start
// delivering.
func NewGateway(ctx context.Context, rdb *redis.Client, replica string, log *slog.Logger) (*Gateway, error) {
	pubsub := rdb.Subscribe(ctx, "realtime:replica:"+replica)
	// Receive once, synchronously, to confirm the SUBSCRIBE: without it, a failure would only
	// show up later inside the receive loop.
	if _, err := pubsub.Receive(ctx); err != nil {
		pubsub.Close()
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	gctx, cancel := context.WithCancel(context.Background())
	return &Gateway{
		rdb: rdb, replica: replica, log: log,
		pubsub: pubsub, clients: map[string]*client{},
		ctx: gctx, cancel: cancel,
	}, nil
}

// Run delivers pushes until ctx is cancelled, then closes every socket with 1001 (going away)
// and waits for their goroutines to finish.
//
// http.Server.Shutdown does not do this part. Its documentation is explicit that it neither
// closes nor waits for hijacked connections, and a WebSocket is one. So shutdown is the
// gateway's job, and main waits for Run to return before closing Redis.
func (g *Gateway) Run(ctx context.Context) {
	msgs := g.pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			g.cancel()     // closes every socket (see client.run)
			g.conns.Wait() // ... and waits for them, presence included
			g.pubsub.Close()
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			g.dispatch(m)
		}
	}
}

// dispatch routes one pub/sub message to the local socket for its account, if there is one.
func (g *Gateway) dispatch(m *redis.Message) {
	account, ok := strings.CutPrefix(m.Channel, userChannelPrefix)
	if !ok {
		return
	}
	var env Message
	if err := json.Unmarshal([]byte(m.Payload), &env); err != nil {
		g.log.Warn("undecodable push dropped", "err", err)
		return
	}

	g.mu.Lock()
	c := g.clients[account]
	var gen int64
	if c != nil {
		gen = c.gen // written by attach under g.mu
	}
	g.mu.Unlock()
	if c == nil {
		return // subscribed a moment ago, or unsubscribing now: nobody to deliver to
	}

	if env.Type == typeSessionReplaced {
		var r sessionReplaced
		if err := json.Unmarshal(env.Payload, &r); err != nil {
			g.log.Warn("undecodable session.replaced dropped", "err", err)
			return
		}
		// Only a NEWER session closes this one. An older one's announcement arriving late is
		// ignored; see attach.
		if r.Gen > gen {
			c.close(CloseSuperseded, "replaced by a newer connection")
		}
		return
	}
	c.enqueue([]byte(m.Payload))
}

// Notify sends a push to a player, wherever their socket is. Delivery is at most once: if the
// player has no socket right now, the message is gone. That is the contract (pushes are nudges).
func (g *Gateway) Notify(ctx context.Context, accountID, msgType string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode push payload: %w", err)
		}
		raw = b
	}
	return g.publish(ctx, accountID, Message{Type: msgType, ID: id.New(), Payload: raw})
}

func (g *Gateway) publish(ctx context.Context, accountID string, msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode push: %w", err)
	}
	if err := g.rdb.Publish(ctx, userChannelPrefix+accountID, b).Err(); err != nil {
		return fmt.Errorf("publish push: %w", err)
	}
	return nil
}

// Status reports presence for many accounts in one round trip.
func (g *Gateway) Status(ctx context.Context, accountIDs []string) (map[string]Status, error) {
	out := make(map[string]Status, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(accountIDs))
	for i, a := range accountIDs {
		keys[i] = presenceKey(a)
	}
	vals, err := g.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read presence: %w", err)
	}
	for i, v := range vals {
		s, _ := v.(string) // nil for a missing key
		switch {
		case strings.HasPrefix(s, presenceConnPrefix):
			out[accountIDs[i]] = StatusOnline
		case s == presenceAway:
			out[accountIDs[i]] = StatusAway
		default:
			out[accountIDs[i]] = StatusOffline
		}
	}
	return out, nil
}

// Register adds the WebSocket route.
func (g *Gateway) Register(mux *http.ServeMux, requireAuth func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/realtime", requireAuth(http.HandlerFunc(g.handle)))
}

func (g *Gateway) handle(w http.ResponseWriter, r *http.Request) {
	accountID, _ := auth.AccountID(r.Context())
	if g.ctx.Err() != nil {
		problem.Write(w, http.StatusServiceUnavailable, problem.TypeInternal, "server shutting down")
		return
	}

	// The http.Server set read and write deadlines on this connection when the request arrived
	// (ReadTimeout, WriteTimeout in internal/server). Those are right for a request and fatal for
	// a socket that lives for hours: hijacking does not clear them, so without this every socket
	// would die 10 s after it opened. http.ResponseController reaches the real connection through
	// the logging middleware's Unwrap.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		g.log.Error("clear read deadline", "err", err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		g.log.Error("clear write deadline", "err", err)
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already written the HTTP error response
	}

	// Not r.Context(): after the hijack it no longer tracks this connection. The socket lives on
	// the gateway's context instead, so shutdown reaches it.
	c := newClient(g, accountID, conn)
	g.conns.Add(1)
	defer g.conns.Done()
	c.run()
}
