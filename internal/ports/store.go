package ports

import (
	"context"
	"errors"
	"time"

	"github.com/privadosIA/self-healing/internal/core/model"
)

var (
	// ErrNotFound is returned when no job matches the delivery id.
	ErrNotFound = errors.New("job not found")
	// ErrInvalidTransition is returned when a job is not in a state that
	// allows the requested change.
	ErrInvalidTransition = errors.New("invalid job state transition")
)

// JobEnqueuer is the part of JobStore that the webhook listener needs.
type JobEnqueuer interface {
	// Enqueue records a queued job for the event. It returns created=false,
	// without an error, when a job for the same delivery already exists.
	Enqueue(ctx context.Context, ev model.WebhookEvent) (created bool, err error)
}

// JobStore persists fix jobs. The lifecycle is queued -> running -> one
// terminal state (skipped, failed or pr_opened).
type JobStore interface {
	JobEnqueuer

	// Get returns the job of a delivery, or ErrNotFound.
	Get(ctx context.Context, deliveryID string) (model.Job, error)
	// Start moves a queued job to running.
	Start(ctx context.Context, deliveryID string) error
	// Finish moves a running job to the terminal state in res.
	Finish(ctx context.Context, deliveryID string, res model.JobResult) error
	// ListQueued returns up to limit queued jobs, oldest first. The worker pool
	// uses it to pick up jobs that survived a restart or did not fit in memory.
	ListQueued(ctx context.Context, limit int) ([]model.Job, error)
	// FailInterrupted marks every running job as failed and returns how many
	// there were. Call it once at startup: a job still running then was cut
	// short by a crash, and it may or may not have opened a pull request.
	// Queued jobs are left untouched so a worker can pick them up.
	FailInterrupted(ctx context.Context) (int, error)
}

// Clock abstracts time so tests are deterministic.
type Clock interface {
	Now() time.Time
}
