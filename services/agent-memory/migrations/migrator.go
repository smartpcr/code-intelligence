package migrations

// The authoritative production Migrator (with the rich Up /
// Down / Reset / AppliedVersions surface and the canonical
// `_schema_migrations` journal table) lives in migrate.go.
//
// An earlier sibling-PR story stage briefly introduced a
// stripped-down duplicate of `Migrator`, `New`, `Up`, and a
// `Has` helper in this file. The duplicate declared a different
// journal table (`schema_migrations` without the leading
// underscore) and stored full filenames (e.g.
// `0022_edge_kind_overrides`) as versions, which conflicted
// with migrate.go's canonical schema (`_schema_migrations`,
// numeric `0022`-style versions extracted via
// `versionRe = ^(\d+[a-z]?)_(.+)\.sql$`) and with every
// production caller and the e2e test
// `test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/
// shared_additive_surfaces_and_dispatcher_edits_schema_
// migration_for_overrides_edge_kind_test.go`. The duplicates
// were removed here to restore a compilable package. New
// migration helpers belong alongside `Migrator` in migrate.go.
