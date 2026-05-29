-- 0022_edge_kind_overrides.sql
--
-- AST-PARSER-FOR-ADDIT Stage 1.2 (architecture §9 R4).
--
-- Append the `overrides` label to the `edge_kind` ENUM so Pass 2d
-- can store trait→impl method-shadow relationships (the Rust
-- "default method body, then `impl Trait for Type` overrides it"
-- shape) as first-class graph edges rather than spilling into
-- `attrs_json`. `ALTER TYPE ... ADD VALUE IF NOT EXISTS` is
-- idempotent in PostgreSQL ≥10 and ordered last by
-- pg_enum.enumsortorder, which `test_migrate_test.go`'s
-- wantEnums["edge_kind"] pins as the canonical tail position.

-- migrate:up
ALTER TYPE edge_kind ADD VALUE IF NOT EXISTS 'overrides';

-- migrate:down
-- ALTER TYPE ... DROP VALUE is intentionally unsupported by
-- PostgreSQL (an in-place removal would silently invalidate
-- any existing edge row that already carries the value),
-- so this migration's logical rollback would require a full
-- enum rebuild plus column rewrites on every table that
-- references `edge_kind`. We deliberately do NOT do that here:
-- 0022 is additive and treated as monotonic. The Down body is
-- a no-op DO block so the round-trip parser still sees a
-- non-empty `-- migrate:down` section (migrate_test.go's
-- TestAll_parsesEveryEmbeddedFile asserts both halves exist)
-- while making it explicit that reverting this migration
-- requires operator-driven schema recovery, not automated
-- migrator action.
DO $$
BEGIN
    RAISE NOTICE '0022_edge_kind_overrides: down is a no-op; enum value removal requires operator-driven schema recovery';
END$$;
