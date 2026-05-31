@story-code-intelligence:REFACTOR-GUIDE @phase-pipeline @stage-planner-and-task-planner-wiring @setup-inline
Feature: Planner and task planner wiring
  The Stage 2.4 pipeline wires the refactor Planner (Stage 8.1) and
  TaskPlanner (Stage 8.2) through the CLI orchestrator helpers. The
  planner reads metric samples + findings via InMemory readers and
  emits HotSpot rows; the task planner reads those HotSpots and emits
  RefactorPlan + RefactorTask rows. All assertions exercise the real
  production types through the in-memory composition root.

  Scenario: hot-spot ranking populated
    Given a fixture run with three findings on three different scopes
    When the planner stage runs
    Then RunArtifact.HotSpots length is 3 and rows are sorted by Score descending then ScopeID ascending

  Scenario: task kinds canonical
    Given a fixture run producing one task per kind
    When the task planner stage runs
    Then every Tasks[i].Kind satisfies refactor.IsCanonicalTaskKind

  Scenario: effort fallback wired
    Given a fixture where no ONNX model is configured
    When the task planner runs
    Then every Tasks[i].EffortHours > 0 and the diagnostics record effort_source == "fallback"

  Scenario: top-n flag override
    Given a fixture with 50 hot-spots and --top-n 5
    When the analyze command runs
    Then RunArtifact.Tasks length is at most 5
