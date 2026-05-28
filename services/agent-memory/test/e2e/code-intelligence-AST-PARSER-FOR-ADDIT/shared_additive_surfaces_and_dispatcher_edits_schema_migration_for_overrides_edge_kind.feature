@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-shared-additive-surfaces-and-dispatcher-edits @stage-schema-migration-for-overrides-edge-kind @setup-compose
Feature: Schema migration for overrides edge kind

  Migration 0022_edge_kind_overrides.sql appends the `overrides`
  label to the closed-set `edge_kind` ENUM. The label is required
  by the Rust trait-method shadow rule (architecture §9 R4) and
  must apply cleanly on top of the existing 9-member set created
  in 0001_enums.sql.

  Scenario: Migration applies cleanly
    Given a fresh schema with migrations through "0021_concept_candidate.sql"
    When "0022_edge_kind_overrides.sql" is applied
    Then it returns no error and "SELECT 'overrides'::edge_kind" succeeds

  Scenario: Migration is idempotent on re-run check
    Given the migration runner skips already-applied migrations by filename
    When "0022_edge_kind_overrides.sql" runs once
    Then a second migration pass does not re-execute it