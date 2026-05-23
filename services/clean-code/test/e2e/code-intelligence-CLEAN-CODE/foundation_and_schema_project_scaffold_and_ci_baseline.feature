@story-code-intelligence:CLEAN-CODE @phase-foundation-and-schema @stage-project-scaffold-and-ci-baseline @setup-inline
Feature: Project scaffold and CI baseline
  Validates that the clean-code service scaffold builds without errors,
  the CI workflow triggers on the correct paths, and the configuration
  loader honours the five operator pin defaults from architecture Sec 1.6.

  Scenario: scaffold-builds-clean
    Given a fresh checkout of the repository
    When "make build lint test" runs in the service directory
    Then the command exits 0 with no missing-target errors
    And the "clean-coded" binary is produced

  Scenario: ci-workflow-triggers
    Given a PR touching "services/clean-code/**"
    When GitHub Actions evaluates the workflow file
    Then ".github/workflows/clean-code-ci.yml" runs make lint test and the container build job and both succeed on the empty scaffold

  Scenario: config-honours-pins
    Given a config file that omits the five operator pins
    When the loader initialises
    Then it returns "embedded" as the AST mode default
    And it returns "Cobertura XML" as the external metric coverage format
    And it returns "warn" as the gate degraded policy
    And it returns "v1 required" as the policy signing required
    And it returns "ML model from historical commits" as the refactor effort source