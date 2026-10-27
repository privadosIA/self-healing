// Command healerd listens for failed CI pipelines and opens auto-fix pull requests.
package main

import (
	"fmt"
	"os"

	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/logging"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "healerd: invalid configuration:\n%v\n", err)
		os.Exit(1)
	}
	log, err := logging.New(os.Stderr, cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healerd: %v\n", err)
		os.Exit(1)
	}
	log.Info("configuration loaded", "listen_addr", cfg.ListenAddr, "workers", cfg.Workers)
	log.Warn("server not implemented yet")
}
