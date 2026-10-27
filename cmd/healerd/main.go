// Command healerd listens for failed CI pipelines and opens auto-fix pull requests.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/privadosIA/self-healing/internal/app"
	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/logging"
)

func main() { os.Exit(run()) }

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
