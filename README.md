# code-intelligence

Monorepo for the **code-intelligence** family of services. Two
services ship here today:

- **agent-memory** -- a hybrid graph + episodic memory layer
  (`code-intelligence:AGENT-MEMORY`).
- **clean-code** -- a clean-code measurement, policy, and refactor
  planning service (`code-intelligence:CLEAN-CODE`).

## Layout

```
.
├── docs/
│   └── stories/
│       ├── code-intelligence-AGENT-MEMORY/   # architecture, tech-spec, implementation-plan, e2e-scenarios
│       └── code-intelligence-CLEAN-CODE/     # ditto, for the clean-code service
└── services/
    ├── agent-memory/                         # Go service: see services/agent-memory/README.md
    └── clean-code/                           # Go service: see services/clean-code/README.md
```

## Sibling design docs

Authoritative specs for the agent-memory service live under
[`docs/stories/code-intelligence-AGENT-MEMORY/`](docs/stories/code-intelligence-AGENT-MEMORY/):

- [`architecture.md`](docs/stories/code-intelligence-AGENT-MEMORY/architecture.md)
- [`tech-spec.md`](docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md)
- [`implementation-plan.md`](docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md)
- [`e2e-scenarios.md`](docs/stories/code-intelligence-AGENT-MEMORY/e2e-scenarios.md)

Authoritative specs for the clean-code service live under
[`docs/stories/code-intelligence-CLEAN-CODE/`](docs/stories/code-intelligence-CLEAN-CODE/):

- [`architecture.md`](docs/stories/code-intelligence-CLEAN-CODE/architecture.md)
- [`tech-spec.md`](docs/stories/code-intelligence-CLEAN-CODE/tech-spec.md)
- [`implementation-plan.md`](docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md)
- [`e2e-scenarios.md`](docs/stories/code-intelligence-CLEAN-CODE/e2e-scenarios.md)

If docs and code disagree, the docs win — open a PR that fixes the
code, not one that rewrites the spec.

## Quick start

```
cd services/agent-memory
make build && make test && make lint

cd ../clean-code
make build && make test && make lint
```

For the agent-memory local dependency stack (PostgreSQL + Qdrant +
OTel), see
[`services/agent-memory/deploy/local/README.md`](services/agent-memory/deploy/local/README.md).

For the clean-code local dependency stack (PostgreSQL + Prometheus
+ OTel + the `clean-coded` service itself), see
[`services/clean-code/deploy/local/README.md`](services/clean-code/deploy/local/README.md).

## CI

GitHub Actions workflows:

- [`.github/workflows/agent-memory-ci.yml`](.github/workflows/agent-memory-ci.yml) --
  `make lint && make build && make test` plus the pre-commit hook
  set and the agent-memory `docker compose` healthcheck +
  PostgreSQL extension assertion sweep.
- [`.github/workflows/clean-code-ci.yml`](.github/workflows/clean-code-ci.yml) --
  `make lint && make build && make test` plus the pre-commit hook
  set and a clean-code container build job.

## Repo conventions

- `.editorconfig` is the source of truth for line endings,
  indentation, and trailing-whitespace policy.
- `.pre-commit-config.yaml` wires `gofmt`, `go vet`, `yamllint`,
  and the standard `pre-commit-hooks` checks. Install locally
  with `pip install pre-commit && pre-commit install`.
- Per-service tooling versions live next to that service
  (e.g. `services/agent-memory/.golangci.yml`,
  `services/agent-memory/go.mod`,
  `services/clean-code/.golangci.yml`,
  `services/clean-code/go.mod`).
