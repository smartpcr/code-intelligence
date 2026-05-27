@story-code-intelligence:CLEAN-CODE @phase-foundation-and-schema @stage-policy-and-audit-and-refactor-schema-migrations @setup-inline
Feature: Policy, Audit, and Refactor schema migrations

  Verify that the PostgreSQL migrations for the clean_code schema
  enforce canonical ENUM values, column presence/absence constraints,
  and the latest-row-wins activation pattern for policy_activation.

  Scenario: verdict-enum-only-canonical
    Given the evaluation_verdict table exists after migrate-up
    When an INSERT supplies verdict 'fail'
    Then PostgreSQL rejects the verdict insert
    When an INSERT supplies verdict 'gated'
    Then PostgreSQL rejects the verdict insert
    When an INSERT supplies verdict 'pass'
    Then PostgreSQL accepts the verdict insert
    When an INSERT supplies verdict 'warn'
    Then PostgreSQL accepts the verdict insert
    When an INSERT supplies verdict 'block'
    Then PostgreSQL accepts the verdict insert

  Scenario: override-no-expires-column
    Given the override table exists after migrate-up
    When listing columns of clean_code.override
    Then the column "expires_at" does not exist

  Scenario: degraded-reason-closed-set
    Given the evaluation_verdict table exists after migrate-up
    When an INSERT supplies degraded_reason 'other'
    Then PostgreSQL rejects the degraded_reason insert
    When an INSERT supplies degraded_reason 'metric_regression'
    Then PostgreSQL accepts the degraded_reason insert
    When an INSERT supplies degraded_reason 'threshold_breach'
    Then PostgreSQL accepts the degraded_reason insert
    When an INSERT supplies degraded_reason 'stale_data'
    Then PostgreSQL accepts the degraded_reason insert
    When an INSERT supplies degraded_reason 'percentile_stale'
    Then PostgreSQL accepts the degraded_reason insert

  Scenario: finding-delta-canonical
    Given the finding table exists after migrate-up
    When an INSERT supplies delta 'regression'
    Then PostgreSQL rejects the delta insert
    When an INSERT supplies delta 'improvement'
    Then PostgreSQL rejects the delta insert
    When an INSERT supplies delta 'flat'
    Then PostgreSQL rejects the delta insert
    When an INSERT supplies delta 'new'
    Then PostgreSQL accepts the delta insert
    When an INSERT supplies delta 'newly_failing'
    Then PostgreSQL accepts the delta insert
    When an INSERT supplies delta 'unchanged'
    Then PostgreSQL accepts the delta insert
    When an INSERT supplies delta 'resolved'
    Then PostgreSQL accepts the delta insert

  Scenario: refactor-task-no-status-column
    Given the refactor_task table exists after migrate-up
    When listing columns of clean_code.refactor_task
    Then the column "status" does not exist
    And the column "expected_metric_delta" does not exist
    And the only columns present are "refactor_task_id,repo_id,finding_id,title,description,metric_name,target_path,priority,created_at,updated_at"

  Scenario: policy-activation-latest-row-wins
    Given the policy_activation table exists after migrate-up
    When two policy_activation rows are inserted for the same policy chain
    Then the row with MAX(created_at) is the active policy
    And no partial unique index exists on clean_code.policy_activation
    And listing columns of clean_code.policy_activation
    And the column "scope" does not exist
    And the column "deactivated_at" does not exist