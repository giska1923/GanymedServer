package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/giska1923/GanymedServer/internal/id"
)

// allocation is what a game server receives from its ready long-poll.
type allocation struct {
	MatchID     string   `json:"match_id"`
	Players     []string `json:"players"`
	ResultURL   string   `json:"result_url"`
	ResultToken string   `json:"result_token"`
}

// server is one game server process. Every field is guarded by agent.mu.
type server struct {
	id      string
	port    int
	addr    string
	cmd     *exec.Cmd
	exited  bool
	state   string // starting, ready, allocated, shutdown
	health  time.Time
	readyAt time.Time       // last moment a ready long-poll was outstanding
	waiting chan allocation // non-nil while a ready long-poll is outstanding
	pending *allocation     // an allocation not yet delivered to a ready call
	match   string          // the match it was given
}

type agent struct {
	c    config
	log  *slog.Logger
	http *http.Client

	mu        sync.Mutex
	servers   map[string]*server
	publicKey string // from the backend; servers cannot start before the first heartbeat
	backend   int    // index into c.backends: the one that last worked
}

func newAgent(c config, log *slog.Logger) *agent {
	return &agent{c: c, log: log, http: &http.Client{Timeout: 30 * time.Second}, servers: map[string]*server{}}
}

func (a *agent) run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.c.listen)
	if err != nil {
		return fmt.Errorf("lifecycle listener: %w", err)
	}
	srv := &http.Server{Handler: a.lifecycleRoutes(), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	a.log.Info("agent started", "lifecycle", a.c.listen, "pool", a.c.pool, "server_cmd", a.c.serverCmd[0])

	var wg sync.WaitGroup
	wg.Go(func() { a.heartbeatLoop(ctx) })
	wg.Go(func() { a.commandLoop(ctx) })
	wg.Go(func() { a.superviseLoop(ctx) })
	<-ctx.Done()

	// Shutdown: stop the loops, then the servers. The backend sees their heartbeats stop and stops
	// allocating to them within 5 s. A real agent would drain (let running matches finish) first.
	srv.Close()
	wg.Wait()
	a.mu.Lock()
	for _, s := range a.servers {
		if !s.exited && s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}
	}
	a.mu.Unlock()
	a.log.Info("agent stopped; servers killed")
	return nil
}

// ---- The warm pool -------------------------------------------------------------------------

// superviseLoop keeps the pool full: it reaps exited servers, kills silent ones, and spawns
// replacements, twice a second.
func (a *agent) superviseLoop(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		live := 0
		for sid, s := range a.servers {
			switch {
			case s.exited:
				a.log.Info("server exited", "server_id", short(sid), "state", s.state)
				delete(a.servers, sid)
				continue
			case time.Since(s.health) > a.c.healthKill:
				a.log.Warn("server silent: killing", "server_id", short(sid), "since_health", time.Since(s.health).Round(time.Millisecond))
				s.cmd.Process.Kill() // its exit is reaped on a later tick
			}
			live++
		}
		key := a.publicKey
		missing := a.c.pool - live
		a.mu.Unlock()

		for i := 0; i < missing && key != ""; i++ {
			if err := a.spawn(key); err != nil {
				a.log.Error("spawn failed", "err", err)
				break
			}
		}
	}
}

func (a *agent) spawn(publicKey string) error {
	a.mu.Lock()
	port := a.freePort()
	sid := id.New()
	addr := fmt.Sprintf("%s:%d", a.c.host, port)
	args := append(append([]string{}, a.c.serverCmd[1:]...),
		"--server-id", sid, "--agent", "http://"+a.c.listen, "--game-port", fmt.Sprint(port),
		"--advertise", addr, "--public-key", publicKey)
	cmd := exec.Command(a.c.serverCmd[0], args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	s := &server{id: sid, port: port, addr: addr, cmd: cmd, state: "starting", health: time.Now()}
	a.servers[sid] = s
	a.mu.Unlock()

	if err := cmd.Start(); err != nil {
		a.mu.Lock()
		delete(a.servers, sid)
		a.mu.Unlock()
		return err
	}
	a.log.Info("server spawned", "server_id", short(sid), "addr", addr, "pid", cmd.Process.Pid)
	// Wait reaps the process; one goroutine per server, ending when it does.
	go func() {
		cmd.Wait()
		a.mu.Lock()
		s.exited = true
		a.mu.Unlock()
	}()
	return nil
}

// freePort is the lowest game port no live server uses. Caller holds a.mu.
func (a *agent) freePort() int {
	used := map[int]bool{}
	for _, s := range a.servers {
		used[s.port] = true
	}
	p := a.c.portBase
	for used[p] {
		p++
	}
	return p
}

// ---- The lifecycle API: game server → agent, localhost HTTP ---------------------------------

func (a *agent) lifecycleRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/servers/{id}/ready", a.handleReady)
	mux.HandleFunc("POST /v1/servers/{id}/allocated", a.handleAllocated)
	mux.HandleFunc("POST /v1/servers/{id}/health", a.handleHealth)
	mux.HandleFunc("POST /v1/servers/{id}/shutdown", a.handleShutdown)
	return mux
}

func (a *agent) find(w http.ResponseWriter, r *http.Request) *server {
	s := a.servers[r.PathValue("id")]
	if s == nil {
		http.Error(w, "unknown server: exit", http.StatusNotFound)
	}
	return s
}

