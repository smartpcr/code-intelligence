@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-c-and-cpp-parsers @stage-cpptreesitterparser-implementation @setup-inline
Feature: C++ tree-sitter parser implementation

  The cppTreeSitterParser stage adds a tree-sitter-backed C++ parser
  that emits ClassDecl nodes with inheritance and LangMeta, and
  MethodDecl nodes for in-class and out-of-line method definitions.

  Scenario: Build under CGO=on
    Given CGO_ENABLED is set to "1"
    When go build runs on the ast package from services/agent-memory
    Then the build succeeds

  Scenario: Class + base + in-class method
    Given C++ source:
      """
      class Greeter : public Base { void greet() {} };
      """
    When the source is parsed with the C++ tree-sitter parser
    Then the result contains a ClassDecl with QualifiedName "Greeter"
    And the ClassDecl Extends list contains "Base"
    And the ClassDecl LangMeta base_access maps "Base" to "public"
    And the result contains a MethodDecl with QualifiedName "Greeter.greet" and EnclosingClass "Greeter"

  Scenario: In-class declaration + out-of-line definition dedupe
    Given C++ source for dedupe:
      """
      class Foo { void bar(); };
      void Foo::bar() { log(); }
      """
    When the dedupe source is parsed with the C++ tree-sitter parser
    Then ParseResult Methods contains exactly one entry with QualifiedName "Foo.bar"
    And that method entry has non-empty body source