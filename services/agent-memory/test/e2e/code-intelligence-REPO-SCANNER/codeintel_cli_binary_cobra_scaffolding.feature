@story-code-intelligence:REPO-SCANNER @phase-codeintel-cli-binary @stage-cobra-scaffolding @setup-inline
Feature: Cobra scaffolding for the codeintel CLI

  The codeintel binary exposes five subcommands (scan, scan-many,
  diagram, serve, version) via cobra, rejects unknown commands
  with a non-zero exit, and honours the --log flag for output
  format.

  Scenario: cli-help-lists-subcommands
    Given a built codeintel binary
    When codeintel runs with "--help"
    Then stdout names the subcommand "scan"
    And stdout names the subcommand "scan-many"
    And stdout names the subcommand "diagram"
    And stdout names the subcommand "serve"
    And stdout names the subcommand "version"

  Scenario: unknown-subcommand-errors
    Given a built codeintel binary
    When codeintel runs with "bogus"
    Then the exit code is non-zero
    And stderr names the offending subcommand "bogus"

  Scenario: log-flag-respected
    Given a built codeintel binary
    When codeintel runs with "--log=json scan"
    Then stderr contains a line that is valid JSON parseable by encoding/json
