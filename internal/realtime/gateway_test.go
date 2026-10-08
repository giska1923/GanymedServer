package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/db/dbtest"
	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/redisdb/redistest"
	"github.com/giska1923/GanymedServer/internal/server"
)

var discard = slog.New(slog.DiscardHandler)

// env is one Redis and one auth service shared by any number of replicas, which is the
// production shape: replicas share the stores and nothing else.
type env struct {
	t     *testing.T
	rdb   *redis.Client
	authn *auth.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{
		t:   t,
		rdb: redistest.New(t),
		authn: auth.NewService(dbtest.New(t), auth.Config{
			JWTSecret: []byte("0123456789abcdef0123456789abcdef"), AccessTokenTTL: time.Hour, RefreshTokenTTL: 2 * time.Hour,
		}, discard),
	}
}

type replica struct {
	gw     *Gateway
	url    string // ws://...
	cancel context.CancelFunc
	done   chan struct{}
}

// replica starts a gateway and an HTTP server for it. configure, if non-nil, adjusts the server
// before it starts.
func (e *env) replica(name string, configure func(*http.Server)) *replica {
	e.t.Helper()
	gw, err := NewGateway(context.Background(), e.rdb, name, discard)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { gw.Run(ctx); close(done) }()

	mux := http.NewServeMux()
	gw.Register(mux, e.authn.RequireAuth)
	srv := httptest.NewUnstartedServer(server.Middleware(mux, discard))
	if configure != nil {
		configure(srv.Config)
	}
	srv.Start()

	r := &replica{gw: gw, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime", cancel: cancel, done: done}
	e.t.Cleanup(func() {
		r.stop()
		srv.Close()
	})
	return r
}

func (r *replica) stop() {
	r.cancel()
	<-r.done
}

// player signs in a fresh device and returns its account ID and access token.
func (e *env) player() (string, string) {
	e.t.Helper()
	sess, err := e.authn.LoginDevice(context.Background(), id.New())
	if err != nil {
		e.t.Fatal(err)
	}
	return sess.AccountID, sess.AccessToken
}

func dial(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func read(t *testing.T, c *websocket.Conn) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// eventually polls cond until it holds or 5 s pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (e *env) status(gw *Gateway, account string) Status {
	st, err := gw.Status(context.Background(), []string{account})
	if err != nil {
		e.t.Fatal(err)
	}
	return st[account]
}

// waitAttached waits until the gateway has subscribed and set presence for the account, after
// which a Notify is guaranteed a subscriber. (Dial returns at the upgrade, slightly before.)
func (e *env) waitAttached(gw *Gateway, account string) {
	e.t.Helper()
	eventually(e.t, "socket attached", func() bool { return e.status(gw, account) == StatusOnline })
}

func TestPushReachesSocket(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	acct, token := e.player()
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	if err := a.gw.Notify(context.Background(), acct, "party.updated", map[string]string{"party_id": "p1"}); err != nil {
		t.Fatal(err)
	}
	m := read(t, c)
	if m.Type != "party.updated" || m.ID == "" || string(m.Payload) != `{"party_id":"p1"}` {
		t.Fatalf("got %+v", m)
	}
}

// The reason the gateway exists: a push sent by one replica reaches a socket held by another.
func TestCrossReplicaPush(t *testing.T) {
	e := newEnv(t)
	a, b := e.replica("a", nil), e.replica("b", nil)
	acct, token := e.player()
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	if err := b.gw.Notify(context.Background(), acct, "party.invite", nil); err != nil {
		t.Fatal(err)
	}
	if m := read(t, c); m.Type != "party.invite" {
		t.Fatalf("got %+v", m)
	}
}

func TestPresenceOnlineAwayOffline(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	acct, token := e.player()

	if s := e.status(a.gw, acct); s != StatusOffline {
		t.Fatalf("before connecting: %s", s)
	}
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	c.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "away after a clean close", func() bool { return e.status(a.gw, acct) == StatusAway })

	ttl := e.rdb.PTTL(context.Background(), presenceKey(acct)).Val()
	if ttl <= 0 || ttl > PresenceTTL {
		t.Fatalf("away TTL %v, want within (0, %v]", ttl, PresenceTTL)
	}
	// Fast-forward the grace rather than waiting 30 s.
	e.rdb.PExpire(context.Background(), presenceKey(acct), time.Millisecond)
	eventually(t, "offline after the grace", func() bool { return e.status(a.gw, acct) == StatusOffline })
}

// A replica that dies cannot mark its players away. Their presence must still end on its own.
func TestPresenceOfCrashedReplicaExpires(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	acct, token := e.player()
	dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	// A crash is "the process stops and nothing more runs". The closest a test gets: the key keeps
	// its conn: value, and only its TTL ends it. Assert the TTL is set, i.e. it WILL end.
	v := e.rdb.Get(context.Background(), presenceKey(acct)).Val()
	ttl := e.rdb.PTTL(context.Background(), presenceKey(acct)).Val()
	if !strings.HasPrefix(v, presenceConnPrefix) || ttl <= 0 || ttl > PresenceTTL {
		t.Fatalf("presence %q with TTL %v: a crashed replica's player would stay online forever", v, ttl)
	}
}

