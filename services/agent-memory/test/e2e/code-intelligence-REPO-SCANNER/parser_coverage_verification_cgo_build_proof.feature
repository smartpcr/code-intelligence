@story-code-intelligence:REPO-SCANNER @phase-parser-coverage-verification @stage-cgo-build-proof @setup-inline
Feature: CGO build proof for parser coverage verification

  The AST parser dispatcher subtree must compile and pass tests under
  both CGO_ENABLED=1 (tree-sitter C/C++/C#/Go/Rust parsers linked)
  and CGO_ENABLED=0 (PowerShell-only registration via parsers_nocgo.go).
  Both `make test-cgo` and `make test-nocgo` Makefile targets are
  exercised directly so the CI-facing toolchain probe and build-tag
  gating are proven end-to-end.

  Scenario: cgo-build-passes
    Given a host with "gcc" or "clang" on PATH and "make" available
    When "make test-cgo" runs from "services/agent-memory"
    Then the make target exits successfully
    And the output includes test results from "internal/repoindexer/ast"

  Scenario: nocgo-build-passes
    Given "make" is available on PATH
    When "make test-nocgo" runs from "services/agent-memory"
    Then the make target exits successfully
    And under CGO_ENABLED=0 "parsers_cgo.go" is excluded by build tags
    And under CGO_ENABLED=0 "parsers_nocgo.go" is included by build tags
    And under CGO_ENABLED=0 "parsers_cgo_rust_test.go" is excluded by build tags

  Scenario: cgo-flag-printed
    Given a host with "gcc" or "clang" on PATH and "make" available
    When "make test-cgo" runs from "services/agent-memory"
    Then the make output contains a "go env CGO_ENABLED" probe line equal to "1"