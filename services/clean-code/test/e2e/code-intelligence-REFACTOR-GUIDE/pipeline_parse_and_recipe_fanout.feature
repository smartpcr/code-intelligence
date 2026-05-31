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
    Given a fixture Go file with exactly 6 lines
    When recipes run
    Then a MetricSampleDraft with MetricKind "loc" and Value exactly 6 is collected

  Scenario: dark cyclo recipe
    Given a fixture Go file with branches
    When recipes run against the branchy file
    Then Recipe.AppliesTo returns false for the cyclo recipe on every parsed AstFile
    And zero MetricSampleDraft rows for metric_kind "cyclo" are emitted

  Scenario: scope binding populated
    Given a fixture Go file with a function "Foo" spanning lines 3 to 5
    When parse and recipe fan-out completes
    Then scopebinding.Table.Get(scopeIDFor("Foo")) returns a row whose Signature ends with "::Foo" and whose StartLine is 3 and EndLine is 5

  Scenario: parser panic is non-fatal
    Given a fixture where only "boom.go" triggers a panic in the parser stub and "clean.go" parses normally
    When the orchestrator runs with the selective panicking parser
    Then a WalkSkip with reason "parser_panic" is emitted for "boom.go"
    And "clean.go" appears in the parsed AstFile results
    And the orchestrator exits cleanly
