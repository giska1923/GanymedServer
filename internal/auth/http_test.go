package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/problem"
)

// The full HTTP path a client takes: login, an authenticated call, an expired token, a refresh.
func TestHTTPFlow(t *testing.T) {
	s := newTestService(t)
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(method, path, token, body string) (*http.Response, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	resp, login := call("POST", "/v1/auth/device", "", `{"device_id":"`+deviceA+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %v", resp.StatusCode, login)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("token response is cacheable")
	}
	access := login["access_token"].(string)

	resp, me := call("GET", "/v1/me", access, "")
	if resp.StatusCode != 200 || me["account_id"] != login["account_id"] {
		t.Fatalf("me: %d %v", resp.StatusCode, me)
	}

	resp, prob := call("GET", "/v1/me", "", "")
	if resp.StatusCode != 401 || prob["type"] != problem.TypeUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("no token: %d %v", resp.StatusCode, prob)
	}

	// Move the verifier's clock past expiry.
	s.access.now = func() time.Time { return time.Now().Add(time.Hour) }
	resp, prob = call("GET", "/v1/me", access, "")
	if resp.StatusCode != 401 || prob["type"] != problem.TypeTokenExpired {
		t.Fatalf("expired: %d %v", resp.StatusCode, prob)
	}
	s.access.now = time.Now

	resp, refreshed := call("POST", "/v1/auth/refresh", "", `{"refresh_token":"`+login["refresh_token"].(string)+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("refresh: %d %v", resp.StatusCode, refreshed)
	}
	resp, prob = call("POST", "/v1/auth/refresh", "", `{"refresh_token":"`+login["refresh_token"].(string)+`"}`)
	if resp.StatusCode != 401 || prob["type"] != problem.TypeInvalidRefreshToken {
		t.Fatalf("reused refresh: %d %v", resp.StatusCode, prob)
	}

	for name, body := range map[string]string{
		"short device id": `{"device_id":"abc"}`,
		"not json":        `{device_id:`,
		"two values":      `{"device_id":"` + deviceA + `"} {}`,
		"empty":           ``,
	} {
		resp, prob := call("POST", "/v1/auth/device", "", body)
		if resp.StatusCode != 400 || prob["type"] != problem.TypeInvalidRequest {
			t.Errorf("%s: %d %v", name, resp.StatusCode, prob)
		}
	}
}
