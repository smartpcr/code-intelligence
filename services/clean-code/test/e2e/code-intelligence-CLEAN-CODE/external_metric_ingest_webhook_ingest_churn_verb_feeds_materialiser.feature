@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-ingest-churn-verb-feeds-materialiser @setup-compose
Feature: Ingest churn verb feeds materialiser

  Validates that the churn ingest verb writes ZERO metric_sample rows
  directly (per implementation-plan Stage 4.4 / tech-spec Sec 4.1.1)
  and instead appends rows to the internal `churn_event` staging
  table that the `modification_count_in_window` materialiser
  consumes. The materialiser is the sole writer of that metric_kind.

  Scenario: churn-writes-no-metric-sample
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid churn webhook POST is sent for SHA "cccc0001"
    Then a scan_run row exists with kind "external_per_row" and status "succeeded"
    And the metric_sample row count is unchanged
    And churn_event rows are appended for the new scan_run

  Scenario: materialiser-consumes-churn
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    And churn_event rows exist for a scope
    When the modification_count_in_window materialiser runs
    Then a metric_sample row exists with metric_kind "modification_count_in_window"
    And the materialiser-emitted sample has pack "base" and source "computed"
