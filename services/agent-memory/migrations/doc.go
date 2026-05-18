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
// Stage 1.3 ships migrations 0007..0014, the episodic + concept
// layer plus pg_partman registration:
//
//	0007_episode.sql                     -- episode (partitioned monthly, provenance CHECKs)
//	0008_episode_update.sql              -- episode_update (partitioned monthly)
//	0009_observation.sql                 -- observation (partitioned monthly, exactly-one-target + role CHECKs)
//	0010_recall_context_log.sql          -- recall_context_log (partitioned monthly, uuid[] arrays)
//	0011_concept.sql                     -- concept, concept_version, concept_support (G6 cross-repo)
//	0012_run_tables.sql                  -- consolidator_run, promoter_run, reranker_model
//	0013_synthetic_positive_unique.sql   -- composite partial UNIQUE on (synthesized_from_feedback_episode_id, created_at) for synthetic_positive rows
//	0014_pg_partman_setup.sql            -- partman.create_parent for the 5 partitioned parents
//
// Stage 1.4 ships migrations 0015..0016, the cross-store
// embedding state-log pair plus the application + admin role
// grants:
//
//	0015_embedding_publish.sql -- embedding_publish, embedding_publish_event (partitioned monthly, append-only; tech-spec §9.6a state machine)
//	0016_roles_grants.sql      -- agent_memory_app (INSERT/SELECT on the §8.7.4 append-only set; INSERT/SELECT/UPDATE on the UPDATE-grantable set) + agent_memory_admin (ALL PRIVILEGES)
//
// Stage 2.2 adds the reader role used by the GraphReader library
// and every recall / mgmt.read.* path:
//
//	0017_reader_role.sql -- agent_memory_ro (SELECT-only on every readable table)
//
// Stage 3.5 introduces the per-repo HMAC secret table that the
// Webhook Receiver looks up to authenticate inbound git-host
// pushes (risk §9.12):
//
//	0018_repo_webhook_secret.sql -- repo_webhook_secret (writer-only; SELECT revoked from agent_memory_ro)
//
// Stage 4.2 ships migrations 0019..0020, the Span Ingestor's
// cross-process per-repo health flag and the destination-Method
// solo-observation aggregate for root OTel spans (tech-spec
// §8.6 root-span row + §C22 closed degraded_reason set):
//
//	0019_repo_health.sql              -- repo_health (UPSERT-grantable; degraded_reason ENUM column)
//	0020_method_solo_observation.sql  -- method_solo_observation (UPSERT-grantable; root-span destination aggregate)
//
// Stage 6.1 iter-4 adds the durable sub-threshold candidate
// support staging table the Consolidator uses to accumulate
// per-signature support across ticks without pinning the
// high-water mark:
//
//	0021_concept_candidate.sql        -- concept_candidate_support (UPDATE-grantable; promoted_to_concept_id flag in lieu of DELETE)
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
