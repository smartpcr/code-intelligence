# Usage

> Last updated: 2026-05-27

## Prerequisites

- Go 1.25.x. Each service pins its version in `go.mod`.
- GNU Make or a compatible `make`.
- Docker with Compose v2 for local dependency stacks and container builds.
- `golangci-lint` v1.61.0 for CI-equivalent linting.
- `pre-commit` for local hooks.
- `protoc` plus pinned Go plugins when regenerating protobuf bindings.
- PostgreSQL client tools (`psql`, `pg_isready`) for migration/integration flows.
- A C compiler (`gcc`, `cc`, or `clang`) for CGO/tree-sitter and race-detector paths.

## Install local hooks

```powershell
pip install pre-commit
pre-commit install
```

Run all hooks:

```powershell
pre-commit run --all-files
```

## agent-memory commands

From `services\agent-memory`:

```powershell
make build
make test
make test-race
make lint
make vet
make tidy
make proto-tools
make proto
```

Local dependency stack:

```powershell
Set-Location services\agent-memory\deploy\local
docker compose up -d --build
docker compose ps
docker compose down -v
```

Local service endpoints:

- Postgres: `postgres://agent_memory:agent_memory@localhost:5432/agent_memory`
- Qdrant: `http://localhost:6333`
- OTel gRPC: `localhost:4317`
- OTel HTTP: `http://localhost:4318`
- OTel health: `http://localhost:13133`
- partition-maintainer: `http://localhost:8087/healthz`
- Prometheus: `http://localhost:9090`

## clean-code commands

From `services\clean-code`:

```powershell
make build
make test
make test-nocgo
make test-cgo
make test-race
make lint
make vet
make tidy
make proto-tools
make proto
make docker-build
make compose-up
make compose-down
```

Local dependency stack:

```powershell
Set-Location services\clean-code
make compose-up
docker compose ps
make compose-down
```

Local service endpoints:

- Postgres: `postgres://clean_code:clean_code@localhost:5432/clean_code`
- HTTP health: `http://localhost:8080/healthz`
- HTTP readiness: `http://localhost:8080/readyz`
- Metrics: `http://localhost:9090/metrics`
- OTel gRPC: `localhost:4317`
- OTel HTTP: `http://localhost:4318`
- OTel health: `http://localhost:13133`
- Prometheus: `http://localhost:9091`

## Root E2E helper

From the repo root:

```powershell
make test-phase-03
```

This expects a clean-code phase-03 Compose stack and uses `CLEAN_CODE_PG_URL`, `CLEAN_CODE_INGESTOR_URL`, and `CLEAN_CODE_OTEL_ENDPOINT` derived from Compose ports.

## Configuration

### agent-memory

Configuration is primarily per-binary and local-stack driven. See:

- `services\agent-memory\deploy\local\README.md`
- `services\agent-memory\cmd\*\main.go`
- `services\agent-memory\Makefile`

### clean-code

`services\clean-code\internal\config` is the single source of truth for `CLEAN_CODE_*` environment variables. Important defaults include:

- `CLEAN_CODE_AST_MODE_DEFAULT=embedded`
- `CLEAN_CODE_EXTERNAL_COVERAGE_FORMAT="Cobertura XML"`
- `CLEAN_CODE_GATE_DEGRADED_POLICY=warn`
- `CLEAN_CODE_POLICY_SIGNING_REQUIRED="v1 required"`
- `CLEAN_CODE_REFACTOR_EFFORT_SOURCE="ML model from historical commits"`
- `CLEAN_CODE_HTTP_ADDR=:8080`
- `CLEAN_CODE_PROMETHEUS_ADDR=:9090`
- `CLEAN_CODE_OTEL_ENDPOINT=localhost:4317`
- `CLEAN_CODE_PG_URL` for PostgreSQL DSN

Production role separation uses additional DSNs such as `CLEAN_CODE_MGMT_PG_URL`; `CLEAN_CODE_ALLOW_SHARED_PG_ROLE` is documented as a dev/E2E-only opt-in.

## Generated files

- Agent-memory proto: run `make proto` from `services\agent-memory` after editing `proto\agent.proto`.
- Clean-code proto: run `make proto` from `services\clean-code` after editing files under `proto\`.
- Commit regenerated `.pb.go` files with the IDL change.

