// Command backend is the GanymedServer service: config, database, migrations, routes, serve.
// Wiring only; every behaviour lives in internal/.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/config"
	"github.com/giska1923/GanymedServer/internal/db"
	"github.com/giska1923/GanymedServer/internal/leaderboard"
	"github.com/giska1923/GanymedServer/internal/profile"
	"github.com/giska1923/GanymedServer/internal/server"
	"github.com/giska1923/GanymedServer/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "backend:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config:\n%w", err)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// SIGINT for Ctrl+C, SIGTERM for `docker stop`. Cancelling ctx starts graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, migrations.FS, log); err != nil {
		return err
	}

	authn := auth.NewService(pool, auth.Config{
		JWTSecret:       cfg.JWTSecret,
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
	}, log)
	profiles := profile.NewService(pool, log)
	// profiles is passed as leaderboard.Names: the leaderboard asks for names, it never joins.
	boards := leaderboard.NewService(pool, profiles, log)

	mux := http.NewServeMux()
	server.RegisterHealth(mux, pool, log)
	authn.Register(mux)
	profiles.Register(mux, authn.RequireAuth)
	boards.Register(mux, authn.RequireAuth)

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	log.Info("listening", "addr", ln.Addr().String())

	// Background work. Defers run last-in first-out, so on return this order is: cancel ctx (so
	// the goroutines stop even if Run returned because serving failed, not because of a signal),
	// then wait for them, then the pool.Close registered above, which they were still using.
	var background sync.WaitGroup
	defer background.Wait()
	defer stop()
	background.Go(func() { boards.ExpireKeys(ctx, time.Hour) })

	srv := server.New(cfg.HTTPAddr, server.Middleware(mux, log), log)
	return server.Run(ctx, srv, ln, cfg.ShutdownTimeout, log)
}
