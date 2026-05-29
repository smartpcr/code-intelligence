@story-code-intelligence:CLEAN-CODE @phase-cross-repo-aggregator @stage-insights-surface-percentile-freshness-banner @setup-inline
Feature: Insights Surface percentile freshness banner
  Validates that the management-surface cross-repo read endpoint surfaces a
  degraded banner when the pre-computed cross_repo_percentile row is stale,
  returns no banner when fresh, and that the eval.gate code-path never emits
  "percentile_stale" as a degraded reason.

  Background:
    Given a running management surface connected to PostgreSQL

  Scenario: stale-percentile-banner-on-insights
    Given a cross_repo_percentile row with built_at older than the freshness window
    When the mgmt.read.cross_repo endpoint is called
    Then the response envelope carries degraded equal to true
    And the response envelope carries degraded_reason equal to "percentile_stale"

  Scenario: fresh-percentile-no-banner
    Given a cross_repo_percentile row with built_at within the freshness window
    When the mgmt.read.cross_repo endpoint is called
    Then the response envelope carries degraded equal to false
    And the response envelope does not contain a degraded_reason field

  Scenario: gate-never-emits-percentile-stale
    Given an eval.gate service connected to PostgreSQL
    When eval.gate is called through every degraded code path
    Then none of the evaluation_verdict rows contain degraded_reason "percentile_stale"