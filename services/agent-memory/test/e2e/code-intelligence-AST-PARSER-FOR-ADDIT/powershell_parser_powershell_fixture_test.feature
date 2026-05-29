@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-powershell-parser @stage-powershell-fixture-test @setup-inline
Feature: PowerShell fixture test — node and edge emission

  The PowerShell subprocess parser, exercised via the dispatcher EmitFile
  path, must emit the correct graph nodes and edges for a compact fixture
  containing one class, three methods (one class method and two free
  functions), one Import-Module directive, and one intra-file function call.
  When pwsh is absent the fixture must skip gracefully, and a parser
  constructed with an empty pwshBin must return the ErrParserUnavailable
  sentinel.

  @needs-pwsh
  Scenario: pwsh-present fixture parses
    Given the embedded PowerShell fixture
    When EmitFile runs for the PowerShell fixture
    Then 1 class, 3 method, and 1 package nodes are emitted for PowerShell
    And the PowerShell fixture emits contains, static_calls, and imports edges

  @no-pwsh
  Scenario: pwsh-absent fixture is skipped
    Given the PowerShell AST implementation is available
    When TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet runs without pwsh on PATH
    Then it calls t.Skip and reports no failure

  @no-pwsh
  Scenario: Sentinel-returning parser
    Given a PowerShell parser with empty pwshBin
    When TestPowerShellParser_NoPwsh_ReturnsSentinel runs
    Then errors.Is returns true for ErrParserUnavailable
    And the ParseResult has no classes methods or imports