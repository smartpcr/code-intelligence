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

	// Verify at least 0022 was recorded in the canonical journal table.
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM "+JournalTable+" WHERE version = '0022'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("query %s: %v", JournalTable, err)
	}
	if count != 1 {
		t.Fatalf("expected migration 0022 to be recorded, got count=%d", count)
	}
}

// TestMigrations_0022_EdgeKindOverrides verifies that the
// 0022_edge_kind_overrides migration is embedded and reachable.
func TestMigrations_0022_EdgeKindOverrides(t *testing.T) {
	db := openTestDB(t)
	m := New(db)

	// Verify the migration is embedded via All().
	all, err := All()
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	found := false
	for _, mg := range all {
		if mg.Version == "0022" && mg.Name == "edge_kind_overrides" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("migration 0022_edge_kind_overrides is not embedded in the migrations package")
	}

	// Apply migrations so the enum value exists.
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Migrator.Up: %v", err)
	}

	// Verify the migration was recorded in the canonical journal table.
	var version string
	err = db.QueryRow(
		"SELECT version FROM "+JournalTable+" WHERE version = '0022'",
	).Scan(&version)
	if err != nil {
		t.Fatalf("0022 not found in %s: %v", JournalTable, err)
	}
	if version != "0022" {
		t.Fatalf("unexpected version: %s", version)
	}
}
