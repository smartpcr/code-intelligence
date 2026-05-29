@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-rust-parser @stage-register-rust-parser-in-parsers-cgo-go @setup-inline
Feature: Register Rust parser in parsers_cgo.go

  Validates that the dispatcher routes .rs extensions to the Rust
  tree-sitter parser when CGO is enabled, and that .rs files are
  skipped gracefully when CGO is off. Build tags on parsers_cgo.go
  and parsers_nocgo.go enforce that each scenario only passes in
  its designated build mode.

  @cgo-on
  Scenario: .rs routes to Rust
    Given the dispatcher under CGO=on
    When selectParser runs for "foo.rs"
    Then the selected parser Language is "rust"

  @cgo-off
  Scenario: .rs skipped under CGO=off
    Given the dispatcher constructed via defaultParsers under CGO=off
    When EmitFile processes a ".rs" file
    Then "ast.dispatch.skip" fires with reason "no_parser"
    And no Nodes or Edges are written