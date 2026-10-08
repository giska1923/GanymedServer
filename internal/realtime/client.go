package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/giska1923/GanymedServer/internal/id"
)

// client is one socket. Its lifetime is one call to run, in the HTTP handler's goroutine, plus
// readLoop's goroutine. Both end when the socket closes.
//
// Two contexts, and keeping them apart is the important part:
//
//   - ctx is control: cancelled to ask run to close the socket (close, or gateway shutdown).
//   - peer is the socket itself: cancelled by readLoop once the connection is really gone.
//
// They cannot be one context because coder/websocket closes the connection outright, with no
// close frame, when the context of an in-progress read or write is cancelled. If the reader read
// on the control context, every close request would kill the TCP connection before run could send
// the close code (4001, 1008, 1001) the contract promises. That was the first version, and the
// tests saw EOF where they expected codes.
type client struct {
	g         *Gateway
	accountID string
	connID    string
	gen       int64 // this account's session generation; set by attach, read by dispatch
	conn      *websocket.Conn
	send      chan []byte

	ctx        context.Context
	cancel     context.CancelFunc
	peer       context.Context
	peerCancel context.CancelFunc

	closeOnce   sync.Once
	closeCode   websocket.StatusCode
	closeReason string
}

func newClient(g *Gateway, accountID string, conn *websocket.Conn) *client {
	ctx, cancel := context.WithCancel(g.ctx)
	peer, peerCancel := context.WithCancel(context.Background())
	// Clients send nothing (the contract), so anything big is abuse. Past the limit the library
	// closes the socket with 1009 (message too big).
	conn.SetReadLimit(1024)
	return &client{
		g: g, accountID: accountID, connID: id.New(), conn: conn,
		send: make(chan []byte, SendQueue), ctx: ctx, cancel: cancel,
		peer: peer, peerCancel: peerCancel,
	}
}

// readLoop is the socket's only reader, for its whole life. Reading is what processes control
// frames: it answers the client's pings, delivers the pongs that Ping waits for, and receives the
// peer's close frame, including the reply to our own close, which the closing handshake needs.
//
// A data message breaks the contract (clients send nothing), so it is drained and run is asked to
// close with 1008, and the loop keeps reading so that close's handshake can complete.
// websocket.CloseRead does nearly this, but on a data message it closes from inside its reader
// goroutine, and then no goroutine is left to read the peer's reply. The socket hung for the
// handshake timeout, and run only noticed at its next ping, 15 s later. Measured, then replaced.
//
// The context is Background on purpose (see the client type comment); the loop ends when the
// connection does, which every path out of run guarantees.
func (c *client) readLoop() {
	defer c.peerCancel()
	for {
		_, r, err := c.conn.Reader(context.Background())
		if err != nil {
			return // closed by either side, or dropped
		}
		_, _ = io.Copy(io.Discard, r)
		c.close(websocket.StatusPolicyViolation, "unexpected data message")
	}
}

// close asks run to close the socket with code. The first reason wins.
func (c *client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closeCode, c.closeReason = code, reason
		c.cancel()
	})
}

// enqueue never blocks: it is called from the gateway's single receive loop, and one slow
// socket must never stall delivery to every other socket on this replica.
func (c *client) enqueue(msg []byte) {
	select {
	case c.send <- msg:
	default:
		c.g.log.Warn("send queue full: disconnecting slow client", "account_id", c.accountID)
		c.close(websocket.StatusPolicyViolation, "send queue full")
	}
}

