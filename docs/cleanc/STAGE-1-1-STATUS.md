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

## 2. Stage 1.1 scope boundary

Stage 1.1 ships the CLI skeleton + global-flag surface **plus**
the foundational support packages required to make the skeleton
self-contained:

- `internal/cli/devpolicy/` ships the `Loader` interface, the
  `LoaderSource.FS` source-resolution choice point, the
  `ErrDevModeUnavailable` / `ErrLoaderNotYetImplemented`
  sentinels, the `BannerText` / `EmitBanner` constraint-C10
  surface, and the build-tag-paired `unsigned_dev.go` /
  `unsigned_prod.go` files. The dev-build `Load` body is a
  Stage 1.1 stub that returns `ErrLoaderNotYetImplemented`;
  Stage 1.4 swaps it for the YAML decoder + unsigned
  `steward.PolicyVersion` synthesiser
  (implementation-plan lines 90-100).
- `internal/cli/repocontext/` ships the foundational
  `MintRepoID`, `DetectHeadSHA`, and `DetectModulePath`
  helpers. Stage 1.2 wires them into the walker / orchestrator
  (implementation-plan lines 46-54).
- `internal/cli/scopebinding/` ships the foundational
  `ScopeBinding` struct, deterministic `MintScopeID` /
  `TryMintScopeID`, the concurrent `Table`, and the
  string-keyed `Store`. Stage 1.2 wires them into the
  walker / orchestrator (implementation-plan lines 46-54).
- `internal/cli/effort/` ships the deterministic
  `FallbackModel.Estimate` used when the ONNX model is
  unavailable. Stage 1.5+ supersedes it with the real ONNX
  estimator.

The operator's literal 2026-05-30 scope phrasing is preserved
verbatim in the exported package constant
[`Stage11ScopeNote`](../../services/clean-code/cmd/cleanc/doc.go)
(pinned by `TestStage11ScopeNote_PinsOperatorDecision` in
`services/clean-code/cmd/cleanc/main_test.go`) for audit
traceability against the iter-9 baseline commit. The
implementation expanded beyond the literal "skeleton only"
phrasing in iter-9 (commit `40b4139`) so the dispatcher
compiles and the skeleton stands alone in the Stage 1.1
deliverable; the next-layer integrations remain owned by
Stages 1.2 / 1.4 / 1.5+.

Operator-doc witness:
[`USAGE.md` §8 "Stage 1.1 scope boundary"](USAGE.md#8-stage-11-scope-boundary).
