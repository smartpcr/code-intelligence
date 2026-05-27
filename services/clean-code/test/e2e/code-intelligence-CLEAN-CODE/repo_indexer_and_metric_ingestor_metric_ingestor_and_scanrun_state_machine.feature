@story-code-intelligence:CLEAN-CODE @phase-repo-indexer-and-metric-ingestor @stage-metric-ingestor-and-scanrun-state-machine @setup-compose
Feature: Metric Ingestor and ScanRun State Machine
  Validates the Metric Ingestor's commit scan_status state transitions and
  the ScanRun lifecycle, including the happy path (pending → scanning →
  scanned with a succeeded scan_run), the failure path (panic → failed),
  and the ScanRun kind enum guard that rejects invalid values before they
  reach PostgreSQL.

  Scenario: happy-path-states
    Given a running Metric Ingestor connected to PostgreSQL
    And the database is migrated and seeded with fixtures
    And a commit exists with scan_status "pending"
    When the Metric Ingestor processes the commit successfully
    Then the commit scan_status transitions through "pending, scanning, scanned"
    And a single scan_run with status "succeeded" is appended for that commit

  Scenario: failure-path-states
    Given a running Metric Ingestor connected to PostgreSQL
    And the database is migrated and seeded with fixtures
    And a commit exists with scan_status "pending"
    When the Metric Ingestor processes a recipe that panics
    Then the commit scan_status is "failed"
    And a single scan_run with status "failed" is appended for that commit

  Scenario: scan-run-kind-enum-rejects-invalid
    Given a running Metric Ingestor connected to PostgreSQL
    And the database is migrated and seeded with fixtures
    And a commit exists with scan_status "pending"
    When the ScanRun writer is asked to insert kind "external_double"
    Then it returns an HTTP 400 or 422 error rejecting the invalid kind
    And no scan_run row with kind "external_double" exists