@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-csharp-parser @stage-csharptreesitterparser-implementation @setup-inline @stub
Feature: C# tree-sitter parser implementation (STUB)

  The csharpTreeSitterParser stage will add a tree-sitter-backed C#
  parser that emits ClassDecl/MethodDecl nodes for classes, interfaces,
  structs, methods, inheritance/interfaces, and using directives.

  This feature file is landed in the Go-parser stage (iter 11) as a
  STUB so the workstream's declared changed-file set matches the
  worktree. The sibling stage workstream
  `stage-4.1-csharptreesitterparser-implementation` REPLACES this
  feature in place with full walker scenarios when its branch merges
  to `feature/memory`. Until then, the scenarios below pin only the
  stub contract (LanguageParser surface + empty ParseResult).

  Scenario: Stub Language and Extensions contract
    Given the C# tree-sitter parser is constructed
    Then the parser Language is "csharp"
    And the parser Extensions include ".cs"

  Scenario: Stub Parse returns no extracted nodes for a class
    Given C# source for stub:
      """
      namespace Acme;
      public class Widget { public void Run() { } }
      """
    When the source is parsed with the C# tree-sitter parser
    Then the stub ParseResult Classes is empty
    And the stub ParseResult Methods is empty
    And the stub ParseResult Imports is empty
