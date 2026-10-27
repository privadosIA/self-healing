// Command healerd listens for failed CI pipelines and opens auto-fix pull requests.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/privadosIA/self-healing/internal/app"
	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/logging"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] == "healthcheck" {
			os.Exit(healthcheck())
		}
		fmt.Fprintf(os.Stderr, "healerd: unknown command %q (the only one is \"healthcheck\")\n", os.Args[1])
		os.Exit(2)
	}
	os.Exit(run())
}

// healthcheck asks the local daemon whether it is healthy. It exists because
// the container image has no curl or wget. It reads HEALER_LISTEN_ADDR to find
// the port and exits 0 when /healthz answers 200.
func healthcheck() int {
	addr := os.Getenv("HEALER_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healerd healthcheck: invalid HEALER_LISTEN_ADDR: %v\n", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	// #nosec G704 -- the address is the operator's own HEALER_LISTEN_ADDR; this is an unauthenticated GET of /healthz whose body is never read
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healerd healthcheck: the daemon is not answering")
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healerd healthcheck: /healthz returned HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func run() int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "healerd: invalid configuration:\n%v\n", err)
		return 1
	}
	log, err := logging.New(os.Stderr, cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healerd: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		log.Error("cannot start", "error", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		log.Error("stopped with an error", "error", err)
		return 1
	}
	return 0
}
