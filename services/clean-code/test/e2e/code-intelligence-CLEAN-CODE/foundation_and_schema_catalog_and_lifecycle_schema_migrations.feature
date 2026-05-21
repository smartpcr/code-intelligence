@story-code-intelligence:CLEAN-CODE @phase-foundation-and-schema @stage-catalog-and-lifecycle-schema-migrations @setup-inline
Feature: Catalog and Lifecycle schema migrations

  Verify that the PostgreSQL migrations for the clean_code schema
  create and tear down correctly via the Makefile lifecycle interface,
  and that ENUM constraints reject invalid values while honouring
  column defaults.

  Scenario: catalog-up-down
    Given an empty PostgreSQL 16 instance
    When "make migrate-up" runs
    And "make migrate-down" runs
    Then both migrations succeed
    And listing tables in clean_code returns zero rows

  Scenario: scan-status-enum-rejects-invalid
    Given the commit table exists after migrate-up
    When an INSERT supplies scan_status 'garbage'
    Then PostgreSQL rejects it with an enum constraint violation
    When an INSERT supplies scan_status 'complete'
    Then PostgreSQL rejects it with an enum constraint violation

  Scenario: scan-status-default-pending
    Given the commit table exists after migrate-up
    When a commit row is inserted without a scan_status value
    Then the row materialises with scan_status 'pending'

  Scenario: scan-run-status-enum
    Given the scan_run table exists after migrate-up
    When an INSERT supplies scan_run status 'orphaned'
    Then PostgreSQL rejects the scan_run insert with an enum constraint violation
    When an INSERT supplies scan_run status 'complete'
    Then PostgreSQL rejects the scan_run insert with an enum constraint violation