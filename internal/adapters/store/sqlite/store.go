package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const (
	timeLayout = time.RFC3339Nano
	// maxErrorLen caps the stored error text, in bytes.
	maxErrorLen = 1000
)

// Store is a ports.JobStore backed by a SQLite file.
type Store struct {
	db    *sql.DB
	clock ports.Clock
}

var _ ports.JobStore = (*Store)(nil)

// Open opens (creating it if needed) the database at path and applies any
// pending schema migrations.
func Open(ctx context.Context, path string, clock ports.Clock) (*Store, error) {
	dsn := url.URL{
		Scheme:   "file",
		Opaque:   path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// One connection serializes writes and avoids SQLITE_BUSY; the load is a
	// handful of jobs per minute.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, clock: clock}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) now() string { return s.clock.Now().UTC().Format(timeLayout) }

// Enqueue inserts a queued job. A second insert for the same delivery id is a
// no-op and reports created=false.
func (s *Store) Enqueue(ctx context.Context, ev model.WebhookEvent) (bool, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return false, fmt.Errorf("encoding event: %w", err)
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (delivery_id, repo_id, commit_sha, status, event, created_at, updated_at)
		 VALUES (?, ?, ?, 'queued', ?, ?, ?)
		 ON CONFLICT(delivery_id) DO NOTHING`,
		ev.DeliveryID, ev.RepoID, ev.CommitSHA, string(payload), now, now)
	if err != nil {
		return false, fmt.Errorf("enqueue job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("enqueue job: %w", err)
	}
	return n == 1, nil
}

// Get returns the job of a delivery.
func (s *Store) Get(ctx context.Context, deliveryID string) (model.Job, error) {
	var (
		j                   model.Job
		event, created, upd string
		status              string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, rule_id, status, pr_url, tokens_in, tokens_out, error, event, created_at, updated_at
		 FROM jobs WHERE delivery_id = ?`, deliveryID).
		Scan(&j.ID, &j.RuleID, &status, &j.PRURL, &j.TokensIn, &j.TokensOut, &j.Error, &event, &created, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Job{}, ports.ErrNotFound
	}
	if err != nil {
		return model.Job{}, fmt.Errorf("get job: %w", err)
	}
	j.Status = model.JobStatus(status)
	if err := json.Unmarshal([]byte(event), &j.Event); err != nil {
		return model.Job{}, fmt.Errorf("decoding stored event: %w", err)
	}
	if j.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return model.Job{}, fmt.Errorf("decoding created_at: %w", err)
	}
	if j.UpdatedAt, err = time.Parse(timeLayout, upd); err != nil {
		return model.Job{}, fmt.Errorf("decoding updated_at: %w", err)
	}
	return j, nil
}

// Start moves a queued job to running.
func (s *Store) Start(ctx context.Context, deliveryID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'running', updated_at = ?
		 WHERE delivery_id = ? AND status = 'queued'`, s.now(), deliveryID)
	return s.checkTransition(ctx, res, err, deliveryID)
}

// Finish moves a running job to the terminal state in res.
func (s *Store) Finish(ctx context.Context, deliveryID string, res model.JobResult) error {
	if !res.Status.Terminal() {
		return fmt.Errorf("%w: %q is not a terminal state", ports.ErrInvalidTransition, res.Status)
	}
	r, err := s.db.ExecContext(ctx,
		`UPDATE jobs
		 SET status = ?, rule_id = ?, pr_url = ?, tokens_in = ?, tokens_out = ?, error = ?, updated_at = ?
		 WHERE delivery_id = ? AND status = 'running'`,
		string(res.Status), res.RuleID, res.PRURL, res.TokensIn, res.TokensOut,
		truncate(res.Error, maxErrorLen), s.now(), deliveryID)
	return s.checkTransition(ctx, r, err, deliveryID)
}

// interruptedMessage is stored on jobs that a restart cut short.
const interruptedMessage = "interrupted by a restart"

// FailInterrupted marks every running job as failed.
func (s *Store) FailInterrupted(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'failed', error = ?, updated_at = ? WHERE status = 'running'`,
		interruptedMessage, s.now())
	if err != nil {
		return 0, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	return int(n), nil
}

// checkTransition turns "no row updated" into ErrNotFound or
// ErrInvalidTransition depending on whether the job exists.
func (s *Store) checkTransition(ctx context.Context, res sql.Result, err error, deliveryID string) error {
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update job: %w", err)
	}
	if n == 1 {
		return nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE delivery_id = ?`, deliveryID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ports.ErrNotFound
	case err != nil:
		return fmt.Errorf("update job: %w", err)
	}
	return ports.ErrInvalidTransition
}

// truncate cuts s to at most max bytes without splitting a UTF-8 character.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
