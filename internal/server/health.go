package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Pinger is what readiness needs from a dependency. Declared here, by the consumer, so this
// package imports neither pgx nor go-redis. *pgxpool.Pool satisfies it as it is.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingFunc adapts a plain function to Pinger, the same trick as http.HandlerFunc: a function type
// with a method. It exists for go-redis, whose Ping returns a command object, not an error:
//
//	server.PingFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
type PingFunc func(ctx context.Context) error

func (f PingFunc) Ping(ctx context.Context) error { return f(ctx) }

// RegisterHealth adds the two probes. They answer different questions, and an orchestrator acts on
// them differently:
//
//   - /healthz (liveness): is the process able to serve at all? Failing means "restart me".
//     It checks nothing external, on purpose. If it pinged the database, a database outage
//     would make every replica restart in a loop, which fixes nothing and adds load.
//   - /readyz (readiness): can it do useful work right now? Failing means "send me no traffic".
//     Every dependency in deps is pinged; the first one down is named in the response.
func RegisterHealth(mux *http.ServeMux, deps map[string]Pinger, log *slog.Logger) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for name, dep := range deps {
			if err := dep.Ping(ctx); err != nil {
				log.Warn("readiness: dependency unreachable", "dependency", name, "err", err)
				http.Error(w, name+" unreachable\n", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
}
