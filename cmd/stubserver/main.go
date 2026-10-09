// Command stubserver is a stand-in game server that implements docs/api/server-lifecycle.md and
// docs/api/connect-token.md, and nothing else. It is the reference GanymedDedicated is written
// against: everything a real game server must do with the fleet, the backend and joining
// players, with the game itself replaced by a timer.
//
// It is an HTTP client only (of its agent, and of the backend for the result), plus a UDP socket for
// players. The fleet agent spawns it with the flags below; run by hand it is useful only for
// reading.
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
	"strings"
	"sync"
	"time"

	"github.com/giska1923/GanymedServer/internal/connecttoken"
)

type allocation struct {
	MatchID     string   `json:"match_id"`
	Players     []string `json:"players"`
	ResultURL   string   `json:"result_url"`
	ResultToken string   `json:"result_token"`
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
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", gamePort))
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()

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

	admitted := s.serveMatch(ctx, conn, alloc)
	s.log.Info("match over", "admitted", admitted, "of", len(alloc.Players))

	if err := s.reportResult(ctx, alloc); err != nil {
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

// serveMatch admits players for the length of the fake match, applying connect-token.md's rules
// 1–8: Verify does 1–6, this function does 7 (an expected player) and 8 (no replays).
func (s *stub) serveMatch(ctx context.Context, conn net.PacketConn, a allocation) int {
	expected := map[string]bool{}
	for _, p := range a.Players {
		expected[p] = true
	}
	var mu sync.Mutex
	seen := map[string]time.Time{} // nonce → expiry, the replay memory
	admitted := map[string]bool{}

	deadline := time.Now().Add(s.matchFor)
	conn.SetReadDeadline(deadline)
	buf := make([]byte, 2048)
	for ctx.Err() == nil {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			break // the deadline: the match is over
		}
		msg := strings.TrimSpace(string(buf[:n]))
		token, ok := strings.CutPrefix(msg, "HELLO ")
		if !ok {
			conn.WriteTo([]byte("DENIED expected HELLO <token>"), from)
			continue
		}

		reply := func() string {
			now := time.Now()
			c, err := connecttoken.Verify(s.pub, token, connecttoken.Expect{ServerAddr: s.advertise, MatchID: a.MatchID}, now)
			if err != nil {
				return "DENIED " + err.Error()
			}
			if !expected[c.AccountID] {
				return "DENIED not a player in this match"
			}
			mu.Lock()
			defer mu.Unlock()
			for nonce, exp := range seen { // forget nonces whose tokens are expired anyway
				if now.After(exp) {
					delete(seen, nonce)
				}
			}
			if _, replay := seen[c.Nonce]; replay {
				return "DENIED token already used"
			}
			seen[c.Nonce] = time.Unix(c.ExpiresAt, 0).Add(connecttoken.Leeway)
			admitted[c.AccountID] = true
			return "WELCOME " + c.AccountID
		}()
		s.log.Info("join attempt", "from", from.String(), "reply", reply)
		conn.WriteTo([]byte(reply), from)
	}
	mu.Lock()
	defer mu.Unlock()
	return len(admitted)
}

// reportResult posts the outcome, retrying network errors and 5xx: the endpoint is idempotent,
// so a retry after an unseen success is harmless.
func (s *stub) reportResult(ctx context.Context, a allocation) error {
	body, _ := json.Marshal(map[string]string{"outcome": s.outcome})
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= 5; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", a.ResultURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+a.ResultToken)
		resp, err := s.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				s.log.Info("result reported", "outcome", s.outcome, "attempt", attempt)
				return nil
			}
			if resp.StatusCode < 500 {
				return fmt.Errorf("result refused: %s", resp.Status) // 4xx: retrying will not help
			}
			err = errors.New(resp.Status)
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
