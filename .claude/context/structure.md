# Structure

> Last updated: 2026-05-27

## Directory layout

```text
.
|-- .github/
|   `-- workflows/
|       |-- agent-memory-ci.yml
|       |-- clean-code-ci.yml
|       |-- e2e-evaluator-surface-and-management-surface.yml
|       |-- e2e-external-metric-ingest-webhook.yml
|       |-- e2e-policy-steward-and-solid-rule-engine.yml
|       `-- e2e-repo-indexer-and-metric-ingestor.yml
|-- docs/
|   `-- stories/
|       |-- code-intelligence-AGENT-MEMORY/
|       `-- code-intelligence-CLEAN-CODE/
|-- pipeline/
|-- services/
|   |-- agent-memory/
|   |   |-- cmd/
|   |   |-- deploy/local/
|   |   |-- internal/
|   |   |-- migrations/
|   |   |-- pkg/
|   |   |-- proto/
|   |   `-- web/
|   `-- clean-code/
|       |-- cmd/
|       |-- deploy/local/
|       |-- docs/
|       |-- internal/
|       |-- migrations/
|       |-- pkg/
|       |-- policy/rulepacks/
|       |-- proto/
|       |-- test/e2e/
|       `-- web/
|-- tests/
|   `-- e2e/
|-- Makefile
|-- README.md
|-- .editorconfig
|-- .pre-commit-config.yaml
`-- .yamllint.yaml
```

## Root files

- `README.md`: monorepo overview, authoritative design-doc pointers, quick start, and repo conventions.
- `Makefile`: root E2E helpers for clean-code phase-03 indexer/ingestor flow.
- `.editorconfig`: source of truth for whitespace, line endings, and indentation.
- `.pre-commit-config.yaml`: repo-wide hooks for merge conflicts, whitespace, YAML/JSON checks, editorconfig, `gofmt`, and `go vet`.
- `.yamllint.yaml`: YAML linting policy used by pre-commit.

## Design docs

- `docs/stories/code-intelligence-AGENT-MEMORY/architecture.md`: component/data/interface contracts for agent-memory.
- `docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md`: scope, constraints, parameter pins, risks.
- `docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md`: phased build order.
- `docs/stories/code-intelligence-AGENT-MEMORY/e2e-scenarios.md`: E2E behavior.
- `docs/stories/code-intelligence-CLEAN-CODE/*`: same document set for clean-code.

## agent-memory layout

- `cmd/agent-api`: agent-facing gRPC service binary.
- `cmd/mgmt-api`: management HTTP API binary.
- `cmd/repoindexer`: static repo indexing worker.
- `cmd/span-ingestor`: OTel span ingestion worker.
- `cmd/consolidator`, `cmd/concept-promoter`, `cmd/reranker-trainer`: learning/background workers.
- `cmd/loadtest-harness`: load-test and calibration harness.
- `internal/agentapi`: recall/observe/expand/summarize domain logic and gRPC adapter.
- `internal/mgmtapi`: management HTTP routes.
- `internal/graphwriter` and `internal/graphreader`: structural graph write/read access.
- `internal/embedding`: embedding publish/search/filter code.
- `internal/repoindexer`: AST indexing pipeline.
- `internal/spaningestor`: dynamic trace ingestion.
- `internal/degraded`, `internal/obs`, `internal/reliability`: degraded-state, metrics/tracing, and reliability helpers.
- `migrations/`: ordered PostgreSQL schema migrations.
- `proto/agent.proto` and `proto/agent/*.pb.go`: AgentService protobuf definition and generated Go bindings.
- `deploy/local/`: local Docker Compose stack.

## clean-code layout

- `cmd/clean-code-indexer`: repo indexer binary.
- `cmd/clean-code-metric-ingestor`: metric ingestion binary.
- `cmd/clean-code-eval-gate`: evaluator gate binary.
- `cmd/clean-code-aggregator`: cross-repo aggregator binary.
- `internal/config`: central configuration loader and operator pins.
- `internal/health`: `/healthz` and `/readyz` handlers.
- `internal/logging`: structured JSON logging and request-id propagation.
- `internal/ast`: AST parsing, scope identity, and canonical AST proto model.
- `internal/metrics`: metric recipes/materializers.
- `internal/rule_engine`: SOLID/decoupling rule evaluation.
- `internal/evaluator`: signed policy verification and gate evaluation.
- `internal/policy`: policy signing keys, steward, and DSL.
- `internal/management`: management read/write and insights projections.
- `internal/storage`: migrations and persistence tests/helpers.
- `migrations/`: PostgreSQL schema and role migration files.
- `policy/rulepacks/solid` and `policy/rulepacks/decoupling`: built-in YAML rule packs.
- `test/e2e/code-intelligence-CLEAN-CODE`: generated Go/Gherkin E2E scenarios.
- `deploy/local/`: local Docker Compose stack.

## CI workflows

- `agent-memory-ci.yml`: lint, build, test, race test, proto drift check, pre-commit, and local stack healthchecks.
- `clean-code-ci.yml`: lint, build, non-CGO and CGO tests, race test, binary artifact upload, migration integration, and container build.
- `e2e-*.yml`: clean-code E2E suites for repo indexer/metric ingestor, webhook, policy/rule engine, and evaluator/management surfaces.

