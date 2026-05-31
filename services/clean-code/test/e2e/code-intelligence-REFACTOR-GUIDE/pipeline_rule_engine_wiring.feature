@story-code-intelligence:REFACTOR-GUIDE @phase-pipeline @stage-rule-engine-wiring @setup-inline
Feature: Rule engine wiring
  The orchestrator's Stage 2.3 engine stage converts a fixture repo into
  metric samples via the full walk-parse-recipe pipeline, seeds an
  InMemoryStore with a dev-mode policy bundle, runs Engine.RunBatch,
  and surfaces findings, verdicts, exit codes, and errors through the
  composition root.

  Scenario: smoke run on fixture
    Given a fixture repo with a 2000-line Go file "big.go"
    And a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    When the engine stage pipeline runs on the fixture
    Then findings contain at least one entry with RuleID "solid.srp.loc_high" and Delta "new"

  Scenario: empty corpus
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And a fixture repo with zero source files
    When the engine stage pipeline runs on the fixture
    Then exit code is 0
    And findings is empty
    And verdict is "pass"

  Scenario: store wiring uses plural insert
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And 3 metric samples for different scopes
    And a spy store recording InsertSamples calls
    When the engine stage loads and runs with the spy store
    Then InsertSamples was called exactly 1 time with 3 samples

  Scenario: engine error surfaces
    Given a dev-mode bundle with rule "solid.srp.loc_high" predicate "metric_kind == 'loc' AND value >= 1500" severity "block"
    And a metric sample for scope "BigClass" kind "class" metric "loc" value 2000
    And a store whose AppendEvaluation returns error "injected append failure"
    When the engine stage runs as the composition root with the failing store
    Then exit code is 70
    And stderr contains "injected append failure"
