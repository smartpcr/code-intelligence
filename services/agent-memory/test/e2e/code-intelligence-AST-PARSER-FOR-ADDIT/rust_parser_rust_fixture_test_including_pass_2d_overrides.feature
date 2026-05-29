@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-rust-parser @stage-rust-fixture-test-including-pass-2d-overrides @setup-inline
Feature: Rust fixture test including Pass 2d overrides

  End-to-end validation of a representative Rust fixture through the
  dispatcher's EmitFile path, verifying node/edge counts including the
  "overrides" edge emitted by Pass 2d, and exercising the cross-file
  miss path where no matching trait method exists in the same file.

  Scenario: Rust fixture node + edge count
    Given the Rust fixture source:
      """
      use std::fmt;

      trait Greeter {
          fn greet(&self) -> String { String::new() }
      }

      struct GreeterImpl;

      impl Greeter for GreeterImpl {
          fn greet(&self) -> String { String::from("hi") }
      }

      fn process() {
          let g = GreeterImpl;
          g.greet();
      }
      """
    When EmitFile runs under CGO on
    Then 2 class nodes are emitted
    And 3 method nodes are emitted
    And 1 package node is emitted
    And 1 implements edge is emitted
    And 1 static_calls edge is emitted
    And 1 imports edge is emitted
    And 1 overrides edge is emitted

  Scenario: Pass 2d overrides same-file emission
    Given a fake parser result with a trait method "Greeter.greet" with nil LangMeta
    And an impl method "GreeterImpl.greet" with LangMeta trait "Greeter" in the same file
    When Pass 2d runs
    Then exactly one edge of kind "overrides" from "GreeterImpl.greet" to "Greeter.greet" is emitted

  Scenario: Cross-file overrides miss is silent
    Given an impl method "GreeterImpl.greet" with LangMeta trait "Greeter"
    But no "Greeter.greet" node exists in the same file methodNodeID map
    When Pass 2d runs
    Then zero overrides edges are emitted
    And attrs_json for "GreeterImpl.greet" contains trait "Greeter"