func TestNewerSocketSupersedesOlder(t *testing.T) {
	e := newEnv(t)
	a, b := e.replica("a", nil), e.replica("b", nil)
	acct, token := e.player()

	first := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)
	second := dial(t, b.url, token) // same account, other replica

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := first.Read(ctx)
	if code := websocket.CloseStatus(err); code != CloseSuperseded {
		t.Fatalf("first socket: got %v (code %d), want close %d", err, code, CloseSuperseded)
	}

	// The survivor still works, and presence belongs to it.
	eventually(t, "a detached", func() bool { a.gw.mu.Lock(); defer a.gw.mu.Unlock(); return a.gw.clients[acct] == nil })
	if err := a.gw.Notify(context.Background(), acct, "party.updated", nil); err != nil {
		t.Fatal(err)
	}
	if m := read(t, second); m.Type != "party.updated" {
		t.Fatalf("second socket got %+v", m)
	}
	if s := e.status(a.gw, acct); s != StatusOnline {
		t.Fatalf("presence after supersede: %s (the old socket's cleanup must not overwrite it)", s)
	}
}

// A client that stops reading must be cut off, not buffered for: one stalled socket would
// otherwise grow memory without bound. And the replica's other sockets must not notice.
func TestSlowClientIsDisconnected(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	slowAcct, slowToken := e.player()
	okAcct, okToken := e.player()
	slow := dial(t, a.url, slowToken) // never read until the end
	fine := dial(t, a.url, okToken)
	e.waitAttached(a.gw, slowAcct)
	e.waitAttached(a.gw, okAcct)

	// Enough to fill the TCP buffers, block the writer, and overflow the send queue.
	payload := map[string]string{"pad": strings.Repeat("x", 4096)}
	ctx := context.Background()
	for i := 0; i < 5000; i++ {
		if err := a.gw.Notify(ctx, slowAcct, "flood", payload); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.gw.Notify(ctx, okAcct, "still.here", nil); err != nil {
		t.Fatal(err)
	}
	if m := read(t, fine); m.Type != "still.here" {
		t.Fatalf("the healthy socket got %+v", m)
	}

	// The server must let go of the slow socket: within the write timeout, plus margin.
	eventually(t, "slow socket detached by the server", func() bool {
		a.gw.mu.Lock()
		defer a.gw.mu.Unlock()
		return a.gw.clients[slowAcct] == nil
	})

	// What the client sees depends on whether the 1008 close frame could still be written. A
	// client that has stopped reading has a full TCP window, so often it cannot, and the write
	// timeout then drops the connection (1006, no close frame). Both are correct; the contract
	// says so. What would be wrong is the stream never ending.
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if _, _, err := slow.Read(rctx); err != nil {
			code := websocket.CloseStatus(err)
			if code != websocket.StatusPolicyViolation && code != -1 {
				t.Fatalf("slow socket ended with %v (code %d), want 1008 or a dropped connection", err, code)
			}
			if rctx.Err() != nil {
				t.Fatal("slow socket never ended")
			}
			t.Logf("slow socket ended with code %d", code)
			return
		}
	}
}

// The server's request timeouts (ReadTimeout and WriteTimeout, 10 s and 15 s in production) are
// deadlines on the connection, and hijacking does not clear them. Here they are 200 ms: a socket
// that survives a second proves the gateway clears them, through the logging middleware's Unwrap.
func TestSocketOutlivesServerTimeouts(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", func(s *http.Server) {
		s.ReadTimeout = 200 * time.Millisecond
		s.WriteTimeout = 200 * time.Millisecond
	})
	acct, token := e.player()
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	time.Sleep(time.Second)
	if err := a.gw.Notify(context.Background(), acct, "late", nil); err != nil {
		t.Fatal(err)
	}
	if m := read(t, c); m.Type != "late" {
		t.Fatalf("got %+v", m)
	}
}

func TestShutdownClosesSocketsGoingAway(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	acct, token := e.player()
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	go a.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("got %v (code %d), want 1001", err, code)
	}
	<-a.done
	if s := e.status(a.gw, acct); s != StatusAway {
		t.Fatalf("presence after shutdown: %s, want away (the grace applies to shutdowns too)", s)
	}
}

func TestClientDataMessageCloses1008(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	acct, token := e.player()
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"hello":1}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
		t.Fatalf("got %v (code %d), want 1008", err, code)
	}
}

func TestUpgradeRequiresToken(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, a.url, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial without a token: resp %v, err %v; want a 401 and no socket", resp, err)
	}
}

// The design's leak check: every goroutine a socket starts must end when it does. 1000 sockets
// opened and closed, and the goroutine count must come back to where it started.
func TestNoGoroutineLeak(t *testing.T) {
	e := newEnv(t)
	a := e.replica("a", nil)
	acct, token := e.player()

	// Warm up once so lazily started goroutines (pools, the pubsub reader) are in the baseline.
	c := dial(t, a.url, token)
	e.waitAttached(a.gw, acct)
	c.Close(websocket.StatusNormalClosure, "")
	settle := func() int {
		var n int
		prev := -1
		for i := 0; i < 50; i++ { // stable for two consecutive samples
			time.Sleep(20 * time.Millisecond)
			n = runtime.NumGoroutine()
			if n == prev {
				break
			}
			prev = n
		}
		return n
	}
	baseline := settle()

	for i := 0; i < 1000; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, _, err := websocket.Dial(ctx, a.url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
		})
		cancel()
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if err := c.Close(websocket.StatusNormalClosure, ""); err != nil && !errors.Is(err, context.Canceled) {
			// A close racing the server's own close (supersede) is fine; anything else is not.
			if websocket.CloseStatus(err) == -1 {
				t.Fatalf("cycle %d close: %v", i, err)
			}
		}
	}

	eventually(t, "every socket detached", func() bool {
		a.gw.mu.Lock()
		defer a.gw.mu.Unlock()
		return len(a.gw.clients) == 0
	})
	after := settle()
	t.Logf("goroutines: baseline %d, after 1000 cycles %d", baseline, after)
	if after > baseline+5 {
		t.Fatalf("goroutines grew from %d to %d over 1000 socket cycles: a leak", baseline, after)
	}
}
