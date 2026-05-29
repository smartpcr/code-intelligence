@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-rust-parser @stage-rusttreesitterparser-implementation @setup-inline
Feature: rustTreeSitterParser implementation

  The Rust tree-sitter parser (parser_treesitter_rust.go) must compile
  under CGO_ENABLED=1 and correctly parse Rust source containing
  trait definitions, impl blocks, and supertrait clauses, emitting
  the correct Methods, ClassDecl.Implements, ClassDecl.Extends, and
  LangMeta fields.

  Scenario: Build under CGO=on
    Given CGO_ENABLED=1
    When go build ./internal/repoindexer/ast/... runs from services/agent-memory
    Then it succeeds

  Scenario: Trait + impl method emit
    Given a Rust source string:
      """
      trait Greeter { fn greet(&self) -> String { String::new() } }
      struct G;
      impl Greeter for G { fn greet(&self) -> String { String::from("hi") } }
      """
    When rustTreeSitterParser.Parse runs
    Then Methods contains "Greeter.greet" with LangMeta trait_default true
    And Methods contains "G.greet" with LangMeta trait "Greeter"
    And ClassDecl "G" has Implements containing "Greeter"

  Scenario: Supertrait extends
    Given a Rust source string:
      """
      trait A {}
      trait B: A {}
      """
    When rustTreeSitterParser.Parse runs
    Then ClassDecl "B" has Extends containing "A"