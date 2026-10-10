// Command stubserver is a stand-in game server that implements docs/api/server-lifecycle.md and
// docs/api/connect-token.md, and nothing else. It is the reference GanymedDedicated is written
// against: everything a real game server must do with the fleet, the backend and joining
// players, with the game itself replaced by a timer.
//
// It is an HTTP client of its agent only, plus a UDP socket for players. It never calls the
// backend: the agent reports the result on its behalf. The fleet agent spawns it with the flags
// below; run by hand it is useful only for reading.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
)

type allocation struct {
	MatchID string   `json:"match_id"`
	Players []string `json:"players"`
}

type stub struct {
	id, agent, advertise string
	pub                  ed25519.PublicKey
	matchFor             time.Duration
	outcome              string
	http                 *http.Client
	log                  *slog.Logger
}

func main() {
	serverID := flag.String("server-id", "", "this process's server ID (from the agent)")
	agent := flag.String("agent", "", "the agent's base URL")
	gamePort := flag.Int("game-port", 0, "UDP port for players")
	advertise := flag.String("advertise", "", "host:port players connect to")
	pubKey := flag.String("public-key", "", "the backend's connect-token public key, base64url")
	matchFor := flag.Duration("match-seconds", 10*time.Second, "how long the fake match lasts")
	outcome := flag.String("outcome", "victory", "the result to report: victory or defeat")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("server_id", short(*serverID))
	pub, err := connecttoken.ParsePublicKey(*pubKey)
	if *serverID == "" || *agent == "" || *gamePort == 0 || *advertise == "" || err != nil {
		log.Error("missing or invalid flags: --server-id, --agent, --game-port, --advertise and --public-key are required", "err", err)
		os.Exit(2)
	}

	s := &stub{id: *serverID, agent: strings.TrimRight(*agent, "/"), advertise: *advertise, pub: pub,
		matchFor: *matchFor, outcome: *outcome, http: &http.Client{Timeout: 30 * time.Second}, log: log}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := s.run(ctx, *gamePort); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func (s *stub) run(ctx context.Context, gamePort int) error {
	// Bind the game port first: a server that cannot accept players must never report ready.
	// It is read from now on, so a HELLO before the allocation gets an answer, not silence.
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", gamePort))
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()
	players := &door{pub: s.pub, advertise: s.advertise, log: s.log}
	go players.serve(conn)

	// Health, every 2 s, from start to exit, on its own goroutine: a server busy running a match
	// still reports health. A non-2xx answer means the agent no longer knows us, so we stop.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			if err := s.post(ctx, "/health", nil, nil); err != nil && ctx.Err() == nil {
				s.log.Warn("health refused: stopping", "err", err)
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	alloc, err := s.waitForMatch(ctx)
	if err != nil {
		return err
	}
	s.log.Info("allocated", "match_id", short(alloc.MatchID), "players", len(alloc.Players))

	// Acknowledge at once, before anything slow: the agent withdraws an allocation not
	// acknowledged within 5 s.
	if err := s.post(ctx, "/allocated", map[string]string{"match_id": alloc.MatchID}, nil); err != nil {
		s.post(context.Background(), "/shutdown", nil, nil)
		return fmt.Errorf("acknowledgement refused (allocation withdrawn?): %w", err)
	}

	// The fake match: admit players for its length.
	players.open(alloc)
	select {
	case <-ctx.Done():
	case <-time.After(s.matchFor):
	}
	admitted := players.close()
	s.log.Info("match over", "admitted", admitted, "of", len(alloc.Players))

	if err := s.reportResult(ctx); err != nil {
		s.log.Error("result not reported", "err", err)
	}
	s.post(context.Background(), "/shutdown", nil, nil)
	return nil
}

// waitForMatch long-polls /ready until the agent answers with an allocation.
func (s *stub) waitForMatch(ctx context.Context) (allocation, error) {
	for {
		var a allocation
		status, err := s.postStatus(ctx, "/ready", nil, &a)
		switch {
		case ctx.Err() != nil:
			return allocation{}, ctx.Err()
		case err != nil:
			s.log.Warn("ready call failed; retrying", "err", err)
			time.Sleep(time.Second)
		case status == http.StatusNoContent:
			// No match within the long-poll: ask again at once.
		case status == http.StatusOK:
			return a, nil
		default:
			return allocation{}, fmt.Errorf("agent refused ready: %d", status)
		}
	}
}

// door answers HELLOs on the game port, from the moment it is bound until the process exits.
// What it answers depends on where the server is in its life: before an allocation is
// acknowledged, every HELLO is "DENIED not allocated" (a replacement server on a dead match's
// port says so at once, instead of letting the client time out); during the match it applies
// connect-token.md's rules 1–8; after it, "DENIED match over".
type door struct {
	pub       ed25519.PublicKey
	advertise string
	log       *slog.Logger

	mu       sync.Mutex
	alloc    *allocation          // nil until the allocation is acknowledged
	over     bool                 // the match has ended
	seen     map[string]time.Time // nonce → expiry, the replay memory (rule 8)
	admitted map[string]bool
}

// open starts admitting players to the acknowledged allocation.
func (d *door) open(a allocation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.alloc, d.seen, d.admitted = &a, map[string]time.Time{}, map[string]bool{}
}

// close ends the match, and returns how many players were admitted to it.
func (d *door) close() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.over = true
	return len(d.admitted)
}

