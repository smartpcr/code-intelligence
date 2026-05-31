@story-code-intelligence:REPO-SCANNER @phase-codeintel-cli-binary @stage-scan-subcommand @setup-inline
Feature: codeintel scan subcommand
  The scan subcommand scans a single repository (local path or git URL),
  runs the AST dispatcher, and persists the resulting code-intelligence
  graph to a pluggable store backend.

  Scenario: scan-local-sqlite
    Given a small local fixture repo with a Go source file
    And a built codeintel binary
    When I run codeintel scan on the fixture with store "sqlite" and output "fixture.db"
    Then the output database file exists
    And the database contains at least 1 node of kind "repo"
    And the database contains at least 1 node of kind "package"
    And the database contains at least 1 node of kind "file"
    And the database contains at least 1 node of kind "class"
    And the database contains at least 1 node of kind "method"
    And the stdout summary lists non-zero counts for each kind

  Scenario: scan-url-with-sha
    Given a git repository served over HTTP with a known commit SHA
    And a built codeintel binary
    When I run codeintel scan with the git URL and SHA using store "sqlite" and output "remote.db"
    Then the output database file exists
    And the database contains at least 1 node of any kind

  Scenario: scan-coverage-degraded-exit-zero
    Given a fixture repo containing ".c" and ".py" source files
    And a codeintel binary built without CGO
    When I run the nocgo binary scan on the fixture with store "memory"
    Then the exit code is 0
    And the stdout summary reports skipped no_parser >= 1
    And stderr contains the per-extension skip count for ".c"

  Scenario: scan-fatal-io-exit-nonzero
    Given a small local fixture repo with a Go source file
    And a built codeintel binary
    When I run codeintel scan on the fixture with output in a nonexistent directory
    Then the exit code is non-zero
    And stderr names the IO error
