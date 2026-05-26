@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-ingest-coverage-verb-cobertura-parser @setup-compose
Feature: Ingest coverage verb Cobertura parser
  Validates that the Cobertura coverage ingest verb emits only canonical
  metric_kind values and skips scopes with no matching scope_binding.

  Scenario: coverage-emits-only-canonical-kinds
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    And scope bindings exist for the following files
      | file_path        |
      | src/main.go      |
      | src/utils.go     |
    When a Cobertura XML coverage report is uploaded for SHA "dddd0001" with files
      | file_path        | lines_valid | lines_covered | branches_valid | branches_covered |
      | src/main.go      | 100         | 85            | 20             | 14               |
      | src/utils.go     | 50          | 50            | 10             | 10               |
    Then each covered file has metric_sample rows with metric_kind IN "coverage_line_ratio,coverage_branch_ratio"
    And no metric_sample rows exist with metric_kind "coverage_line" or "coverage_branch"

  Scenario: unbound-scope-skipped
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    And scope bindings exist for the following files
      | file_path        |
      | src/main.go      |
    When a Cobertura XML coverage report is uploaded for SHA "dddd0001" with files
      | file_path            | lines_valid | lines_covered | branches_valid | branches_covered |
      | src/main.go          | 100         | 80            | 10             | 8                |
      | src/unbound_file.go  | 60          | 30            | 6              | 3                |
    Then no metric_sample row exists for file_path "src/unbound_file.go"
    And the counter "coverage_skipped_unbound_scope" is incremented