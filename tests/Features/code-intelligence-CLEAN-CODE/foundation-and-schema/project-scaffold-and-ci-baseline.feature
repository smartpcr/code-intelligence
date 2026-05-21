@story-code-intelligence-CLEAN-CODE
@phase-foundation-and-schema
@stage-project-scaffold-and-ci-baseline
@setup-inline
@e2e
Feature: Project scaffold and CI baseline for clean-code service
  As a platform engineer
  I need the clean-code service scaffold to build, lint, and test cleanly
  So that the CI pipeline guards code quality from the first commit

  Background:
    Given the code-intelligence repository root is known

  Scenario: scaffold-builds-clean
    Given a fresh checkout
    When make build lint test runs in services/clean-code/
    Then it exits 0 with no missing-target errors
    And it produces the clean-coded binary

  Scenario: ci-workflow-triggers
    Given a PR touching services/clean-code/**
    When GitHub Actions evaluates the workflow file
    Then .github/workflows/clean-code-ci.yml runs make lint test and the container build job
    And both succeed on the empty scaffold

  Scenario: config-honours-pins
    Given a config file that omits the five operator pins
    When the loader initialises
    Then it returns defaults matching architecture Sec 1.6
    And the default AST mode is embedded
    And the default coverage format is Cobertura XML
    And the default severity is warn
    And the default schema version is v1 required
    And the default model source is ML model from historical commits