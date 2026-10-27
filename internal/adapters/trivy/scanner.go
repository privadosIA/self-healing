package trivy

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

// Scanner is a ports.Scanner that reads the Trivy report CI uploaded for a
// commit. Trivy has no server API, so the report is pushed, not pulled.
type Scanner struct {
	reports ports.ReportStore
}

var _ ports.Scanner = (*Scanner)(nil)

// NewScanner returns a Scanner reading from reports.
func NewScanner(reports ports.ReportStore) *Scanner {
	return &Scanner{reports: reports}
}

// Findings returns the vulnerabilities in the report stored for repo and sha.
// When no report was uploaded the error wraps ports.ErrReportNotFound, so a
// caller can tell "not uploaded yet" from "uploaded and clean" (an empty
// result with no error).
func (s *Scanner) Findings(ctx context.Context, repo model.Repo, sha string) ([]model.Finding, error) {
	raw, err := s.reports.GetReport(ctx, repo.ID, strings.ToLower(sha))
	if err != nil {
		return nil, fmt.Errorf("trivy: loading report: %w", err)
	}
	findings, err := Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("trivy: stored report is unreadable: %w", err)
	}
	return findings, nil
}
