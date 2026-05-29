@story-code-intelligence:CLEAN-CODE
@phase-linked-mode-integration-and-rollout
@stage-load-and-conformance-tests
@setup-inline
Feature: Load and conformance tests

  Validates that metric_kind references throughout the repo use only
  canonical names (no non-canonical aliases), and that the evaluator
  gate meets the tech-spec Sec 8.3 SLO latency targets under sustained
  load.

  Scenario: canonical-names-conformance
    Given the conformance test running across the whole repo
    When it inventories metric_kind references
    Then no reference uses a non-canonical alias

  Scenario: load-target-met
    Given 100 repos at 50 scans/min for 30 minutes
    When k6 reports the p99 eval.gate latency
    Then it is below the tech-spec SLO targets of p99 2s and p95 800ms and p50 200ms