@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-go-parser @stage-gotreesitterparser-implementation @setup-inline
Feature: goTreeSitterParser implementation

  The Go tree-sitter parser (parser_treesitter_go.go) must compile
  under CGO_ENABLED=1 and correctly parse Go source containing
  pointer-receiver and value-receiver methods, emitting the correct
  QualifiedName, EnclosingClass, ReceiverAliases, and LangMeta fields.

  Scenario: Build under CGO=on
    Given CGO_ENABLED=1
    When go build ./internal/repoindexer/ast/... runs from services/agent-memory
    Then it succeeds

  Scenario: Pointer receiver canonical
    Given a Go source string containing "func (r *Foo) Bar(s string) {}"
    When goTreeSitterParser.Parse runs
    Then the returned MethodDecl has QualifiedName "*Foo.Bar"
    And EnclosingClass is "Foo"
    And ReceiverAliases equals ["Foo.Bar"]
    And LangMeta receiver_ptr is true

  Scenario: Value receiver canonical
    Given a Go source string containing "func (r Foo) Bar() {}"
    When goTreeSitterParser.Parse runs
    Then the returned MethodDecl has QualifiedName "Foo.Bar"
    And ReceiverAliases is nil
    And LangMeta receiver_ptr is false
