@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-csharp-parser @stage-register-csharp-parser-in-parsers-cgo-go @setup-inline
Feature: Register C# parser in parsers_cgo.go

  Validates that the dispatcher routes .cs and .csx extensions
  to the C# tree-sitter parser when CGO is enabled.

  Scenario: .cs routes to C#
    Given the dispatcher under CGO=on
    When selectParser runs for "foo.cs"
    Then the selected parser Language is "csharp"

  Scenario: .csx script routes to C#
    Given the dispatcher under CGO=on
    When selectParser runs for "foo.csx"
    Then the selected parser Language is "csharp"