func (c *client) run() {
	g := c.g
	log := g.log.With("account_id", c.accountID, "conn_id", c.connID)
	defer c.cancel() // release the control context however run ends
	go c.readLoop()

	// detach is deferred BEFORE attach, and every step of it is safe on a half-attached client.
	// The first version deferred it after a successful attach, so an attach that failed part-way
	// left a map entry pointing at a dead socket. The leak test caught it, intermittently.
	defer func() {
		g.detach(c)
		log.Info("socket closed", "code", int(c.closeCode), "reason", c.closeReason)
	}()
	if err := g.attach(c); err != nil {
		log.Error("attach socket", "err", err)
		c.close(websocket.StatusInternalError, "")
		c.finish()
		return
	}
	log.Info("socket open", "gen", c.gen)

	ping := time.NewTicker(PingInterval)
	defer ping.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.finish()
			return

		case <-c.peer.Done():
			// The peer closed, or the connection dropped. Nothing left to send; just make sure
			// the socket is released.
			c.closeOnce.Do(func() {})
			c.conn.CloseNow()
			return

		case msg := <-c.send:
			// Its own timeout, not derived from c.ctx: a close request must not abort a write in
			// progress (which would drop the connection without a close frame). The loop gets to
			// the close as soon as this write returns.
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				c.close(websocket.StatusPolicyViolation, "write timed out")
			}

		case <-ping.C:
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "ping timed out")
				continue
			}
			current, err := g.refreshPresence(c.ctx, c)
			switch {
			case err != nil:
				log.Warn("refresh presence", "err", err) // Redis hiccup: try again next ping
			case !current:
				// Another socket owns this account's presence. The session.replaced push that
				// should have closed us was lost (pub/sub is at most once); this is the backstop.
				c.close(CloseSuperseded, "replaced by a newer connection")
			}
		}
	}
}

// finish sends the close frame: the recorded reason, or "going away" when the gateway is shutting
// down.
//
// The reason is claimed through the same sync.Once as close, never written directly: the receive
// loop may call close on this client at any moment, and two goroutines writing closeCode
// unsynchronized is a data race. Whoever runs the Once first decides; later closes are no-ops.
func (c *client) finish() {
	c.closeOnce.Do(func() {
		c.closeCode, c.closeReason = websocket.StatusGoingAway, "server shutting down"
	})
	// Close sends the frame and waits briefly for the peer's reply, which readLoop reads, as the
	// closing handshake requires.
	c.conn.Close(c.closeCode, c.closeReason)
}

// attach makes c this account's socket: locally, in presence, and on every other replica.
//
// Its Redis calls use their own timeout, not c.ctx: a close requested while attach is running
// (a newer socket superseding this one) must not abort it half-way. run deals with the close
// once attach returns.
func (g *Gateway) attach(c *client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The session generation: a counter per account, incremented by every new socket on any
	// replica. It orders sockets, which connection IDs cannot. A session.replaced push can still
	// be in flight when a newer socket attaches. Compared by ID, that stale push would close the
	// NEWER socket ("replaced by someone who isn't me"). Compared by generation, it is simply
	// older and ignored. The counter has no TTL: one small key per account that ever connected.
	gen, err := g.rdb.Incr(ctx, sessionKey(c.accountID)).Result()
	if err != nil {
		return fmt.Errorf("session generation: %w", err)
	}

	// Locally next, so a push arriving the moment we subscribe has somewhere to go. A previous
	// socket for the account on this replica is closed directly.
	g.mu.Lock()
	c.gen = gen
	prev := g.clients[c.accountID]
	g.clients[c.accountID] = c
	g.mu.Unlock()
	if prev != nil {
		prev.close(CloseSuperseded, "replaced by a newer connection")
	}

	if err := g.pubsub.Subscribe(ctx, userChannelPrefix+c.accountID); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	if err := g.setPresence(ctx, c); err != nil {
		return err
	}
	// Tell the other replicas. Subscribed first, so this replica receives it too and ignores it
	// (same generation).
	payload, _ := json.Marshal(sessionReplaced{Gen: gen})
	return g.publish(ctx, c.accountID, Message{Type: typeSessionReplaced, ID: c.connID, Payload: payload})
}

type sessionReplaced struct {
	Gen int64 `json:"gen"`
}

func sessionKey(accountID string) string { return "session:" + accountID }

// detach undoes attach, unless a newer socket for the same account has already taken over.
func (g *Gateway) detach(c *client) {
	g.mu.Lock()
	mine := g.clients[c.accountID] == c
	if mine {
		delete(g.clients, c.accountID)
	}
	g.mu.Unlock()

	// A fresh context: c.ctx may be cancelled by now, and this cleanup must still reach Redis.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if mine {
		if err := g.pubsub.Unsubscribe(ctx, userChannelPrefix+c.accountID); err != nil {
			g.log.Warn("unsubscribe", "err", err)
		}
	}
	// Away, not offline: the reconnection grace. Compare-and-set, so a newer socket's presence,
	// on any replica, is never overwritten.
	if err := g.leavePresence(ctx, c); err != nil && !errors.Is(err, context.Canceled) {
		g.log.Warn("mark away", "account_id", c.accountID, "err", err)
	}
}
