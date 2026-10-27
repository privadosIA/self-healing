package ports

import (
	"context"

	"github.com/privadosIA/self-healing/internal/core/model"
)

// Scanner reports the findings of a repository at a commit.
type Scanner interface {
	// Findings returns the open findings for the repository. An empty result is
	// not an error.
	Findings(ctx context.Context, repo model.Repo, sha string) ([]model.Finding, error)
}
