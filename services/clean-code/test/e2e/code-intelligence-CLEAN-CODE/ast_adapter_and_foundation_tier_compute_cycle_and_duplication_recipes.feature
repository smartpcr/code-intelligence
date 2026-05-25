@story-code-intelligence:CLEAN-CODE @phase-ast-adapter-and-foundation-tier-compute @stage-cycle-and-duplication-recipes @setup-inline
Feature: Cycle and duplication recipes
  Validates that the cycle_member recipe correctly flags files participating
  in import cycles, and that the duplication_ratio recipe emits bounded
  values at canonical scope_kinds only.

  Scenario: cycle-member-flags-participants
    Given three files A->B->C->A forming an import cycle and a file D outside the cycle
    When the cycle_member recipe runs
    Then files A, B, and C each emit value 1 at scope_kind "file"
    And file D emits value 0 at scope_kind "file"
    And the cycle_member recipe NEVER emits at scope_kind "module"

  Scenario: duplication-ratio-bounded-zero-to-one
    Given a source corpus with known duplicated blocks
    When the duplication_ratio recipe runs at scope_kind "file"
    Then the emitted value is between 0 and 1 inclusive
    And the row scope_kind is in "file,package"
    And the duplication_ratio recipe NEVER emits at scope_kind "function" or "module"