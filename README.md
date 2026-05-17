# code-intelligence

Monorepo for the **code-intelligence** family of services. The first
service shipped here is **agent-memory**, a hybrid graph + episodic
memory layer for the `code-intelligence:AGENT-MEMORY` story.

## Layout

```
.
├── docs/
│   └── stories/
│       └── code-intelligence-AGENT-MEMORY/   # architecture, tech-spec, implementation-plan, e2e-scenarios
└── services/
    └── agent-memory/                         # Go service: see services/agent-memory/README.md
```

## Sibling design docs

Authoritative specs for the agent-memory service live under
[`docs/stories/code-intelligence-AGENT-MEMORY/`](docs/stories/code-intelligence-AGENT-MEMORY/):

- [`architecture.md`](docs/stories/code-intelligence-AGENT-MEMORY/architecture.md)
- [`tech-spec.md`](docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md)
- [`implementation-plan.md`](docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md)
- [`e2e-scenarios.md`](docs/stories/code-intelligence-AGENT-MEMORY/e2e-scenarios.md)

If docs and code disagree, the docs win — open a PR that fixes the
code, not one that rewrites the spec.

## Quick start

```
cd services/agent-memory
make build && make test && make lint
```

For the full local dependency stack (PostgreSQL + Qdrant + OTel),
see [`services/agent-memory/deploy/local/README.md`](services/agent-memory/deploy/local/README.md).

## CI

GitHub Actions workflow:
[`.github/workflows/agent-memory-ci.yml`](.github/workflows/agent-memory-ci.yml).
It runs `make lint && make build && make test`, the
`pre-commit` hook set, and the `docker compose` healthcheck +
PostgreSQL extension assertion sweep that Stage 1.1 of the
implementation plan requires.

## Repo conventions

- `.editorconfig` is the source of truth for line endings,
  indentation, and trailing-whitespace policy.
- `.pre-commit-config.yaml` wires `gofmt`, `go vet`, `yamllint`,
  and the standard `pre-commit-hooks` checks. Install locally
  with `pip install pre-commit && pre-commit install`.
- Per-service tooling versions live next to that service
  (e.g. `services/agent-memory/.golangci.yml`,
  `services/agent-memory/go.mod`).
