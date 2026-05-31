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
├── cmd/              # one package per binary; Makefile CMD_DIRS auto-discovers them
│   ├── cleanc/                       # Stage 1.1 dev-laptop CLI (see docs/cleanc/USAGE.md)
│   ├── clean-code-aggregator/        # aggregator service
│   ├── clean-code-eval-gate/         # eval.gate transport
│   ├── clean-code-gateway/           # public gateway
│   ├── clean-code-indexer/           # indexer (Stage 1 stub)
│   ├── clean-code-metric-ingestor/   # metric ingestor
│   └── clean-code-refactor-planner/  # refactor planner
├── internal/         # service-private libraries
│   ├── config/       # env + file config loader; exposes the five operator pins
│   ├── health/       # /healthz + /readyz HTTP handler + readiness check registry
│   ├── logging/      # slog JSON wrapper with request-id propagation
│   └── version/      # Version / Commit / BuildTime set by `-ldflags -X`
├── migrations/       # ordered SQL migrations (Stage 1.2+ adds the DDL)
├── pkg/              # reusable, importable libraries (none yet)
├── proto/            # protobuf / gRPC service definitions (none yet)
├── web/              # static assets / mgmt UI bundles (none yet)
├── Dockerfile        # multi-stage build (SERVICE build-arg picks which cmd/* to ship)
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
make build      # builds bin/<cmd> for every cmd/*/main.go (see CMD_DIRS in Makefile)
make build-prod # same fleet built with `-tags prod`, plus bin/cleanc-prod alias
make test       # go test -count=1 ./... (portable; no -race)
make test-race  # go test -race -count=1 ./... (CGO; Linux CI only)
make lint       # golangci-lint run ./...
```

`make test` is the portable target invoked from any developer laptop.
`make test-race` is the same suite with the race detector on and is
what CI runs on Linux runners where CGO is available.

### Build-tag matrix (dev vs. prod)

The `cleanc` CLI ships in two flavours selected at compile time via
Go build tags. `make build` emits the **dev** binary (no build tag);
`make build-prod` emits the **prod** binary (`-tags prod`) and an
explicit `bin/cleanc-prod` alias. The two flavours differ only in
`internal/cli/devpolicy`: `unsigned_dev.go` carries `//go:build !prod`
and ships the YAML-decoding unsigned-policy loader the dev mode
needs, while `unsigned_prod.go` carries `//go:build prod` and ships
a sentinel loader whose `Load` always returns
`devpolicy.ErrDevModeUnavailable` (`"devpolicy: dev-mode policy
bypass not available in prod build"`) at the earliest reachable
layer (architecture Sec 7.2). The mutual-exclusion is compile-time
fused -- a prod binary literally does not link the YAML decoder, so
the unsigned-policy bypass cannot be smuggled in via a flag, env
var, or hidden subcommand. The `build-prod` job in
`.github/workflows/clean-code-ci.yml` enforces both halves: it runs
`make build-prod` (proving the prod binary compiles) and then
`go test -tags prod -run TestProdBuildExcludesDevBypass
./internal/cli/devpolicy/...` (proving the sentinel ships in place
of the loader). The guarantee lives in a build-tag-gated unit
test, NOT in a hidden CLI subcommand, because architecture Sec 3.6
and tech-spec Sec 4.1 pin the CLI surface to four subcommands only.

The local dependency stack (PostgreSQL 16 with `pgcrypto`, Prometheus
scrape target, OTel Collector, plus the clean-code service container
itself) is started with:

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
5. `docker build` of the clean-code service container image

### End-to-end cleanc golden tests

`tests/e2e/cleanc/` ships a shell-driven, compose-less harness that
exercises the `cleanc` binary against checked-in sample repos and
diffs the produced `report.md` / `findings.json` / `diag.json`
against checked-in golden files. The harness is compose-less because
`cleanc` is a single static binary with no PostgreSQL / HTTP /
docker-stack dependencies, so each scenario is a plain `bash run.sh`
with no `docker compose up` in front of it.

Run all scenarios locally:

```bash
bash ./tests/e2e/cleanc/run_all.sh
```

The two P0 scenarios shipped today are:

