@story-code-intelligence:REPO-SCANNER @phase-parser-coverage-verification @stage-per-language-parse-smoke-fixtures @setup-inline
Feature: Per-language parse smoke fixtures for parser coverage verification

  The polyglot smoke test (`parsers_polyglot_smoke_test.go`, gated by
  `//go:build cgo`) exercises the AST dispatcher's EmitFile path for all
  eight supported languages (TypeScript, Python, C, C++, C#, Go, Rust,
  PowerShell) against canonical fixtures under `testdata/polyglot/hello.<ext>`.
  Each fixture must emit at least one class/type node, one method node, and
  one `static_calls` edge.

  Under CGO_ENABLED=0 the polyglot smoke test file is excluded by its cgo
  build tag — tree-sitter parsers are not registered at all
  (`parsers_nocgo.go` registers only PowerShell). The nocgo-degraded
  scenario verifies this by injecting a temporary build-tag-free test into
  the ast package that exercises the same table-driven pattern: for each of
  the six additional languages it calls `SelectParser` and, when no parser
  is registered, calls `t.Skip` with a message keyed on
  `ErrParserUnavailable`. Under CGO=0 only the PowerShell subtest passes
  while the five tree-sitter subtests (C, C++, C#, Go, Rust) are skipped —
  directly demonstrating the degraded coverage behaviour.

  Scenario: polyglot-smoke-cgo
    Given CGO_ENABLED is "1" and a C compiler is on PATH
    When the polyglot smoke test "TestPolyglotParseSmoke" runs under CGO_ENABLED "1"
    Then every subtest passes without skips
    And the test output shows a PASS for language "typescript"
    And the test output shows a PASS for language "python"
    And the test output shows a PASS for language "c"
    And the test output shows a PASS for language "cpp"
    And the test output shows a PASS for language "csharp"
    And the test output shows a PASS for language "go"
    And the test output shows a PASS for language "rust"
    And the test output shows a PASS for language "powershell"

  Scenario: polyglot-smoke-nocgo-degraded
    Given CGO_ENABLED is "0"
    When the nocgo polyglot degradation test exercises the dispatcher under CGO_ENABLED "0"
    Then only the "powershell" fixture passes in the nocgo degradation test
    And the nocgo degradation output confirms the powershell fixture emits non-empty Node and Edge sets
    And the "c" fixture is skipped via t.Skip keyed on ErrParserUnavailable
    And the "cpp" fixture is skipped via t.Skip keyed on ErrParserUnavailable
    And the "csharp" fixture is skipped via t.Skip keyed on ErrParserUnavailable
    And the "go_lang" fixture is skipped via t.Skip keyed on ErrParserUnavailable
    And the "rust" fixture is skipped via t.Skip keyed on ErrParserUnavailable
    And "parsers_nocgo.go" is compiled under CGO_ENABLED "0"
    And "parsers_cgo.go" is not compiled under CGO_ENABLED "0"

  Scenario: pwsh-missing-skip
    Given CGO_ENABLED is "1" and a C compiler is on PATH and pwsh is absent from PATH
    When the polyglot smoke test "TestPolyglotParseSmoke" runs without pwsh under CGO_ENABLED "1"
    Then the powershell subtest is skipped
    And all non-powershell subtests pass
    And the ErrParserUnavailable sentinel test confirms the pwsh_not_available reason
    And the dispatcher ErrParserUnavailable skip test passes under CGO_ENABLED "1"