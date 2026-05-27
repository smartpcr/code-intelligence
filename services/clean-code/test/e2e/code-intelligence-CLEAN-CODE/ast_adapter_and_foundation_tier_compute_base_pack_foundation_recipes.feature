@story-code-intelligence:CLEAN-CODE @phase-ast-adapter-and-foundation-tier-compute @stage-base-pack-foundation-recipes @setup-inline
Feature: Base pack foundation recipes
  Validates that the base recipe pack registers only the canonical metric
  kinds (cyclo, cognitive_complexity, loc), that the cyclo recipe produces
  correct values for known control-flow structures, and that the loc recipe
  counts physical lines at file scope.

  Scenario: base-recipes-only-canonical-kinds
    Given the recipe registry after init
    When listing the registered metric_kinds for pack "base"
    Then the result is exactly "cyclo,cognitive_complexity,loc"

  Scenario: cyclo-known-value
    Given a Go fixture method with two if branches and one for loop
    When the cyclo recipe runs
    Then it emits a MetricSampleDraft with metric_kind "cyclo" and value 4 at scope_kind "method"

  Scenario: loc-counts-physical-lines
    Given a 42-line Python source file fixture
    When the loc recipe runs at scope_kind "file"
    Then it emits value 42