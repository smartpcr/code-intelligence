@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-csharp-parser @stage-csharptreesitterparser-implementation @setup-inline
Feature: C# tree-sitter parser implementation

  The csharpTreeSitterParser stage adds a tree-sitter-backed C# parser
  that emits ClassDecl nodes for classes, interfaces, and structs with
  correct Extends/Implements partitioning and LangMeta base_raw tracking.

  Scenario: Build under CGO=on
    Given CGO_ENABLED is set to "1"
    When go build runs on the ast package from services/agent-memory
    Then the build succeeds

  Scenario: Class with same-file interface implements
    Given C# source with interface:
      """
      interface IFoo {}
      class Foo : IFoo {}
      """
    When the source is parsed with the C# tree-sitter parser
    Then the result contains a ClassDecl with QualifiedName "Foo"
    And the Foo ClassDecl has empty Extends
    And the Foo ClassDecl Implements contains "IFoo"
    And the Foo ClassDecl LangMeta base_raw contains "IFoo"

  Scenario: Class with same-file class extends
    Given C# source with base class:
      """
      class Bar {}
      class Foo : Bar {}
      """
    When the extends source is parsed with the C# tree-sitter parser
    Then the extends result contains a ClassDecl with QualifiedName "Foo"
    And the extends Foo ClassDecl Extends contains "Bar"
    And the extends Foo ClassDecl has empty Implements
    And the extends Foo ClassDecl LangMeta base_raw contains "Bar"

  Scenario: Mixed same-file partition
    Given C# source with mixed inheritance:
      """
      class Bar {}
      interface IBaz {}
      class Foo : Bar, IBaz {}
      """
    When the mixed source is parsed with the C# tree-sitter parser
    Then the mixed result contains a ClassDecl with QualifiedName "Foo"
    And the mixed Foo ClassDecl Extends contains "Bar"
    And the mixed Foo ClassDecl Implements contains "IBaz"