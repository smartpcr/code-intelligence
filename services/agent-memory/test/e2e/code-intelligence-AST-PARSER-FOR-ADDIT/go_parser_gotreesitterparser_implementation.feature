@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-go-parser @stage-gotreesitterparser-implementation @setup-inline
Feature: Go tree-sitter parser implementation

  The goTreeSitterParser stage adds a tree-sitter-backed Go parser that
  emits ClassDecl nodes for structs / interfaces / type aliases (with
  embeds metadata), MethodDecl nodes for free functions and
  receiver-bound methods (with receiver / receiver_type / receiver_ptr
  LangMeta), and Import nodes for `import` declarations (with
  dot_import / blank_import LangMeta).

  Unlike the sibling C / C# / C++ e2e files in this directory, this
  feature is NOT a stub -- the goTreeSitterParser stage owns the real
  Go parser implementation in
  `internal/repoindexer/ast/parser_treesitter_go.go`, so every scenario
  below exercises real walker output rather than empty placeholders.

  Scenario: Language and Extensions contract
    Given the Go tree-sitter parser is constructed
    Then the parser Language is "go"
    And the parser Extensions include ".go"

  Scenario: Struct declaration with embedded field
    Given Go source for struct fixture:
      """
      package fixture
      type Base struct { id int }
      type Greeter struct {
        Base
        Name string
      }
      """
    When the source is parsed with the Go tree-sitter parser
    Then the result contains a ClassDecl with QualifiedName "Greeter" and Kind "struct"
    And the ClassDecl "Greeter" LangMeta embeds list contains "Base"

  Scenario: Interface declaration with method spec and embedded interface
    Given Go source for interface fixture:
      """
      package fixture
      import "io"
      type ReadCloser interface {
        io.Reader
        Close() error
      }
      """
    When the source is parsed with the Go tree-sitter parser
    Then the result contains a ClassDecl with QualifiedName "ReadCloser" and Kind "interface"
    And the ClassDecl "ReadCloser" LangMeta embeds list contains "io.Reader"
    And the result contains a MethodDecl with QualifiedName "ReadCloser.Close" and EnclosingClass "ReadCloser"

  Scenario: Pointer-receiver method emits star QualifiedName and alias
    Given Go source for pointer-receiver fixture:
      """
      package fixture
      type Counter struct { n int }
      func (c *Counter) Inc() { c.n = c.n + 1 }
      """
    When the source is parsed with the Go tree-sitter parser
    Then the result contains a MethodDecl with QualifiedName "*Counter.Inc" and EnclosingClass "Counter"
    And the MethodDecl "*Counter.Inc" ReceiverAliases list contains "Counter.Inc"
    And the MethodDecl "*Counter.Inc" LangMeta receiver_type is "Counter"
    And the MethodDecl "*Counter.Inc" LangMeta receiver_ptr is true

  Scenario: Type alias declaration
    Given Go source for type-alias fixture:
      """
      package fixture
      type Celsius float64
      """
    When the source is parsed with the Go tree-sitter parser
    Then the result contains a ClassDecl with QualifiedName "Celsius" and Kind "type_alias"

  Scenario: Grouped import with alias, dot, and blank specs
    Given Go source for import fixture:
      """
      package fixture
      import (
        "strings"
        io "io"
        . "fmt"
        _ "embed"
      )
      """
    When the source is parsed with the Go tree-sitter parser
    Then the result contains an Import with Module "strings"
    And the result contains an Import with Module "io" and Alias "io"
    And the result contains an Import with Module "fmt" and dot_import LangMeta
    And the result contains an Import with Module "embed" and blank_import LangMeta
