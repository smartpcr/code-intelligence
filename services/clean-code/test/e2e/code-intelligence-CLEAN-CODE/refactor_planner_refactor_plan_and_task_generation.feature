@story-code-intelligence:CLEAN-CODE @phase-refactor-planner @stage-refactor-plan-and-task-generation @setup-compose
Feature: Refactor plan and task generation
  The refactor-planner generates canonical refactor_task rows from
  hotspot findings. Tasks carry a constrained kind enum and the
  originating rule_id. Effort is stored inline on refactor_task
  (no separate effort_estimate table). The refactor_task table
  excludes status and expected_metric_delta columns to guard
  against schema drift.

  Scenario: plan-generates-canonical-task-kinds
    Given a hotspot flagged by rule "solid.srp"
    When the planner generates tasks
    Then a refactor_task row exists with kind "split_class" and rule_id "solid.srp"
    And inserting a refactor_task with kind "reduce_lcom" is rejected by the CHECK constraint
    And inserting a refactor_task with kind "introduce_interface" is rejected by the CHECK constraint

  Scenario: no-effort-estimate-table
    Given the schema after Phase 1 migrations
    When the planner persists effort for the hotspot
    Then refactor_task.effort_hours is populated
    And no table named "effort_estimate" exists in the schema

  Scenario: refactor-task-has-no-status-column
    Given the canonical refactor_task table
    When information_schema.columns is queried for refactor_task
    Then no column named "status" exists
    And no column named "expected_metric_delta" exists