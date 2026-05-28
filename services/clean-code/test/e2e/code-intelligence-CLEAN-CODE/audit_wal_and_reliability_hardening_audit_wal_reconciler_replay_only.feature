@story-code-intelligence:CLEAN-CODE
@phase-audit-wal-and-reliability-hardening
@stage-audit-wal-reconciler-replay-only
@setup-compose
Feature: Audit WAL Reconciler replay only

  The WAL reconciler replays missing rows from WAL frames into the three
  Audit tables (evaluation_run, evaluation_verdict, finding) and preserves
  the original caller identity. Its database role is restricted to Audit
  tables only.

  Scenario: reconciler-replays-missing-rows
    Given a WAL frame for a row missing from "finding"
    When the reconciler runs
    Then the row is INSERTed into "finding" and reads return it

  Scenario: reconciler-preserves-caller
    Given a WAL frame for "evaluation_run" with caller "ci-bot"
    When the reconciler replays after restart
    Then the replayed row's caller column equals "ci-bot"

  Scenario: reconciler-cannot-write-non-audit
    Given the "clean_code_wal_reconciler" role bound to a session
    When the reconciler attempts INSERT into "metric_sample"
    Then PostgreSQL returns permission denied
    And INSERT into "evaluation_run" succeeds
    And INSERT into "evaluation_verdict" succeeds
    And INSERT into "finding" succeeds