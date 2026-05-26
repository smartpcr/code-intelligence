@story-code-intelligence:CLEAN-CODE @phase-evaluator-surface-and-management-surface @stage-evaluator-gate-verb-and-synchronous-solid-delegation @setup-compose
Feature: Evaluator gate verb and synchronous SOLID delegation
  Validates that the Verdict enum is canonical, eval.gate delegates to the
  Rule Engine synchronously, degraded conditions map to warn without invoking
  the Rule Engine, no double-write of verdict rows occurs, and
  percentile_stale is rejected as an eval.gate degraded reason.

  Scenario: verdict-enum-only-canonical
    Given the production Verdict enum imported from the domain package
    When iterating the production AllVerdicts function
    Then the values are exactly "pass", "warn", "block" and no "fail" or "gated" exist

  Scenario: gate-delegates-synchronous-rule-pass
    Given a unique clean SHA with samples present and a valid policy signature
    And the rule-engine HTTP invocation count is snapshotted via its metrics endpoint
    When eval.gate is called for the clean-pass path
    Then the rule-engine HTTP invocation count increased by exactly one proving RunSync was called
    And exactly one new evaluation_run row for this SHA with caller "eval_gate" exists
    And exactly one new evaluation_verdict row referencing that run exists
    And N new finding rows referencing that run exist with N greater than zero
    And the evaluation_run and evaluation_verdict and finding rows share the same xmin proving RunSync wrote them in one transaction
    And the verdict column equals the severity rollup of the findings

  Scenario: degraded-maps-to-warn
    Given a unique SHA for the degraded scenario
    And the rule-engine HTTP invocation count is snapshotted via its metrics endpoint
    And a samples_pending degraded condition is configured with no metric samples
    When eval.gate is called for the degraded path
    Then the rule-engine HTTP invocation count did not change proving the Rule Engine was not invoked
    And zero new finding rows exist for this SHA
    And one new evaluation_run row with caller "eval_gate" and the test repo_id and SHA and policy_version_id exists
    And one new evaluation_verdict with verdict "warn" and degraded true and degraded_reason "samples_pending" referencing that run exists
    And the degraded evaluation_run and evaluation_verdict share the same xmin proving same-transaction write
    And the degraded evaluation_verdict has a non-null evaluation_run_id FK and a non-null created_at timestamp

  Scenario: gate-does-not-double-write-verdict
    Given a unique SHA for the clean-pass double-write check
    And the rule-engine HTTP invocation count is snapshotted via its metrics endpoint
    And eval.gate has completed the clean-pass path writing one run and one verdict and N findings
    When checking for double-write on the clean-pass run
    Then exactly one evaluation_verdict row exists for that run
    And the rule-engine HTTP invocation count is snapshotted again for the signature-invalid call
    And a signature-invalid eval.gate call with a unique SHA produces exactly one new run and one new verdict
    And the rule-engine was not invoked for the signature-invalid call
    And the signature-invalid verdict has the canonical schema with non-null evaluation_run_id FK and created_at and no scope column and no settled_at column
    And a samples_pending eval.gate call with a unique SHA produces exactly one new run and one new verdict
    And the information_schema confirms evaluation_verdict has no scope or settled_at columns

  Scenario: percentile-stale-not-on-gate
    Given the evaluator service is reachable
    When the degraded_reason validator is invoked with "percentile_stale"
    Then the response rejects "percentile_stale" as an invalid eval.gate reason