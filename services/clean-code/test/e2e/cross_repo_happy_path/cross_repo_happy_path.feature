# Stage 10.4 -- Cross repo end to end happy path
#
# Drives the canonical operator flow end-to-end on a compose-
# backed PG database:
#
#   mgmt.register_repo (3 repos)
#   -> coverage uploads land on the Metric Ingestor
#   -> scan runs reach 'succeeded' (commit.scan_status='scanned')
#   -> Cross-Repo Aggregator runs one tick
#   -> mgmt.read.cross_repo returns a single fresh row
#      (degraded=false; built_at within freshness_window)
#   -> eval.gate(repo_id, sha) per repo returns a canonical
#      verdict in {pass, warn, block}
#
# Then advances the fake clock past freshness_window_seconds by
# back-dating the cross_repo_percentile row's built_at column and
# re-issues the read + gate calls:
#
#   mgmt.read.cross_repo returns degraded=true,
#     degraded_reason='percentile_stale'
#   eval.gate degraded_reason is drawn ONLY from the eval-side
#     allowed set {samples_pending, policy_signature_invalid,
#     xrepo_edges_unavailable}; 'percentile_stale' MUST NOT
#     leak onto evaluation_verdict (architecture Sec 8.2 --
#     `percentile_stale` is an INSIGHTS-only banner).

Feature: cross-repo end-to-end happy path
  Iter-1 evaluator items 4-6 pin: gate verdicts are
  exhaustively in {pass, warn, block}; `built_at` is asserted
  against the freshness window (not merely parsed); the stale
  re-read carries `degraded_reason='percentile_stale'` while
  gate degraded_reason values never carry that token.

  Background:
    Given the Management and Evaluator surfaces are reachable
    And three repos are registered via mgmt.register_repo

  Scenario: cross-repo-happy-path-fresh
    Given coverage uploads have landed and scan runs reached scanned state
    And a fresh policy version is activated
    When the Cross-Repo Aggregator runs one tick
    And mgmt.read.cross_repo('coverage_line_ratio', 'package') is called
    Then the response carries exactly one row with populated p50, p90, p99 and histogram_json
    And the response carries degraded=false with no degraded_reason banner
    And the row's built_at is within the freshness window
    When eval.gate(repo_id, sha) is called for each registered repo
    Then every call returns a canonical verdict in {pass, warn, block}
    And no gate call carries degraded_reason='percentile_stale'

  Scenario: cross-repo-happy-path-stale
    Given coverage uploads have landed and scan runs reached scanned state
    And a fresh policy version is activated
    And the Cross-Repo Aggregator has written a snapshot row
    When the fake clock is advanced past freshness_window_seconds
    And mgmt.read.cross_repo('coverage_line_ratio', 'package') is called
    Then the response carries degraded=true and degraded_reason='percentile_stale'
    And the row's built_at age exceeds the freshness window
    When eval.gate(repo_id, sha) is called for each registered repo
    Then every gate degraded_reason is in {samples_pending, policy_signature_invalid, xrepo_edges_unavailable}
    And no gate call carries degraded_reason='percentile_stale'
    And no evaluation_verdict row written during the scenario carries degraded_reason='percentile_stale'
