@story-code-intelligence:REFACTOR-GUIDE @phase-hardening-and-release @stage-build-tag-matrix @setup-inline
Feature: Build Tag Matrix
  The `-tags prod` build gate ensures that the dev-mode unsigned-policy
  bypass is excluded at compile time. The prod binary is compiled with
  `go build -tags prod`, and the resulting artifact (`bin/cleanc-prod`)
  must exist and be runnable. The unit tests gated behind `//go:build prod`
  verify that `devpolicy.LoadUnsignedBundle(...)` returns
  `devpolicy.ErrDevModeUnavailable` whose message contains the phrase
  "dev-mode policy bypass not available in prod build".

  Scenario: prod build compiles
    Given the source tree
    When CI runs make build-prod
    Then it exits 0
    And bin/cleanc-prod exists

  Scenario: prod build excludes bypass via unit test
    Given the services/clean-code source tree
    When CI runs go test -tags prod -run TestProdBuildExcludesDevBypass ./internal/cli/devpolicy/...
    Then the exit code is 0
    And the test output contains "PASS"

  Scenario: prod tests pass
    Given the services/clean-code source tree
    When CI runs go test -tags prod for the prod-gated packages
    Then the exit code is 0
    And the test output contains "PASS"
