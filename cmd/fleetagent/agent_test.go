package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestAgent is an agent with one known server, talking to the given backend URLs. No process is
// spawned: the lifecycle handlers only need the server's record.
func newTestAgent(backends ...string) (*agent, *server) {
	a := newAgent(config{backends: backends, agentID: "agent-test", secret: "agent-secret"}, slog.New(slog.DiscardHandler))
	s := &server{id: "srv-1", state: "allocated", match: "match-1", resultToken: "result-secret"}
	a.servers[s.id] = s
	return a, s
}

func postResult(a *agent, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.lifecycleRoutes().ServeHTTP(w, httptest.NewRequest("POST", "/v1/servers/srv-1/result", strings.NewReader(body)))
	return w
}

// The result goes to the next replica when the first is down, with the match's result token (not
// the agent's secret), and the backend's answer comes back unchanged.
func TestResultFailsOverAndRelays(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"match_id":"match-1","outcome":"victory","rating_change":16}`))
	}))
	defer up.Close()

	a, _ := newTestAgent(down.URL, up.URL)
	w := postResult(a, `{"outcome":"victory"}`)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rating_change":16`) {
		t.Fatalf("relayed %d %q, want 200 with the backend's body", w.Code, w.Body.String())
	}
	if gotPath != "/v1/matches/match-1/result" || gotAuth != "Bearer result-secret" || gotBody != `{"outcome":"victory"}` {
		t.Fatalf("backend saw %s, %q, %q", gotPath, gotAuth, gotBody)
	}
}

// A 4xx is the backend's answer, not a failure: it is relayed, and not retried on another replica.
func TestResultRelaysRefusal(t *testing.T) {
	calls := 0
	refuse := func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"type":"urn:ganymed:problem:result-conflict"}`))
	}
	b1, b2 := httptest.NewServer(http.HandlerFunc(refuse)), httptest.NewServer(http.HandlerFunc(refuse))
	defer b1.Close()
	defer b2.Close()

	a, _ := newTestAgent(b1.URL, b2.URL)
	w := postResult(a, `{"outcome":"defeat"}`)

	if w.Code != http.StatusConflict || w.Header().Get("Content-Type") != "application/problem+json" || calls != 1 {
		t.Fatalf("relayed %d %q after %d calls, want 409 problem+json after 1", w.Code, w.Header().Get("Content-Type"), calls)
	}
}

// With no replica up, the server gets a 5xx, which tells it to retry.
func TestResultNoBackend(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := down.URL
	down.Close()

	a, _ := newTestAgent(url)
	if w := postResult(a, `{"outcome":"victory"}`); w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", w.Code)
	}
}

// A server with no acknowledged allocation has nothing to report.
func TestResultWithoutAllocation(t *testing.T) {
	a, s := newTestAgent("http://127.0.0.1:1")
	s.match, s.resultToken = "", ""
	if w := postResult(a, `{"outcome":"victory"}`); w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", w.Code)
	}
}
