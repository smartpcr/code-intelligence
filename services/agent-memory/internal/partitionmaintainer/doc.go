// Package partitionmaintainer is the Stage 8.2 partition-rotation
// automation for every pg_partman-managed parent table in the
// agent-memory service.
//
// Architectural intent
// --------------------
// tech-spec §8.7.3 routes seven tables to weekly / monthly RANGE
// partitioning on `created_at` (or `started_at`):
//
//   - `trace_observation_log`  weekly
//   - `episode`                monthly
//   - `episode_update`         monthly
//   - `observation`            monthly
//   - `recall_context_log`     monthly
//   - `embedding_publish`      monthly
//   - `embedding_publish_event` monthly
//
// All seven are registered in `partman.part_config` via migrations
// 0014 and 0015 with `p_premake := 3`. pg_partman's job is to
// keep that 3-interval forward buffer ahead of `now()` so writes
// for the current period (and the next two/three) always have a
// matching child partition to route into.
//
// pg_partman ships a background worker (`pg_partman_bgw`) that
// runs `partman.run_maintenance` on a fixed interval. The Stage
// 8.2 SLO tightens that interval from the docker stack's prior
// hourly cadence to every 10 minutes, AND adds a per-parent
// `partition_provision_lag` gauge so an alert can fire when the
// forward buffer drains. Both behaviours are implemented by this
// package's `Service`. The bgw is retained as a redundant
// backstop (its interval is also pinned to 10 minutes in
// `deploy/local/docker-compose.yml`).
//
// What this package does
// ----------------------
//
//   - `Service.RunMaintenance(ctx)` -- one tick. Calls
//     `partman.run_maintenance(p_analyze := false)` either over
//     EVERY registered parent (default) or over the
//     ParentTables / SchemaFilter scope. Each call is wrapped in
//     a per-tick timeout so a stuck lock acquisition cannot
//     stall the loop.
//
//   - `Service.ScrapeLag(ctx)` -- one scrape. For each in-scope
//     parent, computes the per-parent lag:
//
//	         parent_lag_seconds = max(0,
//	             (now() + interval '1 day' - max(child_end_time)))
//
//     where `child_end_time` is read via `partman.show_partition_info`
//     for every non-default child of the parent. The
// `partition_provision_lag` gauge (whole seconds) is then set to the
//     MAX across in-scope parents (= "the oldest next-day
//     partition that is missing" per implementation-plan §8.2).
//     A healthy steady-state value is 0 -- the premake=3 buffer
//     keeps every parent's latest child_end_time well past
//     `now()+1 day`. The Stage 8.2 alert rule
//     (`deploy/local/prometheus/rules/partition_rotation.rules.yml`)
//     fires when the gauge exceeds 86400 (1 day) for >10 minutes.
//
//   - `Service.Run(ctx)` -- the long-running loop. Two
//     independent goroutines:
//
//       1. Maintenance ticker (`Config.MaintenanceInterval`,
//          default 10m): calls RunMaintenance.
//       2. Lag-scrape ticker (`Config.LagScrapeInterval`,
//          default 1m): calls ScrapeLag.
//
//     Both run an initial pass on startup so a fresh deploy does
//     not have to wait one whole tick for first observability.
//     Per-tick errors are logged at Warn but do NOT stop the
//     loop -- a transient PostgreSQL hiccup must not orphan
//     partition provisioning for 10 minutes.
//
//   - `Service.Metrics()` -- snapshot of the package counters and
//     the lag gauge (see metrics.go for the exposed names).
//
// What this package does NOT do
// -----------------------------
//
//   - It does NOT detach old partitions for retention -- that is
//     `internal/tracelogpruner`'s exclusive remit (the only
//     §8.1 30-day retention table is `trace_observation_log`).
//     The other six parents have "forever" retention per
//     architecture.md §5.1, so retention is purely a forward-
//     provisioning concern here.
//
//   - It does NOT register parents with pg_partman -- that is
//     owned by the migrations (0014, 0015, and any future
//     `<NNNN>_<table>.sql` that introduces a new partitioned
//     parent).
//
//   - It does NOT mutate `partman.part_config`. The scrape and
//     run_maintenance calls are read-only against that catalog.
//
// Role and privileges
// -------------------
// `partman.run_maintenance` issues `CREATE TABLE` for every
// newly-provisioned forward child and (when `retention_keep_table`
// is configured on a part_config row, which we do NOT set here)
// `ALTER TABLE ... DETACH PARTITION` for expired ones. Both
// require OWNERSHIP of the partitioned parent. This binary MUST
// connect as the migration-runner owner role -- the same
// ownership requirement called out in
// `internal/tracelogpruner/doc.go`. `agent_memory_admin` from
// migration 0016 holds GRANT ALL but NOT ownership; the same
// out-of-band `ALTER TABLE ... OWNER TO` step would be required
// if an operator wanted to demote this binary to the admin role.
//
// Configuration surface
// ---------------------
// `Config.ParentTables` is a fixed allow-list of schema-qualified
// parent names (e.g. `"public.episode"`). When non-empty it bypasses
// the `partman.part_config` lookup, which is useful in tests that
// want to scope to a specific test schema and avoid touching
// other tenants' parents.
//
// `Config.SchemaFilter` (mutually exclusive with ParentTables when
// non-empty) restricts the `partman.part_config` lookup to parents
// whose first dotted component matches the filter. The filter is
// compared via `split_part(parent_table, '.', 1)` rather than a
// `LIKE 'schema.%'` predicate so a schema name like `foo` cannot
// accidentally match `foobar.table`.
package partitionmaintainer
