@story-code-intelligence:CLEAN-CODE @phase-repo-indexer-and-metric-ingestor @stage-retraction-and-rescan-flow @setup-compose
Feature: Retraction and Rescan Flow

  Validates retraction and rescan lifecycle operations: retracting a sample
  appends a retraction row and records a scan_run, while rescanning enqueues
  a new full scan_run. Per tech-spec Sec 7.2 line 1248 REVOKE DELETE, the
  metric_sample_active pointer row must remain in place after retraction.

  Scenario: retract-appends-retraction-row
    Given a running metric ingestor connected to PostgreSQL
    And the database is migrated and seeded with an active sample
    When mgmt.retract_sample is invoked with reason "quality-regression"
    Then a metric_retraction row appears with reason "quality-regression"
    And a scan_run with kind "retract" and status "succeeded" is recorded
    And the metric_sample_active pointer row remains in place
    And SHA-pinned reader joins through metric_retraction correctly filter out the retracted sample

  Scenario: rescan-enqueues-scan-run
    Given a running metric ingestor connected to PostgreSQL
    And the database is migrated and seeded with an active sample
    When mgmt.rescan is invoked with the repo_id and sha
    Then a service-internal rescan request is logged for that repo and sha
    And a scan_run with kind "full" and status "running" is observable
    And no rescan_intent RepoEvent kind is emitted