@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-predicate-dsl-evaluator @setup-compose
Feature: Predicate DSL evaluator
  Validates that the predicate DSL parser rejects unknown metric kinds
  and that evaluation is deterministic for the same predicate and
  MetricSample input.

  Scenario: dsl-rejects-unknown-metric-kind
    Given a predicate referencing metric_kind "lines_of_code"
    When the parser runs
    Then it returns a validation error naming the unknown metric_kind

  Scenario: dsl-deterministic
    Given the same predicate and the same MetricSample input
    When evaluated twice
    Then it returns the same boolean result