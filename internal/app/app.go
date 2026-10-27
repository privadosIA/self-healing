// Package app assembles the daemon from its parts: it builds the adapters from
// the configuration, wires them into the use case and the worker pool, and
// serves the HTTP endpoints.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/privadosIA/self-healing/internal/adapters/gitea"
	"github.com/privadosIA/self-healing/internal/adapters/llm/openai"
	"github.com/privadosIA/self-healing/internal/adapters/sonar"
	"github.com/privadosIA/self-healing/internal/adapters/store/sqlite"
	"github.com/privadosIA/self-healing/internal/adapters/trivy"
	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/core"
	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/listener"
	"github.com/privadosIA/self-healing/internal/ports"
	"github.com/privadosIA/self-healing/internal/safehttp"
)

const (
	apiTimeout = 30 * time.Second
	// maxWebhookBytes bounds a webhook body; Gitea payloads are a few kilobytes.
	maxWebhookBytes = 4 << 20
	httpShutdown    = 10 * time.Second
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// App is the assembled daemon.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	store   *sqlite.Store
	pool    *core.Pool
	metrics *listener.Metrics
	handler http.Handler
}

// New builds the daemon. It opens the database and marks jobs that a previous
// run left running as failed, but it contacts no remote system.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	store, err := sqlite.Open(ctx, cfg.DBPath, systemClock{})
	if err != nil {
		return nil, err
	}
	a, err := assemble(ctx, cfg, log, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return a, nil
}

func assemble(ctx context.Context, cfg config.Config, log *slog.Logger, store *sqlite.Store) (*App, error) {
	if n, err := store.FailInterrupted(ctx); err != nil {
		return nil, err
	} else if n > 0 {
		log.Warn("jobs interrupted by the previous run were marked failed", "count", n)
	}

	// Every outbound HTTP client enforces the allowlist; git is checked by the
	// Gitea adapter, which owns the clone URL.
	apiClient := safehttp.NewClient(cfg.AllowedHosts, apiTimeout)

	var scanners []ports.Scanner
	if cfg.SonarURL != "" {
		s, err := sonar.New(sonar.Config{BaseURL: cfg.SonarURL, Token: cfg.SonarToken.Reveal(), Client: apiClient})
		if err != nil {
			return nil, err
		}
		scanners = append(scanners, s)
	}
	scanners = append(scanners, trivy.NewScanner(store))

	llm, err := openai.NewClient(openai.Config{
		BaseURL: cfg.LLMURL, APIKey: cfg.LLMAPIKey.Reveal(), Model: cfg.LLMModel,
		Client: safehttp.NewClient(cfg.AllowedHosts, cfg.LLMTimeout),
	})
	if err != nil {
		return nil, err
	}
	vcs, err := gitea.New(gitea.Config{
		BaseURL: cfg.GiteaURL, Token: cfg.GiteaToken.Reveal(), Allow: cfg.AllowedHosts,
		AuthorName: cfg.GitAuthorName, AuthorEmail: cfg.GitAuthorEmail, HTTP: apiClient,
	})
	if err != nil {
		return nil, err
	}
	selector, err := core.NewSelector(cfg.ExcludeGlobs)
	if err != nil {
		return nil, err
	}

	metrics := listener.NewMetrics()
	healer := &core.Healer{
		Store: store, Scanners: scanners, VCS: vcs, LLM: llm, Selector: selector, Log: log,
		Config: core.HealerConfig{
			MaxOpenPRs: cfg.MaxOpenPRs, ContextLines: cfg.SnippetContext, Model: cfg.LLMModel,
			GiteaURL: cfg.GiteaURL, SonarURL: cfg.SonarURL,
		},
	}
	pool, err := core.NewPool(core.PoolConfig{
		Runner: healer, Source: store, Workers: cfg.Workers, JobTimeout: cfg.JobTimeout, Grace: cfg.ShutdownGrace,
		OnResult: func(r model.JobResult) { metrics.JobFinished(r.Status) }, Log: log,
	})
	if err != nil {
		return nil, err
	}

	handler, err := routes(cfg, log, store, &core.Intake{Store: store, Pool: pool, Log: log}, metrics)
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, log: log, store: store, pool: pool, metrics: metrics, handler: handler}, nil
}

// routes builds the HTTP surface. The webhook is behind the HMAC check; the
// report upload exists only when a token is configured.
func routes(cfg config.Config, log *slog.Logger, reports ports.ReportStore, intake ports.JobEnqueuer, metrics *listener.Metrics) (http.Handler, error) {
	mux := http.NewServeMux()

	signed, err := listener.RequireSignature([]byte(cfg.WebhookSecret.Reveal()), maxWebhookBytes, log)
	if err != nil {
		return nil, err
	}
	mux.Handle("POST /webhook", signed(listener.NewWebhookHandler(intake, log, metrics)))
	mux.Handle("GET /healthz", listener.HealthHandler())
	mux.Handle("GET /metrics", metrics.Handler())

	if token := cfg.ReportsToken.Reveal(); token != "" {
		validate := func(r io.Reader) error { _, err := trivy.Parse(r); return err }
		h, err := listener.NewReportHandler(reports, validate, token, listener.DefaultMaxReportBytes, log, metrics)
		if err != nil {
			return nil, err
		}
		mux.Handle(listener.ReportsRoute, h)
	}
	return mux, nil
}

// Handler returns the HTTP handler, for tests.
func (a *App) Handler() http.Handler { return a.handler }

// Run listens on the configured address and serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", a.cfg.ListenAddr, err)
	}
	return a.Serve(ctx, ln)
}

// Serve serves on ln and runs the worker pool until ctx is cancelled, then
// shuts down in order: stop taking requests, let running jobs finish for the
// grace period, close the database.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	defer func() { _ = a.store.Close() }()

	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // report uploads can be large
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	poolDone := make(chan struct{})
	go func() { defer close(poolDone); a.pool.Run(runCtx) }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	a.log.Info("healerd is listening", "addr", ln.Addr().String(), "workers", a.cfg.Workers)

	var result error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = fmt.Errorf("http server: %w", err)
		}
	}

	a.log.Info("shutting down")
	sctx, stop := context.WithTimeout(context.WithoutCancel(ctx), httpShutdown)
	defer stop()
	if err := srv.Shutdown(sctx); err != nil && result == nil {
		result = fmt.Errorf("stopping the http server: %w", err)
	}
	cancel()
	<-poolDone
	a.log.Info("stopped")
	return result
}
