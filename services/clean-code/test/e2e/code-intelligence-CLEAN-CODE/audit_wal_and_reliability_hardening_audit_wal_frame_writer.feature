@story-code-intelligence:CLEAN-CODE @phase-audit-wal-and-reliability-hardening @stage-audit-wal-frame-writer @setup-compose
Feature: Audit WAL frame writer scope and schema constraints
  The audit WAL writer lives in internal/audit/wal and must only
  be consumed by the evaluator and rule-engine subsystems. No
  legacy projection tables (audit_event, audit_anchor) may exist
  in the clean_code schema — audit semantics are carried exclusively
  by evaluation_run, evaluation_verdict, and finding.

  Scenario: wal-scope-only-audit-tables
    Given any code path in the service
    When grepping the writer call sites
    Then "internal/audit/wal" is referenced only from "internal/evaluator/" and "internal/rule_engine/"

  Scenario: no-projection-table
    Given the database schema is available
    When listing tables in the "clean_code" schema
    Then no tables named "audit_event" or "audit_anchor" exist
    And tables "evaluation_run", "evaluation_verdict", "finding" carry audit semantics