@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-cross-cutting-tests-documentation-validation @stage-cross-language-dispatcher-tests @setup-compose
Feature: Cross-language dispatcher tests

  Validates extension routing for all new language parsers,
  duplicate-registration semantics, multimap collision rules,
  the ErrParserUnavailable skip path, and first-class attr
  key protection.

  Scenario: Every new extension routes to its parser
    Given the dispatcher is configured with parsers for all new languages
    When selectParser runs for each of ".c", ".h", ".cpp", ".cxx", ".cs", ".go", ".rs", ".ps1", ".psm1", ".psd1"
    Then each extension returns a non-nil parser with the expected Language value

  Scenario: .h pinning under CGO=on
    Given the dispatcher has a C parser claiming ".c" and ".h"
    And the dispatcher has a C++ parser claiming ".cpp" and ".cxx"
    When selectParser runs for "foo.h" with hints "cpp"
    Then the returned parser Language is "c"

  Scenario: Duplicate registration last-wins
    Given two stub parsers both claiming ".go" registered via WithParsers
    When selectParser runs for "x.go"
    Then the returned parser is the second registered parser

  Scenario: Multimap collision drops end-to-end
    Given a Go fixture file "testdata/fixture_collision.go" with both receiver variants of "Bar" and a sibling caller
    When EmitFile runs on the Go fixture
    Then zero static_calls edges target "Bar"
    And verbatim "Bar" persists on calls_raw

  Scenario: Multimap pointer-only resolves end-to-end
    Given a Go fixture file "testdata/fixture_pointer_only.go" with only the pointer-receiver method
    And the fixture parser maps the pointer-receiver method "Bar" on "*Foo" with alias "Foo.Bar" to "*Foo.Bar"
    When EmitFile runs on the Go fixture
    Then exactly one static_calls edge from the sibling to "*Foo.Bar" is emitted

  Scenario: ErrParserUnavailable surfaces as skip-not-error
    Given a stub parser returning ErrParserUnavailable with reason "test_unavailable" for ".xx"
    When EmitFile processes an ".xx" file
    Then the log key is "ast.dispatch.skip" with reason "test_unavailable"
    And EmitFile returns a zero EmitResult and nil error

  Scenario: First-class attr key cannot be overridden
    Given a parser populates LangMeta with "language" set to "bogus"
    When methodAttrs writes the result with dispatcher language "go"
    Then attrs_json "language" equals "go"