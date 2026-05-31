@story-code-intelligence:REFACTOR-GUIDE @phase-p0-reports-and-delivery @stage-p0-fixture-corpus-and-golden-snapshots @setup-inline
Feature: P0 fixture corpus and golden snapshots
  The checked-in fixture corpus under testdata/fixtures/ and the golden
  snapshots under testdata/golden/ are the byte-stable reference outputs
  for the analyze pipeline. These scenarios verify that the orchestrator
  pipeline reproduces those golden files exactly and that key structural
  signals (cycle findings, break_cycle tasks) are present.

  Scenario: golden match Go corpus
    Given the Go fixture is loaded
    When runAnalyze runs against the Go fixture
    Then report.md byte-matches the golden file for "p0-go-cycle"
    And findings.json byte-matches the golden file for "p0-go-cycle"

  Scenario: cycle detected
    Given the Go fixture is loaded
    When runAnalyze runs against the Go fixture
    Then findings.json Findings contains at least one row with RuleID matching "decoupling.cycle_member"
    And at least one RefactorTask has Kind "break_cycle"

  Scenario: cross-language coverage
    Given the four-language fixture set is loaded
    When runAnalyze runs sequentially per language
    Then each language's report.md byte-matches its golden file

  Scenario: deterministic re-run
    Given the Go fixture is loaded
    When runAnalyze runs twice back-to-back
    Then both runs produce byte-identical report.md and findings.json outputs
