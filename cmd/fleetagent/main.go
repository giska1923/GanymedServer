// Command fleetagent keeps a warm pool of game server processes on this machine and connects them
// to the backend: it is Agones' sidecar-plus-fleet-controller, for one machine.
//
//	fleetagent [flags] -- <game server command> [its own flags]
//	fleetagent -pool 3 -- bin/stubserver.exe -match-seconds 15
//
// The game server command gets the lifecycle flags (--server-id, --agent, --game-port,
// --advertise, --public-key) appended, so any binary implementing docs/api/server-lifecycle.md
// can be run, stubserver today and GanymedDedicated later.
//
// It dials out, never in: a heartbeat to the backend every second, and a long-poll for commands.
// The backend never needs to know how to reach this machine.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type config struct {
	backends   []string // tried in turn: any replica will do
	agentID    string
	secret     string
	pool       int
	host       string // the address players reach the servers at
	portBase   int
	listen     string // the lifecycle API, localhost only
	serverCmd  []string
	healthKill time.Duration
}

func main() {
	var c config
	backends := flag.String("backend", "http://localhost:8080,http://localhost:8082", "backend base URLs, comma-separated; any replica")
	host, _ := os.Hostname()
	flag.StringVar(&c.agentID, "id", "agent-"+host, "this agent's ID, stable across restarts")
	flag.IntVar(&c.pool, "pool", 2, "warm pool size: game servers kept ready")
	flag.StringVar(&c.host, "host", "127.0.0.1", "the host players connect to")
	flag.IntVar(&c.portBase, "port-base", 7001, "first UDP game port")
	flag.StringVar(&c.listen, "listen", "127.0.0.1:7600", "lifecycle API address (keep it on localhost)")
	flag.Parse()
	c.backends = strings.Split(*backends, ",")
	c.serverCmd = flag.Args()
	c.secret = os.Getenv("GS_FLEET_AGENT_SECRET")
	c.healthKill = 6 * time.Second

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("agent", c.agentID)
	if len(c.serverCmd) == 0 || c.secret == "" {
		fmt.Fprintln(os.Stderr, "usage: GS_FLEET_AGENT_SECRET=... fleetagent [flags] -- <game server command> [args]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := newAgent(c, log)
	if err := a.run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}
