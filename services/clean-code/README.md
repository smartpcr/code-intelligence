# clean-code

Clean-code measurement and policy service for the
`code-intelligence:CLEAN-CODE` story. Computes a fixed catalogue of
foundation, ingested, and system-tier metric_kinds against arbitrary
source-code repositories; evaluates signed `PolicyVersion` rule packs;
serves an evaluator gate (`eval.gate`), a refactor planner, and an
insights surface.

This subtree is the **service root**. Every binary, library,
migration, proto, and deploy artefact for the service lives under
this directory and nowhere else.

## Sibling design docs

Authoritative specs live one tree up under
[`docs/stories/code-intelligence-CLEAN-CODE/`](../../docs/stories/code-intelligence-CLEAN-CODE/):

- [`architecture.md`](../../docs/stories/code-intelligence-CLEAN-CODE/architecture.md)
  -- system architecture, sub-stores (Catalog / Lifecycle /
  Measurement / Policy / Audit / Refactor), G1-G6 invariants, the
  five operator pins (Sec 1.6) implemented by `internal/config/`.
- [`tech-spec.md`](../../docs/stories/code-intelligence-CLEAN-CODE/tech-spec.md)
  -- PostgreSQL 16 schema (Sec 8.7), role grants (Sec 7.2),
  partitioning (Sec 8.1.4), numeric defaults (Sec 8.2), policy
  signing (Sec 8.4), transport / authN (Sec 8.5).
- [`implementation-plan.md`](../../docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md)
  -- phased stage / step / scenario plan that this scaffold realises.
- [`e2e-scenarios.md`](../../docs/stories/code-intelligence-CLEAN-CODE/e2e-scenarios.md)
  -- end-to-end Given/When/Then scenarios that gate the service.

If those docs and this scaffold disagree, the docs win -- open a PR
that fixes the code, not one that rewrites the spec.

## Layout

```
services/clean-code/
├── cmd/              # main packages, one per binary (clean-coded, future workers)
│   └── clean-coded/  # primary service binary: /healthz + /readyz + future surfaces
├── internal/         # service-private libraries
│   ├── config/       # env + file config loader; exposes the five operator pins
│   ├── health/       # /healthz + /readyz HTTP handler + readiness check registry
│   ├── logging/      # slog JSON wrapper with request-id propagation
│   └── version/      # Version / Commit / BuildTime set by `-ldflags -X`
├── migrations/       # ordered SQL migrations (Stage 1.2+ adds the DDL)
├── pkg/              # reusable, importable libraries (none yet)
├── proto/            # protobuf / gRPC service definitions (none yet)
├── web/              # static assets / mgmt UI bundles (none yet)
├── Dockerfile        # multi-stage build for the clean-coded container
└── deploy/
    └── local/        # docker compose stack for local dev + CI integration
        ├── docker-compose.yml
        ├── postgres/    # PostgreSQL 16 image with pgcrypto + pg_partman seed
        ├── prometheus/  # Prometheus scrape config
        └── otel/        # OpenTelemetry Collector config
```

## Local development

```
cd services/clean-code
make build      # go build ./... -> bin/clean-coded
make test       # go test -count=1 ./... (portable; no -race)
make test-race  # go test -race -count=1 ./... (CGO; Linux CI only)
make lint       # golangci-lint run ./...
```

`make test` is the portable target invoked from any developer laptop.
`make test-race` is the same suite with the race detector on and is
what CI runs on Linux runners where CGO is available.

The local dependency stack (PostgreSQL 16 with `pgcrypto`, Prometheus
scrape target, OTel Collector, plus the `clean-coded` service itself)
is started with:

```
make compose-up   # docker compose up -d --build
make compose-down # docker compose down -v
```

See [`deploy/local/README.md`](deploy/local/README.md) for the
per-container details (host ports, healthcheck commands, environment
overrides).

## Configuration

`internal/config/` exposes the five normative operator pins from
architecture Sec 1.6 as typed fields with defaults pinned in
tech-spec Sec 8.2 / Sec 1.6:

| Operator pin                       | Env var                                   | Default                              |
| ---------------------------------- | ----------------------------------------- | ------------------------------------ |
| `ast-mode-default`                 | `CLEAN_CODE_AST_MODE_DEFAULT`             | `embedded`                           |
| `external-metric-coverage-format`  | `CLEAN_CODE_EXTERNAL_COVERAGE_FORMAT`     | `Cobertura XML`                      |
| `gate-degraded-policy`             | `CLEAN_CODE_GATE_DEGRADED_POLICY`         | `warn`                               |
| `policy-signing-required`          | `CLEAN_CODE_POLICY_SIGNING_REQUIRED`      | `v1 required`                        |
| `refactor-effort-source`           | `CLEAN_CODE_REFACTOR_EFFORT_SOURCE`       | `ML model from historical commits`   |

Network bind addresses and the PostgreSQL DSN are also env-driven; see
[`internal/config/config.go`](internal/config/config.go) for the full
list.

## CI

GitHub Actions workflow:
[`.github/workflows/clean-code-ci.yml`](../../.github/workflows/clean-code-ci.yml).
On every PR / push that touches `services/clean-code/**` or the
workflow file itself it runs:

1. `make lint`
2. `make build`
3. `make test`
4. `make test-race` (Linux runner, CGO available)
5. `docker build` of the `clean-coded` container image
