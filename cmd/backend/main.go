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
	"syscall"

	"github.com/giska1923/GanymedServer/internal/auth"
	"github.com/giska1923/GanymedServer/internal/config"
	"github.com/giska1923/GanymedServer/internal/db"
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

	mux := http.NewServeMux()
	server.RegisterHealth(mux, pool, log)
	auth.NewService(pool, auth.Config{
		JWTSecret:       cfg.JWTSecret,
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
	}, log).Register(mux)

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	log.Info("listening", "addr", ln.Addr().String())

	srv := server.New(cfg.HTTPAddr, server.Middleware(mux, log), log)
	return server.Run(ctx, srv, ln, cfg.ShutdownTimeout, log)
}
