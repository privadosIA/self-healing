package ports

import (
	"context"
	"errors"
)

// ErrReportNotFound is returned when no report exists for a repository and commit.
var ErrReportNotFound = errors.New("report not found")

// ReportStore keeps scanner reports uploaded by CI, keyed by repository and
// commit. A newer upload for the same key replaces the previous one.
type ReportStore interface {
	// PutReport stores the raw report, replacing any existing one.
	PutReport(ctx context.Context, repoID int64, sha string, report []byte) error
	// GetReport returns the stored report, or ErrReportNotFound.
	GetReport(ctx context.Context, repoID int64, sha string) ([]byte, error)
}
