@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-shared-additive-surfaces-and-dispatcher-edits @stage-additive-parser-go-struct-surfaces @setup-compose
Feature: Additive parser Go struct surfaces

  The Go struct surfaces stage adds LangMeta, ReceiverAliases, and
  ErrParserUnavailable to the AST parser package. These new fields
  and sentinel must be backward-compatible with the existing TS and
  Python parsers and safe at their zero values.

  Scenario: LangMeta nil compiles unchanged
    Given existing TS and Python parsers
    When go build ./... runs from the module root
    Then it succeeds with exit code 0
    And existing parser_typescript_test.go tests pass
    And existing parser_python_test.go tests pass
    And each parser parses a source file that emits classes methods and imports
    And every ClassDecl LangMeta is nil
    And every MethodDecl LangMeta is nil
    And every Import LangMeta is nil

  Scenario: ReceiverAliases default nil
    Given a MethodDecl zero value
    When the dispatcher iterates m.ReceiverAliases
    Then the iteration yields zero elements without panic

  Scenario: ErrParserUnavailable identity
    Given a wrapped error fmt.Errorf wrapping ast.ErrParserUnavailable
    When errors.Is is evaluated against ast.ErrParserUnavailable
    Then it returns true