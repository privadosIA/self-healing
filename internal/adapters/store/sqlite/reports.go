package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/privadosIA/self-healing/internal/ports"
)

// maxStoredReports bounds the table: past it, the least recently updated
// reports are deleted. A report is only useful while its pipeline is being
// healed, so old ones have no value.
const maxStoredReports = 500

// PutReport stores the report for a repository and commit, replacing an earlier one,
// and prunes the oldest reports beyond the limit.
func (s *Store) PutReport(ctx context.Context, repoID int64, sha string, report []byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store report: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO reports (repo_id, commit_sha, report, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id, commit_sha) DO UPDATE SET report = excluded.report, updated_at = excluded.updated_at`,
		repoID, sha, report, now, now); err != nil {
		return fmt.Errorf("store report: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM reports WHERE (repo_id, commit_sha) NOT IN
		 (SELECT repo_id, commit_sha FROM reports ORDER BY updated_at DESC, rowid DESC LIMIT ?)`,
		s.maxReports); err != nil {
		return fmt.Errorf("prune reports: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store report: %w", err)
	}
	return nil
}

// GetReport returns the stored report.
func (s *Store) GetReport(ctx context.Context, repoID int64, sha string) ([]byte, error) {
	var report []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT report FROM reports WHERE repo_id = ? AND commit_sha = ?`, repoID, sha).Scan(&report)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ports.ErrReportNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get report: %w", err)
	}
	return report, nil
}
