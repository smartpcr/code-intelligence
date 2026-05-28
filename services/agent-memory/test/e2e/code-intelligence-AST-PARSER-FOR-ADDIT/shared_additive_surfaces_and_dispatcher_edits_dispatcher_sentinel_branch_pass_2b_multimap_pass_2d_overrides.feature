@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-shared-additive-surfaces-and-dispatcher-edits @stage-dispatcher-sentinel-branch-pass-2b-multimap-pass-2d-overrides @setup-compose
Feature: Dispatcher sentinel branch, Pass 2b multimap, Pass 2d overrides

  Validates the ErrParserUnavailable skip-log path, the Pass 2b multimap
  collision-drop rule, the multimap pointer-only resolution path, and
  the Pass 2d overrides edge insertion for trait/impl methods.

  Scenario: ErrParserUnavailable skip-log path
    Given a stub parser whose Parse returns ErrParserUnavailable with reason "stub_missing"
    When EmitFile processes a file routed to that parser
    Then the structured log emits "ast.dispatch.skip" with reason "stub_missing"
    And EmitFile returns a zero EmitResult and nil error
    And the writer receives zero InsertNode and InsertEdge calls

  Scenario: Multimap collision drops
    Given a Go fixture with both "func (r Foo) Bar()" and "func (r *Foo) Bar()" in the same file
    And a third method calling "r.Bar()" via receiver
    When emit runs
    Then no static_calls edge is emitted for "Bar"
    And "Bar" persists on calls_raw

  Scenario: Multimap pointer-only resolves
    Given a Go fixture with only "func (r *Foo) Bar()" plus a sibling method calling "r.Bar()" from inside Foo
    When emit runs
    Then exactly one static_calls edge from the sibling method to "*Foo.Bar" is emitted
    And the edge was resolved via ReceiverAliases entry "Foo.Bar"

  Scenario: Pass 2d overrides edge
    Given a fake parser result with a trait method "Greeter.greet" with nil LangMeta
    And an impl method "GreeterImpl.greet" with LangMeta trait "Greeter" in the same file
    When Pass 2d runs
    Then one edge of kind "overrides" is inserted from "GreeterImpl.greet" to "Greeter.greet"

  Scenario: Pass 2d cross-file miss drops
    Given an impl method "GreeterImpl.greet" with LangMeta trait "Greeter"
    But no "Greeter.greet" node exists in the same file methodNodeID map
    When Pass 2d runs
    Then zero overrides edges are inserted
    And the trait name "Greeter" remains on attrs_json