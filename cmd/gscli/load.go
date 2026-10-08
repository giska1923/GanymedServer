package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// load signs in n fresh players, queues them all for mode, then polls until every ticket has left
// the queue, and reports how long that took and what the matches looked like. It drives the
// backend the way n separate clients would, from one process, with bounded concurrency so the
// client itself is not the bottleneck being measured.
func (c *cli) load(mode string, n int) error {
	const workers = 32
	type player struct{ token, ticket string }
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
	start := time.Now()
	if err := parallel(func(i int) error {
		status, body, err := call("POST", "/v1/matchmaking/tickets", players[i].token, map[string]string{"mode": mode})
		if err != nil || status != http.StatusCreated {
			return fmt.Errorf("queue %d: %d %v %v", i, status, body, err)
		}
		players[i].ticket = body["ticket_id"].(string)
		return nil
	}); err != nil {
		return err
	}
	queued := time.Since(start)
	fmt.Printf("all queued in %v; waiting for the queue to drain...\n", queued.Round(time.Millisecond))

	states := make([]string, n)
	sizes := make([]int, n)
	for {
		if err := parallel(func(i int) error {
			if states[i] != "" && states[i] != "queued" {
				return nil
			}
			status, body, err := call("GET", "/v1/matchmaking/ticket", players[i].token, nil)
			if err != nil || status != http.StatusOK {
				return fmt.Errorf("poll %d: %d %v", i, status, err)
			}
			t, _ := body["ticket"].(map[string]any)
			states[i], _ = t["state"].(string)
			if m, ok := t["match"].(map[string]any); ok {
				sizes[i] = len(m["players"].([]any))
			}
			return nil
		}); err != nil {
			return err
		}
		waiting := 0
		for _, s := range states {
			if s == "queued" {
				waiting++
			}
		}
		if waiting == 0 {
			break
		}
		if time.Since(start) > 3*time.Minute {
			return fmt.Errorf("%d tickets still queued after 3 minutes", waiting)
		}
		time.Sleep(250 * time.Millisecond)
	}
	drained := time.Since(start)

	byState := map[string]int{}
	bySize := map[int]int{}
	for i := range states {
		byState[states[i]]++
		if states[i] == "matched" {
			bySize[sizes[i]]++
		}
	}
	fmt.Printf("drained: every ticket left the queue %v after the first was queued\n", drained.Round(time.Millisecond))
	fmt.Printf("states: %v\n", byState)
	var ks []int
	for k := range bySize {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		fmt.Printf("players in a %d-player match: %d (%d matches)\n", k, bySize[k], bySize[k]/k)
	}
	return nil
}
