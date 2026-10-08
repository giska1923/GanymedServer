package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/giska1923/GanymedServer/internal/problem"
)

// Cancelling the context must let an in-flight request finish, refuse new connections, and make
// Run return nil. That is the whole contract of graceful shutdown.
func TestRunDrainsInFlightRequests(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, New("", mux, log), ln, 5*time.Second, log) }()

	type result struct {
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := http.Get(addr + "/slow")
		if err != nil {
			slow <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		slow <- result{body: string(b)}
	}()

	<-started
	cancel()

	if r := <-slow; r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body %q, err %v", r.body, r.err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := http.Get(addr + "/slow"); err == nil {
		t.Fatal("a new request succeeded after shutdown")
	}
}

func TestMiddlewareRecoversAndLogsRoute(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom/{id}", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: Middleware(mux, log)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/boom/secret-looking-id")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p problem.Details
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if resp.StatusCode != 500 || p.Type != problem.TypeInternal || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("got %d %+v", resp.StatusCode, p)
	}

	out := logs.String()
	if !strings.Contains(out, `"route":"GET /boom/{id}"`) {
		t.Errorf("request log does not name the route pattern:\n%s", out)
	}
	if strings.Contains(out, "secret-looking-id") {
		t.Errorf("request log contains the raw path:\n%s", out)
	}
}
