package migrations

// This file previously contained `TestMigrator_Up_AppliesAll`
// and `TestMigrations_0022_EdgeKindOverrides`, written against
// the duplicate `Migrator` and `Has` helper that lived in
// migrator.go before that file was reduced to a comment-only
// stub. Those tests asserted against the wrong journal table
// (`schema_migrations` instead of the canonical
// `_schema_migrations`) and the wrong version format
// (`0022_edge_kind_overrides` instead of the canonical numeric
// prefix `0022`), so they could never pass against the
// production Migrator in migrate.go.
//
// Equivalent coverage for migration `0022_edge_kind_overrides`
// is preserved by:
//
//   - `migrate_test.go::TestAll_filenamesMatchPlannedSet`
//     pins `0022_edge_kind_overrides.sql` as an expected
//     embedded filename.
//   - `migrate_test.go::TestAll_parsesEveryEmbeddedFile`
//     pins `"0022"` as the parsed Migration.Version.
//   - `test_migrate_test.go` exercises the live Up / Down /
//     AppliedVersions round trip via the canonical Migrator
//     (skipped when AGENT_MEMORY_PG_URL is unset).
//   - The e2e suite in
//     `test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/
//     shared_additive_surfaces_and_dispatcher_edits_schema_
//     migration_for_overrides_edge_kind_test.go` validates
//     the same migration end-to-end against `_schema_migrations`.
