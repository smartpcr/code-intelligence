@story-code-intelligence:CLEAN-CODE
@phase-audit-wal-and-reliability-hardening
@stage-ast-subprocess-isolation-and-mode-flip-safety
@setup-compose
Feature: AST subprocess isolation and mode flip safety

  Validates that parser subprocesses are properly isolated so that OOM
  conditions do not crash the host, and that mode flips drain in-flight
  scans before switching.

  Scenario: subprocess-oom-returns-error
    Given a parser subprocess that allocates beyond its rlimit
    When the parse runs
    Then the host process receives a typed "ParserOOM" error and the host remains running

  Scenario: mode-flip-drains-scans
    Given two in-flight scans for repo_id "r1"
    When mgmt set_mode is called with repo_id "r1" and mode "linked"
    Then both scans complete under the old mode and the next scan starts under "linked"