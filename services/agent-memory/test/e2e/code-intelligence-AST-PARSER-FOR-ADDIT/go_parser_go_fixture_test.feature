@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-go-parser @stage-go-fixture-test @setup-inline
Feature: Go fixture test

  Validates the end-to-end Dispatcher.EmitFile pipeline for a canonical
  Go fixture file. A probe test is injected into the ast package at
  runtime; it calls NewDispatcher with a spy writer and the real
  GoTreeSitterParser, then EmitFile to capture the emitted graph nodes
  (package, class, method) and edges (contains, static_calls, imports,
  writes). Assertions run against the real CGO/tree-sitter pipeline.

  Scenario: Go fixture node count
    Given the embedded Go fixture
    When EmitFile runs under CGO=on
    Then 1 class and 2 method and 1 package nodes are emitted
    And 3 contains and 1 static_calls and 1 imports edges are emitted

  Scenario: Pointer receiver QualifiedName has star
    Given the embedded Go fixture
    When EmitFile runs under CGO=on
    Then the captured method signature contains the substring "#*Greeter.Greet("

  Scenario: Member write
    Given a Go fixture with method body "g.prefix = name"
    When EmitFile runs under CGO=on
    Then exactly one writes edge from the method to a field member named "prefix" is emitted