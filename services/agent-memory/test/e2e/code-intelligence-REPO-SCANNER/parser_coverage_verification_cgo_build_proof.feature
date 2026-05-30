@story-code-intelligence:REPO-SCANNER @phase-parser-coverage-verification @stage-cgo-build-proof @setup-inline
Feature: CGO build proof for parser coverage verification

  The AST parser dispatcher subtree must compile and pass tests under
  both CGO_ENABLED=1 (tree-sitter C/C++/C#/Go/Rust parsers linked)
  and CGO_ENABLED=0 (PowerShell-only registration via parsers_nocgo.go).
  The `make test-cgo` target must also print `go env CGO_ENABLED` = 1
  so CI logs record the active toolchain.

  Scenario: cgo-build-passes
    Given a host with "gcc" or "clang" on PATH
    When "make test-cgo" runs from "services/agent-memory"
    Then the suite under "internal/repoindexer/ast/" passes

  Scenario: nocgo-build-passes
    Given the same checkout
    When "make test-nocgo" runs from "services/agent-memory"
    Then the suite passes
    And "parsers_cgo_rust_test.go" is excluded by build tags

  Scenario: cgo-flag-printed
    Given a host with "gcc" or "clang" on PATH
    When "make test-cgo" executes its toolchain probe
    Then the printed "go env CGO_ENABLED" line equals "1"