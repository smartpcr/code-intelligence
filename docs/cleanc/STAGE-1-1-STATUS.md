# Stage 1.1 acceptance status — `cleanc` CLI binary skeleton

> **Scope:** mapping of each Stage 1.1 acceptance criterion from
> [`implementation-plan.md`](../stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md)
> §"Stage 1.1: CLI Binary Skeleton" to its in-tree code anchor and
> pinning test.
>
> **Pairs with** [`USAGE.md`](USAGE.md) (operator how-to). USAGE
> tells you how to drive the CLI; this file tells you which
> contracts are pinned where.
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
