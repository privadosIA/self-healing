package ports

import (
	"context"

	"github.com/privadosIA/self-healing/internal/core/model"
)

// JobStore persists fix jobs.
type JobStore interface {
	// Enqueue records a queued job for the event. It returns created=false,
	// without an error, when a job for the same delivery already exists.
	Enqueue(ctx context.Context, ev model.WebhookEvent) (created bool, err error)
}