// serve reads the socket until it is closed.
func (d *door) serve(conn net.PacketConn) {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// On Windows, an ICMP port unreachable from one client's dead port surfaces as an
			// error on this read (WSAECONNRESET). It says nothing about the socket: keep reading.
			continue
		}
		reply := d.answer(strings.TrimSpace(string(buf[:n])))
		d.log.Info("join attempt", "from", from.String(), "reply", reply)
		conn.WriteTo([]byte(reply), from)
	}
}

// answer applies connect-token.md's rules: Verify does 1–6, this function does 7 (an expected
// player) and 8 (no replays).
func (d *door) answer(msg string) string {
	token, ok := strings.CutPrefix(msg, "HELLO ")
	if !ok {
		return "DENIED expected HELLO <token>"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.alloc == nil:
		return "DENIED not allocated"
	case d.over:
		return "DENIED match over"
	}

	now := time.Now()
	c, err := connecttoken.Verify(d.pub, token, connecttoken.Expect{ServerAddr: d.advertise, MatchID: d.alloc.MatchID}, now)
	if err != nil {
		return "DENIED " + err.Error()
	}
	if !slices.Contains(d.alloc.Players, c.AccountID) {
		return "DENIED not a player in this match"
	}
	for nonce, exp := range d.seen { // forget nonces whose tokens are expired anyway
		if now.After(exp) {
			delete(d.seen, nonce)
		}
	}
	if _, replay := d.seen[c.Nonce]; replay {
		return "DENIED token already used"
	}
	d.seen[c.Nonce] = time.Unix(c.ExpiresAt, 0).Add(connecttoken.Leeway)
	d.admitted[c.AccountID] = true
	return "WELCOME " + c.AccountID
}

// reportResult posts the outcome to the agent, which forwards it to the backend. It retries
// network errors and 5xx (a 502 is the agent finding no backend replica up): the backend's result
// endpoint is idempotent, so a retry after an unseen success is harmless.
func (s *stub) reportResult(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= 5; attempt++ {
		status, err := s.postStatus(ctx, "/result", map[string]string{"outcome": s.outcome}, nil)
		if err == nil {
			if status == http.StatusOK {
				s.log.Info("result reported", "outcome", s.outcome, "attempt", attempt)
				return nil
			}
			if status < 500 {
				return fmt.Errorf("result refused: %d", status) // 4xx: retrying will not help
			}
			err = fmt.Errorf("agent answered %d", status)
		}
		s.log.Warn("result report failed; retrying", "attempt", attempt, "err", err)
		time.Sleep(backoff)
		backoff *= 2
	}
	return errors.New("gave up reporting the result")
}

func (s *stub) post(ctx context.Context, path string, body, out any) error {
	status, err := s.postStatus(ctx, path, body, out)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("agent answered %d", status)
	}
	return nil
}

func (s *stub) postStatus(ctx context.Context, path string, body, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.agent+"/v1/servers/"+s.id+path, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return 0, err
		}
	}
	return resp.StatusCode, nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
