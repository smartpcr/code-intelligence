@story-code-intelligence:CLEAN-CODE @phase-foundation-and-schema @stage-measurement-schema-and-active-row-index @setup-inline
Feature: Measurement schema and active row index
  Verify that the PostgreSQL measurement tables (metric_sample,
  metric_sample_active, scope_binding, cross_repo_percentile) enforce
  the expected constraints, defaults, and uniqueness guarantees after
  running migrations.

  Scenario: active-row-quintuple-uniqueness
    Given the measurement tables exist after migrate-up
    When a metric_sample_active row is inserted for quintuple "repo1","sha1","scope1","complexity","v1"
    And a second metric_sample_active INSERT for the same quintuple runs
    Then it fails with a PRIMARY KEY violation
    When an UPSERT on metric_sample_active for the same quintuple sets a new sample_id
    Then the UPSERT succeeds and only one row exists for that quintuple
    And metric_sample is never UPDATEd

  Scenario: pack-source-enum-rejects-invalid
    Given the measurement tables exist after migrate-up
    When an INSERT into metric_sample supplies pack 'unknown'
    Then PostgreSQL rejects the metric_sample insert
    When an INSERT into metric_sample supplies source 'external'
    Then PostgreSQL rejects the metric_sample insert

  Scenario: degraded-defaults-false
    Given the measurement tables exist after migrate-up
    When a metric_sample row is inserted without a degraded value
    Then the row materialises with degraded false and degraded_reason IS NULL

  Scenario: scope-binding-stable-across-shas
    Given the measurement tables exist after migrate-up
    When the ScopeBinding writer inserts a row for repo "r1", scope_kind "function", signature "pkg.Foo", first_seen_sha "sha-A"
    And the ScopeBinding writer runs again for the same natural key at sha "sha-B"
    Then the second call scope_id equals the first
    And only one row exists in scope_binding for that natural key

  Scenario: cross-repo-percentile-shape
    Given the measurement tables exist after migrate-up
    Then cross_repo_percentile has exactly columns "percentile_id,metric_kind,scope_kind,histogram_json,p50,p90,p99,built_at"