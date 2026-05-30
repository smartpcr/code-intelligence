@story-code-intelligence:REPO-SCANNER @phase-parser-coverage-verification @stage-degradation-matrix-in-docs @setup-inline
Feature: Degradation matrix in docs for parser coverage verification

  The AST parser package must ship a COVERAGE.md file that documents
  the eight supported languages, each naming its source parser_*.go
  implementation file, and .claude/context/tests.md must link to that
  file so the degradation matrix is discoverable from the project's
  top-level test documentation.

  Scenario: coverage-doc-present
    Given a fresh clone
    When "cat services/agent-memory/internal/repoindexer/ast/COVERAGE.md" runs
    Then all eight language rows are present
    And each row names its source "parser_*.go" file

  Scenario: docs-link-resolves
    Given a fresh clone
    When a Markdown link checker runs against ".claude/context/tests.md"
    Then the link to "COVERAGE.md" resolves to an existing file