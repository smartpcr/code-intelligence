@story-code-intelligence:CLEAN-CODE @phase-linked-mode-integration-and-rollout @stage-cross-repo-end-to-end-happy-path @setup-inline
Feature: Cross repo end to end happy path

  End-to-end validation that the full cross-repo pipeline — repo
  registration, coverage upload, aggregator tick, management read,
  and evaluator gate — produces correct percentile data and
  canonical verdicts when data is fresh, and correctly flags
  staleness when the freshness window expires.

  Background:
    Given three registered repos with coverage uploads
    And one aggregator tick has completed

  Scenario: cross-repo-e2e-fresh
    When the e2e script asserts on the read paths
    Then the cross_repo response has populated percentile columns
    And the cross_repo response has degraded equal to false
    And eval.gate returns a canonical verdict

  Scenario: cross-repo-e2e-stale
    Given the fake clock is advanced past freshness_window_seconds
    When the e2e script asserts on the read paths
    Then mgmt.read.cross_repo carries percentile_stale
    And eval.gate never emits percentile_stale
