package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/giska1923/GanymedServer/internal/id"
)

// allocation is what a game server receives from its ready long-poll. The match's result token is
// not in it: the agent keeps it and reports the result for the server (handleResult).
type allocation struct {
	MatchID string   `json:"match_id"`
	Players []string `json:"players"`
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

	// The credential for reporting that match's result, from the allocate command. A secret:
	// never logged, never sent to the server.
	resultToken string
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
	mux.HandleFunc("POST /v1/servers/{id}/result", a.handleResult)
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
		// commandLoop sends while holding a.mu, and the channel has room for one, so a send can
		// land after this call stopped listening (the timeout and the command in the same
		// instant). Take it back and park it for the next ready call rather than lose it.
		select {
		case alloc := <-ch:
			s.pending = &alloc
		default:
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

// handleResult reports the server's match result to the backend, through the same replica
// failover as every other agent call. That failover is the reason the server reports here rather
// than to the backend itself: a result posted to one replica is lost while that replica is down.
//
// The body ({"outcome": ...}) is forwarded unread, and the backend's answer (status and body) is
// relayed back, so the backend stays the one place that validates a result. When no replica
// answers, the server gets a 502 and retries, as it would for a 5xx.
func (a *agent) handleResult(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s := a.find(w, r)
	if s == nil {
		a.mu.Unlock()
		return
	}
	sid, match, token := s.id, s.match, s.resultToken
	a.mu.Unlock()
	if match == "" || token == "" {
		http.Error(w, "this server has no acknowledged allocation", http.StatusConflict)
		return
	}

	status, ctype, resp, err := a.do(r.Context(), "POST", "/v1/matches/"+match+"/result", token, body)
	if err != nil {
		a.log.Warn("result not delivered: no backend answered", "server_id", short(sid), "match_id", short(match), "err", err)
		http.Error(w, "no backend answered: "+err.Error(), http.StatusBadGateway)
		return
	}
	a.log.Info("result forwarded", "server_id", short(sid), "match_id", short(match), "status", status)
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	w.Write(resp)
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
			Type        string `json:"type"`
			ServerID    string `json:"server_id"`
			ResultToken string `json:"result_token"`
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
			s.resultToken = cmd.ResultToken
			s.waiting <- cmd.allocation
			s.waiting = nil
		default:
			// Between two ready calls: park it for the next one.
			s.resultToken = cmd.ResultToken
			alloc := cmd.allocation
			s.pending = &alloc
		}
		a.mu.Unlock()
	}
}

// call makes an agent-authenticated JSON request to the backend (see do), decoding a 200's body
// into out.
func (a *agent) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	status, _, resp, err := a.do(ctx, method, path, a.c.secret, buf)
	if err == nil && out != nil && status == http.StatusOK {
		json.Unmarshal(resp, out)
	}
	return status, err
}

// do makes a request to the backend with the given bearer credential, moving on to the next
// backend URL when one fails (a network error or a 5xx). Any replica can answer any agent request,
// which is what makes this failover a loop and not a protocol. It returns the answer of the first
// replica that gave one below 500: status, Content-Type and body.
func (a *agent) do(ctx context.Context, method, path, bearer string, body []byte) (int, string, []byte, error) {
	var lastErr error
	for i := 0; i < len(a.c.backends); i++ {
		a.mu.Lock()
		base := a.c.backends[a.backend]
		a.mu.Unlock()

		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
		if err != nil {
			return 0, "", nil, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.http.Do(req)
		if err == nil && resp.StatusCode < 500 {
			defer resp.Body.Close()
			out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				return 0, "", nil, fmt.Errorf("read response: %w", err)
			}
			return resp.StatusCode, resp.Header.Get("Content-Type"), out, nil
		}
		if err == nil {
			resp.Body.Close()
			err = errors.New(resp.Status)
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, "", nil, ctx.Err()
		}
		a.mu.Lock()
		a.backend = (a.backend + 1) % len(a.c.backends)
		a.mu.Unlock()
	}
	return 0, "", nil, lastErr
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
