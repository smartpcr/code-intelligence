@story-code-intelligence:REFACTOR-GUIDE @phase-pipeline @stage-parse-and-recipe-fanout @setup-inline
Feature: Parse and recipe fan-out
  The Stage 2.2 orchestrator feeds every WalkedFile through the parser
  registry, fans the resulting AstFiles out to every applicable recipe,
  populates the scope-binding table, and surfaces parser panics as
  non-fatal WalkSkip rows rather than crashing.

  Scenario: parse all four languages
    Given a fixture repo with one file each of Go, Python, TypeScript, and Java
    When the orchestrator runs the parse stage
    Then four AstFile rows are collected
    And zero WalkSkip rows with reason "unsupported_language" are emitted

  Scenario: loc recipe lights up
    Given a fixture Go file of known line count
    When recipes run
    Then a MetricSampleDraft with MetricKind "loc" and the expected value is collected

  Scenario: dark cyclo recipe
    Given a fixture Go file with branches
    When recipes run against the branchy file
    Then zero MetricSampleDraft rows for metric_kind "cyclo" are emitted

  Scenario: scope binding populated
    Given a fixture Go file with a function "Foo"
    When parse and recipe fan-out completes
    Then the scope binding table contains a method binding whose Signature ends with "Foo" and whose StartLine and EndLine enclose the function body

  Scenario: parser panic is non-fatal
    Given a fixture file that triggers a panic in the parser stub
    When the orchestrator runs with the panicking parser
    Then a WalkSkip with reason "parser_panic" is emitted for the panicking file
    And the orchestrator exits cleanly