// handleReady is the long-poll: held up to 20 s, answered at once if an allocation is already
// waiting, or the moment one arrives.
func (a *agent) handleReady(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	s := a.find(w, r)
	if s == nil {
		a.mu.Unlock()
		return
	}
	if s.state == "starting" {
		s.state = "ready"
		a.log.Info("server ready", "server_id", short(s.id), "addr", s.addr)
	}
	if s.pending != nil {
		alloc := *s.pending
		a.mu.Unlock()
		writeJSON(w, alloc)
		return
	}
	ch := make(chan allocation, 1)
	s.waiting, s.readyAt = ch, time.Now()
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		if s.waiting == ch {
			s.waiting = nil
		}
		s.readyAt = time.Now()
		a.mu.Unlock()
	}()
	select {
	case alloc := <-ch:
		writeJSON(w, alloc)
	case <-time.After(20 * time.Second):
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

// handleAllocated relays the server's acknowledgement to the backend, and the backend's answer
// back: a 409 there means the allocation was withdrawn, and the server must shut down.
func (a *agent) handleAllocated(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MatchID string `json:"match_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	s := a.find(w, r)
	if s == nil {
		a.mu.Unlock()
		return
	}
	sid := s.id
	a.mu.Unlock()

	status, err := a.call(r.Context(), "POST", "/v1/fleet/agents/"+a.c.agentID+"/servers/"+sid+"/allocated",
		map[string]string{"match_id": req.MatchID}, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if status != http.StatusNoContent {
		a.log.Warn("backend refused the acknowledgement", "server_id", short(sid), "status", status)
		http.Error(w, "allocation withdrawn", http.StatusConflict)
		return
	}
	a.mu.Lock()
	s.state, s.match, s.pending = "allocated", req.MatchID, nil
	a.mu.Unlock()
	a.log.Info("server allocated", "server_id", short(sid), "match_id", short(req.MatchID))
	w.WriteHeader(http.StatusNoContent)
}

func (a *agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.find(w, r); s != nil {
		s.health = time.Now()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *agent) handleShutdown(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.find(w, r); s != nil {
		s.state = "shutdown"
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- The backend: agent → backend, HTTP, dialing out ----------------------------------------

func (a *agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		type reported struct {
			ServerID string `json:"server_id"`
			Address  string `json:"address"`
			State    string `json:"state"`
		}
		a.mu.Lock()
		list := []reported{}
		for _, s := range a.servers {
			state := s.state
			// Ready means "would accept an allocation now": a ready call outstanding, or one that
			// ended a moment ago (the server is about to call again).
			if state == "ready" && s.waiting == nil && time.Since(s.readyAt) > 2*time.Second {
				state = "starting"
			}
			list = append(list, reported{s.id, s.addr, state})
		}
		a.mu.Unlock()

		var resp struct {
			PublicKey string   `json:"connect_token_public_key"`
			Retire    []string `json:"retire"`
		}
		status, err := a.call(ctx, "POST", "/v1/fleet/agents/"+a.c.agentID+"/heartbeat",
			map[string]any{"servers": list}, &resp)
		if err == nil && status == http.StatusOK {
			a.mu.Lock()
			if a.publicKey == "" {
				a.log.Info("connected to the backend; starting the pool")
			}
			a.publicKey = resp.PublicKey
			// Servers whose allocation was withdrawn and that the backend will never allocate
			// again: kill them, and the supervisor spawns replacements.
			for _, sid := range resp.Retire {
				if s := a.servers[sid]; s != nil && !s.exited && s.cmd.Process != nil {
					a.log.Warn("server retired by the backend (allocation withdrawn): killing", "server_id", short(sid))
					s.cmd.Process.Kill()
				}
			}
			a.mu.Unlock()
		} else if ctx.Err() == nil {
			a.log.Warn("heartbeat failed", "status", status, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// commandLoop long-polls for commands and delivers allocations to their servers.
func (a *agent) commandLoop(ctx context.Context) {
	for ctx.Err() == nil {
		var cmd struct {
			Type     string `json:"type"`
			ServerID string `json:"server_id"`
			allocation
		}
		status, err := a.call(ctx, "GET", "/v1/fleet/agents/"+a.c.agentID+"/commands", nil, &cmd)
		if err != nil || (status != http.StatusOK && status != http.StatusNoContent) {
			if ctx.Err() == nil {
				a.log.Warn("command long-poll failed", "status", status, "err", err)
				time.Sleep(time.Second)
			}
			continue
		}
		if status == http.StatusNoContent || cmd.Type != "allocate" {
			continue
		}

		a.mu.Lock()
		s := a.servers[cmd.ServerID]
		switch {
		case s == nil || s.exited:
			// Gone since it was claimed. Say nothing: the backend withdraws it after 5 s without
			// an acknowledgement, and tries another server.
			a.log.Warn("allocation for a server that is gone", "server_id", short(cmd.ServerID))
		case s.waiting != nil:
			s.waiting <- cmd.allocation
			s.waiting = nil
		default:
			// Between two ready calls: park it for the next one.
			alloc := cmd.allocation
			s.pending = &alloc
		}
		a.mu.Unlock()
	}
}

// call makes a request to the backend, moving on to the next backend URL when one fails. Any
// replica can answer any agent request, which is what makes this failover a loop and not a
// protocol.
func (a *agent) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var lastErr error
	for i := 0; i < len(a.c.backends); i++ {
		a.mu.Lock()
		base := a.c.backends[a.backend]
		a.mu.Unlock()

		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, &buf)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+a.c.secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.http.Do(req)
		if err == nil && resp.StatusCode < 500 {
			defer resp.Body.Close()
			if out != nil && resp.StatusCode == http.StatusOK {
				json.NewDecoder(resp.Body).Decode(out)
			}
			return resp.StatusCode, nil
		}
		if err == nil {
			resp.Body.Close()
			err = errors.New(resp.Status)
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		a.mu.Lock()
		a.backend = (a.backend + 1) % len(a.c.backends)
		a.mu.Unlock()
	}
	return 0, lastErr
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
