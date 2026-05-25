@story-code-intelligence:CLEAN-CODE @phase-ast-adapter-and-foundation-tier-compute @stage-solid-pack-foundation-recipes @setup-inline
Feature: SOLID pack foundation recipes
  Validates that the solid recipe pack registers only the canonical metric
  kinds (lcom4, fan_in, fan_out, depth_of_inheritance, interface_width,
  coupling_between_objects), that the lcom4 recipe produces a correct value
  for a class with disjoint method clusters, and that the coupling_between_objects
  recipe counts distinct external class references.

  Scenario: solid-recipes-only-canonical-kinds
    Given the registry after init
    When listing the registered metric_kinds for pack "solid"
    Then the metric_kinds are exactly "coupling_between_objects,depth_of_inheritance,fan_in,fan_out,interface_width,lcom4"

  Scenario: lcom4-class-known-value
    Given a Java class fixture with two disjoint method clusters sharing no fields
    When the lcom4 recipe runs
    Then it emits value 2

  Scenario: cbo-counts-distinct-targets
    Given a class referencing four distinct external classes
    When the coupling_between_objects recipe runs
    Then it emits value 4