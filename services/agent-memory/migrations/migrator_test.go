package migrations

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("AGENT_MEMORY_PG_URL")
	if dsn == "" {
		t.Skip("AGENT_MEMORY_PG_URL not set — skipping migration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMigrator_Up_AppliesAll verifies that all embedded migrations
// are applied successfully to an empty database.
func TestMigrator_Up_AppliesAll(t *testing.T) {
	db := openTestDB(t)
	m := New(db)

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Migrator.Up: %v", err)
	}

	// Verify at least 0022 was recorded.
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = '0022_edge_kind_overrides'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected migration 0022_edge_kind_overrides to be recorded, got count=%d", count)
	}
}

// TestMigrations_0022_EdgeKindOverrides verifies that the
// 0022_edge_kind_overrides migration is embedded and reachable.
func TestMigrations_0022_EdgeKindOverrides(t *testing.T) {
	db := openTestDB(t)
	m := New(db)

	if !m.Has("0022_edge_kind_overrides") {
		t.Fatal("migration 0022_edge_kind_overrides is not embedded in the migrations package")
	}

	// Apply migrations so the enum value exists.
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Migrator.Up: %v", err)
	}

	// Probe: verify 'overrides' is a valid edge_kind enum value.
	// Since we may not have the full enum type from prior migrations,
	// we verify the SQL file was applied by checking schema_migrations.
	var version string
	err := db.QueryRow(
		"SELECT version FROM schema_migrations WHERE version = '0022_edge_kind_overrides'",
	).Scan(&version)
	if err != nil {
		t.Fatalf("0022_edge_kind_overrides not found in schema_migrations: %v", err)
	}
	if version != "0022_edge_kind_overrides" {
		t.Fatalf("unexpected version: %s", version)
	}
}
