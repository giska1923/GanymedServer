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
	"github.com/giska1923/GanymedServer/internal/matchmaking"
	"github.com/giska1923/GanymedServer/internal/party"
	"github.com/giska1923/GanymedServer/internal/profile"
	"github.com/giska1923/GanymedServer/internal/realtime"
	"github.com/giska1923/GanymedServer/internal/redisdb"
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

	rdb, err := redisdb.Open(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer rdb.Close()

	log = log.With("replica", cfg.ReplicaID)

	authn := auth.NewService(pool, auth.Config{
		JWTSecret:       cfg.JWTSecret,
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
	}, log)
	profiles := profile.NewService(pool, log)
	// profiles is passed as leaderboard.Names: the leaderboard asks for names, it never joins.
	boards := leaderboard.NewService(pool, profiles, log)
	gateway, err := realtime.NewGateway(ctx, rdb, cfg.ReplicaID, log)
	if err != nil {
		return err
	}
	// The gateway is both of party's dependencies on realtime: Presence and Notifier.
	parties := party.NewService(rdb, gateway, gateway, profiles, log)
	// Matchmaking reads party rosters and ratings, and pushes through the gateway.
	matcher := matchmaking.NewService(rdb, parties, profiles, gateway, log)

	mux := http.NewServeMux()
	server.RegisterHealth(mux, map[string]server.Pinger{
		"postgres": pool,
		"redis":    server.PingFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
	}, log)
	authn.Register(mux)
	profiles.Register(mux, authn.RequireAuth)
	boards.Register(mux, authn.RequireAuth)
	parties.Register(mux, authn.RequireAuth)
	gateway.Register(mux, authn.RequireAuth)
	matcher.Register(mux, authn.RequireAuth)

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	log.Info("listening", "addr", ln.Addr().String())

	// Background work. Defers run last-in first-out, so on return this order is: cancel ctx (so
	// the goroutines stop even if Run returned because serving failed, not because of a signal),
	// then wait for them, then the rdb.Close and pool.Close registered above, which they were
	// still using.
	//
	// gateway.Run is here too: http.Server.Shutdown neither closes nor waits for hijacked
	// connections, so the gateway closes its sockets itself when ctx ends, and waiting for Run is
	// what waits for them.
	var background sync.WaitGroup
	defer background.Wait()
	defer stop()
	background.Go(func() { gateway.Run(ctx) })
	background.Go(func() { parties.RunSweeper(ctx, 5*time.Second) })
	background.Go(func() { boards.ExpireKeys(ctx, time.Hour) })
	// Every replica runs the director loop; the lease decides which one actually matches.
	background.Go(func() { matcher.RunDirector(ctx, cfg.ReplicaID, matchmaking.DefaultDirector) })

	srv := server.New(cfg.HTTPAddr, server.Middleware(mux, log), log)
	return server.Run(ctx, srv, ln, cfg.ShutdownTimeout, log)
}
