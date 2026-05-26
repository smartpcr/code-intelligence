@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-ingest-defects-verb-store-only @setup-compose
Feature: Ingest defects verb store only

  Validates that the defects ingest verb stores the upload as a scan_run
  but does NOT write any metric_sample rows (store-only semantics).
  Also validates idempotent replay of the same payload.

  Scenario: defects-v1-writes-no-metric
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid defects webhook POST is sent for SHA "dddd0001"
    Then a scan_run row exists with kind "external_per_row" and status "succeeded"
    And the metric_sample row count is unchanged
    And no metric_kind "defect_density" row exists for that scan_run

  Scenario: defects-idempotent
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid defects webhook POST is sent for SHA "dddd0001"
    Then the response status code is 2xx and a scan_run_id is returned
    When the same defects payload is POSTed again with a valid signature
    Then the same scan_run_id is returned
    And no second scan_run row is appended for that payload hash