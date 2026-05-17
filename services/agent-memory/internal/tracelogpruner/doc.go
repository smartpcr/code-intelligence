// Package tracelogpruner is the Stage 4.3 retention pruner for
// the partitioned `trace_observation_log` table.
//
// Architectural intent
// --------------------
// `trace_observation_log` is partitioned weekly on `started_at`
// per tech-spec §8.7.3 with a §8.1 30-day rolling retention
// window. Pruning at the partition granularity (whole partitions
// detached, not row-by-row DELETE) is the load-bearing
// optimisation: a 30-day rolling window is implemented as the
// daily detach of partitions whose upper bound has crossed
// `NOW() - 30 days`. Architectural invariant C8 (architecture.md
// §5.2.3) is that the `trace_observation` aggregate row — the
// mutable per-edge counter — is NEVER touched by retention. That
// invariant is structural here: this package's only DML surface
// is `partman.drop_partition_time` against the partitioned log
// parent. There is no SQL path in this package that could touch
// `trace_observation`.
//
// What this package does
// ----------------------
//
//   - `Service.Prune(ctx)` -- a single retention pass. Calls
//     `partman.drop_partition_time(p_parent_table, p_retention,
//     p_keep_table)` and reads the integer return value (the
//     count of partitions detached or dropped). pg_partman v5
//     declares the function as `RETURNS int`; the returned
//     count is the per-run metric increment.
//
//   - `Service.Run(ctx)` -- the daily-cron loop. Runs `Prune`
//     once on startup (so a fresh deploy doesn't have to wait a
//     full day for the first sweep) then on a `time.Ticker`
//     interval. Each invocation runs under a per-run timeout
//     derived from the parent context so a stuck DETACH
//     PARTITION cannot stall the loop indefinitely.
//
//   - `Service.Metrics()` -- snapshot of the package counter
//     `trace_log_partitions_dropped_total` (per the
//     implementation-plan Stage 4.3 metric contract).
//
// What this package does NOT do
// -----------------------------
//
//   - It does NOT maintain forward partitions. Provisioning of
//     future weekly partitions is the responsibility of the
//     `pg_partman_bgw` background worker configured by the
//     Stage 1.1 docker stack (see `deploy/local/docker-compose.yml`).
//
//   - It does NOT run retention on the other partitioned tables
//     (`episode`, `episode_update`, `observation`,
//     `recall_context_log`). Those have "forever" retention per
//     architecture.md §5.1 ("EpisodicLog physical retention:
//     forever") and tech-spec §8.1. The pruner is intentionally
//     scoped to a single parent table so an operator error
//     cannot misconfigure the loop into pruning a forever-retain
//     table by accident.
//
// Role and privileges
// -------------------
// `partman.drop_partition_time` with `p_keep_table := true`
// issues `ALTER TABLE ... DETACH PARTITION ...` against each
// expired child. PostgreSQL `ALTER TABLE` requires table
// OWNERSHIP (not merely GRANT ALL PRIVILEGES), so this binary
// MUST connect as a role that owns the partitioned parent
// (typically the migration-runner owner role, e.g. the
// database-owner role from `POSTGRES_USER`). Migration 0016
// creates `agent_memory_admin` and grants it `ALL PRIVILEGES`,
// but ownership is NOT transferred — `agent_memory_admin` is
// not a sufficient pruner role on its own. Operators who want
// to run the pruner as `agent_memory_admin` must also issue
// `ALTER TABLE ... OWNER TO agent_memory_admin` (out of scope
// for this story; see open question in the workstream brief).
//
// Configuration surface
// ---------------------
// `Config.ParentTable` MUST be schema-qualified (e.g.
// `"public.trace_observation_log"`). pg_partman keys
// `partman.part_config` rows on the exact schema-qualified
// string, so the pruner cannot fall back to `search_path`
// resolution at run time. The binary's env-driven config
// applies a `"public.trace_observation_log"` default;
// integration tests supply their per-test schema name.
package tracelogpruner
