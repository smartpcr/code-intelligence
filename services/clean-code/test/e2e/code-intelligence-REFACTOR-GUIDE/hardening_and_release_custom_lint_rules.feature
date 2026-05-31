@story-code-intelligence:REFACTOR-GUIDE @phase-hardening-and-release @stage-custom-lint-rules @setup-inline
Feature: Custom Lint Rules
  The CLI source tree enforces two custom lint rules via `make lint-cli`:
  `no-production-sql-import` forbids `database/sql` and `*_sql_store`
  imports in `cmd/cleanc/...` and `internal/cli/...`;
  `no-production-build-tag-bypass` forbids constructing
  `steward.PolicyVersion{Signature: nil}` without a `//go:build !prod`
  constraint in `internal/cli/devpolicy/`.

  Scenario: SQL import refused
    Given a test fixture file under internal/cli importing database/sql
    When make lint-cli runs
    Then it exits non-zero
    And stderr names the file and the "no-production-sql-import" rule

  Scenario: missing build tag refused
    Given a test fixture file under internal/cli/devpolicy constructing an unsigned PolicyVersion without a prod build tag
    When make lint-cli runs
    Then it exits non-zero
    And stderr names the file and the "no-production-build-tag-bypass" rule

  Scenario: clean tree passes
    Given the actual CLI source tree
    When make lint-cli runs
    Then it exits 0
