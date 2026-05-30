@story-code-intelligence:REFACTOR-GUIDE @phase-foundations @stage-cli-binary-skeleton @setup-inline
Feature: CLI binary skeleton
  The cleanc binary exposes a sub-command dispatcher with version,
  analyze, report, apply, and help verbs. Stage 1.1 wires the
  dispatcher shell, exit codes, and the version output format so
  downstream stages can compose on a stable CLI surface.

  Run via: make test-e2e
  (which calls: go test -tags e2e ./test/e2e/... -count=1)

  Scenario: version sub-command
    Given a built cleanc binary
    When the user runs cleanc version
    Then stdout includes "version="
    And stdout includes "parsers=[go,python,typescript,java]"
    And the exit code is 0

  Scenario: unknown sub-command
    Given a built cleanc binary
    When the user runs cleanc frobnicate with --out pointing to a temp file
    Then stderr includes "unknown sub-command"
    And the exit code is 64
    And stdout is empty
    And no output is emitted to the --out path

  Scenario: help on missing args
    Given a built cleanc binary
    When the user runs cleanc analyze with no path argument
    Then stderr prints the analyze usage block
    And the exit code is 64

  Scenario: makefile discovery
    Given a clean checkout
    When the developer runs make -C services/clean-code build
    Then services/clean-code/bin/cleanc exists and is executable