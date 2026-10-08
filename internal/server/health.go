package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Pinger is what readiness needs from the database. Declared here, by the consumer, so this
// package does not import pgx; *pgxpool.Pool satisfies it without knowing it does.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RegisterHealth adds the two probes. They answer different questions, and an orchestrator acts on
// them differently:
//
//   - /healthz (liveness): is the process able to serve at all? Failing means "restart me".
//     It checks nothing external, on purpose. If it pinged the database, a database outage
//     would make every replica restart in a loop, which fixes nothing and adds load.
//   - /readyz (readiness): can it do useful work right now? Failing means "send me no traffic".
//     A database outage belongs here.
func RegisterHealth(mux *http.ServeMux, db Pinger, log *slog.Logger) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.Warn("readiness: database unreachable", "err", err)
			http.Error(w, "database unreachable\n", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
}
