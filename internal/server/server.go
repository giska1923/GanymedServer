// Package server owns the HTTP listener: the mux, middleware, timeouts and graceful shutdown.
// Modules register their own routes on the mux; this package holds no business logic.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// New builds the http.Server with explicit timeouts.
//
// A zero-valued http.Server has no timeouts at all. A client that opens a connection and sends
// headers one byte a minute (Slowloris) then holds a goroutine and a file descriptor forever.
// ReadHeaderTimeout bounds that; the others bound slow bodies, slow readers and idle keep-alives.
// WriteTimeout also caps how long any handler may run, which B3's WebSocket route will need to
// lift per connection (http.ResponseController.SetWriteDeadline).
func New(addr string, handler http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// Run serves on ln until ctx is cancelled, then shuts down gracefully: it stops accepting new
// connections, lets in-flight requests finish, and gives up after shutdownTimeout.
//
// It takes a listener rather than calling ListenAndServe so that tests can bind port 0 and learn
// the address, and so that a bind failure is reported by the caller before anything else starts.
func Run(ctx context.Context, srv *http.Server, ln net.Listener, shutdownTimeout time.Duration, log *slog.Logger) error {
	serveErr := make(chan error, 1)
	go func() {
		// Serve always returns non-nil; ErrServerClosed is the normal result of Shutdown.
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", shutdownTimeout.String())
	// A fresh context: ctx is already cancelled, and Shutdown must be given time to drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
