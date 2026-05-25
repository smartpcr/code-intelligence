@story-code-intelligence:CLEAN-CODE @phase-repo-indexer-and-metric-ingestor @stage-repo-indexer-and-commit-lifecycle @setup-compose
Feature: Repo Indexer and Commit Lifecycle

  Validates the canonical ScanStatus enum and the Repo Indexer's
  new-SHA processing path: inserting a commit row with
  scan_status='pending' and appending a repo_event(kind='registered').

  Scenario: commit-states-only-canonical
    Given the ScanStatus enum at compile time
    When we enumerate its values via AllScanStatuses
    Then exactly "pending, scanning, scanned, failed" are present
    And no value "complete" exists in the enum
    And no value "superseded" exists in the enum
    And no value "orphaned" exists in the enum

  Scenario: new-sha-inserts-pending
    Given a running Repo Indexer connected to PostgreSQL
    And the database is migrated and seeded
    When a webhook payload for a new SHA "abc0cafe1234" is processed
    Then a commit row appears with scan_status "pending"
    And a single repo_event with kind "registered" is appended for that commit