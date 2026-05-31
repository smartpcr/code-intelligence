@story-code-intelligence:REFACTOR-GUIDE @phase-p1-structured-prompt-emitter @stage-reserved-verbs-and-flags @setup-inline
Feature: Reserved verbs and flags
  The cleanc binary reserves the `apply` verb and several flags
  (`--telemetry-otlp`, `--with-churn`, `--snippet-cap-lines`) for future
  phases. Each must reject early with exit code 64 and emit the
  contract-mandated stderr message before any pipeline stage starts.

  Scenario: apply not implemented
    Given a built cleanc binary for reserved verbs
    When cleanc apply 00000000-0000-0000-0000-000000000000 runs
    Then the reserved exit code is 64
    And reserved stderr contains "pending operator pin cli-l7-authority"

  Scenario: telemetry flag rejected
    Given a built cleanc binary for reserved verbs
    And a fixture repo for reserved flags
    When cleanc analyze . --telemetry-otlp http://localhost:4317 runs
    Then the reserved exit code is 64
    And no --out or --findings file is created
    And reserved stderr contains "--telemetry-otlp is reserved for a future story"

  Scenario: churn flag rejected
    Given a built cleanc binary for reserved verbs
    And a fixture repo for reserved flags
    When cleanc analyze . --with-churn runs
    Then the reserved exit code is 64 BEFORE any pipeline stage starts
    And no --out or --findings file is created
    And reserved stderr contains "--with-churn is reserved for P2 and rejected in P0/P1"

  Scenario: snippet cap reserved
    Given a built cleanc binary for reserved verbs
    And a fixture repo for reserved flags
    When cleanc analyze . --snippet-cap-lines 100 runs
    Then the reserved exit code is 64 before any pipeline stage starts
    And reserved stderr contains "reserved for a future minor release"
