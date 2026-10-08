package leaderboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/problem"
	"github.com/giska1923/GanymedServer/internal/profile"
)

// The contract as a client sees it: auth, profile and leaderboard wired the way main wires them.
func TestHTTPContract(t *testing.T) {
	s := newTestService(t)
	log := slog.New(slog.DiscardHandler)
	authn := auth.NewService(s.pool, auth.Config{
		JWTSecret: []byte("0123456789abcdef0123456789abcdef"), AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour,
	}, log)
	mux := http.NewServeMux()
	authn.Register(mux)
	profile.NewService(s.pool, log).Register(mux, authn.RequireAuth)
	s.Register(mux, authn.RequireAuth)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sess, err := authn.LoginDevice(context.Background(), newID())
	if err != nil {
		t.Fatal(err)
	}

	type reply struct {
		status int
		header http.Header
		body   map[string]any
	}
	call := func(method, path, body string, headers ...string) reply {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return reply{resp.StatusCode, resp.Header, out}
	}
	expectProblem := func(name string, r reply, status int, typ string) {
		t.Helper()
		if r.status != status || r.body["type"] != typ {
			t.Errorf("%s: got %d %v, want %d %s", name, r.status, r.body, status, typ)
		}
	}

	// No score yet: 200 with nulls, not an error.
	r := call("GET", "/v1/leaderboards/proving-ground/me", "")
	if r.status != 200 || r.body["rank"] != nil || r.body["best"] != nil {
		t.Fatalf("me before scoring: %d %v", r.status, r.body)
	}

	key := newID()
	r = call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":1234}`, "Idempotency-Key", key)
	if r.status != 200 || r.body["rank"] != 1.0 || r.body["best"] != 1234.0 || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("submit: %d %v %v", r.status, r.body, r.header)
	}
	r = call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":1234}`, "Idempotency-Key", key)
	if r.status != 200 || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry: %d %v", r.status, r.header)
	}

	expectProblem("key reused", call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":99}`, "Idempotency-Key", key),
		422, problem.TypeIdempotencyKeyReused)
	expectProblem("no key", call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":1}`),
		400, problem.TypeInvalidRequest)
	expectProblem("key not a uuid", call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":1}`, "Idempotency-Key", "abc"),
		400, problem.TypeInvalidRequest)
	expectProblem("float score", call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":12.0}`, "Idempotency-Key", newID()),
		400, problem.TypeInvalidRequest)
	expectProblem("negative score", call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":-1}`, "Idempotency-Key", newID()),
		400, problem.TypeInvalidRequest)
	expectProblem("score above 2^53-1", call("POST", "/v1/leaderboards/proving-ground/scores", `{"score":9007199254740992}`, "Idempotency-Key", newID()),
		400, problem.TypeInvalidRequest)
	expectProblem("missing score", call("POST", "/v1/leaderboards/proving-ground/scores", `{}`, "Idempotency-Key", newID()),
		400, problem.TypeInvalidRequest)
	expectProblem("unknown board", call("POST", "/v1/leaderboards/nope/scores", `{"score":1}`, "Idempotency-Key", newID()),
		404, problem.TypeNotFound)
	expectProblem("bad limit", call("GET", "/v1/leaderboards/proving-ground?limit=0", ""),
		400, problem.TypeInvalidRequest)

	r = call("PATCH", "/v1/me/profile", `{"display_name":"Tester"}`)
	if r.status != 200 || r.body["display_name"] != "Tester" {
		t.Fatalf("rename: %d %v", r.status, r.body)
	}
	r = call("GET", "/v1/leaderboards/proving-ground?limit=5", "")
	entries, _ := r.body["entries"].([]any)
	if r.status != 200 || len(entries) != 1 {
		t.Fatalf("top: %d %v", r.status, r.body)
	}
	if e := entries[0].(map[string]any); e["display_name"] != "Tester" || e["score"] != 1234.0 || e["rank"] != 1.0 {
		t.Errorf("entry: %v", e)
	}

	// Unauthenticated access is refused on every leaderboard route.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/leaderboards/proving-ground", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
}
