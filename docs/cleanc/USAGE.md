# `cleanc` — operator usage guide

> **Scope:** the sub-command dispatcher, global-flag surface,
> exit-code contract, reserved-surface rejections, and the
> full `analyze` pipeline (walker, parser fan-out, rule
> engine, refactor planner, report renderer + JSON sidecars,
> JSONL prompt emitter). The `report` re-render verb is
> wired and shares its flag set with `analyze`; the `apply`
> verb is reserved and rejected at the dispatcher pending
> operator pin `cli-l7-authority`.
>
> **Authority order** (per repository `README.md`): when this
> document and the source disagree, the **specs win** —
> `docs/stories/code-intelligence-REFACTOR-GUIDE/`.  This file
> is operator documentation; the contract lives in
> [`tech-spec.md`](../stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md)
> Sec 8 and [`e2e-scenarios.md`](../stories/code-intelligence-REFACTOR-GUIDE/e2e-scenarios.md)
> Phases 1 & 4.

## 1. Synopsis

```
cleanc <subcommand> [flags]
```

Canonical sub-commands (the dispatcher's closed set):

| Verb      | Status             | Purpose                                                              |
| --------- | ------------------ | -------------------------------------------------------------------- |
| `analyze` | implemented        | Walk a repo, evaluate the rule engine, write a markdown report + JSON sidecars (+ optional `--emit-prompts` JSONL). |
| `report`  | implemented        | Re-render markdown from a previously written `findings.json`.        |
| `version` | implemented        | Print binary version + build tag + parser set + rule-pack set.       |
| `apply`   | reserved (exit 64) | Apply a refactor task; pending operator pin `cli-l7-authority`.      |
| `help`    | implemented        | Print global usage (no arg) or per-verb usage (`cleanc help <verb>`).|

The `help`, `-h`, `--help` triplet is accepted at the dispatcher
level; per-sub-command `-h` also works (each verb registers its
own flag-set with `flag.ContinueOnError`).

## 2. Global flags (tech-spec Sec 8.1)

Every flag in this table is registered on **both** `analyze` and
`report` so the two verbs share a byte-identical surface. The
defaults are pinned in `services/clean-code/internal/cli/flags`
(constants `Default*`) and asserted in
`flags_test.go`. The single source of truth for the default
matrix is the flags package, not this document — if the table
below ever disagrees with `flags.go`, the source wins.

| Flag                   | Default       | Notes                                                                                          |
| ---------------------- | ------------- | ---------------------------------------------------------------------------------------------- |
| `--out <path>`         | `""` (stdout) | Markdown report destination; empty string means "write to stdout".                             |
| `--findings <path>`    | `findings.json` | JSON findings artifact destination.                                                          |
| `--emit-prompts <path>`| `""`          | JSONL refactor-prompt sidecar; empty string disables emission (L7 lands in Stage 4.1).         |
| `--policy <path>`      | `""` (embed)  | Policy-bundle directory; empty string uses the embedded `policy/rulepacks/` YAML packs.        |
| `--with-churn`         | `false`       | **Reserved for P2** — rejected with exit 64 on this build.                                     |
| `--top-n <int>`        | `0`           | Hot-spot table cap; `0` means "use the policy default of 20" (`PolicyDefaultTopN`).            |
| `--exit-on <sev>`      | `block`       | Severity threshold for exit code 1; closed set `{info, warn, block}`.                          |
| `--diagnostics <path>` | `""`          | Diagnostics JSON sidecar destination; empty string disables emission.                          |
| `--dev-mode`           | build-tag paired | Default `true` on dev builds (no build tag), `false` on `-tags prod` builds (compile-time). |
| `--telemetry-otlp <url>` | `""`        | **Reserved for a future story** — rejected with exit 64 on this build.                         |

### 2.1 Dev-mode vs prod-mode

The `--dev-mode` default is split across build-tag-paired files
in `internal/cli/flags`:

| Build           | `DefaultDevMode` | Behaviour                                                                |
| --------------- | ---------------- | ------------------------------------------------------------------------ |
| `make build`    | `true`           | Permits unsigned policy bundles (development loop).                      |
| `make build-prod` (with `-tags prod`) | `false` | Compile-time excludes the unsigned-policy bypass (production posture).|

The build-tag matrix is enforced at COMPILE time. A prod build
that imports `flags` gets `DefaultDevMode == false` regardless
of any dispatcher-side overrides.

## 3. Exit codes (tech-spec Sec 8.6)

| Code | BSD name      | When the dispatcher returns it                                                                   |
| ---- | ------------- | ------------------------------------------------------------------------------------------------ |
| `0`  | (success)     | Clean run; no `--exit-on` severity threshold tripped.                                            |
| `1`  | (find)        | Clean run; maximum finding severity met or exceeded `--exit-on` (one of `info`/`warn`/`block`).   |
| `2`  | (walker)      | Walker failure — missing root path, permission denied on a traversed directory, etc.              |
| `64` | `EX_USAGE`    | Operator-facing usage error: unknown sub-command, malformed flag, missing/surplus positional, reserved verb (`apply`), or reserved flag (`--telemetry-otlp` / `--with-churn` / `--snippet-cap-lines`). |
| `70` | `EX_SOFTWARE` | Internal engine error (parser panic, planner crash, renderer I/O failure). |

## 4. Reserved surface (tech-spec Sec 8.1 + e2e Stage 4.4)

The dispatcher actively rejects every entry in this table with
exit `64` AND a literal stderr substring. Each row is exercised
by the table-driven `TestReservedSurface` test in
`cmd/cleanc/main_test.go`; adding a reserved entry to
tech-spec Sec 8.1 requires adding it here AND to that test.

| Reserved entry         | Exit | Stderr substring (literal)                                                |
| ---------------------- | ---- | ------------------------------------------------------------------------- |
| `apply`                | `64` | `not implemented; pending operator pin cli-l7-authority` (no backticks)   |
| `apply`                | `64` | `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3`   |
| `--telemetry-otlp`     | `64` | `--telemetry-otlp is reserved for a future story`                         |
| `--with-churn`         | `64` | `--with-churn is reserved for P2 and rejected in P0/P1`                   |
| `--snippet-cap-lines`  | `64` | `reserved for a future minor release`                                     |

> **Implementation note.** `--snippet-cap-lines` is NOT
> registered on the analyze/report flag-set (it is reserved,
> not optional). The dispatcher pre-scans `args` with
> `flags.IsReservedSnippetCapLinesArg` BEFORE `fs.Parse` runs,
> so the contract-mandated substring is emitted instead of the
> stdlib's `flag provided but not defined` error. Supported
> forms: `--snippet-cap-lines`, `-snippet-cap-lines`,
> `--snippet-cap-lines=<value>`, `-snippet-cap-lines=<value>`.

## 5. `cleanc version` output format

Line 1 (strict regex, e2e-scenarios.md line 146):

```
cleanc <SEMVER> (build-tag=(|prod)) (parsers=<csv>) (rule-packs=<csv>)
```

Followed by impl-plan substrings (line 41) and operator-debug
stamps (commit + build_time):

```
version=<full-semver>
commit=<git-sha>
build_time=<rfc3339>
parsers=[go,python,typescript,java]
rule-packs=[decoupling,solid]
```

Both the `parsers=` and `rule-packs=` CSV values are
asserted as sets (not strings) by the test suite so additions
do NOT have to be appended at the end.

## 5.1 Dev-mode banner (architecture Sec 7.2, constraint C10)

When the binary runs with `--dev-mode=true` (the default on
`make build`), the dispatcher writes a single LOUD warning line to
**stderr** before the first analyze artifact, exactly:

```
WARNING: dev-mode policy is unsigned. Do NOT use cleanc output as the source of truth for a production gate.
```

The banner is followed by a single `\n`. The byte-for-byte content is
pinned in [`internal/cli/devpolicy/bypass.go`](../../services/clean-code/internal/cli/devpolicy/bypass.go)
(`BannerText` constant) and asserted by
`TestEmitBanner_PinsByteForByteContent`; any rewording is a
breaking change to the operator-facing contract.

Behaviour summary:

| Build               | `--dev-mode` default | Banner emitted? | Unsigned policy bundle accepted? |
| ------------------- | -------------------- | --------------- | -------------------------------- |
| `make build`        | `true`               | Yes (stderr)    | Yes (`devpolicy` YAML loader)    |
| `make build-prod`   | `false`              | No              | No (`ErrDevModeUnavailable`)     |

The prod binary literally does not link the YAML decoder (the
`unsigned_dev.go` build-tag-paired file is excluded via
`//go:build !prod`), so passing `--dev-mode=true` on the prod build
also fails closed at startup with
`devpolicy: dev-mode policy bypass not available in prod build`
and exit code 64.

## 6. Example invocations

```powershell
# Print the version header + impl-plan substrings.
cleanc version

# Per-verb help (prints usage + flag table).
cleanc help analyze
cleanc analyze -h

# Stage 5.x — full analyze pipeline (walker, rule engine,
# planner, report + JSON sidecars). Exits 0 on a clean run,
# 1 when a finding crosses --exit-on, or a >=64 BSD code
# on usage / internal failures (see §3).
cleanc analyze .
cleanc analyze . --out report.md --findings findings.json --exit-on warn

# Reserved-flag rejection — exits 64.
cleanc analyze . --telemetry-otlp http://localhost:4317
cleanc analyze . --with-churn
cleanc analyze . --snippet-cap-lines 100

# Reserved verb — exits 64 with pin id + architecture pointer.
cleanc apply 00000000-0000-0000-0000-000000000000

# Unknown verb — exits 64 with the literal phrase `unknown sub-command`.
cleanc not-a-verb
```

## 6.1 End-to-end golden tests

The shell-driven harness at `tests/e2e/cleanc/` exercises the
binary end-to-end against checked-in sample repos and diffs the
produced artifacts against checked-in golden files. The harness
is **compose-less** because `cleanc` is a single static binary
with no PostgreSQL / HTTP / docker-stack dependencies, so each
scenario is a plain `bash run.sh` with no `docker compose up`
in front of it.

```bash
# Run every scenario sequentially.  Invoke via `bash` so the
# executable bit on run.sh / run_all.sh does not need to
# survive a git checkout where core.fileMode drops it.
bash ./tests/e2e/cleanc/run_all.sh

# Or run one scenario in isolation.
bash ./tests/e2e/cleanc/scenarios/p0-go-cycle/run.sh
```

Per-scenario layout:

```text
tests/e2e/cleanc/scenarios/<name>/
  repo/        # tarball-able sample repo (the cleanc input)
  golden/      # checked-in expected artifacts
  run.sh       # builds cleanc, runs analyze, normalises outputs,
               # diffs against golden/
```

The two P0 scenarios shipped today are:

| Scenario          | Sample repo                             | Assertion                                                  |
| ----------------- | --------------------------------------- | ---------------------------------------------------------- |
| `p0-go-cycle`     | Go module with a `pkg/a ↔ pkg/b` cycle  | byte-match against golden `report.md`, `findings.json`, `diag.json` |
| `p0-mixed-langs`  | one source file each of Go/Py/TS/Java   | `RunArtifact.Files[].language` contains all four languages  |

`findings.json` byte-match goes through `lib/normalize.jq`, which
masks the random `EvaluationRunID` / `VerdictID` / `FindingID` /
`HotSpotID` / `RefactorPlanID` / `RefactorTaskID` UUIDs to
`"<uuid>"`, every ISO-8601 timestamp to `"<timestamp>"`, and
canonicalises array order via `sort_by(tojson)`. `report.md`
byte-match goes through `lib/normalize-md.sh`, which sorts any
contiguous block of `- ` bullet lines so the engine's
non-deterministic insertion order doesn't bleed into the diff.

`diag.json` carries no UUIDs and no timestamps -- only
dark-metric counts and the effort-source tag -- so it is
byte-matched as-is.

## 6.2 P0 walkthrough -- `analyze` + report

P0 is the minimum-viable analyze loop: walk a repo, evaluate the
embedded rule packs, write a markdown report + a `findings.json`
JSON sidecar. No AI-coder hand-off, no patch generation.

```bash
# 1. Build the dev binary (no build tag, --dev-mode default true).
cd services/clean-code
make build

# 2. Analyze a local repo. The dev-mode banner lands on stderr
#    immediately; the markdown report lands on --out (or stdout
#    when --out is empty).
bin/cleanc analyze /path/to/repo \
    --out report.md \
    --findings findings.json \
    --diagnostics diag.json \
    --exit-on warn

# 3. Re-render the markdown report from the JSON sidecar without
#    re-running the walker + engine + planner pipeline.
bin/cleanc report --findings findings.json --out report.md
```

The artifacts:

| File              | Format    | Contents                                                                                       |
| ----------------- | --------- | ---------------------------------------------------------------------------------------------- |
| `report.md`       | markdown  | Operator-facing summary: findings table, hot-spot table, categorical refactor task list.       |
| `findings.json`   | JSON      | Stable machine-readable run artifact (`report.RunArtifact`); the canonical re-render source.   |
| `diag.json`       | JSON      | Per-`(metric_kind, language)` dark-metric inventory + effort-source tag (`ml` vs `fallback`).  |

Exit codes follow §3 above: `0` on a clean run, `1` when the maximum
finding severity meets or exceeds `--exit-on`, `2` on walker failure,
`64` on usage error, `70` on internal engine failure.

## 6.3 P1 walkthrough -- `--emit-prompts` workflow with an AI coder

P1 adds the L7 Option A structured-prompt emitter: one JSONL record
per `RefactorTask`, ready to be piped into an AI coder (Copilot Chat,
Claude, etc.) for patch synthesis. The wire shape is documented in
[`PROMPT-FORMAT.md`](PROMPT-FORMAT.md) and pinned by
[`internal/cli/suggest/record.go`](../../services/clean-code/internal/cli/suggest/record.go).

```bash
# 1. Run analyze with --emit-prompts. The JSONL sidecar lands
#    AFTER the markdown report and findings.json so the operator
#    can pipe it into a separate tool without re-running analysis.
bin/cleanc analyze . \
    --out report.md \
    --findings findings.json \
    --emit-prompts prompts.jsonl

# 2. Inspect one record at a time (JSONL == one JSON object per line).
head -n 1 prompts.jsonl | jq .

# 3. Hand a single record off to an AI coder. The exact framing
#    depends on the tool; the canonical pattern is:
#
#      "Here is a structured refactor request emitted by cleanc.
#       Synthesise a unified diff that implements the
#       prose_suggestion against the source_snippet, preserving
#       the scope's signature and respecting the metric_evidence
#       thresholds. Reply with the diff only."
#
#    Then paste the JSON object as the message body.
jq -c '.' prompts.jsonl | head -n 1 | \
    pbcopy   # or: xclip -selection clipboard, clip.exe, etc.
```

The emitter is **strictly downstream** of the planner: it never
rewrites source bytes (per CLEAN-CODE architecture Sec 1.2 "no
auto-fix" clause) and the JSONL sidecar is the AI coder's only
hand-off surface today. The `apply` verb -- which would land
mechanical patches -- is reserved at exit code 64 pending operator
pin `cli-l7-authority` (architecture Sec 6.3).

## 6.4 Build-tag matrix recap

| Build               | Build tag | `--dev-mode` default | Unsigned policy YAML loader | Banner |
| ------------------- | --------- | -------------------- | --------------------------- | ------ |
| `make build`        | (none)    | `true`               | linked (`unsigned_dev.go`)  | yes    |
| `make build-prod`   | `prod`    | `false`              | excluded (`unsigned_prod.go` sentinel returns `ErrDevModeUnavailable`) | no |

The mutual-exclusion is compile-time fused -- the prod binary
literally does not link the YAML decoder, so the unsigned-policy
bypass cannot be smuggled in via a runtime flag, environment
variable, or hidden subcommand. The `build-prod` job in
`.github/workflows/clean-code-ci.yml` enforces both halves: it runs
`make build-prod` (proving the prod binary compiles) and then
`go test -tags prod -run TestProdBuildExcludesDevBypass
./internal/cli/devpolicy/...` (proving the sentinel ships in place
of the loader). See architecture Sec 7.2 / tech-spec Sec 8.9 for
the normative pins.

## 7. Cross-references

- `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
  - **Sec 8.1** — flag defaults.
  - **Sec 8.6** — exit codes.
  - **Sec 8.9** — build-tag matrix (`-tags prod` excludes the unsigned-policy bypass).
  - **Sec 8.10** — `cmd/cleanc` linter constraint (no `*_sql_store` / `*sql.DB` imports).
- `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
  - **Sec 1.3** — `cli-binary-location` operator pin (binary lives next to the six service binaries).
  - **Sec 6.3** — `cli-l7-authority` operator pin gating the `apply` verb.
- `docs/stories/code-intelligence-REFACTOR-GUIDE/e2e-scenarios.md`
  - **Phase 1** — version output, sub-command surface, `analyze` missing-path.
  - **Phase 4 / Stage 4.4** — reserved surface (`apply` + `--telemetry-otlp` + `--with-churn` + `--snippet-cap-lines`).
- `docs/stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md`
  - **Stages 1.1 / 1.2 / 1.4 / 2.x / 3.x / 4.x / 5.x** — the
    incremental rollout from the original CLI binary skeleton to
    today's fully wired walker + parser + rule engine + planner
    + report + JSON sidecars + JSONL emitter surface.  All
    stages cited above have shipped; the implementation-plan
    history is preserved verbatim there for audit traceability.

## 8. Implementation history

The CLI shipped incrementally across the REFACTOR-GUIDE
implementation-plan stages.  The original Stage 1.1 binary
skeleton (sub-command dispatcher, global-flag surface, exit
codes, reserved-surface rejections) has been **fully
superseded** by the wired analyze / report pipeline described
in §1–§6 above.  The Stage 1.1 support packages are still
present in the source tree because the downstream stages
extended them rather than replacing them wholesale:

- `cmd/cleanc/` — entry, sub-command dispatcher, build-tag-paired defaults.
- `internal/cli/flags/` — exit codes, verb names, flag defaults.
- `internal/cli/devpolicy/` — `Loader` interface, source-resolution
  choice point (`LoaderSource.FS`), `ErrDevModeUnavailable` /
  `ErrLoaderNotYetImplemented` sentinels, `BannerText` /
  `EmitBanner`, and the build-tag matrix (`unsigned_dev.go` /
  `unsigned_prod.go`). The dev-build `Load` body is a stub
  that returns `ErrLoaderNotYetImplemented`.
- `internal/cli/repocontext/` — foundational `MintRepoID`,
  `DetectHeadSHA`, `DetectModulePath` helpers.
- `internal/cli/scopebinding/` — foundational `ScopeBinding`
  struct, `MintScopeID` / `TryMintScopeID`, concurrent `Table`,
  string-keyed `Store`.
- `internal/cli/effort/` — `FallbackModel.Estimate` deterministic
  effort score used when the ONNX model is unavailable.

The next-layer integrations have all landed in downstream
workstreams: **Stage 1.2** wired `repocontext` / `scopebinding`
into the walker / orchestrator; **Stage 1.4** swapped the
dev-build `Loader.Load` stub for the real YAML decoder +
unsigned `steward.PolicyVersion` synthesiser; **Stage 1.5+**
left the deterministic `effort` fallback in place because the
ONNX model is still optional.  See the implementation-plan
history for the per-stage diff trail.

The operator's literal 2026-05-30 scope phrasing is preserved
verbatim in the `Stage11ScopeNote` constant in
`services/clean-code/cmd/cleanc/doc.go` (pinned by
`TestStage11ScopeNote_PinsOperatorDecision`) for audit
traceability against the iter-9 baseline commit.
