@story-code-intelligence:REPO-SCANNER @phase-codeintel-cli-binary @stage-cobra-scaffolding @setup-inline
Feature: Cobra scaffolding for the codeintel CLI

  The codeintel binary exposes five subcommands (scan, scan-many,
  diagram, serve, version) via cobra, rejects unknown commands
  with a non-zero exit, and honours the --log flag for output
  format.

  Scenario: cli-help-lists-subcommands
    Given a built codeintel root command
    When the user runs codeintel with "--help"
    Then the stdout output names the subcommand "scan"
    And the stdout output names the subcommand "scan-many"
    And the stdout output names the subcommand "diagram"
    And the stdout output names the subcommand "serve"
    And the stdout output names the subcommand "version"

  Scenario: unknown-subcommand-errors
    Given a built codeintel root command
    When the user runs codeintel with "bogus"
    Then the exit code is non-zero
    And the error output names the offending subcommand "bogus"

  Scenario: log-flag-respected
    Given a built codeintel root command
    When the user runs codeintel with "--log=json scan"
    Then the stderr contains a line that is valid JSON parseable by encoding/json
