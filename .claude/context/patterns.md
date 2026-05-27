# Patterns

> Last updated: 2026-05-27

## Repository conventions

- Treat `docs/stories/**` as normative design input. Root and service READMEs explicitly say specs win over code when they disagree.
- Each service is its own Go module with its own `go.mod`, `Makefile`, `.golangci.yml`, migrations, deploy files, and tests.
- Use `cmd/` for executable composition roots, `internal/` for service-private libraries, `pkg/` only for intentionally reusable packages, and `proto/` for IDL/generated bindings.
- Keep generated protobuf bindings checked in and regenerate with the service `make proto` target after IDL changes.
- Follow `.editorconfig`: LF, final newline, trim trailing whitespace except Markdown, tabs for Go/Makefile, 2 spaces for YAML/JSON/TOML/SQL/proto.

## Package boundaries

Narrow interfaces are preferred at package boundaries. Examples:

- `agent-memory/internal/agentapi` defines small read-side interfaces such as `QueryEmbedder`, `VectorSearcher`, `PublishFilter`, and `HealthSource` instead of importing broad concrete dependencies.
- `clean-code/internal/evaluator` keeps signature verification independent from the rule-evaluation pipeline.
- Binary `cmd/*` packages do the concrete wiring; domain packages stay transport- or infrastructure-light where possible.

## Construction and options

- Constructors are usually named `New` or `New*`.
- Required dependencies fail fast when nil if a half-wired service is unsafe, e.g. `graphwriter.New` and `agentapi.NewGRPCServer` panic on required nil dependencies.
- Optional dependencies use option functions or zero-value defaults, e.g. `agentapi.WithObserveService`, `health.SetMandatoryChecks`, and handler `Options` structs.
- Test seams are explicit fields/functions such as clocks, secret generators, fake stores, health sources, and metric observers.

## Error handling

- Prefer sentinel errors for caller-visible categories and wrap details with `%w`.
- Use typed errors where callers need structured classification, e.g. `graphwriter.WriteContractViolation` wraps PostgreSQL SQLSTATE 42501.
- Map domain errors to transport status in a single adapter layer; `agentapi/grpc_server.go` maps validation/domain errors to gRPC codes.
- Avoid silent success-shaped fallbacks. Where a dependency is optional, handlers generally return `Unimplemented`, `Not Ready`, or stamped degraded responses rather than pretending success.

## Logging and observability

- Structured logging uses `log/slog`; `clean-code/internal/logging` wraps `slog.JSONHandler` and propagates request IDs through context.
- Writer/handler packages aim for one structured audit/event record per public operation.
- Degraded states are explicit closed-set values. Responses may carry degraded envelopes, and metrics count `(verb, degraded_reason)` pairs.
- OpenTelemetry spans are first-class in agent-memory ingestion and exposed in both local stacks through OTel Collector.

## Persistence and identity

- Append-only storage is a recurring design rule. Updates are replaced by new rows, state logs, tombstones, or retractions depending on the model.
- Deterministic identity matters:
  - agent-memory Node/Edge fingerprints include repo, kind, canonical signature or endpoints, and first-seen SHA.
  - clean-code scope IDs use deterministic UUIDs derived from repo/scope/canonical signature/first-seen SHA.
- PostgreSQL role grants are treated as enforcement mechanisms, not just documentation.

## Testing patterns

- Unit tests live next to packages as `*_test.go`.
- SQL behavior is covered with `go-sqlmock` and live PostgreSQL integration tests where required.
- E2E scenarios are represented by `.feature` files plus generated/paired Go tests under service `test/e2e/` folders.
- CGO/tree-sitter and non-CGO fallback paths are tested separately in clean-code.
- CI also runs pre-commit hooks, formatter/vet checks, and local stack healthchecks.

