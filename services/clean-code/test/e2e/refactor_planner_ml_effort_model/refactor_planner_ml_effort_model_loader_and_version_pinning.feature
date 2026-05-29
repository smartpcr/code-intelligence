@story-code-intelligence:CLEAN-CODE @phase-refactor-planner @stage-ml-effort-model-loader-and-version-pinning @setup-compose
Feature: ML effort-model loader and version pinning
  The Stage 9.3 refactor-planner loads its effort-model from the
  operator-pinned `refactor-effort-source` (architecture Sec 1.6).
  When the resolved source is `ml`, the planner requires both
  CLEAN_CODE_ML_MODEL_URI and CLEAN_CODE_ML_MODEL_VERSION env
  vars; the loaded model's version is matched against
  `policy_version.refactor_weights.effort_model_version` at every
  estimate so the architecture Sec 5.5.3 reproducibility
  invariant survives operator misconfig. A version drift aborts
  the whole atomic plan + tasks write -- a `refactor_task` row
  never lands with a `effort_hours` produced by an unpinned model.

  Scenario: ml-source-with-matching-version-stamps-effort
    Given the refactor-planner is started with CLEAN_CODE_EFFORT_SOURCE=ml
    And CLEAN_CODE_ML_MODEL_URI is "file:///models/effort_model.onnx"
    And CLEAN_CODE_ML_MODEL_VERSION is "1.0.0"
    And the active policy_version pins effort_model_version to "1.0.0"
    When the planner generates refactor tasks for a hotspot
    Then every refactor_task row has effort_hours > 0
    And every refactor_task row has effort_hours <= 40.0
    And the planner emits no version-mismatch error

  Scenario: ml-source-with-mismatched-version-aborts-batch
    Given the refactor-planner is started with CLEAN_CODE_EFFORT_SOURCE=ml
    And CLEAN_CODE_ML_MODEL_URI is "file:///models/effort_model.onnx"
    And CLEAN_CODE_ML_MODEL_VERSION is "1.0.0"
    And the active policy_version pins effort_model_version to "2.0.0"
    When the planner generates refactor tasks for a hotspot
    Then the planner exits with a version-mismatch error
    And no refactor_plan row is written for this run
    And no refactor_task rows are written for this run

  Scenario: ml-source-without-model-uri-fails-fast
    Given the refactor-planner is started with CLEAN_CODE_EFFORT_SOURCE=ml
    And CLEAN_CODE_ML_MODEL_URI is unset
    And CLEAN_CODE_ML_MODEL_VERSION is "1.0.0"
    When the planner starts
    Then the planner exits non-zero with a missing-model-URI error
    And no refactor_plan or refactor_task rows are written

  Scenario: zero-source-preserves-placeholder-semantics
    Given the refactor-planner is started with CLEAN_CODE_EFFORT_SOURCE=zero
    When the planner generates refactor tasks for a hotspot
    Then every refactor_task row has effort_hours equal to 0.0

  Scenario: canonical-architecture-pin-resolves-to-ml
    Given the refactor-planner is started with CLEAN_CODE_REFACTOR_EFFORT_SOURCE="ML model from historical commits"
    And CLEAN_CODE_ML_MODEL_URI is "file:///models/effort_model.onnx"
    And CLEAN_CODE_ML_MODEL_VERSION is "1.0.0"
    And the active policy_version pins effort_model_version to "1.0.0"
    When the planner generates refactor tasks for a hotspot
    Then every refactor_task row has effort_hours > 0
