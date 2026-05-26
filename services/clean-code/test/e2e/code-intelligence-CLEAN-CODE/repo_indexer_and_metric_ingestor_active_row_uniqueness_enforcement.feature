@story-code-intelligence:CLEAN-CODE @phase-repo-indexer-and-metric-ingestor @stage-active-row-uniqueness-enforcement @setup-compose
Feature: Active row uniqueness enforcement
  Validates that the Metric Ingestor upholds the active-row uniqueness
  invariant across re-ingestion scenarios.

  # Compose: tests/e2e/phase-03-indexer-ingestor/docker-compose.yml
  # Bootstrap: make migrate-up && make seed-fixtures-phase-03

  Background:
    Given a running Metric Ingestor connected to PostgreSQL
    And the database is migrated and seeded with fixtures

  Scenario: re-ingest-without-retract-is-idempotent
    Given a metric_sample row already present and pointed-to by metric_sample_active
    When the Metric Ingestor re-ingests the same SHA
    Then metric_sample_active.sample_id remains stable
    And metric_sample row count is unchanged or grows by exactly one

  Scenario: re-ingest-after-retract-succeeds
    Given a metric_sample row already present and pointed-to by metric_sample_active
    And the sample is retracted via metric_retraction
    When the Metric Ingestor re-ingests the same SHA
    Then a new metric_sample row appears with a fresh sample_id
    And metric_sample_active is UPSERTed to point at the new row
    And the original metric_sample row remains in place
    And reader queries join through metric_retraction to filter the prior tombstone