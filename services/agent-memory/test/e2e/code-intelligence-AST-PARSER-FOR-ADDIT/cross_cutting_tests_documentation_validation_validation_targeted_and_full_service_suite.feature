@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-cross-cutting-tests-documentation-validation @stage-validation-targeted-and-full-service-suite @setup-compose
Feature: Validation — targeted and full service suite

  End-to-end validation that the AST parser additions compile,
  pass tests under both CGO modes, and keep the linter clean.

  Scenario: Targeted AST tests pass under CGO=on
    Given CGO_ENABLED is set to "1"
    When "go test ./internal/repoindexer/ast -count=1" runs from the module root
    Then the exit code is 0

  Scenario: Targeted AST tests pass under CGO=off
    Given CGO_ENABLED is set to "0"
    When "go test ./internal/repoindexer/ast -count=1 -v" runs from the module root
    Then the exit code is 0
    And the new tree-sitter parser tests are excluded by build tags

  Scenario: Full service suite passes
    Given CGO_ENABLED is set to "1"
    When "go test ./... -count=1" runs from the module root
    Then the exit code is 0

  Scenario: Lint clean
    Given CGO_ENABLED is set to "1"
    When "make lint" runs from the module root
    Then the exit code is 0