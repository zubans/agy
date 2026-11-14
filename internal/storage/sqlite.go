package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cli-ai-gateway/migrations"
	_ "modernc.org/sqlite"
)

// DB wraps the SQLite metadata database connection.
type DB struct {
	sql *sql.DB
}

// OpenSQLite opens (or creates) the SQLite metadata database, configures WAL mode and pragmas,
// applies embedded migrations, and marks any runs left in `running`/`queued` state as `interrupted` (Section 13.2).
func OpenSQLite(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db directory %q: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// SQLite handles concurrent writers cleanly with single writer connection + WAL.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)

	if err := sqldb.PingContext(ctx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	db := &DB{sql: sqldb}
	if err := db.migrate(ctx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := db.RecoverInterrupted(ctx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("recover interrupted runs: %w", err)
	}
	return db, nil
}

// Close closes the underlying SQLite handle.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// SQL exposes the underlying *sql.DB for internal storage/auth/session packages.
func (d *DB) SQL() *sql.DB {
	return d.sql
}

// Ping verifies database connectivity.
func (d *DB) Ping(ctx context.Context) error {
	var one int
	return d.sql.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var count int
		if err := d.sql.QueryRowContext(ctx, "SELECT COUNT(1) FROM schema_migrations WHERE filename = ?", name).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		raw, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return err
		}
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(raw)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (filename, applied_at) VALUES (?, ?)", name, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// RecoverInterrupted marks incomplete runs and in-progress responses left over from an abrupt restart
// as interrupted and clears stale session locks (Section 13.2).
func (d *DB) RecoverInterrupted(ctx context.Context) error {
	now := time.Now().Unix()
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE runs
		SET status = 'interrupted',
		    error_type = CASE WHEN error_type = '' THEN 'gateway_restarted' ELSE error_type END,
		    error_message = CASE WHEN error_message = '' THEN 'run interrupted by gateway restart' ELSE error_message END,
		    finished_at = COALESCE(finished_at, ?)
		WHERE status IN ('queued', 'running')
	`, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE responses
		SET status = 'interrupted',
		    error_code = CASE WHEN error_code = '' THEN 'gateway_restarted' ELSE error_code END,
		    error_message = CASE WHEN error_message = '' THEN 'response interrupted by gateway restart' ELSE error_message END,
		    completed_at = COALESCE(completed_at, ?)
		WHERE status = 'in_progress'
	`, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_run_id = '' WHERE active_run_id != ''`); err != nil {
		return err
	}

	// Reset active file refcounts on startup since no task can be running across restart.
	if _, err := tx.ExecContext(ctx, `UPDATE files SET active_refs = 0 WHERE active_refs != 0`); err != nil {
		return err
	}

	return tx.Commit()
}
