// Package migrations provides an embedded SQL migrator for the
// agent-memory schema. Migrations are applied in lexicographic order.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.sql
var sqlFiles embed.FS

// Migrator applies embedded SQL migrations against a database.
type Migrator struct {
	db *sql.DB
}

// New creates a Migrator for the given database connection.
func New(db *sql.DB) *Migrator {
	return &Migrator{db: db}
}

// Up applies all unapplied migrations in lexicographic order.
// Each migration runs inside a transaction. The migrator creates a
// schema_migrations table to track which migrations have been applied.
func (m *Migrator) Up(ctx context.Context) error {
	// Ensure the tracking table exists.
	if _, err := m.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// List all embedded SQL files.
	entries, err := fs.ReadDir(sqlFiles, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")

		// Check if already applied.
		var count int
		if err := m.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE version = $1", version,
		).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if count > 0 {
			continue
		}

		// Read and execute.
		content, err := sqlFiles.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}

		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec migration %s: %w", name, err)
		}

		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version) VALUES ($1)", version,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}

	return nil
}

// Has checks whether a specific migration version is embedded.
func (m *Migrator) Has(version string) bool {
	entries, err := fs.ReadDir(sqlFiles, ".")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.TrimSuffix(e.Name(), ".sql") == version {
			return true
		}
	}
	return false
}
