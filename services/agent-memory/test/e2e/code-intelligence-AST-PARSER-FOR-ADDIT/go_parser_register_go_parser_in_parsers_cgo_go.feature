@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-go-parser @stage-register-go-parser-in-parsers-cgo-go @setup-inline
Feature: Register Go parser in parsers_cgo.go

  Validates that the dispatcher routes .go extensions to the Go
  tree-sitter parser when CGO is enabled, and that .go files are
  skipped gracefully when CGO is off.

  Scenario: Extension routing under CGO=on
    Given the dispatcher constructed with defaultParsers under CGO=on
    When selectParser runs for "foo.go"
    Then the returned parser Language is "go"

  Scenario: Skip under CGO=off
    Given the dispatcher constructed with defaultParsers under CGO=off
    When EmitFile processes a ".go" file
    Then the structured log emits ast.dispatch.skip with reason "no_parser"
    And no Node or Edge is inserted