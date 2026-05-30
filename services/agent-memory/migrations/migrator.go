// Package migrations provides an embedded SQL migrator for the
// agent-memory schema. Migrations are applied in lexicographic order.
//
// The canonical Migrator, New(), Up(), Down(), and journal table
// (_schema_migrations) live in migrate.go. This file is intentionally
// kept empty to avoid introducing a duplicate migration system.
package migrations
