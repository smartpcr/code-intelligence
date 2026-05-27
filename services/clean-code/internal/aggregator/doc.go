// Package aggregator hosts the Cross-Repo Aggregator (architecture
// Sec 3.10 / implementation-plan Stage 7.1).
//
// # Role
//
// The aggregator is a cadence-driven worker that, on every tick:
//
//  1. Reads the ACTIVE `metric_sample` rows across all repos via
//     the canonical `metric_sample_active` side-relation join +
//     `metric_retraction` anti-join (tech-spec Sec 7.1.b, mirrors
//     `internal/management/pg_metrics_backend.go` reader pattern).
//  2. Groups observations by `(repo_id, metric_kind, scope_kind)`
//     for the per-repo snapshot and by `(metric_kind, scope_kind)`
//     for the cross-repo + portfolio rows.
//  3. Materialises three derived views into the Measurement
//     sub-store (architecture Sec 5.2.4 -- Sec 5.2.6):
//
//     - `clean_code.repo_metric_snapshot` -- per-repo count + mean
//     + p50 + p90 + p99 with a fresh `built_at`.
//     - `clean_code.cross_repo_percentile` -- cross-repo p50 / p90
//     / p99 computed over the FLAT observation-value set across
//     ALL contributing repos (architecture Sec 3.10 line 644:
//     "the full per-metric percentile vector across all repos"),
//     plus a `histogram_json` carrying one entry per contributing
//     repo for the Insights portfolio UI (architecture Sec 5.2.5
//     line 1101 -- "Per-repo histogram for portfolio UI
//     rendering"). Large repos (more observations) therefore
//     contribute proportionally more weight in the cross-repo
//     percentile; the per-repo unweighted view lives in the
//     sibling histogram_json + the portfolio_snapshot
//     `aggregate_json.unweighted_mean` field.
//     - `clean_code.portfolio_snapshot` -- `aggregate_json`
//     carrying the operator-pinned aggregate shape across repos
//     (architecture Sec 5.2.6 line 1115).
//
// # Writer ACL (architecture G1 / Sec 1.5)
//
// The aggregator is the SOLE writer of `repo_metric_snapshot`,
// `cross_repo_percentile`, and `portfolio_snapshot`. The
// `clean_code_xrepo_aggregator` Postgres role has
// `INSERT, SELECT` and explicit `REVOKE UPDATE, DELETE` on each
// of the three snapshot tables (migration
// `0004_roles.up.sql` lines 395-397 / 416-418). Snapshot rows
// are append-only derivative views per G6 -- the aggregator
// inserts a fresh row set every tick, all sharing the SAME
// `built_at` timestamp captured once at tick start; readers
// pick "latest by built_at".
//
// # Cadence
//
// Default `15m` (tech-spec Sec 8.2 `aggregator_cadence`, surfaced
// at [config.DefaultAggregatorCadence]). The cadence is shorter
// than the `freshness_window_seconds` policy default (3600s) so
// the Insights freshness banner has 3-4x headroom under nominal
// load (architecture Sec 3.10).
//
// # Concurrency
//
// Stage 7.1 targets the single-aggregator-process shape -- one
// worker per deployment. The Postgres role ACL prevents
// non-aggregator components from writing the snapshot tables but
// does NOT prevent two aggregator processes from concurrently
// inserting overlapping rows. Production deployments MUST run
// exactly one aggregator replica; horizontal scale (singleton via
// leader-election / advisory lock) is a Phase 7+ follow-up.
//
// # Determinism (G6)
//
// All three snapshot tables are derived views. Their contents at
// any `built_at` are a pure function of the `metric_sample` /
// `metric_sample_active` / `metric_retraction` state at the
// instant the tick read its inputs. The aggregator may be
// restarted, lose its working set, and recompute everything on
// the next tick without loss of correctness.
package aggregator
