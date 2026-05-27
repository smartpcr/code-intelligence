@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-ingest-churn-verb-feeds-materialiser @setup-compose
Feature: Ingest churn verb feeds materialiser

  Validates that the churn ingest verb stores the upload as a
  `clean_code.churn_event` row PER (file_path, sha) AND a
  parent `scan_run` row, but writes ZERO `metric_sample` rows
  directly. The `modification_count_in_window` materialiser
  is the ONLY canonical writer of churn-derived
  `metric_sample` rows; the verb's contract is to feed the
  materialiser via the staging table.

  This pins the Stage 4.4 / brief-Sec-6.4 invariant that
  the churn verb has NO `metric_sample` writer in its import
  graph -- the `ChurnVerbHandler` depends on a minimal
  `ChurnIngester` interface satisfied by `churn.Ingester`,
  which itself depends only on `churn.ChurnEventWriter`.

  The "materialiser actually runs" half of the handoff (a
  separate sweeper that SELECTs `churn_event` rows and emits
  `metric_sample` rows of metric_kind=`modification_count_in_window`)
  is owned by a downstream workstream. THIS feature pins the
  verb-side contract: the staged rows have the schema the
  materialiser will read.

  Scenario: churn-v1-writes-no-metric-sample-directly
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid churn webhook POST is sent for SHA "cccc000100000000000000000000000000000001"
    Then a scan_run row exists with kind "external_per_row" and status "succeeded"
    And the metric_sample row count is unchanged
    And one or more churn_event rows exist for that scan_run

  Scenario: churn-stages-rows-the-materialiser-can-consume
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid churn webhook POST is sent for SHA "cccc000200000000000000000000000000000002"
    Then one or more churn_event rows exist for that scan_run
    And the staged churn_event rows carry the materialiser shape

  Scenario: churn-idempotent-same-payload
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid churn webhook POST is sent for SHA "cccc000300000000000000000000000000000003"
    Then the response status code is 2xx and a scan_run_id is returned
    When the same churn payload is POSTed again with a valid signature
    Then the same scan_run_id is returned
    And no second scan_run row is appended for that payload hash
    And no duplicate churn_event rows are appended for the same (file_path, sha)
