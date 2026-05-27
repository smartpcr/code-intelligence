@story-code-intelligence:CLEAN-CODE @phase-evaluator-surface-and-management-surface @stage-management-read-verbs-and-insights-projections @setup-compose
Feature: Management read verbs and insights projections
  Validates that management-surface read verbs return correct data from
  pre-materialised views: sha-pinned metric_sample reads return only active
  (non-retracted) rows, and cross-repo dashboard reads return pre-computed
  percentile snapshots without on-the-fly recomputation.

  Scenario: sha-pinned-returns-active-row
    Given two metric_sample rows exist for the same repo, commit SHA, file path, metric name, and scope with the older one retracted
    When the mgmt.read.metric_sample endpoint is called for that quintuple
    Then exactly one row is returned
    And the returned row is the active non-retracted sample
    And the retracted row is not present in the response

  Scenario: latest-dashboard-returns-snapshot
    Given a populated cross_repo_percentile row exists with p50, p90, p99, and histogram_json columns
    When the mgmt.read.cross_repo endpoint is called
    Then the response contains the p50 value from the materialised row
    And the response contains the p90 value from the materialised row
    And the response contains the p99 value from the materialised row
    And the response contains the histogram_json from the materialised row
    And no on-the-fly recompute query was executed