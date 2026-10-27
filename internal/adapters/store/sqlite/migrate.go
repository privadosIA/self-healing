package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; the number applied is kept in
// PRAGMA user_version. Never edit an entry that has shipped: append a new one.
var migrations = []string{
	`CREATE TABLE jobs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		delivery_id TEXT    NOT NULL UNIQUE,
		repo_id     INTEGER NOT NULL,
		commit_sha  TEXT    NOT NULL,
		rule_id     TEXT    NOT NULL DEFAULT '',
		status      TEXT    NOT NULL CHECK (status IN ('queued','running','skipped','failed','pr_opened')),
		pr_url      TEXT    NOT NULL DEFAULT '',
		tokens_in   INTEGER NOT NULL DEFAULT 0,
		tokens_out  INTEGER NOT NULL DEFAULT 0,
		error       TEXT    NOT NULL DEFAULT '',
		event       TEXT    NOT NULL,
		created_at  TEXT    NOT NULL,
		updated_at  TEXT    NOT NULL
	);
	CREATE INDEX jobs_status ON jobs(status);`,
	`CREATE TABLE reports (
		repo_id    INTEGER NOT NULL,
		commit_sha TEXT    NOT NULL,
		report     BLOB    NOT NULL,
		created_at TEXT    NOT NULL,
		updated_at TEXT    NOT NULL,
		PRIMARY KEY (repo_id, commit_sha)
	);
	CREATE INDEX reports_updated ON reports(updated_at);`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if err := applyMigration(ctx, db, i); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, i int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
		return err
	}
	// PRAGMA does not accept bound parameters; i is not user input.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return err
	}
	return tx.Commit()
}
