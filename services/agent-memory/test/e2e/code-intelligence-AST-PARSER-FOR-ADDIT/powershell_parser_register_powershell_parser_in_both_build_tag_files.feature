@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-powershell-parser @stage-register-powershell-parser-in-both-build-tag-files @setup-inline
Feature: Register PowerShell parser in both build tag files

  Validates that parsers_cgo.go and parsers_nocgo.go both register the
  PowerShell subprocess parser so that .ps1 files route correctly under
  both CGO=on and CGO=off build modes, and that the dispatcher logs
  ast.dispatch.skip (not ast.parse.error) when pwsh is unavailable.

  Scenario: .ps1 routes to PowerShell under CGO=on
    Given the dispatcher constructed via defaultParsers() under CGO=on
    When selectParser("foo.ps1", nil) runs
    Then Language() == "powershell"

  Scenario: .ps1 routes to PowerShell under CGO=off
    Given the dispatcher constructed via defaultParsers() under CGO=off
    When selectParser("foo.ps1", nil) runs
    Then Language() == "powershell"

  Scenario: pwsh-not-available logs skip-not-error
    Given a host without pwsh on PATH
    When EmitFile processes a ".ps1" file
    Then "ast.dispatch.skip" fires with reason "pwsh_not_available" not "ast.parse.error"
    And EmitFile returns zero EmitResult with nil error