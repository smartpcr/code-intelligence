// Package migrations owns the on-disk PostgreSQL schema for the
// agent-memory service.
//
// Stage 1.2 (implementation-plan.md) ships migrations 0001..0006a,
// the structural-graph subset of the schema:
//
//	0001_enums.sql           -- every named ENUM from tech-spec §8.7.1
//	0002_repo_commit.sql     -- repo, commit
//	0003_node_edge.sql       -- node, edge (G2 fingerprint CHECK + UNIQUE)
//	0004_retirements.sql     -- node_retirement, edge_retirement (G5 tombstones)
//	0005_trace_observation.sql -- trace_observation + partitioned trace_observation_log
//	0006_repo_event.sql      -- repo_event (closed ENUM kind)
//	0006a_ingest_jobs.sql    -- durable job-queue with ingest_mode/status ENUMs
//
// Later stages append more files; the migrator picks them up by
// sorted filename so the lexicographic order matches the apply
// order.
//
// Each migration file is a single .sql file containing both an
// up and a down block, separated by the sentinel markers
// `-- migrate:up` and `-- migrate:down` (dbmate-style). This
// matches the filenames called out literally in
// implementation-plan.md Stage 1.2 (e.g. `0001_enums.sql`, not
// `0001_enums.up.sql`).
//
// The runner is intentionally minimal: it embeds the .sql files
// at build time, journals applied versions in
// `_schema_migrations`, and exposes `Up` / `Down` against a
// `*sql.DB`. Production CLI consumers will wrap it in their own
// `cmd/migrate` binary in Stage 4; this package stays library-only.
package migrations
