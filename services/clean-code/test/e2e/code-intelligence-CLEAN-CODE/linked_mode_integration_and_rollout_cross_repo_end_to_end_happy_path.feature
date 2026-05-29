@story-code-intelligence:CLEAN-CODE @phase-linked-mode-integration-and-rollout @stage-cross-repo-end-to-end-happy-path @setup-compose
Feature: Cross repo end to end happy path
  Validates the full operator flow for three registered repos: the
  management surface accepts `mgmt.register_repo`, coverage samples
  land via the Metric Ingestor (modelled here as direct samples for
  each repo at SHA), the Cross-Repo Aggregator's one-tick output
  (`clean_code.cross_repo_percentile`) is returned VERBATIM by
  `mgmt.read.cross_repo('coverage_line_ratio', 'package')` with
  `p50`, `p90`, `p99`, and `histogram_json` populated and
  `built_at` inside the freshness window (so the response carries
  `degraded=false` -- NO `percentile_stale` banner), AND
  `eval.gate(repo_id, sha)` returns one of the three canonical
  verdicts `pass | warn | block` for EACH of the three repos
  (iter 1 evaluator item 6 -- no `degraded`, `unknown`, or
  free-form verdict values escape the gate).

  The companion stale scenario advances the freshness clock past
  `freshness_window_seconds` by re-writing the snapshot row's
  `built_at`. The follow-up `mgmt.read.cross_repo` MUST then carry
  `degraded=true` AND `degraded_reason='percentile_stale'` while
  `eval.gate` calls for the SAME repos continue to emit
  `degraded_reason` values DRAWN ONLY from the three gate-allowed
  reasons (`samples_pending | policy_signature_invalid |
  xrepo_edges_unavailable`) -- `percentile_stale` is an
  Insights-ONLY signal and MUST NOT propagate to any
  `evaluation_verdict.degraded_reason` row (iter 1 evaluator
  item 8 regression guard, arch Sec 8.2).

  Background:
    Given the management surface, evaluator gate, and PostgreSQL are reachable
    And three repos are registered via mgmt.register_repo

  Scenario: cross-repo-e2e-fresh
    Given coverage uploads land for each repo at its scanned SHA
    And the Cross-Repo Aggregator has run one tick with built_at within the freshness window
    When mgmt.read.cross_repo is called for metric_kind "coverage_line_ratio" and scope_kind "package"
    Then the response carries a single row with p50, p90, p99, and histogram_json populated
    And the response envelope carries degraded equal to false
    And the response envelope does not contain a percentile_stale degraded_reason
    And eval.gate returns a canonical verdict in pass, warn, or block for each registered repo

  Scenario: cross-repo-e2e-stale
    Given coverage uploads land for each repo at its scanned SHA
    And the Cross-Repo Aggregator's last tick is older than the freshness window
    When mgmt.read.cross_repo is called for metric_kind "coverage_line_ratio" and scope_kind "package"
    Then the response envelope carries degraded equal to true
    And the response envelope carries degraded_reason equal to "percentile_stale"
    And eval.gate degraded_reason values are drawn only from samples_pending, policy_signature_invalid, or xrepo_edges_unavailable for each registered repo
    And no evaluation_verdict row carries degraded_reason "percentile_stale"
