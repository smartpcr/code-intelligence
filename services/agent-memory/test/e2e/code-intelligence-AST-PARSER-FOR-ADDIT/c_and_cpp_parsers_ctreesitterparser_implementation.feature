@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-c-and-cpp-parsers @stage-ctreesitterparser-implementation @setup-inline
Feature: C tree-sitter parser implementation

  The cTreeSitterParser stage adds a tree-sitter-backed C parser that
  emits ClassDecl nodes for structs and MethodDecl nodes for free
  functions, and correctly marks relative includes so the dispatcher
  drops them in Pass 0.

  Scenario: Build under CGO=on
    Given CGO_ENABLED is set to "1"
    When go build runs on the ast package from services/agent-memory
    Then the build succeeds

  Scenario: C struct + free function
    Given C source:
      """
      struct Greeter { int n; };
      int greet(int n) { return n; }
      """
    When the source is parsed with the C tree-sitter parser
    Then the result contains a ClassDecl with QualifiedName "Greeter" and Kind "struct"
    And the result contains a MethodDecl with QualifiedName "greet" and ParamSignature "int n"

  Scenario: Relative include dropped
    Given C source with includes:
      """
      #include <stdio.h>
      #include "local.h"
      int main() { return 0; }
      """
    When the dispatcher EmitFile processes the C source in Pass 0
    Then zero imports edges are emitted whose target contains "local.h"
    And at least one imports edge is emitted whose target contains "stdio.h"