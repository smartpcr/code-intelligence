@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-c-and-cpp-parsers @stage-cpp-fixture-test @setup-inline
Feature: C++ fixture node and edge emission
  The C++ tree-sitter parser, exercised via the dispatcher EmitFile path,
  must emit the correct number and kind of graph nodes and edges for
  compact C++ fixtures covering classes, methods, inheritance, includes,
  intra-file calls, and declaration/definition deduplication.

  Scenario: C++ fixture node + edge baseline
    Given the embedded C++ fixture
    When EmitFile runs under CGO=on
    Then 2 class, 3 method, and 1 package nodes are emitted
    And 5 contains, 1 extends, 1 static_calls, and 1 imports edges are emitted
    And zero imports edges target a package node whose module starts with "./"
    And the sole imports edge targets a package node whose signature contains "iostream"

  Scenario: Dedupe collapses declaration + definition
    Given the embedded dedupe fixture
    When EmitFile runs under CGO=on
    Then exactly 1 method node with signature containing "Foo.bar" is emitted
    And the method node with signature containing "Foo.bar" has non-empty BodySource
    And exactly 1 static_calls edge targeting a signature containing "log_global" is emitted

  Scenario: base_access attrs persist
    Given the inheritance fixture
    When EmitFile runs under CGO=on
    Then the class node with signature containing "Greeter" has base_access "Base" equal to "public"