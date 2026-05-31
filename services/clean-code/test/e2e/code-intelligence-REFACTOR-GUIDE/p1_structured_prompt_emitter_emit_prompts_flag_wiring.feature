@story-code-intelligence:REFACTOR-GUIDE @phase-p1-structured-prompt-emitter @stage-emit-prompts-flag-wiring @setup-inline
Feature: Emit prompts flag wiring
  The --emit-prompts flag wires the JSONL refactor-prompt emitter into the
  cleanc analyze pipeline. These scenarios verify file creation, stdout
  routing, zero-task handling, stdout collision rejection, bare-flag
  rejection, and diagnostics-count stamping per implementation-plan
  Stage 4.3.

  Scenario: file written
    Given a built cleanc binary for emit-prompts wiring
    And a fixture with 5 refactor tasks
    When cleanc analyze runs with --emit-prompts prompts.jsonl
    Then prompts.jsonl exists with exactly 5 lines
    And each prompts.jsonl line is a valid JSON object
    And the emit-prompts exit code is 0

  Scenario: stdout sink with explicit out
    Given a built cleanc binary for emit-prompts wiring
    And a fixture with 5 refactor tasks
    When cleanc analyze runs with --emit-prompts - and --out report.md and --findings findings.json
    Then report.md exists with the markdown report
    And stdout receives the JSONL stream with one line per task verified against findings.json
    And the emit-prompts exit code is 0

  Scenario: zero tasks
    Given a built cleanc binary for emit-prompts wiring
    And a minimal fixture producing zero tasks
    When cleanc analyze runs with --emit-prompts prompts.jsonl against the zero-task fixture
    Then prompts.jsonl exists and is zero bytes
    And the emit-prompts exit code is 0

  Scenario: stdout collision refused
    Given a built cleanc binary for emit-prompts wiring
    When cleanc analyze runs with --emit-prompts - and no --out flag
    Then the emit-prompts exit code is 64
    And emit-prompts stderr contains "--emit-prompts - requires --out <path>; cannot route both markdown and JSONL to stdout"
    And no emit-prompts pipeline stage starts

  Scenario: bare emit-prompts refused
    Given a built cleanc binary for emit-prompts wiring
    When cleanc analyze runs with bare --emit-prompts
    Then the emit-prompts exit code is 64
    And emit-prompts stderr contains "--emit-prompts requires a path or '-' for stdout"

  Scenario: diagnostics count
    Given a built cleanc binary for emit-prompts wiring
    And a fixture with 7 refactor tasks
    When cleanc analyze runs with --emit-prompts prompts.jsonl and --out report.md
    Then prompts.jsonl has exactly 7 lines
    And the diagnostics block contains "Prompts emitted: 7 to prompts.jsonl"