- `tests/e2e/cleanc/scenarios/p0-go-cycle/` — Go fixture with a
  package-level import cycle; asserts byte-match against a
  path-normalised, UUID-/timestamp-masked golden report.
- `tests/e2e/cleanc/scenarios/p0-mixed-langs/` — one source file
  per supported language (Go / Python / TypeScript / Java);
  asserts all four languages appear in `RunArtifact.Files`.

See [`tests/e2e/cleanc/README.md`](../../tests/e2e/cleanc/README.md)
for the normalisation strategy and how to add new scenarios.

## `cleanc` CLI

`bin/cleanc` is the single-binary, no-server developer-laptop CLI
delivered for the `code-intelligence:REFACTOR-GUIDE` story. It walks a
local repo, evaluates the embedded rule packs, and writes a markdown
report + JSON sidecars (+ an optional JSONL refactor-prompt stream for
AI coders). There is no PostgreSQL, no HTTP gateway, and no docker
stack -- everything ships in one statically-linked Go binary.

End-user docs:

- [`docs/cleanc/USAGE.md`](../../docs/cleanc/USAGE.md) -- operator
  walkthrough: P0 (`analyze` + report), P1 (`--emit-prompts` workflow
  with an AI coder), dev-mode banner, build-tag matrix.
- [`docs/cleanc/PROMPT-FORMAT.md`](../../docs/cleanc/PROMPT-FORMAT.md)
  -- the `RefactorPromptRecord` JSONL contract consumed by AI coders.

### Install

```bash
cd services/clean-code
make build            # emits bin/cleanc (dev build, --dev-mode default true)
make build-prod       # emits bin/cleanc-prod (-tags prod, --dev-mode default false)
```

`make build` writes one binary per `cmd/*/main.go` under `bin/`; the
`cleanc` entry lives at `bin/cleanc`. The prod variant additionally
emits `bin/cleanc-prod` so the two flavours can coexist in the same
working tree.

### Basic usage

```bash
bin/cleanc analyze <repo-path>                          # report to stdout
bin/cleanc analyze . --out report.md                    # report to file
bin/cleanc analyze . --out report.md --findings findings.json
bin/cleanc analyze . --emit-prompts prompts.jsonl       # P1 AI-coder hand-off
bin/cleanc version                                      # version + parsers + rule-packs
bin/cleanc help analyze                                 # per-verb usage
```

