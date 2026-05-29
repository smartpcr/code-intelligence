@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-powershell-parser @stage-powershellparser-subprocess-implementation @setup-inline
Feature: powershellParser subprocess implementation

  The PowerShell subprocess parser (parser_powershell.go) must compile
  under both CGO_ENABLED=1 and CGO_ENABLED=0 (it has no build tags),
  return the ErrParserUnavailable sentinel when pwsh is missing, and
  return a non-sentinel error when pwsh times out.

  Scenario: Build under both CGO=on and CGO=off
    Given the file has no build tags
    When go build ./internal/repoindexer/ast/... runs from services/agent-memory under CGO_ENABLED=1
    And go build ./internal/repoindexer/ast/... runs from services/agent-memory under CGO_ENABLED=0
    Then both builds succeed

  Scenario: pwsh missing returns sentinel
    Given a PowerShell parser constructed with pwsh absent from PATH
    When Parse "foo.ps1" with source "function Foo {}" runs
    Then it returns an error wrapping ErrParserUnavailable
    And the ParseResult is empty

  Scenario: pwsh timeout returns error not sentinel
    Given a PowerShell parser constructed with a fake pwsh that sleeps
    When Parse "foo.ps1" with source "function Foo {}" runs on the slow parser
    Then it returns a non-nil error
    And the error does not wrap ErrParserUnavailable