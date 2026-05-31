@story-code-intelligence:REFACTOR-GUIDE @phase-pipeline @stage-rule-engine-wiring @setup-inline
Feature: Rule engine wiring
  The orchestrator's Stage 2.3 engine stage seeds an InMemoryStore with
  a dev-mode policy bundle and metric samples, runs Engine.RunBatch,
  and surfaces findings, verdicts, and errors to the CLI composition root.

  Scenario: smoke run on fixture
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And a metric sample for scope "BigClass" kind "class" metric "loc" value 2000
    When the orchestrator runs the engine stage
    Then findings contain at least one entry with RuleID "solid.srp.loc_high" and Delta "new"

  Scenario: empty corpus
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And zero metric samples
    When the orchestrator runs the engine stage
    Then findings is empty
    And verdict is "pass"

  Scenario: store wiring uses plural insert
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And 3 metric samples for different scopes
    When LoadStore is called with the samples
    Then the store contains exactly 3 metric samples

  Scenario: engine error surfaces
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And a metric sample for scope "BigClass" kind "class" metric "loc" value 2000
    And a store whose AppendEvaluation returns error "injected append failure"
    When RunBatch executes against the failing store
    Then RunBatch returns an error containing "injected append failure"
    And the error maps to exit code 70
