@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-solid-rule-engine-batch-worker-and-synchronous-mode @setup-compose
Feature: SOLID Rule Engine batch worker and synchronous mode
  Validates that the rule engine emits findings when metric samples exceed
  SOLID thresholds, respects muted overrides, computes finding deltas across
  SHAs, and supports synchronous evaluation that commits run/verdict/findings
  in a single transaction.

  Scenario: finding-emitted-on-rule-hit
    Given a SHA with a metric_sample of kind "lcom4" and value 12 exceeding the SRP threshold of 10
    When the rule engine runs
    Then a finding with rule_id "solid.srp" and severity "warn" and delta "new" exists
    And the finding has a policy_version_id pinned
    And the finding has metric_sample_ids JSONB referencing the triggering sample

  Scenario: muted-scope-skipped
    Given an override with scope "repo-a/src/BigClass.cs" and rule_id "solid.srp" and mute true as the latest row
    And a metric_sample exists for that muted scope exceeding the threshold
    When the rule engine evaluates that scope via RunSync
    Then no finding row is appended for that scope and rule
    And the evaluation_run for the muted scope completed successfully

  Scenario: delta-newly-failing
    Given the same scope and rule evaluated at SHA A with severity "warn"
    And the same scope and rule evaluated at SHA B with severity "block"
    When the worker writes the SHA B finding
    Then the SHA B finding has delta "newly_failing"

  Scenario: sync-mode-writes-run-verdict-and-findings
    Given RuleEngine.RunSync called with valid inputs
    When it returns
    Then exactly one evaluation_run with caller "eval_gate" exists
    And exactly one evaluation_verdict referencing that run exists
    And at least one finding row referencing that run exists
    And all rows were committed in the same transaction
    And the verdict column matches the severity rollup of unmuted findings