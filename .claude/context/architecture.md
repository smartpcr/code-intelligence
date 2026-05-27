# Architecture

> Last updated: 2026-05-27

## System type

`code-intelligence` is a Go monorepo with two service roots:

- `services/agent-memory`: hybrid graph + episodic memory for coding agents.
- `services/clean-code`: clean-code metrics, policy evaluation, and refactor-planning service.

The root `README.md` states that authoritative design specs live in `docs/stories/code-intelligence-AGENT-MEMORY/` and `docs/stories/code-intelligence-CLEAN-CODE/`; if docs and code disagree, the docs win.

## Core services

### agent-memory

Purpose: build a durable graph-memory substrate for agents. It combines:

- structural repo graph: `Repo -> Package -> File -> Class -> Method -> Block`
- static and dynamic edges from AST/indexing and OpenTelemetry spans
- global Concepts and ConceptVersions
- append-only episodic memory from `agent.observe`
- semantic recall via embeddings/Qdrant plus Postgres publish-state filters

Key components:

- `cmd/agent-api`: gRPC agent surface for recall, observe, expand, summarize.
- `cmd/mgmt-api`: HTTP management/read surface.
- `cmd/repoindexer`: static AST/git ingest worker.
- `cmd/span-ingestor`: OTel span ingestion.
- `cmd/consolidator`, `cmd/concept-promoter`, `cmd/reranker-trainer`: background learning loop.
- `internal/graphwriter`: single DML writer for structural graph tables.
- `internal/graphreader`: read-side graph access.
- `internal/agentapi`: in-process domain service plus gRPC adapter.
- `internal/mgmtapi`: management HTTP handler.
- `internal/embedding`: vector publishing, search, and recall filtering.
- `pkg/fingerprint`: deterministic graph identity helpers.

Primary data flow:

1. Management/webhook events register repos and request full or delta ingest.
2. Repo indexer walks source trees, emits structural nodes/edges, and writes through `graphwriter`.
3. Span ingestor consumes OTel spans and appends observed dynamic-call evidence.
4. Agent calls `agent.recall`; the service embeds the query, searches Qdrant, filters unpublished points, expands/reranks, and returns a replayable context.
5. Agent calls `agent.observe`; observations append to episodic logs and later feed consolidation.
6. Consolidator/promoters/reranker trainer turn episodes into Concepts and improved retrieval signals.

Major external dependencies:

- PostgreSQL 16 with `pgcrypto` and `pg_partman`
- Qdrant
- OpenTelemetry Collector
- gRPC/protobuf
- tree-sitter for AST parsing

Important invariants from the specs:

- Reads never mutate graph state; writes never block on read latency.
- Node and Edge identity is deterministic fingerprint-based.
- Episodic, Concept, Node, and Edge histories are append-only; retirement uses tombstone rows.
- Concepts are global; structural Nodes/Edges are repo-scoped.

## clean-code

Purpose: measure and gate clean-code standards for large repos and portfolios. It computes foundation metrics, ingests external metrics, evaluates signed policies/rulepacks, exposes management/insights reads, and produces refactor-planning records.

Key components:

- `cmd/clean-code-indexer`: repo indexing lifecycle.
- `cmd/clean-code-metric-ingestor`: metric ingestion and materialization.
- `cmd/clean-code-eval-gate`: evaluator gate surface.
- `cmd/clean-code-aggregator`: cross-repo aggregator.
- `internal/ast`: AST adapter, scope identity, and generated proto model.
- `internal/metrics`: foundation/system metric materializers and recipes.
- `internal/rule_engine`: SOLID and decoupling rule evaluation.
- `internal/evaluator`: signed-policy gate and verdict production.
- `internal/policy`: Policy Steward, signing keys, DSL, and rulepack support.
- `internal/management`: management reads/writes and insights projections.
- `internal/storage`: migration and SQL persistence helpers.
- `policy/rulepacks`: built-in SOLID and decoupling rule packs.

Primary data flow:

1. Management registers repos and scan/run scope.
2. Repo indexer records commits/repo events and scan lifecycle state.
3. AST adapter parses source into canonical scopes and emits foundation metrics.
4. External metric webhook ingests coverage/churn/defects/test-balance data.
5. Metric ingestor writes active metric samples and materialized derived inputs.
6. Rule engine evaluates policy rulepacks against metrics and writes findings/verdicts.
7. Cross-repo aggregator writes system-tier rows and portfolio snapshots.
8. Evaluator surface serves `eval.gate`; Management/Insights expose read/write verbs.

Major external dependencies:

- PostgreSQL 16
- tree-sitter
- Prometheus endpoint
- OpenTelemetry Collector
- gRPC/protobuf

Important invariants from the specs:

- Sub-stores have explicit writer ownership and read-only consumers.
- Active `MetricSample` identity is `(repo_id, sha, scope_id, metric_kind, metric_version)`.
- Metric history is append-only; retraction uses tombstones.
- Policy bundles are signed and verified through active signing keys with overlap windows.
- System-tier metrics are written by the Cross-Repo Aggregator only.

## Cross-service relationship

`clean-code` can run in embedded AST mode by default and does not need `agent-memory`. In linked mode it may consume agent-memory graph data for cross-repo/call-edge context. The shared conceptual model is deterministic source identity: clean-code `scope_id` is derived from a canonical signature recipe aligned with agent-memory graph fingerprints, but it does not alias agent-memory `node_id`.

