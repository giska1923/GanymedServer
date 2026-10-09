package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// hello sends one "HELLO <token>" datagram to a game server and returns its one-line reply
// (docs/api/server-lifecycle.md, Players).
func hello(addr, token string) (string, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HELLO " + token)); err != nil {
		return "", err
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("no answer from %s: %w", addr, err)
	}
	return string(buf[:n]), nil
}

// load signs in n fresh players, queues them all for mode, and follows every ticket to its end,
// reporting what happened at each stage. It drives the backend the way n separate clients would,
// from one process, with bounded concurrency so the client itself is not the bottleneck being
// measured.
//
// Without a fleet, tickets stop at "matched" (then fail with no_server after 30 s). With an agent
// running, they go on to ready, every player joins its server over UDP, and the servers report
// results.
func (c *cli) load(mode string, n int) error {
	const workers = 32
	type player struct {
		token, ticket      string
		state, reason      string
		size               int
		readyAfter         time.Duration
		addr, connectToken string
		joined             string // the first word of the server's answer to HELLO
		outcome            string
		ratingChange       int
	}
	players := make([]player, n)

	call := func(method, route, token string, body any) (int, map[string]any, error) {
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req, err := http.NewRequest(method, c.server+route, &buf)
		if err != nil {
			return 0, nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, nil
	}

	// parallel runs fn(i) for every i in [0, n) on a fixed pool of goroutines: the worker-pool
	// pattern. A channel of indexes is the work queue; closing it tells the workers to finish.
	parallel := func(fn func(i int) error) error {
		work := make(chan int)
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Go(func() {
				for i := range work {
					if err := fn(i); err != nil {
						errs <- err
					}
				}
			})
		}
		for i := 0; i < n; i++ {
			work <- i
		}
		close(work)
		wg.Wait()
		close(errs)
		return <-errs // the first error, or nil
	}

	// poll reads every player's ticket until done(state) holds for all, or the deadline passes.
	start := time.Now()
	poll := func(done func(string) bool, limit time.Duration) error {
		for {
			if err := parallel(func(i int) error {
				p := &players[i]
				if done(p.state) {
					return nil
				}
				status, body, err := call("GET", "/v1/matchmaking/ticket", p.token, nil)
				if err != nil || status != http.StatusOK {
					return fmt.Errorf("poll %d: %d %v", i, status, err)
				}
				t, _ := body["ticket"].(map[string]any)
				p.state, _ = t["state"].(string)
				p.reason, _ = t["failure_reason"].(string)
				if m, ok := t["match"].(map[string]any); ok {
					p.size = len(m["players"].([]any))
				}
				if s, ok := t["server"].(map[string]any); ok {
					if p.readyAfter == 0 {
						p.readyAfter = time.Since(start)
					}
					p.addr, _ = s["address"].(string)
					p.connectToken, _ = s["connect_token"].(string)
				}
				// Join the moment the ticket is ready, as a real client would. Joining later is
				// wrong under load: a short match can end, and its port be reused by another
				// match's server, before the slowest ticket is ready.
				if p.state == "ready" && p.joined == "" && p.connectToken != "" {
					reply, err := hello(p.addr, p.connectToken)
					if err != nil {
						reply = "ERROR " + err.Error()
					}
					p.joined, _, _ = strings.Cut(reply, " ")
				}
				if r, ok := t["result"].(map[string]any); ok {
					p.outcome, _ = r["outcome"].(string)
					rc, _ := r["rating_change"].(float64)
					p.ratingChange = int(rc)
				}
				return nil
			}); err != nil {
				return err
			}
			pending := 0
			for _, p := range players {
				if !done(p.state) {
					pending++
				}
			}
			if pending == 0 {
				return nil
			}
			if time.Since(start) > limit {
				return fmt.Errorf("%d tickets still not done after %v", pending, limit)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	counts := func() string {
		m := map[string]int{}
		for _, p := range players {
			k := p.state
			if p.reason != "" {
				k += "(" + p.reason + ")"
			}
			m[k]++
		}
		return fmt.Sprint(m)
	}

	fmt.Printf("signing in %d players...\n", n)
	if err := parallel(func(i int) error {
		status, body, err := call("POST", "/v1/auth/device", "", map[string]string{"device_id": newUUID()})
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("login %d: %d %v %v", i, status, body, err)
		}
		players[i].token = body["access_token"].(string)
		return nil
	}); err != nil {
		return err
	}

	fmt.Printf("queueing %d tickets for %q...\n", n, mode)
	start = time.Now()
	if err := parallel(func(i int) error {
		status, body, err := call("POST", "/v1/matchmaking/tickets", players[i].token, map[string]string{"mode": mode})
		if err != nil || status != http.StatusCreated {
			return fmt.Errorf("queue %d: %d %v %v", i, status, body, err)
		}
		players[i].ticket = body["ticket_id"].(string)
		players[i].state = "queued"
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("all queued in %v\n", time.Since(start).Round(time.Millisecond))

	// Stage 1: the queue drains.
	if err := poll(func(s string) bool { return s != "queued" }, 3*time.Minute); err != nil {
		return err
	}
	fmt.Printf("queue drained %v after the first ticket: %s\n", time.Since(start).Round(time.Millisecond), counts())
	bySize := map[int]int{}
	for _, p := range players {
		if p.size > 0 {
			bySize[p.size]++
		}
	}
	var ks []int
	for k := range bySize {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		fmt.Printf("  players in a %d-player match: %d (%d matches)\n", k, bySize[k], bySize[k]/k)
	}

	// Stage 2: servers. Stop if there is no fleet (everything stays matched).
	if err := poll(func(s string) bool { return s != "matched" && s != "allocating" && s != "queued" }, 40*time.Second); err != nil {
		fmt.Printf("no game servers took these matches (%v). Is a fleet agent running?\n", err)
		return nil
	}
	var waits []time.Duration
	for _, p := range players {
		if p.readyAfter > 0 {
			waits = append(waits, p.readyAfter)
		}
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	fmt.Printf("servers ready: %s\n", counts())
	if len(waits) > 0 {
		fmt.Printf("  queue-to-ready, as seen by polling: median %v, slowest %v\n",
			waits[len(waits)/2].Round(time.Millisecond), waits[len(waits)-1].Round(time.Millisecond))
	}

	// Stage 3: every player joined its server over UDP as its ticket became ready (in poll).
	replies := map[string]int{}
	for _, p := range players {
		if p.joined != "" {
			replies[p.joined]++
		}
	}
	fmt.Printf("UDP joins, each sent as its ticket became ready: %v\n", replies)

	// Stage 4: the matches end and report.
	if err := poll(func(s string) bool { return s == "finished" || s == "failed" || s == "cancelled" }, 3*time.Minute); err != nil {
		return err
	}
	changes := map[string]int{}
	for _, p := range players {
		if p.outcome != "" {
			changes[fmt.Sprintf("%s %+d", p.outcome, p.ratingChange)]++
		}
	}
	fmt.Printf("ended %v after the first ticket: %s; results %v\n", time.Since(start).Round(time.Millisecond), counts(), changes)
	return nil
}
