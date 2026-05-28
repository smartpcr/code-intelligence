-- 0022_edge_kind_overrides.sql
--
-- Stage 1.2 of the AST-PARSER-FOR-ADDIT story
-- (docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/
-- implementation-plan.md §"Stage 1.2: Schema migration for
-- overrides edge kind"): append a single new label `overrides`
-- to the closed-set `edge_kind` ENUM created in
-- 0001_enums.sql lines 28-38.
--
-- Architecture references
-- -----------------------
-- * §4.4 preface and §4.4 row for `migrations/0022_edge_kind_overrides.sql`
--   state this is the ONE schema change the story requires; every
--   other surface lands as additive Go code.
-- * §5.2.2 defines `Edge.kind` as a closed set; the closed-set
--   invariant in §5 requires that every new edge-kind value land
--   in its own dedicated, single-line migration so the diff is
--   auditable from the migrations tree alone.
-- * §7.2 walks the same-file Rust trait-impl scenario that emits
--   the new edge.
-- * §9 R4 -- the **Rust trait method shadow rule** (PINNED via
--   operator answer `rust-trait-overrides-edge`):
--
--     A Rust trait declaration with a default-bodied method
--     produces a MethodDecl on the trait class (Kind="trait")
--     AND, separately, an `impl Trait for Type` method that
--     shadows it on the Type class. When BOTH the trait
--     default-bodied method AND the impl-block method are
--     present in the SAME FILE, the dispatcher's new Pass 2d
--     emits an `overrides` edge from the impl method to the
--     trait method. Resolution key is
--       methodNodeID[traitName + "." + simpleName(implMethod)]
--     where `traitName = LangMeta["trait"]`. Cross-file
--     trait/impl pairs are deferred (A4 same-file resolution).
--     Same-name same-trait shadow collisions drop per A5.
--
-- Why a separate migration
-- ------------------------
-- 0001_enums.sql intentionally pins the original 9-member
-- `edge_kind` set. Modifying that file in place would erase the
-- closed-set audit trail; instead every new label lands as its
-- own additive migration (this one). The existing set
--   contains, imports, static_calls, observed_calls,
--   extends, implements, reads, writes, renamed_to
-- is preserved verbatim, and `overrides` takes the next
-- enumsortorder slot at the END of the type so existing rows
-- are byte-unchanged.
--
-- Why this is safe to ship by itself
-- ----------------------------------
-- `ALTER TYPE ... ADD VALUE` is additive in PostgreSQL:
--   * no table rewrite of `edge` (or any other edge_kind column),
--   * no index invalidation on `edge.kind`,
--   * pre-existing rows whose `kind` is one of the original 9
--     labels are unchanged,
--   * the new label gets the highest enumsortorder, matching the
--     "monotonically appended" guarantee the golden test
--     (`test_migrate_test.go::TestUp_appliesEntireStage12_andEveryExpectedObjectExists`)
--     pins via its ordered slice for `edge_kind`.
-- PostgreSQL 12+ permits `ALTER TYPE ... ADD VALUE` inside a
-- transaction block when the new value is not referenced in the
-- same transaction; the Migrator never references the value
-- itself, so the in-file BEGIN/COMMIT (kept for psql -f
-- ergonomics) is benign.

-- migrate:up
BEGIN;

ALTER TYPE edge_kind ADD VALUE 'overrides';

COMMIT;

-- migrate:down
BEGIN;

-- PostgreSQL has no `ALTER TYPE ... DROP VALUE` form, and direct
-- `DELETE FROM pg_enum` requires `allow_system_table_mods=on`
-- which is intentionally off on production clusters. Reverting
-- this migration is therefore a logical no-op at this layer:
-- the Migrator drops the journal row regardless, and the
-- round-trip test
-- (`test_migrate_test.go::TestRoundTrip_schemaIsByteIdenticalAfterDownUp`)
-- relies on the reverse-order Down pass reaching migration
-- 0001's `DROP TYPE edge_kind` to clear the entire enum. The
-- subsequent Up re-adds `overrides` via this migration's up
-- block, producing a byte-identical schema fingerprint.
-- Operationally, edge-kind retraction is not a supported
-- workflow: a future "deprecate" migration would document the
-- label as unused rather than attempt to remove it from the
-- closed set.
SELECT 1;

COMMIT;
