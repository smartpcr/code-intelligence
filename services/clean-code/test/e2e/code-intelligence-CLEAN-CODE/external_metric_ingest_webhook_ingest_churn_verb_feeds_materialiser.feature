@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-ingest-churn-verb-feeds-materialiser @setup-compose
Feature: Ingest churn verb feeds materialiser
  Validates that the churn ingest verb writes only churn_event rows (no
  metric_sample), and that the modification_count materialiser later
  consumes those churn rows to emit the canonical metric sample.

  Scenario: churn-writes-no-metric-sample
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a churn upload is submitted for SHA "dddd0001" with files
      | file_path        | additions | deletions |
      | src/main.go      | 12        | 3         |
      | src/utils.go     | 5         | 0         |
    Then the verb returns HTTP 2xx
    And "SELECT COUNT(*) FROM metric_sample WHERE producer_run_id=$1" returns 0 for the producer run
    And churn_event rows are appended for every uploaded file

  Scenario: materialiser-consumes-churn
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    And scope bindings exist for churn files
      | file_path        |
      | src/main.go      |
      | src/utils.go     |
    When a churn upload is submitted for SHA "dddd0001" with files
      | file_path        | additions | deletions |
      | src/main.go      | 12        | 3         |
      | src/utils.go     | 5         | 0         |
    And the modification_count materialiser runs next
    Then it emits a metric_sample with metric_kind "modification_count_in_window" and pack "base" and source "computed"