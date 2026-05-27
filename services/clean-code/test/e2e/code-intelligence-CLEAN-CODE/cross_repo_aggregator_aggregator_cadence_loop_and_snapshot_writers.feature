@story-code-intelligence:CLEAN-CODE @phase-cross-repo-aggregator @stage-aggregator-cadence-loop-and-snapshot-writers @setup-inline
Feature: Aggregator cadence loop and snapshot writers

  Verify that the Cross-Repo Aggregator service's tick endpoint reads
  active metric_sample rows, writes per-repo snapshots into
  repo_metric_snapshot, and computes cross-repo percentiles into
  cross_repo_percentile with non-null statistics and histogram.
  Also verify that only the aggregator database role may INSERT into
  cross_repo_percentile.

  Background:
    Given a running Cross-Repo Aggregator connected to PostgreSQL

  Scenario: tick-writes-snapshots
    Given five repos with active metric_sample rows for "lcom4"
    When the aggregator tick endpoint is invoked
    Then repo_metric_snapshot has five rows for metric_kind "lcom4"
    And cross_repo_percentile has one row with non-null p50 p90 p99 histogram_json built_at

  Scenario: aggregator-is-sole-writer
    Given a non-aggregator database role
    When it attempts INSERT into cross_repo_percentile
    Then PostgreSQL returns permission denied
    When the aggregator role attempts INSERT into cross_repo_percentile
    Then the aggregator INSERT succeeds
