@story-code-intelligence:REFACTOR-GUIDE @phase-hardening-and-release @stage-end-to-end-golden-tests @setup-inline
Feature: End-to-end golden tests for cleanc CLI

  Each scenario runs the checked-in run.sh script for the given
  scenario directory, then asserts acceptance conditions against
  the artifacts that run.sh produces.

  Scenario: Go cycle e2e
    Given the scenario directory "tests/e2e/cleanc/scenarios/p0-go-cycle"
    And the cleanc dev binary is built
    When run.sh executes for the scenario
    Then exit code matches the scenario expected_exit_code
    And the artifact "report.md" byte-matches the golden file for "p0-go-cycle"

  Scenario: mixed langs e2e
    Given the scenario directory "tests/e2e/cleanc/scenarios/p0-mixed-langs"
    And the cleanc dev binary is built
    When run.sh executes for the scenario
    Then findings.json lists exactly four Files entries with distinct language values

  Scenario: prompt emission e2e
    Given the scenario directory "tests/e2e/cleanc/scenarios/p1-prompts"
    And the cleanc dev binary is built
    When run.sh executes for the scenario
    Then prompts.jsonl line count equals the scenario expected_task_count
    And every prompts.jsonl line is valid JSON with prompt_format_version "v1.2026.05"

  Scenario Outline: exit codes matrix
    Given the scenario directory "tests/e2e/cleanc/scenarios/exit-codes/<sub_case>"
    And the cleanc dev binary is built
    When run.sh executes for the scenario
    Then the observed exit code equals <expected_code>

    Examples:
      | sub_case            | expected_code |
      | clean-run           | 0             |
      | severity-trigger    | 1             |
      | missing-root        | 2             |
      | invalid-flag        | 64            |
      | reserved-apply      | 64            |
      | reserved-telemetry  | 64            |
      | reserved-churn      | 64            |
      | injected-engine-error | 70          |
