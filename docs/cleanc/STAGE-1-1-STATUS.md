# Stage 1.1 acceptance status — `cleanc` CLI binary skeleton

> **Scope:** mapping of each Stage 1.1 acceptance criterion from
> [`implementation-plan.md`](../stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md)
> §"Stage 1.1: CLI Binary Skeleton" to its in-tree code anchor and
> pinning test, plus the open environmental issue that has
> intermittently tripped the per-iter test gate.
>
> **Pairs with** [`USAGE.md`](USAGE.md) (operator how-to). USAGE
> tells you how to drive the CLI; this file tells you which
> contracts are pinned where and what is currently flaky.
>
> **Authority:** when this file disagrees with the source, the
> specs win (per repository `README.md`).

## 1. Acceptance criteria → code witnesses

| Impl-plan criterion (Stage 1.1) | Code anchor | Pinning test(s) |
| --- | --- | --- |
| Sub-command dispatcher returns the closed exit-code set `{0, 1, 2, 64, 70}` (tech-spec Sec 8.6) | `services/clean-code/cmd/cleanc/main.go` | `TestExitCodeValuesPinned`, `TestUnknownSubcommandExitsUsage`, `TestNoArgsExitsUsage` |
| Global flag surface (`--out`, `--findings`, `--emit-prompts`, `--policy`, `--with-churn`, `--top-n`, `--exit-on`, `--diagnostics`, `--dev-mode`, `--telemetry-otlp`) with tech-spec Sec 8.1 defaults | `services/clean-code/internal/cli/flags/flags.go` | `TestReportRegistersFullGlobalFlagSurface`, `TestGlobalsValidateAcceptsDefaults`, `TestDefaultExitOnPinned`, `TestDefaultFindingsPinned` |
| `cleanc version` line matches the e2e regex `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$` and contains impl-plan substrings | `services/clean-code/cmd/cleanc/main.go` (`runVersion`) | `TestVersionFormatMatchesE2ERegex`, `TestVersionContainsImplPlanSubstrings`, `TestBuildTagIsEmptyOnDevBuild` |
| `cleanc help <verb>` text scraped from per-verb `usage` constant; unknown verb → exit 64 with literal `unknown sub-command` | `services/clean-code/cmd/cleanc/main.go` (`usage*` consts) | `TestHelpNoArgExitsZero`, `TestHelpVerbPrintsPerVerbUsage`, `TestUnknownSubcommandExitsUsage` |
| Makefile `CMD_DIRS` auto-discovers `cleanc` (no per-binary list edit needed) | `services/clean-code/Makefile` (`CMD_DIRS` glob over `cmd/*/main.go`) | `make build` smoke verification |

## 2. Operator scope pin (Stage 1.1 boundary)

The operator answered the scope question on 2026-05-30 (Option A):
**Stage 1.1 ships the CLI skeleton + global-flag surface only.**
`internal/cli/repocontext/`, `internal/cli/scopebinding/`, and the
`internal/cli/devpolicy` loader **body** are deferred to Stages 1.2
and 1.4 respectively; Stage 1.1 keeps only the devpolicy package
**shell** so the dispatcher imports compile.

Code-level witness: the exported package constant
[`Stage11ScopeNote`](../../services/clean-code/cmd/cleanc/doc.go)
(pinned by `TestStage11ScopeNote_PinsOperatorDecision` in
`services/clean-code/cmd/cleanc/main_test.go`).

Operator-doc witness:
[`USAGE.md` §8 "Stage 1.1 scope boundary"](USAGE.md#8-stage-11-scope-boundary).

## 3. Open: recurring environmental gate flake

The per-iter Forge test gate has intermittently failed with
`FAIL services/clean-code/policy/rulepacks [build failed]` while
the sibling `policy/rulepacks/solid/` and
`policy/rulepacks/decoupling/` packages built and tested OK in the
same run.

**Iter history (this workstream):** iter 5, 7, 9, 10, 13 failed
the test gate with this signature. Iter 6, 8, 11, 12 passed.
The flake has not been reproduced on the developer workstation
(local `go test ./... -run '<gate-regex>'` from repo root: 5/5
exit-0 runs, 17–24s each).

**Structural hypothesis:** the same eight YAML files
(`policy/rulepacks/solid/*.yaml`,
`policy/rulepacks/decoupling/*.yaml`) are opened by the Go
compiler for THREE `//go:embed` directives that get built in
parallel by `go test ./...`:

- `policy/rulepacks/embedded_fs.go` →
  `//go:embed solid/*.yaml decoupling/*.yaml`
- `policy/rulepacks/solid/loader.go` → `//go:embed *.yaml`
- `policy/rulepacks/decoupling/loader.go` → `//go:embed *.yaml`

On Windows runners with active virus scanning, parallel-compiler
opens of the same byte ranges can transiently produce a sharing
violation, which the toolchain surfaces as `[build failed]` for
exactly one of the three packages. The pattern (parent fails,
children pass) and the iter history (parent's embed file was
introduced in iter-5, and all five flakes are after that point)
support this reading without proving it.

**Why not fixed in this stage:** the cleanest structural fix —
remove the parent's duplicate embed and have the siblings consume
`fs.Sub(rulepacks.EmbeddedFS, "<family>")` instead — requires
editing `policy/rulepacks/solid/loader.go` and
`policy/rulepacks/decoupling/loader.go`, both of which are owned
by earlier completed workstreams (`policy-steward-and-solid-rule-engine`
families). That crosses the Stage 1.1 boundary pinned in §2.
Authorising the cross-stage refactor is an operator decision
captured as an open question in this workstream's iter notes.
