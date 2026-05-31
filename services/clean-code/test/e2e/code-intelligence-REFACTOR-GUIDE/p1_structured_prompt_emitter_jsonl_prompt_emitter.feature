@story-code-intelligence:REFACTOR-GUIDE @phase-p1-structured-prompt-emitter @stage-jsonl-prompt-emitter @setup-inline
Feature: JSONL prompt emitter
  The suggest.JSONL emitter serialises one RefactorPromptRecord per task as
  a single JSON-Lines artifact. These scenarios verify line-count fidelity,
  prompt_format_version stamping, rejected-task-kind gating, and byte-stable
  determinism per implementation-plan Sec 4.2 and architecture.md Sec 4.6.

  Scenario: one line per task
    Given a RunArtifact with 10 tasks
    When JSONL.Emit runs
    Then the output has exactly 10 lines
    And each line is parseable as a standalone JSON object

  Scenario: prompt format version stamped
    Given a RunArtifact with 1 tasks
    When JSONL.Emit runs
    Then every emitted record has prompt_format_version "v1.2026.05"

  Scenario: rejected task kind
    Given a RunArtifact with a task of kind "refactor_everything"
    When the composition root runs the emitter and maps the result
    Then exit code is 70
    And stderr names the offending task id

  Scenario: byte-stable emission
    Given a RunArtifact with 5 tasks
    When JSONL.Emit runs twice on the same artifact
    Then the two outputs are byte-identical
