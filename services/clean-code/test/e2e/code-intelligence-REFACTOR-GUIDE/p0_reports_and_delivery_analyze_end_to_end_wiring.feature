@story-code-intelligence:REFACTOR-GUIDE @phase-p0-reports-and-delivery @stage-analyze-end-to-end-wiring @setup-inline
Feature: Analyze end-to-end wiring
  The `cleanc analyze` command composes the full L1–L6 pipeline:
  walker, parser, recipes, rule engine, refactor planner, and report
  renderers. These scenarios verify the wiring produces the expected
  artifacts and exit codes for representative inputs.

  Scenario: happy path
    Given a built cleanc binary for analyze wiring
    And a fixture repo with one block-severity finding
    When cleanc analyze runs with --out report.md --findings findings.json --exit-on block
    Then report.md is written and is non-empty
    And findings.json is written and contains exactly 1 distinct block-severity rule violation
    And the analyze exit code is 1

  Scenario: walker error exit code
    Given a built cleanc binary for analyze wiring
    When cleanc analyze runs against a non-existent root path
    Then the analyze exit code is 2
    And analyze stderr contains "ErrRootNotFound"

  Scenario: invalid exit-on
    Given a built cleanc binary for analyze wiring
    When cleanc analyze runs with --exit-on critical
    Then the analyze exit code is 64
    And analyze stderr contains "--exit-on must be one of info, warn, block"
    And no pipeline stage started before the rejection

  Scenario: dev banner emitted
    Given a built cleanc binary for analyze wiring
    And a minimal fixture repo
    When cleanc analyze runs against the fixture repo
    Then analyze stderr begins with the exact C10 banner string

  Scenario: stdout default
    Given a built cleanc binary for analyze wiring
    And a minimal fixture repo
    When cleanc analyze runs against the fixture repo without --out
    Then markdown is written to stdout
    And the analyze exit code is 0 or 1
