@story-code-intelligence:CLEAN-CODE @phase-foundation-and-schema @stage-postgresql-role-grants-and-schema-isolation @setup-inline
Feature: PostgreSQL role grants and schema isolation
  Validates that each writer role can only INSERT into its owned tables
  per the architecture G1 grant matrix and tech-spec Sec 7.2 two-writer
  carve-out, and that UPDATE/DELETE are revoked from all roles including PUBLIC.

  Scenario: role-isolation-matrix
    Given the clean_code schema exists after migrate-up with all roles provisioned
    When each writer role attempts INSERT on a table outside its writer-ownership
    Then PostgreSQL returns permission denied for every disallowed INSERT
    And each writer role succeeds on its owned tables
    And the ingestor role can INSERT into metric_sample and metric_retraction and metric_sample_active
    And the aggregator role can INSERT into metric_sample and metric_retraction and metric_sample_active

  Scenario: audit-tables-three-writer-grant
    Given the clean_code schema exists after migrate-up with all roles provisioned
    When the clean_code_evaluator role attempts INSERT on evaluation_run and evaluation_verdict and finding
    Then all three audit INSERTs by the evaluator succeed
    When the clean_code_solid_batch role attempts INSERT on evaluation_run and evaluation_verdict and finding
    Then all three audit INSERTs by the solid_batch succeed
    When the clean_code_wal_reconciler role attempts INSERT on evaluation_run and evaluation_verdict and finding
    Then all three audit INSERTs by the wal_reconciler succeed
    When any non-audit writer role attempts INSERT on the three audit tables
    Then PostgreSQL returns permission denied for every non-audit role on audit tables
    And UPDATE and DELETE are revoked from all roles including PUBLIC on the three audit tables

  Scenario: aggregator-also-writes-active-pointer
    Given the clean_code schema exists after migrate-up with all roles provisioned
    When the aggregator role INSERTs a row with pack 'system' into metric_sample
    Then the aggregator metric_sample INSERT succeeds
    When the aggregator role upserts the matching metric_sample_active pointer
    Then the aggregator metric_sample_active upsert succeeds
    When the aggregator role attempts INSERT with pack 'base' into metric_sample
    Then the application-layer per-metric_kind partitioning check rejects the base pack INSERT