@story-code-intelligence:CLEAN-CODE @phase-ast-adapter-and-foundation-tier-compute @stage-modification-count-in-window-materialiser @setup-inline
Feature: modification_count_in_window materialiser

  The materialiser aggregates churn rows (file-level modification events)
  within a configurable sliding window (default 90 days) and emits a
  MetricSample of kind `modification_count_in_window` for each scope that
  was touched during the window. Rows outside the window are silently
  dropped so historical noise does not inflate the count.

  Scenario: materialiser-emits-canonical-kind
    Given churn rows for scope "pkg.Foo.bar" dated within the last 90 days
    When the materialiser runs
    Then it emits a MetricSample with metric_kind "modification_count_in_window" and pack "base" and source "computed"
    And the sample records attrs_json provenance "ingested"

  Scenario: out-of-window-rows-ignored
    Given churn rows for scope "pkg.Stale.old" dated older than 90 days
    When the materialiser runs
    Then no metric_sample row is written for scope "pkg.Stale.old"