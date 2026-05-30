@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-cross-cutting-tests-documentation-validation @stage-documentation-support-matrix-update @setup-compose
Feature: Documentation support matrix update

  The documentation file .claude/context/tests.md must contain an
  up-to-date AST language support matrix showing CGO=on / CGO=off
  columns and must document the pwsh_not_available skip key used
  by PowerShell fixture tests.

  Scenario: tests.md has the support matrix
    Given the edited ".claude/context/tests.md"
    When a grep for "Language | CGO=on" is run against the file
    Then it matches a non-empty line in the new matrix

  Scenario: tests.md lists the pwsh-not-available skip key
    Given the edited ".claude/context/tests.md"
    When grep for "pwsh_not_available" runs
    Then it matches at least one line