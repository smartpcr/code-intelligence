# agent-memory

Hybrid graph-memory service for the `code-intelligence:AGENT-MEMORY`
story. Implements a top-down repo / call-chain graph plus episodic
memory layer that downstream agents can recall, observe, and
consolidate against.

This subtree is the **service root**. Every binary, library,
migration, proto, and deploy artifact for the service lives under
this directory and nowhere else.

## Sibling design docs

Authoritative specs for the service live one tree up under
[`docs/stories/code-intelligence-AGENT-MEMORY/`](../../docs/stories/code-intelligence-AGENT-MEMORY/):

- [`architecture.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/architecture.md)
  — system-level architecture (§5 data model, §3.3 ingest pipeline,
  §8.5 transport, §8.6 observability).
- [`tech-spec.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md)
  — concrete PostgreSQL 16+ DDL (§8.7), role grants (§8.7.4),
  partitioning plan (§8.7.3), and Qdrant collection layout.
- [`implementation-plan.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md)
  — phased stage / step / scenario plan that this scaffold realises.
- [`e2e-scenarios.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/e2e-scenarios.md)
  — end-to-end Given/When/Then scenarios that gate the service.

If those docs and this scaffold disagree, the docs win — open a PR
that fixes the code, not one that rewrites the spec.

## Layout

```
services/agent-memory/
├── cmd/              # main packages, one per binary (agent-api, repo-indexer, …)
├── internal/         # service-private libraries (graphwriter, graphreader, …)
├── migrations/       # ordered SQL migrations + the migrate runner test
├── pkg/              # reusable, importable libraries (fingerprint, …)
├── proto/            # protobuf / gRPC service definitions (§8.5 Agent transport)
├── web/              # static assets / mgmt UI bundles, if any
└── deploy/
    └── local/        # docker compose stack for local dev + CI integration tests
```

## Local development

```
cd services/agent-memory
make build      # go build ./...
make test       # go test -count=1 ./... (portable; no -race)
make test-race  # go test -race -count=1 ./... (CGO; Linux CI only)
make lint       # golangci-lint run ./...
```

`make test` is the portable target invoked from any developer
laptop. `make test-race` is the same suite with the race detector
on and is what CI runs on Linux runners where CGO is available.

The local dependency stack (PostgreSQL 16 + `pgcrypto` + `pg_partman`,
Qdrant, OTel Collector) is started with:

```
cd services/agent-memory/deploy/local
docker compose up -d
docker compose ps    # wait for "running (healthy)" on all three rows
```

CI runs the same `docker compose` + `make test` sequence on every PR
opened against this story's workstream branches; see
[`.github/workflows/agent-memory-ci.yml`](../../.github/workflows/agent-memory-ci.yml).