Sub-command surface (the dispatcher's closed set):

| Verb      | Status             | Purpose                                                                                |
| --------- | ------------------ | -------------------------------------------------------------------------------------- |
| `analyze` | implemented        | Walk a repo, evaluate the rule engine, write a markdown report + JSON sidecars (+ optional `--emit-prompts` JSONL). |
| `report`  | implemented        | Re-render markdown from a previously written `findings.json`.                          |
| `version` | implemented        | Print binary version + build tag + parser set + rule-pack set.                         |
| `apply`   | reserved (exit 64) | Apply a refactor task; pending operator pin `cli-l7-authority` (architecture Sec 6.3). |
| `help`    | implemented        | Print global usage (no arg) or per-verb usage (`cleanc help <verb>`).                  |

### Flag table (tech-spec Sec 8.1)

Every flag below is registered on **both** `analyze` and `report` so
the two verbs share a byte-identical surface. The defaults are pinned
in `services/clean-code/internal/cli/flags` and asserted by
`flags_test.go` -- the source mirrors this contract; any drift is a
bug in the source, not in this document (per the repo / story
"docs win" authority rule).

| Flag                     | Default          | Purpose                                                                                          |
| ------------------------ | ---------------- | ------------------------------------------------------------------------------------------------ |
| `--out <path>`           | stdout (`""`)    | Markdown report path.                                                                            |
| `--findings <path>`      | `findings.json`  | Machine-readable run artifact (JSON sidecar).                                                    |
| `--emit-prompts <path>`  | unset (`""`)     | L7 Option A JSONL refactor-prompt emitter; empty disables emission.                              |
| `--policy <path>`        | embedded (`""`)  | Override the embedded rule packs with a directory of YAML files.                                 |
| `--with-churn`           | `false`          | Reserved for P2; rejected in P0/P1 with exit 64.                                                 |
| `--top-n <int>`          | `0`              | Hot-spot table cap; `0` means "use policy default of 20" (`PolicyDefaultTopN`).                  |
| `--exit-on <sev>`        | `block`          | Severity threshold for exit 1; closed set `{info, warn, block}`.                                 |
| `--diagnostics <path>`   | unset (`""`)     | Dark-metric inventory + effort-source JSON sidecar; empty disables emission.                     |
| `--dev-mode`             | build-tag paired | `true` on `make build`, `false` on `make build-prod` (`-tags prod`).                             |
| `--telemetry-otlp <url>` | unset (`""`)     | Reserved for a future story; rejected in P0/P1 with exit 64.                                     |

The `--snippet-cap-lines <int>` flag is **reserved** for a future minor
release (tech-spec Sec 8.2) and is not registered today; the
dispatcher pre-scans argv for it and exits 64 with a literal
`reserved for a future minor release` stderr substring.

### Exit-code matrix (tech-spec Sec 8.6)

| Code | BSD name      | When the dispatcher returns it                                                                   |
| ---- | ------------- | ------------------------------------------------------------------------------------------------ |
| `0`  | (success)     | Clean run; no `--exit-on` severity threshold tripped.                                            |
| `1`  | (find)        | Clean run; maximum finding severity met or exceeded `--exit-on`.                                 |
| `2`  | (walker)      | Walker failure -- missing root path, permission denied, non-readable directory.                   |
| `64` | `EX_USAGE`    | Invalid usage: bad/unknown flag, missing/surplus positional, reserved verb (`apply`) or reserved flag (`--telemetry-otlp`, `--with-churn`, `--snippet-cap-lines`). |
| `70` | `EX_SOFTWARE` | Internal engine error: parser panic, rule-engine internal error, non-canonical TaskKind reached the emitter, renderer I/O failure. |

No other codes are emitted in P0/P1. Codes `3`-`63` and `65`-`69` are
reserved for forward compatibility (P3 may add `3` for "patch apply
conflict"); the dispatcher MUST NOT alias them today.

### Stage 1.1 -- `cleanc` CLI binary

`bin/cleanc` is the Stage 1.1 deliverable -- a single-binary, no-server
dev-laptop CLI for `cleanc analyze <repo-path>` (see workstream
`ws-code-intelligence-refactor-guide-phase-foundations-stage-cli-binary-skeleton`).

Stage 1.1 owns the dispatcher, the global flag surface, and the
foundational internal CLI support packages required to make the
skeleton self-contained:

- `cmd/cleanc/` -- entry, sub-command dispatcher, build-tag-gated defaults.
- `internal/cli/flags/` -- exit codes, verb names, flag defaults.
- `internal/cli/devpolicy/` -- bypass interface, dev/prod build-tag
  paired sentinels (`unsigned_dev.go`, `unsigned_prod.go`), embed
  alias to `policy/rulepacks.EmbeddedFS`, and the foundational
  `loader.go` that wires sentinels + embed together.
- `internal/cli/repocontext/` -- `MintRepoID`, `DetectHeadSHA`,
  `DetectModulePath` foundational helpers.
- `internal/cli/scopebinding/` -- `ScopeBinding`, `Store`, `Table`
  foundational types.
- `internal/cli/effort/` -- foundational `fallback` constants for
  the dev-mode effort score path.

These foundational packages compile and pass tests as part of
Stage 1.1 so the skeleton runs end-to-end. Subsequent stages own
the next-layer integrations against them: Stage 1.2 (repo context
wiring into the orchestrator), Stage 1.4 (the full dev-mode policy
loader pipeline that consumes the sentinels), and Stage 1.5+ (the
ONNX effort model that supersedes the fallback). See
[`../../docs/cleanc/STAGE-1-1-STATUS.md`](../../docs/cleanc/STAGE-1-1-STATUS.md)
for the per-acceptance-criterion mapping to code witnesses.

Operator-facing usage, exit codes, and the Stage 1.1 scope boundary
appendix live in [`../../docs/cleanc/USAGE.md`](../../docs/cleanc/USAGE.md).
The same boundary is code-pinned by `Stage11ScopeNote` in
[`cmd/cleanc/doc.go`](cmd/cleanc/doc.go).
