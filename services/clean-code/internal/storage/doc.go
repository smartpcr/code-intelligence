// Package storage provides shared helpers for the clean-code
// service's PostgreSQL access path: connection pooling, migration
// discovery, and the small assertion helpers tests use to verify
// migration round-trips land the `clean_code` schema in a
// well-known state.
//
// The package is intentionally thin in Stage 1.2 -- only the
// migration discovery helpers ship today. Later stages add the
// pgx-backed connection pool and the table-specific writer /
// reader interfaces that the service uses at runtime.
//
// The `clean_code` schema layout is owned by the SQL files under
// `services/clean-code/migrations/`; this package never embeds
// or duplicates that DDL. Tests apply the SQL files directly via
// `psql` (see `migrate_test.go`) so the schema the test exercises
// is byte-for-byte the schema the operator runs in production
// via `make migrate-up`.
package storage
