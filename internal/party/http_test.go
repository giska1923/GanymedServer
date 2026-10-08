package party

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
	"github.com/giska1923/GanymedServer/internal/db/dbtest"
	"github.com/giska1923/GanymedServer/internal/id"
	"github.com/giska1923/GanymedServer/internal/problem"
)

// The party routes as a client sees them: status codes, problem types, and the JSON shapes in
// docs/api/openapi.yaml.
func TestHTTPContract(t *testing.T) {
	f := newFixture(t)
	authn := auth.NewService(dbtest.New(t), auth.Config{
		JWTSecret: []byte("0123456789abcdef0123456789abcdef"), AccessTokenTTL: time.Hour, RefreshTokenTTL: 2 * time.Hour,
	}, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	f.s.Register(mux, authn.RequireAuth)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	type player struct{ id, token string }
	login := func() player {
		s, err := authn.LoginDevice(context.Background(), id.New())
		if err != nil {
			t.Fatal(err)
		}
		return player{s.AccountID, s.AccessToken}
	}
	call := func(p player, method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+p.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	expect := func(name string, status int, body map[string]any, wantStatus int, wantType string) {
		t.Helper()
		if status != wantStatus || (wantType != "" && body["type"] != wantType) {
			t.Errorf("%s: got %d %v, want %d %s", name, status, body, wantStatus, wantType)
		}
	}

	ana, ben, cat := login(), login(), login()

	// No party is a normal state: 200 with null.
	status, body := call(ana, "GET", "/v1/party", "")
	if status != 200 || body["party"] != nil {
		t.Fatalf("no party: %d %v", status, body)
	}

	status, body = call(ana, "POST", "/v1/party", "")
	party, _ := body["party"].(map[string]any)
	if status != 201 || party == nil || party["leader_id"] != ana.id {
		t.Fatalf("create: %d %v", status, body)
	}
	pid := party["party_id"].(string)
	members := party["members"].([]any)
	if m := members[0].(map[string]any); m["leader"] != true || m["status"] != "online" || m["display_name"] == "" {
		t.Errorf("member shape: %v", m)
	}

	s, b := call(ana, "POST", "/v1/party", "")
	expect("create twice", s, b, 409, problem.TypeAlreadyInParty)

	s, _ = call(ana, "POST", "/v1/party/invites", `{"account_id":"`+ben.id+`"}`)
	expect("invite", s, nil, 204, "")
	s, b = call(ana, "POST", "/v1/party/invites", `{"account_id":"not-a-uuid"}`)
	expect("invite bad id", s, b, 400, problem.TypeInvalidRequest)
	s, b = call(ana, "POST", "/v1/party/invites", `{"account_id":"`+ana.id+`"}`)
	expect("invite self", s, b, 400, problem.TypeInvalidRequest)

	status, body = call(ben, "GET", "/v1/party/invites", "")
	invites, _ := body["invites"].([]any)
	if status != 200 || len(invites) != 1 {
		t.Fatalf("list invites: %d %v", status, body)
	}
	if inv := invites[0].(map[string]any); inv["party_id"] != pid || inv["from"].(map[string]any)["account_id"] != ana.id {
		t.Errorf("invite shape: %v", inv)
	}
	if status, body = call(cat, "GET", "/v1/party/invites", ""); status != 200 || body["invites"] == nil {
		t.Errorf("no invites must be [], not null: %d %v", status, body)
	}

	status, body = call(ben, "POST", "/v1/party/invites/"+pid+"/accept", "")
	if status != 200 || len(body["party"].(map[string]any)["members"].([]any)) != 2 {
		t.Fatalf("accept: %d %v", status, body)
	}
	s, b = call(cat, "POST", "/v1/party/invites/"+pid+"/accept", "")
	expect("accept without an invite", s, b, 404, problem.TypeNotFound)

	s, b = call(ben, "POST", "/v1/party/kick", `{"account_id":"`+ana.id+`"}`)
	expect("non-leader kick", s, b, 403, problem.TypeNotPartyLeader)
	s, b = call(ana, "POST", "/v1/party/kick", `{"account_id":"`+cat.id+`"}`)
	expect("kick a non-member", s, b, 404, problem.TypeNotFound)
	s, _ = call(ana, "POST", "/v1/party/kick", `{"account_id":"`+ben.id+`"}`)
	expect("kick", s, nil, 204, "")

	s, _ = call(ana, "POST", "/v1/party/leave", "")
	expect("leave", s, nil, 204, "")
	s, b = call(ana, "POST", "/v1/party/leave", "")
	expect("leave with no party", s, b, 404, problem.TypeNotFound)

	s, _ = call(cat, "POST", "/v1/party/invites/"+pid+"/decline", "")
	expect("decline is idempotent", s, nil, 204, "")
}
