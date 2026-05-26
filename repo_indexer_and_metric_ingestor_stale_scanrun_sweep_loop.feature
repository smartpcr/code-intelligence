@story-code-intelligence:CLEAN-CODE @phase-repo-indexer-and-metric-ingestor @stage-stale-scanrun-sweep-loop @setup-compose
Feature: Stale ScanRun sweep loop

  The indexer service periodically sweeps scan_runs that have been stuck in
  "running" status for longer than the configured timeout (30 minutes).
  Stale runs transition to "failed" and any commits still in "scanning"
  that belonged to a failed run also transition to "failed".

  Scenario: stale-scan-run-becomes-failed
    Given a scan_run row with status "running" whose updated_at is older than 30 minutes
    When the sweep loop executes
    Then the scan_run row transitions to status "failed"
    And the scan_run row does NOT have status "orphaned" or "superseded"

  Scenario: stale-commit-becomes-failed
    Given a commit row with scan_status "scanning" linked to a scan_run that was just marked "failed"
    When the sweep finalises
    Then the commit row transitions to scan_status "failed"
