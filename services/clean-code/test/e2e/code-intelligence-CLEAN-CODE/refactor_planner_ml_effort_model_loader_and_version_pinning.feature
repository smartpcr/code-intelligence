@story-code-intelligence:CLEAN-CODE @phase-refactor-planner @stage-ml-effort-model-loader-and-version-pinning @setup-compose
Feature: ML effort model loader and version pinning
  Validates that the refactor-planner service enforces an ML effort model
  configuration at startup and that the loaded model version is pinned via
  the hotspot → policy_version → refactor_weights chain so that
  refactor_task rows always reference a deterministic model artefact.

  Scenario: missing-model-blocks-startup
    Given refactor-effort-source=ML model from historical commits and no model URI configured
    When the planner initialises
    Then startup exits non-zero with an error naming the missing config

  Scenario: effort-model-version-pinned-via-hotspot
    Given a generated refactor_task and the refactor_plan that owns it
    When traversing refactor_plan.hotspot_ids[0] -> hot_spot.policy_version_id -> policy_version.refactor_weights.effort_model_version
    Then the value matches the loaded model artefact version