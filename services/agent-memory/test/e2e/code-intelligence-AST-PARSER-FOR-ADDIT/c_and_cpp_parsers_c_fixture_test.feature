@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-c-and-cpp-parsers @stage-c-fixture-test @setup-inline
Feature: C fixture node and edge emission
  The C tree-sitter parser, exercised via the dispatcher EmitFile path,
  must emit the correct number and kind of graph nodes and edges for a
  compact C fixture that contains a struct, two free functions, includes,
  and an intra-file function call.

  Scenario: C fixture node + edge count
    Given the embedded C fixture
    When EmitFile runs under CGO=on
    Then 1 class, 2 method, and 1 package nodes are emitted
    And 3 contains, 1 static_calls, and 1 imports edges are emitted

  Scenario: Relative include dropped at dispatcher
    Given the embedded C fixture
    When EmitFile runs under CGO=on
    Then zero imports edges target a package node whose module starts with "./"