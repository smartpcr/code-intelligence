# Story `code-intelligence:REFACTOR-GUIDE` -- End-to-End Scenarios

> Sibling docs (read together, do NOT duplicate):
> - `architecture.md` -- L1 - L9 component layout, in-memory data
>   model (Sec 4), interface bindings (Sec 5), sequence flows
>   (Sec 6), operator pins (Sec 1.3), cross-cutting invariants
>   (Sec 1.4, Sec 10), resolved decisions (Sec 8).
> - `tech-spec.md` -- hard constraints `C1 - C15` (Sec 7),
>   parameter pins (Sec 8: flag defaults, snippet cap, size cap,
>   rule-pack distribution, effort formula, exit codes, dark-metric
>   taxonomy, concurrency, build-tag matrix, lint rules), risk
>   register `R1 - R12` (Sec 9), locked-decisions roll-up `D1 -
>   D15` (Sec 10).
> - `implementation-plan.md` -- five build phases (`Foundations`,
>   `Pipeline`, `P0 Reports and Delivery`, `P1 Structured Prompt
>   Emitter`, `Hardening and Release`) each carrying ordered
>   stages with their own `Test Scenarios` checklists.
>
> Authority: per the repo `README.md`, when docs and code disagree
> the docs win. The base service architecture lives in the
> upstream `docs/stories/code-intelligence-CLEAN-CODE/architecture.md`
> ("CLEAN-CODE arch" below) and remains the single source of
> truth for the metric catalogue, the policy / rule data model,
> the writer-ownership invariants `G1 - G7`, and the refactor
> planner. This document encodes the **QA contract** for the
> `cleanc analyze <repo-path>` CLI -- a developer-laptop
> single-binary on-ramp that scans a local checkout and emits
> refactor suggestions WITHOUT PostgreSQL, WITHOUT a HTTP
> gateway, and WITHOUT a docker stack.

## How to read this doc

- **Phase H1** (`# Phase N: ...`) -- one phase per build slice;
  phase numbers and slugs mirror `implementation-plan.md` H1s
  one-for-one (`Foundations` / `Pipeline` / `P0 Reports and
  Delivery` / `P1 Structured Prompt Emitter` / `Hardening and
  Release`).
- Each Phase H1 is **immediately** followed by `### Setup`
  (H3, exactly five sub-bullets per the evaluator contract) and
  then `### Scenarios` (H3, one or more Gherkin `Feature` blocks).
- **Background blocks** at the top of `### Scenarios` apply to
  every scenario in that phase.
- **Anchors.** Every scenario is anchored with `[arch Sec X.Y]`,
  `[tech-spec Sec X.Y]`, `[tech-spec C##]`, or `[impl-plan Stage
  X.Y]` so the evaluator and downstream agents can prove behaviour
  back to the spec. When an anchor cites the source tree (e.g.
  `services/clean-code/internal/refactor/task_planner.go:77-118`)
  the line range matches the symbol the sibling docs name.
- **Tags** on scenarios:
  - `@happy` -- nominal path; the developer runs `cleanc analyze`
    and the binary produces the expected artifacts.
  - `@edge` -- boundary or unusual input (empty repo, oversize
    file, symlink loop, snippet cap, etc.).
  - `@dark` -- a parser-attr gap path; the recipe is documented
    as silently emitting nothing today (`cyclo`,
    `cognitive_complexity`, `fan_in`, `fan_out`, `lcom4`) and
    the diagnostic surfaces it as `metric dark` per [arch Sec
    3.3] / [tech-spec Sec 8.7].
  - `@invariant` -- canon-guard; failure breaks `G1 - G7`
    inherited from CLEAN-CODE arch or one of `C1 - C15` in
    `tech-spec.md` Sec 7.
  - `@security` -- dev-mode bypass, signed-policy boundary,
    build-tag exclusion.
  - `@determinism` -- byte-stable output across re-runs (covers
    `tech-spec` C11).
  - `@cross-platform` -- Linux vs Windows divergence (covers
    the symlink-loop guard from [arch Sec 3.1]).
  - `@reserved` -- reserved sub-command or flag that exits 64
    in P0 / P1 with a specific stderr message
    ([impl-plan Stage 4.4]).
- **Canonical names only.** Scenarios use the exact names locked
  upstream:
  - **TaskKinds:** `split_class`, `extract_method`,
    `invert_dependency`, `break_cycle`,
    `consolidate_duplication` (the canonical five-value enum at
    `services/clean-code/internal/refactor/task_planner.go:77-118`;
    [arch Sec 4.8]; [tech-spec C5]).
  - **Sub-commands:** `analyze`, `report`, `version`,
    `apply` (reserved); rejected variants like `scan`, `lint`,
    `fix` MUST exit 64 ([impl-plan Stage 1.1]).
  - **Exit codes:** `0`, `1`, `2`, `64` (`EX_USAGE`), `70`
    (`EX_SOFTWARE`) ([tech-spec Sec 8.6]).
  - **Verdicts:** `pass`, `warn`, `block` ([arch Sec 3.7.1
    step 2]; CLEAN-CODE arch Sec 5.4.1).
  - **Severity values:** `info`, `warn`, `block`
    ([tech-spec Sec 8.1] row `--exit-on`).
  - **Delta values:** `new`, `newly_failing`, `unchanged`,
    `resolved` (CLEAN-CODE arch Sec 5.4.1); the CLI's root-commit
    registration ([arch Sec 3.4] step 4) means every firing rule
    in P0 emits `delta=new`.
  - **PromptFormatVersion:** `"v1.2026.05"` ([arch Sec 4.6]
    last row; [impl-plan Stage 4.1]).
  - **SchemaVersion:** `"v1.2026.05"` ([impl-plan Stage 3.2]
    `JSON.Render`).
- **Connection strings and file paths are env-var names**, NEVER
  literal hostnames or credentials. The env-vars referenced
  across phases:

| Env var | Purpose | Default in CI |
| --- | --- | --- |
| `CLEANC_REPO_ROOT` | Absolute path of the fixture repo passed to `cleanc analyze` | per-scenario tmp directory |
| `CLEANC_POLICY_DIR` | Override directory for `--policy <path>` (dev builds only) | unset (use embedded set) |
| `CLEANC_OUT_REPORT` | Path passed to `--out`; markdown report destination | `$RUNNER_TEMP/report.md` |
| `CLEANC_OUT_FINDINGS` | Path passed to `--findings`; JSON artifact destination | `$RUNNER_TEMP/findings.json` |
| `CLEANC_OUT_PROMPTS` | Path passed to `--emit-prompts`; JSONL prompt sink | unset (skip) |
| `CLEANC_OUT_DIAGNOSTICS` | Path passed to `--diagnostics`; JSON diagnostics sink | unset (skip) |
| `CLEANC_BUILD_TAG` | Build tag the binary under test was compiled with (`""` or `prod`) | `""` |
| `CLEANC_BINARY_PATH` | Absolute path to the binary under test (`bin/cleanc` or `bin/cleanc-prod`) | `services/clean-code/bin/cleanc` |
| `CLEANC_TELEMETRY_ENDPOINT` | Reserved future env (binds to `--telemetry-otlp`); MUST be unset in P0 / P1 | unset |

No `CLEAN_CODE_PG_URL`, `CLEAN_CODE_KMS_URL`, `CLEAN_CODE_OIDC_*`,
or other production-service env-vars appear in any phase. The
CLI does NOT speak to Postgres, KMS, or OIDC ([arch Sec 1.1],
[tech-spec C8]); scenarios that probe this absence live under
`@invariant @security`.

---

# Phase 1: Foundations

Implements `implementation-plan.md` Phase 1 (Stages 1.1 - 1.4):
the CLI binary skeleton, the `RepoContext` / `ScopeBinding`
in-memory layer, the deterministic effort-estimator fallback,
and the dev-mode policy loader (embedded `//go:embed` rule packs
plus the unsigned-policy bypass that compiles only under no-tag
builds). No pipeline stages run yet -- this phase locks the
foundations the later phases compose on top of.

### Setup
- **Type**: inline
- **Local**: `cd services/clean-code && make build && go test ./cmd/cleanc/... ./internal/cli/repocontext/... ./internal/cli/scopebinding/... ./internal/cli/effort/... ./internal/cli/devpolicy/... -count=1`
- **CI runner**: GitHub-hosted `ubuntu-latest`; no lab hardware required (the CLI has no DB / HTTP / collector dependency per [arch Sec 1.1] / [tech-spec C8]).
- **Secrets**: none (the dev-mode policy loader is unsigned by design per [arch Sec 3.8] / [tech-spec C6]; no KMS key, no Vault entry, no GitHub environment).
- **Pre-test bootstrap**: `make build` (compiles `bin/cleanc` from `cmd/cleanc/main.go` so the version + help scenarios can shell out to the actual binary; the test code does not invoke `go run`).

### Scenarios

```gherkin
Background:
  Given the binary at `$CLEANC_BINARY_PATH` has been built from the current source tree
    And the binary's build tag is the empty string (no-tag dev build)
    And the working directory is a writable temp directory
    And no production-service env-vars (`CLEAN_CODE_PG_URL`, `CLEAN_CODE_KMS_URL`, `CLEAN_CODE_OIDC_*`) are set
```

```gherkin
@happy @invariant
Feature: cleanc version surfaces the canonical CLI metadata [impl-plan Stage 1.1] [tech-spec Sec 8.1]
  Scenario: version output matches the locked format
    When the user runs `cleanc version`
    Then exit code is 0
     And stdout matches the regex `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`
     And stdout contains `parsers=` whose CSV value is exactly the four-language set `go,python,typescript,java` (in any order; the test parses the CSV and compares as a set, per [arch Sec 1.1] L2 disposition)
     And no `\u001b` escape sequence is emitted (the CLI does not colour-format `version` output)
```

```gherkin
@happy @invariant
Feature: Sub-command surface is exactly four verbs [impl-plan Stage 1.1] [arch Sec 3.6]
  Scenario Outline: Each canonical sub-command is recognised
    When the user runs `cleanc <verb>` with no further arguments
    Then exit code is one of `{0, 64}` (0 for `version`; 64 for the others because they require arguments)
     And stderr does NOT contain the literal phrase `unknown sub-command`
    Examples:
      | verb    |
      | analyze |
      | report  |
      | version |
      | apply   |

  Scenario Outline: Non-canonical sub-commands are rejected with exit 64
    When the user runs `cleanc <verb>`
    Then exit code is 64
     And stderr contains the literal phrase `unknown sub-command`
     And no file at `$CLEANC_OUT_REPORT` is created
    Examples:
      | verb       |
      | scan       |
      | lint       |
      | fix        |
      | refactor   |
      | frobnicate |
```

```gherkin
@edge
Feature: analyze without a path argument prints usage [impl-plan Stage 1.1]
  Scenario: Missing positional path
    When the user runs `cleanc analyze` with no positional argument
    Then exit code is 64
     And stderr contains the literal phrase `usage: cleanc analyze`
     And no pipeline stage starts (no `bin/cleanc` log line containing `walker:` or `engine:` appears)
```

```gherkin
@happy @determinism
Feature: MintRepoID is deterministic across re-runs [impl-plan Stage 1.2] [arch Sec 1.4 G2]
  Scenario: Two calls on the same absolute path produce the same UUID
    Given a temp directory at `$CLEANC_REPO_ROOT`
    When `repocontext.MintRepoID($CLEANC_REPO_ROOT)` is called twice in the same process
    Then both calls return the same UUID-v5 value byte-for-byte
     And the value's variant bits identify it as RFC 4122 v5 (namespace = URL, name = `cleanc.local-repo/<normalised path>`)

  Scenario: Forward-slash normalisation on Windows path
    Given a Windows path `C:\Users\dev\repo`
    When `repocontext.MintRepoID` is called
    Then the UUID is byte-for-byte equal to the value produced from the equivalent forward-slash form `C:/Users/dev/repo`
     And both forms are equal to the UUID produced from the absolute form after `filepath.ToSlash`
```

```gherkin
@happy @edge
Feature: HEAD SHA fallback to "working-copy" for non-git roots [impl-plan Stage 1.2] [arch Sec 4.1]
  Scenario: Non-git directory yields the literal "working-copy" sentinel
    Given a temp directory with no `.git` subdirectory
    When `repocontext.DetectHeadSHA(rootPath)` runs
    Then it returns the exact string `working-copy`
     And the companion `IsGitRepo` flag is `false`

  Scenario: Git working copy yields the real HEAD SHA
    Given a temp directory initialised with `git init` and one commit
    When `repocontext.DetectHeadSHA(rootPath)` runs
    Then it returns the same value as `git -C <rootPath> rev-parse HEAD`
     And `IsGitRepo` is `true`
     And the SHA matches the regex `^[0-9a-f]{40}$`
```

```gherkin
@happy
Feature: Per-language ModulePath detection [impl-plan Stage 1.2] [arch Sec 4.1]
  Scenario Outline: ModulePath is extracted from the canonical manifest per language
    Given a fixture repo at `$CLEANC_REPO_ROOT` whose manifest is <manifest> declaring module <module>
    When `repocontext.DetectModulePath(rootPath, "<language>")` runs
    Then it returns the exact string `<module>`
    Examples:
      | language   | manifest         | module                            |
      | go         | go.mod           | github.com/example/foo            |
      | typescript | package.json     | @example/foo                      |
      | java       | top-level pkg    | com.example.foo                   |
      | python     | pyproject.toml   | example-foo                       |
```

```gherkin
@happy @determinism
Feature: ScopeBinding table is a round-trippable in-memory mirror [impl-plan Stage 1.2] [arch Sec 4.3]
  Scenario: Insert then Get returns the same row
    Given `scopebinding.MintScopeID(repoID, "class", "pkg.Foo", "abcd1234")` produces UUID X
    When a `ScopeBinding{ScopeID: X, ScopeKind: "class", Signature: "pkg.Foo", FilePath: "pkg/foo.go", StartLine: 10, EndLine: 200, Language: "go"}` is inserted
      And `scopebinding.Table.Get(X)` is called
    Then the returned struct equals the inserted struct field-for-field

  Scenario: ScopeID stability across re-runs
    Given the same `(repoID, scope_kind, canonical_signature, first_seen_sha)` tuple
    When `MintScopeID` is called in two different process invocations
    Then both invocations produce the same UUID byte-for-byte
     And the value derives from UUID-v5 per CLEAN-CODE arch G2
```

```gherkin
@happy @determinism
Feature: Effort fallback formula is deterministic and clamped [impl-plan Stage 1.3] [tech-spec Sec 8.5]
  Scenario Outline: Formula output matches the locked decision
    Given fixture inputs `{loc=<loc>, cyclo=<cyclo>, fan_in=<fanIn>, TaskKind=<kind>}`
    When `effort.FallbackModel.Estimate(...)` runs
    Then the returned `effort_hours` equals <expected>
     And the value is clamped to the closed interval `[0.1, 80.0]` per [tech-spec Sec 8.5]
     And the value is rounded half-up to 1 decimal place
    Examples:
      | loc  | cyclo | fanIn | kind                    | expected |
      | 500  | 20    | 8     | split_class             | 20.1     |
      | 500  | 20    | 8     | extract_method          | 9.4      |
      | 500  | 20    | 8     | invert_dependency       | 17.4     |
      | 500  | 20    | 8     | break_cycle             | 18.8     |
      | 500  | 20    | 8     | consolidate_duplication | 13.4     |

  Scenario: Upper-clamp at 80.0 hours
    Given fixture inputs that would compute to a raw value above 80.0
    When `Estimate` runs
    Then the returned value is exactly `80.0`

  Scenario: Lower-clamp at 0.1 hours
    Given fixture inputs that would compute to a raw value below 0.1
    When `Estimate` runs
    Then the returned value is exactly `0.1`

  Scenario: Dark-metric inputs contribute zero
    Given the `EffortInputProvider` returns `ok=false` for `fan_in` on a given scope ID
    When `Estimate` runs for that scope with `TaskKind=split_class`
    Then the formula evaluates `fan_in` as `0` (not skipped, not erroring)
     And the result still falls within `[0.1, 80.0]`
```

```gherkin
@invariant
Feature: Effort fallback advertises its mode in diagnostics [impl-plan Stage 1.3] [tech-spec C15]
  Scenario: Mode tag is "fallback" when ONNX is missing
    Given no ONNX model is configured (`PolicyVersion.RefactorWeights.EffortModelVersion = "fallback-2026.05"`)
    When the effort estimator is constructed
    Then `effort.FallbackModel.Mode()` returns the exact string `fallback`
     And the WARNING log line on first invocation contains `deterministic fallback formula`
```

```gherkin
@happy @invariant
Feature: Dev-mode loader synthesises an unsigned PolicyVersion from embedded packs [impl-plan Stage 1.4] [arch Sec 3.8] [tech-spec C6]
  Scenario: Embedded rule packs load with a stable PolicyVersionID
    Given a no-tag (dev) build
    When `devpolicy.Loader.Load(LoaderSource{UseEmbedded: true})` runs
    Then the returned `Bundle.Rules` length is non-zero
     And every rule's `RuleID` traces to a YAML file under `services/clean-code/policy/rulepacks/{solid,decoupling}/`
     And `Bundle.PolicyVersion.Signature` is nil ([tech-spec C6])
     And calling the loader twice yields the identical `PolicyVersionID` byte-for-byte ([impl-plan Stage 1.4] derivation `UUID-v5(namespace=cleanc.dev-policy, name=sha256(sorted_rule_ids)+effort_model_version)`)
```

```gherkin
@happy
Feature: --policy directory override loads custom YAML packs [impl-plan Stage 1.4]
  Scenario: Filesystem override accepted in dev build
    Given a temp directory at `$CLEANC_POLICY_DIR` containing one `.yaml` file in the canonical rule-pack shape
    When `devpolicy.Loader.Load(LoaderSource{UseEmbedded: false, DirPath: $CLEANC_POLICY_DIR})` runs in a dev build
    Then the returned `Bundle.RulePacks` length is exactly 1
     And the rule ids match the YAML's `rules:` entries one-for-one
```

```gherkin
@invariant @security
Feature: Production build excludes the unsigned-policy bypass [impl-plan Stage 1.4] [tech-spec C6] [arch Sec 7.2]
  Scenario: -tags prod build fails closed on Load
    Given the `internal/cli/devpolicy` package built with `-tags prod`
    When the test invokes the constructor `devpolicy.LoadUnsignedBundle(...)`
    Then it returns the sentinel `devpolicy.ErrDevModeUnavailable` whose `Error()` is the literal `dev-mode policy bypass not available in prod build`
     And no `steward.PolicyVersion` with nil `Signature` is constructed anywhere in the call stack

  Scenario: -tags prod build excludes the bypass file
    Given the source tree
    When `go build -tags prod ./cmd/cleanc/...` runs
    Then the build succeeds with exit code 0
     And the resulting binary, when invoked, never emits the dev-mode banner
     And `go list -tags prod -f '{{.GoFiles}}' ./internal/cli/devpolicy/...` does NOT include `unsigned_dev.go` (the file carrying `//go:build !prod`)
```

```gherkin
@invariant @security
Feature: Dev banner text is byte-exact and uncustomisable [impl-plan Stage 1.4] [tech-spec C10] [arch Sec 3.8]
  Scenario: EmitBanner writes the locked string
    Given a `bytes.Buffer`
    When `devpolicy.EmitBanner(buf)` runs in a dev build
    Then the buffer contents exactly equal `WARNING: dev-mode policy is unsigned. Do NOT use cleanc output as the source of truth for a production gate.` followed by a single `\n`
     And no flag, env var, or config knob can suppress this line (an operator who wants to silence it must recompile, per [arch Sec 3.8] "kill switch" rationale)
```

---

# Phase 2: Pipeline

Implements `implementation-plan.md` Phase 2 (Stages 2.1 - 2.5):
the repo walker, the parse + recipe fan-out, the rule-engine
wiring, the planner + task-planner wiring, and the dark-metric
diagnostics. The pipeline is in-process; the rule engine,
planner, and task planner read from / write to the in-memory
`*InMemory*` stores listed in [arch Sec 2].

### Setup
- **Type**: inline
- **Local**: `cd services/clean-code && make build && go test ./internal/cli/walk/... ./internal/cli/orchestrator/... -count=1`
- **CI runner**: GitHub-hosted `ubuntu-latest`; the same job re-runs on `windows-latest` for the `@cross-platform` symlink-loop scenario per [arch Sec 3.1] "Failure modes".
- **Secrets**: none.
- **Pre-test bootstrap**: `make build`; `make fixtures-cli` extracts the in-tree fixture corpus at `services/clean-code/internal/cli/testdata/fixtures/{go,python,typescript,java}/` (the corpus is checked in; the make target is a no-op when files already exist).

### Scenarios

```gherkin
Background:
  Given the binary at `$CLEANC_BINARY_PATH` is the no-tag dev build
    And `$CLEANC_REPO_ROOT` is a temp directory seeded from a fixture under `services/clean-code/internal/cli/testdata/fixtures/`
    And no external services are reachable from the test job (no Postgres, no KMS, no OTel collector)
```

```gherkin
@happy @determinism
Feature: Walker honours hard-coded skip directories [impl-plan Stage 2.1] [arch Sec 3.1]
  Scenario Outline: Files under conventional skip directories never reach the parser
    Given a fixture repo containing `<skip_dir>/foo.<ext>`
    When the walker traverses `$CLEANC_REPO_ROOT`
    Then no `WalkedFile` row for `<skip_dir>/foo.<ext>` is emitted
     And exactly one `WalkSkip` row with `Reason="directory_skip"` is emitted for `<skip_dir>/`
    Examples:
      | skip_dir       | ext  |
      | .git           | txt  |
      | node_modules   | js   |
      | vendor         | go   |
      | target         | java |
      | dist           | ts   |
      | build          | java |
      | .next          | ts   |
      | __pycache__    | pyc  |
      | .venv          | py   |
      | venv           | py   |
```

```gherkin
@happy @edge
Feature: .gitignore matches are surfaced as walk skips [impl-plan Stage 2.1] [arch Sec 3.1]
  Scenario: Gitignored file is skipped
    Given a fixture git repo whose `.gitignore` lists the line `secret.txt`
      And a file `secret.txt` exists at the repo root
    When the walker traverses `$CLEANC_REPO_ROOT`
    Then exactly one `WalkSkip{Reason: "gitignore", Path: "secret.txt"}` row is emitted
     And no `WalkedFile` row for `secret.txt` is emitted
```

```gherkin
@edge
Feature: Per-file size cap enforced at 2 MiB [impl-plan Stage 2.1] [tech-spec Sec 8.3]
  Scenario: Oversize file emits skip without reading content
    Given a 3 MiB Go source file at `$CLEANC_REPO_ROOT/large.go`
    When the walker traverses
    Then exactly one `WalkSkip{Reason: "size_cap", Path: "large.go"}` row is emitted
     And the file's content bytes are NOT read into memory (verified via a test-double `fs.FS` whose `Open` for `large.go` panics if called)

  Scenario: File at exactly the 2 MiB cap is admitted
    Given a 2,097,152-byte (exactly 2 MiB) Go source file
    When the walker traverses
    Then a `WalkedFile` row is emitted (the cap is inclusive on the wire; oversize means strictly greater)
```

```gherkin
@happy @determinism
Feature: Walker emits files in deterministic lexicographic order [impl-plan Stage 2.1] [arch Sec 10]
  Scenario: Re-runs produce identical ordering
    Given a fixture with files `b.go`, `a.go`, `c.go` under one directory
    When the walker traverses twice in the same process
    Then both runs emit `WalkedFile` rows in the order `a.go`, `b.go`, `c.go`
```

```gherkin
@edge
Feature: Missing root path exits with code 2 [impl-plan Stage 2.1] [tech-spec Sec 8.6]
  Scenario: Non-existent root
    When the user runs `cleanc analyze /no/such/path`
    Then exit code is 2
     And stderr contains the literal phrase `ErrRootNotFound`
     And no file at `$CLEANC_OUT_REPORT` or `$CLEANC_OUT_FINDINGS` is created
```

```gherkin
@edge @cross-platform
Feature: Symlink-loop guard works on POSIX and Windows [impl-plan Stage 2.1] [arch Sec 3.1]
  Scenario: POSIX device-id + inode dedup
    Given on a Linux runner a directory `a/` whose child `b/` symlinks to `a/`
    When the walker traverses `$CLEANC_REPO_ROOT/a`
    Then the loop is broken after exactly one visit to the `a/` inode
     And no infinite recursion occurs (test asserts wall time under 5 seconds)
     And one `WalkSkip{Reason: "symlink_loop"}` row is emitted

  Scenario: Windows canonical-path dedup
    Given on a Windows runner a junction at `a\b` pointing to `a\`
    When the walker traverses `$CLEANC_REPO_ROOT\a`
    Then the loop is broken using the canonical path string (POSIX inode dedup is not available on NTFS)
     And one `WalkSkip{Reason: "symlink_loop"}` row is emitted
```

```gherkin
@happy
Feature: Four-language parse fan-out [impl-plan Stage 2.2] [arch Sec 3.2]
  Scenario: One file per pinned language parses successfully
    Given a fixture repo with one of each `{a.go, b.py, c.ts, d.java}` at the root
    When the orchestrator's parse stage runs
    Then exactly four `*parser.AstFile` rows are collected
     And zero `WalkSkip{Reason: "unsupported_language"}` rows are emitted
     And `parser.DefaultRegistry().Languages()` is the source of truth for the supported set

  Scenario: Non-v1 language file is skipped
    Given a fixture with a `Program.cs` (C#) and a `main.rs` (Rust)
    When the walker stage runs
    Then exactly two `WalkSkip{Reason: "unsupported_language"}` rows are emitted (one per file)
     And no `*AstFile` row is collected for either path
```

```gherkin
@happy @invariant
Feature: loc recipe lights up on every language [impl-plan Stage 2.2] [arch Sec 3.3]
  Scenario Outline: loc value equals the file's line count
    Given a fixture `<file>` whose physical line count is <N>
    When the recipe stage runs
    Then exactly one `MetricSampleDraft{MetricKind: "loc", Value: <N>}` is collected for the file scope
     And the draft's `Pack` is `base` and `SourceComputed` is `true`
    Examples:
      | file     | N    |
      | a.go     | 42   |
      | b.py     | 17   |
      | c.ts     | 88   |
      | d.java   | 123  |
```

```gherkin
@dark @invariant
Feature: Dark recipes silently emit nothing today [impl-plan Stage 2.2] [arch Sec 3.3] [tech-spec Sec 8.7]
  Scenario Outline: <metric_kind> draft is NOT emitted under today's parser fleet
    Given a fixture Go file containing one function with branches and method calls
    When the recipe stage runs
    Then `Recipe.AppliesTo(file)` returns `false` for `metric_kind=<metric_kind>` because the parser does not stamp `<attr>` on the AST
     And zero `MetricSampleDraft` rows for `metric_kind=<metric_kind>` are collected
     And the dark-metric diagnostic registers one `(metric_kind, language)` pair with `missing_attrs=[<attr>]`
    Examples:
      | metric_kind          | attr             |
      | cyclo                | decision_blocks  |
      | cognitive_complexity | decision_blocks  |
      | fan_in               | call_edges       |
      | fan_out              | call_edges       |

  Scenario: lcom4 dark recipe requires BOTH attrs
    Given a fixture Java file containing one class with three methods
    When the recipe stage runs
    Then `Recipe.AppliesTo(file)` returns `false` for `metric_kind=lcom4`
     And the dark-metric diagnostic registers `missing_attrs=["call_edges","field_accesses"]` (sorted lexicographically; [tech-spec Sec 8.7])
```

```gherkin
@happy
Feature: cycle_member emits 1.0 inside cycles, 0.0 elsewhere [impl-plan Stage 2.2] [arch Sec 3.3]
  Scenario: Three-file Go cycle a->b->c->a
    Given a fixture Go repo at `$CLEANC_REPO_ROOT/cycle/{a,b,c}.go` whose imports form a cycle
    When the recipe stage runs the project-level `cycle_member` recipe
    Then exactly three `MetricSampleDraft{MetricKind: "cycle_member", Value: 1.0}` rows are emitted (one per cycle participant)
     And every non-participant file in the corpus emits `cycle_member` with `Value: 0.0`
```

```gherkin
@happy
Feature: duplication_ratio computed on canonicalised source bytes [impl-plan Stage 2.2] [arch Sec 3.3]
  Scenario: Two TypeScript files with whitespace-only differences
    Given two TS functions differing only in indentation
    When the project-level `duplication_ratio` recipe runs
    Then the value for the containing file is `> 0.95` and `<= 1.0`
     And the recipe reads `AttrSourceBytes` (raw file bytes, NOT the parser's normalised form, per [arch Sec 3.3] note)
```

```gherkin
@happy @invariant
Feature: ScopeBinding populated by parse + recipe fan-out [impl-plan Stage 2.2] [arch Sec 4.3]
  Scenario: Function binding round-trips by ScopeID
    Given a fixture Go file declaring `func Foo() { ... }` between lines 10 and 25
    When the parse + recipe stage completes
    Then `scopebinding.Table.Get(scopeIDFor("Foo"))` returns a row whose
        | field      | value                                                    |
        | ScopeKind  | method                                                   |
        | Signature  | <repo-relative path>::Foo                                |
        | FilePath   | <repo-relative path>                                     |
        | StartLine  | 10                                                       |
        | EndLine    | 25                                                       |
        | Language   | go                                                       |
```

```gherkin
@edge
Feature: Parser panic is non-fatal per file [impl-plan Stage 2.2] [tech-spec Sec 8.6]
  Scenario: One file triggers a panic; the others still process
    Given a fixture corpus where one file triggers a panic in the parser stub (via test double)
    When the orchestrator runs the parse stage
    Then exactly one `WalkSkip{Reason: "parser_panic", Path: <file>}` row is emitted
     And the remaining files in the corpus produce `*AstFile` rows
     And the binary's overall exit code is NOT 70 (per-file panics are recovered)
```

```gherkin
@happy @invariant
Feature: Rule engine wiring uses batched InsertSamples and root-commit registration [impl-plan Stage 2.3] [arch Sec 3.4] [tech-spec C8]
  Scenario: Smoke run on a high-LOC fixture produces a "new" delta finding
    Given a fixture Go file with a 2,000-line class (exceeds the SRP-`loc` threshold of 1,500)
    When the orchestrator's engine stage runs
    Then `RunArtifact.Findings` length is `>= 1`
     And at least one finding's `RuleID` traces to the SRP / cohesion rule pack
     And every firing finding has `Delta == "new"` (root-commit registration per [arch Sec 3.4] step 4)
     And `store.InsertSamples(repoID, headSHA, samples)` is called exactly ONCE with the full batch (no per-row `InsertSample` calls; the canonical signature at `services/clean-code/internal/rule_engine/inmem_store.go:146-151` is plural-only)

  Scenario: Empty corpus produces verdict=pass
    Given a fixture repo with zero source files
    When the engine stage runs
    Then `RunArtifact.Findings` is empty
     And `Verdict.Verdict == "pass"`
     And exit code is 0
```

```gherkin
@edge @invariant
Feature: Engine internal error exits 70 with stderr context [impl-plan Stage 2.3] [tech-spec Sec 8.6]
  Scenario: AppendEvaluation surface returns an error
    Given a test-double `rule_engine.Store` whose `AppendEvaluation` returns an error (the genuine error-returning Store surface at `services/clean-code/internal/rule_engine/inmem_store.go:477`)
    When the orchestrator's engine stage runs
    Then `engine.RunBatch` returns the error
     And the binary exits with code 70
     And stderr contains the engine error string
```

```gherkin
@invariant
Feature: CLI imports zero Postgres / SQL packages [impl-plan Stage 2.3] [tech-spec C8] [tech-spec Sec 8.10]
  Scenario: Lint rule no-production-sql-import passes for the entire CLI tree
    Given the source trees `cmd/cleanc/...` and `internal/cli/...`
    When `make lint-cli` runs the `no-production-sql-import` rule
    Then exit code is 0 (no file under either tree imports `database/sql` or any package whose name matches `*_sql_store`)
```

```gherkin
@happy @determinism
Feature: Planner and TaskPlanner emit canonical TaskKinds in stable order [impl-plan Stage 2.4] [arch Sec 4.8] [tech-spec C5]
  Scenario: Hot-spot ranking obeys (Score DESC, ScopeID ASC)
    Given a fixture run producing three findings on three different scopes
    When the planner stage runs
    Then `RunArtifact.HotSpots` length is 3
     And the rows are sorted by `Score` descending, ties broken by `ScopeID` ascending

  Scenario: All emitted TaskKinds belong to the canonical five-value enum
    Given a fixture run producing one task per kind
    When the task planner stage runs
    Then every `Tasks[i].Kind` satisfies `refactor.IsCanonicalTaskKind`
     And no task carries an alias (`extract_function`, `reduce_lcom`, `introduce_interface`, etc. -- the `ValidateTaskKind` guard at `services/clean-code/internal/refactor/task_planner.go:181-191` rejects them)
```

```gherkin
@happy @invariant
Feature: Effort fallback wires into the TaskPlanner via WithEffortModel [impl-plan Stage 2.4] [arch Sec 3.5] [tech-spec C15]
  Scenario: Every task carries a non-zero EffortHours from the fallback
    Given a fixture run with no ONNX model configured
    When the task planner runs
    Then every `Tasks[i].EffortHours > 0`
     And `Diagnostics.EffortSource == "fallback"`
     And the markdown report's diagnostics block names the active mode
```

```gherkin
@happy
Feature: --top-n flag caps the emitted task set [impl-plan Stage 2.4] [arch Sec 3.6]
  Scenario: 50 hotspots, top-n=5
    Given a fixture producing 50 hot-spots
    When `cleanc analyze . --top-n 5` runs
    Then `RunArtifact.Tasks` length is `<= 5`
     And the surviving tasks correspond to the top-5 hot-spots by score
```

```gherkin
@dark @invariant
Feature: Dark-metric diagnostic carries the locked taxonomy [impl-plan Stage 2.5] [tech-spec Sec 8.7] [arch Sec 3.3]
  Scenario: cyclo dark row on a Go fixture
    Given a fixture Go file with one function
    When the orchestrator runs and writes `$CLEANC_OUT_DIAGNOSTICS`
    Then `Diagnostics.DarkMetrics` contains the row
        | field                 | value             |
        | metric_kind           | cyclo             |
        | language              | go                |
        | missing_attrs         | ["decision_blocks"] |
        | affected_scope_count  | 1                 |
        | closure_phase         | P2                |

  Scenario: loc is NOT in the dark-metric inventory
    Given the same fixture
    When the orchestrator runs
    Then `Diagnostics.DarkMetrics` does NOT contain any row with `metric_kind == "loc"`
     And `Diagnostics.DarkMetrics` does NOT contain any row with `metric_kind == "duplication_ratio"`

  Scenario: Unknown parser attr fails closed [tech-spec Sec 8.7 last paragraph]
    Given a test that injects a `bogus_attr` value into the orchestrator's `metricAttrRequirements` table
    When the orchestrator runs
    Then exit code is 70
     And stderr contains the literal `orchestrator: unknown parser attr in dark-metric table: bogus_attr`
```

---

# Phase 3: P0 Reports and Delivery

Implements `implementation-plan.md` Phase 3 (Stages 3.1 - 3.5):
the markdown report renderer, the JSON findings artifact, the
end-to-end `analyze` wiring, the `report` / `version`
sub-commands, and the P0 golden fixture corpus. Phase 3 is the
first phase where the binary produces user-visible files; every
output is byte-stable per `tech-spec C11`.

### Setup
- **Type**: inline
- **Local**: `cd services/clean-code && make build && go test ./internal/cli/report/... ./internal/cli/orchestrator/... ./cmd/cleanc/... -count=1`
- **CI runner**: GitHub-hosted `ubuntu-latest`; the golden-file snapshot tests also run on `windows-latest` for the `@cross-platform` byte-identical scenario (line endings must be normalised to `\n` in golden files regardless of host).
- **Secrets**: none.
- **Pre-test bootstrap**: `make build`; `make fixtures-cli` ensures the in-tree fixture corpus is present; `make update-cli-golden=0` is the default (golden files are checked in and read-only during the test run, per [impl-plan Stage 3.5]).

### Scenarios

```gherkin
Background:
  Given the binary at `$CLEANC_BINARY_PATH` is the no-tag dev build
    And `$CLEANC_REPO_ROOT` is seeded from a fixture under `services/clean-code/internal/cli/testdata/fixtures/`
    And the test job's `RUNNER_TEMP` is writable
```

```gherkin
@happy @determinism
Feature: Markdown report has the seven canonical sections in stable order [impl-plan Stage 3.1] [arch Sec 3.7.1]
  Scenario: Header, Verdict, Findings, Hot-spots, Plan, Tasks, Diagnostics
    Given a fixture run on the P0 Go cycle corpus
    When `cleanc analyze $CLEANC_REPO_ROOT --out $CLEANC_OUT_REPORT --findings $CLEANC_OUT_FINDINGS` runs
    Then `$CLEANC_OUT_REPORT` contains the H2 headings, in this exact order:
        | order | heading                  |
        | 1     | ## Header                |
        | 2     | ## Verdict               |
        | 3     | ## Findings by Severity  |
        | 4     | ## Hot-spot Ranking      |
        | 5     | ## Refactor Plan         |
        | 6     | ## Refactor Tasks        |
        | 7     | ## Diagnostics           |
     And the Header section contains the keys `Repo path`, `HEAD SHA`, `Policy id`, `Policy version`, `Parsers`, `Dark metrics`
     And the Verdict section is a single line matching `^Verdict: (pass|warn|block)$`
```

```gherkin
@determinism @invariant
Feature: Markdown re-render is byte-identical for the same RunArtifact [impl-plan Stage 3.1] [tech-spec C11]
  Scenario: Two consecutive renders produce identical bytes
    Given a single `RunArtifact` value
    When `report.Markdown.Render` writes to two separate buffers
    Then both buffers are byte-identical (including trailing newline)
     And the SHA-256 of the two outputs is equal
```

```gherkin
@happy
Feature: Suggested-refactor excerpt is extracted from the rule's DescriptionMD [impl-plan Stage 3.1] [arch Sec 3.7.1]
  Scenario: Rule with "Suggested refactor:" prose
    Given a rule whose `DescriptionMD` contains the line `Suggested refactor: split the class along the cohesion boundaries (SRP).`
    When the renderer extracts the excerpt
    Then the markdown row's suggestion column contains the suffix `split the class along the cohesion boundaries (SRP).`
```

```gherkin
@dark
Feature: Dark-metric summary surfaces in the markdown header and diagnostics [impl-plan Stage 3.1] [arch Sec 3.7.1]
  Scenario: Cyclo is dark on Go corpus
    Given a `RunArtifact` whose `Diagnostics.DarkMetrics` includes a `cyclo` row
    When `report.Markdown.Render` runs
    Then the Header section contains the literal phrase `metric dark: cyclo`
     And the Diagnostics section contains a row naming `cyclo` and `decision_blocks`
```

```gherkin
@happy @determinism @invariant
Feature: JSON findings artifact is round-trippable and byte-stable [impl-plan Stage 3.2] [tech-spec C11]
  Scenario: Marshal then Unmarshal yields equal struct
    Given a `RunArtifact` rendered by `report.JSON.Render`
    When the bytes are `json.Unmarshal`ed back into a `RunArtifact`
    Then the resulting struct deep-equals the original

  Scenario: Two consecutive marshals are byte-identical
    Given the same `RunArtifact`
    When `report.JSON.Render` writes twice
    Then the two outputs are byte-identical
     And every slice (findings, hot-spots, tasks, samples, walk-skips, dark-metrics) is sorted before marshalling

  Scenario: SchemaVersion is stamped
    Given any non-empty `RunArtifact`
    When `report.JSON.Render` runs
    Then the output JSON contains `"schemaVersion": "v1.2026.05"` (camelCase marshaller tag)
```

```gherkin
@happy
Feature: analyze end-to-end happy path [impl-plan Stage 3.3] [arch Sec 6.1] [tech-spec Sec 8.6]
  Scenario: Block-severity finding triggers exit 1
    Given a fixture with one block-severity finding
    When the user runs `cleanc analyze $CLEANC_REPO_ROOT --out $CLEANC_OUT_REPORT --findings $CLEANC_OUT_FINDINGS --exit-on block`
    Then exit code is 1 ([tech-spec Sec 8.6] row `1`)
     And `$CLEANC_OUT_REPORT` is written
     And `$CLEANC_OUT_FINDINGS` is written
     And both files are non-empty and parse as markdown / JSON respectively
     And the report.md is written EVEN WHEN exit code is 1 (CI must be able to attach it as an artifact)
```

```gherkin
@happy
Feature: stdout default for --out [impl-plan Stage 3.3] [arch Sec 3.6]
  Scenario: No --out flag routes markdown to stdout
    When the user runs `cleanc analyze $CLEANC_REPO_ROOT --findings $CLEANC_OUT_FINDINGS`
    Then stdout receives the markdown report
     And stderr contains only the dev banner (no markdown content)
     And exit code is 0 or 1 (severity dependent)
```

```gherkin
@edge
Feature: Walker error exits 2; reported flag values do not matter [impl-plan Stage 3.3] [tech-spec Sec 8.6]
  Scenario: Non-existent root
    When `cleanc analyze /no/such/path --out $CLEANC_OUT_REPORT` runs
    Then exit code is 2
     And `$CLEANC_OUT_REPORT` is NOT created
     And stderr contains the literal `ErrRootNotFound`
```

```gherkin
@edge
Feature: --exit-on accepts only the closed set {info, warn, block} [impl-plan Stage 3.3] [tech-spec Sec 8.1]
  Scenario: Invalid severity is rejected at flag-parse time
    When the user runs `cleanc analyze . --exit-on critical`
    Then exit code is 64
     And the rejection happens BEFORE any pipeline stage starts (no `walker:` or `engine:` log line appears)
     And stderr contains the literal `--exit-on must be one of info, warn, block`

  Scenario Outline: Each valid severity is accepted
    When the user runs `cleanc analyze $CLEANC_REPO_ROOT --exit-on <sev>`
    Then exit code is one of `{0, 1}` (a successful flag-parse may still trigger 1 if a finding crosses the threshold)
    Examples:
      | sev   |
      | info  |
      | warn  |
      | block |
```

```gherkin
@invariant @security
Feature: Dev banner emitted on every analyze run in dev builds [impl-plan Stage 3.3] [tech-spec C10]
  Scenario: Banner on stderr precedes any other output
    Given a dev build
    When `cleanc analyze $CLEANC_REPO_ROOT --out $CLEANC_OUT_REPORT` runs
    Then stderr begins with the exact C10 banner string
     And stdout (which receives the markdown report when `--out` is unset) does NOT contain the banner -- the banner is stderr-only so it can never contaminate a piped artifact

  Scenario: --dev-mode=false in a no-tag build refuses to start
    Given a dev build
    When `cleanc analyze $CLEANC_REPO_ROOT --dev-mode=false` runs
    Then exit code is 64
     And stderr contains `dev-mode disabled but no signed-policy loader available; rebuild with -tags prod`
```

```gherkin
@happy @determinism
Feature: report sub-command re-renders findings.json without re-running pipeline [impl-plan Stage 3.4]
  Scenario: Replay matches the original analyze output
    Given an `analyze` run wrote `$CLEANC_OUT_FINDINGS` and `$CLEANC_OUT_REPORT`
    When `cleanc report $CLEANC_OUT_FINDINGS --out $RUNNER_TEMP/replay.md` runs
    Then `$RUNNER_TEMP/replay.md` is byte-identical to `$CLEANC_OUT_REPORT`
     And the orchestrator does NOT re-walk `$CLEANC_REPO_ROOT` (verified by deleting `$CLEANC_REPO_ROOT` between the two calls; the report sub-command still succeeds)
```

```gherkin
@edge
Feature: report refuses a findings.json with a mismatched schemaVersion [impl-plan Stage 3.4]
  Scenario: Old-schema artifact
    Given a `findings.json` whose `schemaVersion` is the string `v0.0.0`
    When `cleanc report findings.json` runs
    Then exit code is 64
     And stderr names BOTH the expected schema version (`v1.2026.05`) and the observed value (`v0.0.0`)
```

```gherkin
@happy @determinism
Feature: Golden snapshots for the four-language corpus [impl-plan Stage 3.5]
  Scenario: Go cycle corpus
    Given the fixture at `services/clean-code/internal/cli/testdata/fixtures/go-cycle/`
    When `runAnalyze` runs end-to-end
    Then `report.md` is byte-identical to `internal/cli/testdata/golden/go-cycle/report.md`
     And `findings.json` is byte-identical to `internal/cli/testdata/golden/go-cycle/findings.json`
     And the golden `findings.json` carries at least one row with `RuleID` matching the regex `^decoupling\.cycle_member` and at least one task with `Kind == "break_cycle"`

  Scenario Outline: Cross-language coverage
    Given the fixture at `<fixture>`
    When `runAnalyze` runs sequentially
    Then `report.md` byte-matches `<golden>/report.md`
    Examples:
      | fixture                                                            | golden                                              |
      | services/clean-code/internal/cli/testdata/fixtures/go-cycle/       | internal/cli/testdata/golden/go-cycle/              |
      | services/clean-code/internal/cli/testdata/fixtures/python-srp/     | internal/cli/testdata/golden/python-srp/            |
      | services/clean-code/internal/cli/testdata/fixtures/typescript-dup/ | internal/cli/testdata/golden/typescript-dup/        |
      | services/clean-code/internal/cli/testdata/fixtures/java-dit/       | internal/cli/testdata/golden/java-dit/              |

  Scenario: Deterministic re-run within one process
    Given any fixture from the table above
    When `runAnalyze` runs twice back-to-back in the same process
    Then both runs produce byte-identical `report.md` and `findings.json` outputs
     And the SHA-256 of the two outputs is equal
```

---

# Phase 4: P1 Structured Prompt Emitter

Implements `implementation-plan.md` Phase 4 (Stages 4.1 - 4.4):
the `RefactorPromptRecord` shape and snippet extractor, the
JSONL emitter, the `--emit-prompts` flag wiring, and the
reserved verbs / flags (`apply`, `--telemetry-otlp`,
`--with-churn`, `--snippet-cap-lines`). Phase 4 ships the L7
Option A artefact per operator pin `cli-l7-authority` ([arch
Sec 1.3]); L7 Options B / C (mechanical patches) remain deferred
to P3.

### Setup
- **Type**: inline
- **Local**: `cd services/clean-code && make build && go test ./internal/cli/suggest/... ./cmd/cleanc/... -count=1`
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none.
- **Pre-test bootstrap**: `make build`; `make fixtures-cli` ensures the in-tree fixture corpus is present; no AI-coder service is contacted (the prompts are written to disk and never POSTed anywhere).

### Scenarios

```gherkin
Background:
  Given the binary at `$CLEANC_BINARY_PATH` is the no-tag dev build
    And `$CLEANC_OUT_PROMPTS` is set to `$RUNNER_TEMP/prompts.jsonl`
    And the test job's `RUNNER_TEMP` is writable
```

```gherkin
@happy @invariant
Feature: RefactorPromptRecord shape matches arch Sec 4.6 field-for-field [impl-plan Stage 4.1] [arch Sec 4.6]
  Scenario: Every required field present and typed correctly
    Given a fixture run producing one `RefactorTask` for `Kind=split_class`
    When `suggest.JSONL.Emit` writes one line
    Then the line parses as a JSON object whose key set is exactly:
        | key                       | type             |
        | task_id                   | string (UUID)    |
        | plan_id                   | string (UUID)    |
        | repo_id                   | string (UUID)    |
        | head_sha                  | string           |
        | policy_version_id         | string (UUID)    |
        | task_kind                 | string (enum)    |
        | rule_id                   | string           |
        | rule_version              | integer          |
        | severity                  | string           |
        | scope                     | object           |
        | source_snippet            | string           |
        | source_snippet_truncated  | boolean          |
        | metric_evidence           | array of objects |
        | prose_suggestion          | string           |
        | effort_hours              | number           |
        | effort_source             | string (`ml` or `fallback`) |
        | prompt_format_version     | string           |
     And `prompt_format_version` equals the exact literal `v1.2026.05`
     And `task_kind` is one of `{split_class, extract_method, invert_dependency, break_cycle, consolidate_duplication}` ([tech-spec C5])
     And `scope` has exactly the keys `signature`, `kind`, `file_path`, `start_line`, `end_line`
```

```gherkin
@edge
Feature: Source snippet is capped at 200 lines by default [impl-plan Stage 4.1] [tech-spec Sec 8.2] [tech-spec C12]
  Scenario: 500-line scope is truncated to 200 lines
    Given a 500-line method scope in a Go fixture
    When `suggest.ExtractSnippet(absFilePath, startLine, endLine, 200)` runs
    Then the returned string has exactly 200 lines
     And `truncated == true`
     And the LAST retained line is exactly `... [truncated 300 lines]`

  Scenario: Small scope is not truncated
    Given a 50-line method scope
    When `ExtractSnippet(..., 200)` runs
    Then `truncated == false`
     And the snippet contains exactly 50 lines
     And no `... [truncated` sentinel appears
```

```gherkin
@invariant
Feature: Snippet preserves raw bytes (no parser normalisation) [impl-plan Stage 4.1] [tech-spec C12] [tech-spec R4]
  Scenario: UTF-8 + tab preserved byte-for-byte
    Given a fixture file containing a literal tab character followed by a multi-byte UTF-8 sequence on line 5
    When `ExtractSnippet` reads lines 1 through 10
    Then the returned bytes for line 5 equal the file's bytes for line 5 (no whitespace collapsing, no Unicode normalisation, no comment stripping)
```

```gherkin
@happy
Feature: metric_evidence joins the originating Sample [impl-plan Stage 4.1] [arch Sec 4.6]
  Scenario: One task firing on one metric_kind
    Given a fixture run producing one `RefactorTask` whose rule fired on `metric_kind=loc` with `value=2000` against `threshold=1500`
    When the aggregator computes `metric_evidence` for that task
    Then `metric_evidence` contains exactly one entry
     And the entry equals
        | field        | value |
        | metric_kind  | loc   |
        | value        | 2000  |
        | threshold    | 1500  |
        | op           | >=    |
```

```gherkin
@happy @determinism
Feature: JSONL emitter writes one line per task in stable order [impl-plan Stage 4.2] [tech-spec C11]
  Scenario: Ten tasks, ten lines, each line a valid JSON object
    Given a `RunArtifact` with 10 `Tasks`
    When `suggest.JSONL.Emit` writes to a buffer
    Then the buffer contains exactly 10 newline-terminated lines
     And each line parses as a standalone JSON object via `json.Unmarshal`
     And the lines are sorted by `(severity DESC, score DESC, scope_id ASC)`

  Scenario: Two emissions of the same artifact are byte-identical
    Given the same `RunArtifact`
    When the emitter writes twice
    Then both outputs are byte-identical
     And the SHA-256 of the two outputs is equal
```

```gherkin
@invariant
Feature: Non-canonical TaskKind reaching the emitter exits 70 [impl-plan Stage 4.2] [tech-spec C5]
  Scenario: Test-only injection of an alias
    Given a fixture run with a `RefactorTask{Kind: "refactor_everything"}` injected into the artifact via the test-only seam
    When the emitter runs
    Then exit code is 70
     And stderr contains the literal phrase `rejected task kind`
     And stderr names the offending `task_id`
     And no line is written to `$CLEANC_OUT_PROMPTS`
```

```gherkin
@happy
Feature: --emit-prompts writes JSONL on a fixture with 5 tasks [impl-plan Stage 4.3]
  Scenario: File created with 5 lines
    Given a fixture producing exactly 5 tasks
    When `cleanc analyze $CLEANC_REPO_ROOT --emit-prompts $CLEANC_OUT_PROMPTS` runs
    Then `$CLEANC_OUT_PROMPTS` exists
     And its line count is exactly 5
     And each line parses as a JSON object
     And exit code is 0 (no `--exit-on` trigger)
```

```gherkin
@happy @edge
Feature: --emit-prompts - routes JSONL to stdout when --out is a file [impl-plan Stage 4.3]
  Scenario: Sink to stdout with explicit --out
    Given `--emit-prompts -` AND `--out $CLEANC_OUT_REPORT`
    When `cleanc analyze $CLEANC_REPO_ROOT --out $CLEANC_OUT_REPORT --emit-prompts -` runs
    Then `$CLEANC_OUT_REPORT` exists with the markdown report
     And stdout receives the JSONL stream (one line per task)
     And exit code matches the severity threshold

  Scenario: stdout collision refused
    Given `--emit-prompts -` AND no `--out` flag (markdown would also default to stdout)
    When the command runs
    Then exit code is 64
     And stderr contains the exact phrase `--emit-prompts - requires --out <path>; cannot route both markdown and JSONL to stdout`
     And no pipeline stage starts

  Scenario: bare --emit-prompts refused
    Given `--emit-prompts` with no value supplied on the command line
    When the command runs
    Then exit code is 64
     And stderr contains the exact phrase `--emit-prompts requires a path or '-' for stdout`
```

```gherkin
@happy
Feature: Zero-task fixture produces an empty prompts.jsonl with exit 0 [impl-plan Stage 4.3]
  Scenario: Empty corpus + --emit-prompts
    Given a fixture producing zero tasks
    When `cleanc analyze $CLEANC_REPO_ROOT --emit-prompts $CLEANC_OUT_PROMPTS` runs
    Then `$CLEANC_OUT_PROMPTS` exists
     And the file is exactly 0 bytes
     And exit code is 0 (NOT 1; the absence of tasks is not a failure)
```

```gherkin
@happy
Feature: PromptCount surfaces in diagnostics and report [impl-plan Stage 4.3]
  Scenario: Diagnostics block names the prompt count
    Given `--emit-prompts $CLEANC_OUT_PROMPTS` with a fixture producing 7 tasks
    When the markdown report is generated
    Then the Diagnostics section contains the literal phrase `Prompts emitted: 7 to $CLEANC_OUT_PROMPTS`
     And `RunArtifact.Diagnostics.PromptCount == 7`
```

```gherkin
@reserved @edge
Feature: cleanc apply <task_id> stub returns exit 64 [impl-plan Stage 4.4] [arch Sec 3.6] [arch Sec 6.3]
  Scenario: Apply not implemented
    When the user runs `cleanc apply 00000000-0000-0000-0000-000000000000`
    Then exit code is 64
     And stderr contains the literal phrase `not implemented; pending operator pin cli-l7-authority`
     And stderr references `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3`
```

```gherkin
@reserved @edge
Feature: Reserved flags exit 64 BEFORE any pipeline stage starts [impl-plan Stage 4.4] [tech-spec Sec 8.1]
  Scenario: --telemetry-otlp rejected
    When `cleanc analyze $CLEANC_REPO_ROOT --telemetry-otlp http://localhost:4317 --out $CLEANC_OUT_REPORT --findings $CLEANC_OUT_FINDINGS` runs
    Then exit code is 64
     And neither `$CLEANC_OUT_REPORT` nor `$CLEANC_OUT_FINDINGS` is created
     And stderr contains the literal phrase `--telemetry-otlp is reserved for a future story`

  Scenario: --with-churn rejected (P2 surface, not P0 / P1)
    When `cleanc analyze $CLEANC_REPO_ROOT --with-churn --out $CLEANC_OUT_REPORT` runs
    Then exit code is 64
     And neither `$CLEANC_OUT_REPORT` nor `$CLEANC_OUT_FINDINGS` is created
     And stderr contains the literal phrase `--with-churn is reserved for P2 and rejected in P0/P1`

  Scenario: --snippet-cap-lines rejected (future minor release)
    When `cleanc analyze $CLEANC_REPO_ROOT --snippet-cap-lines 100 --out $CLEANC_OUT_REPORT` runs
    Then exit code is 64
     And stderr contains the literal phrase `reserved for a future minor release`
```

```gherkin
@invariant
Feature: TestReservedSurface table guards every reserved verb/flag in one place [impl-plan Stage 4.4]
  Scenario: Test enumerates every reserved entry from tech-spec Sec 8.1
    Given the `TestReservedSurface` table-driven test
    When the test runs
    Then it exercises EVERY reserved entry from [tech-spec Sec 8.1] (`apply`, `--telemetry-otlp`, `--with-churn`, `--snippet-cap-lines`)
     And every row's expected `(exit_code, stderr_substring)` pair matches the observed values byte-for-byte
     And accidentally activating any reserved surface causes this test to fail in PR CI
```

---

# Phase 5: Hardening and Release

Implements `implementation-plan.md` Phase 5 (Stages 5.1 - 5.4):
the build-tag matrix (`-tags prod` excludes the unsigned-policy
bypass), the custom lint rules (`no-production-sql-import`,
`no-production-build-tag-bypass`), the end-to-end golden tests
under `tests/e2e/cleanc/`, and the user-facing documentation
(README cleanc section, USAGE.md, PROMPT-FORMAT.md, CHANGELOG).
Phase 5 is the release gate: a PR merging without all Phase 5
scenarios green is a regression even if Phases 1 - 4 all pass.

### Setup
- **Type**: inline
- **Local**: `cd services/clean-code && make build && make build-prod && make lint-cli && make test-prod && make e2e-cleanc`
- **CI runner**: GitHub-hosted `ubuntu-latest` for the primary matrix entry plus `windows-latest` for the `@cross-platform` golden-snapshot scenario; `make build-prod`, `make test-prod`, and `make lint-cli` each run as their own matrix entries on `ubuntu-latest` (the prod-build job MUST be green for the merge gate per `tech-spec D11`).
- **Secrets**: none (the prod build path explicitly excludes the unsigned-policy bypass at compile time and therefore needs no signing key for these tests; signed-policy authoring is a separate operator workflow scoped out of this story per [arch Sec 3.8]).
- **Pre-test bootstrap**: `make build` (no-tag dev binary at `bin/cleanc`); `make build-prod` (prod binary at `bin/cleanc-prod`); `make fixtures-cli` ensures the e2e scenario corpora under `tests/e2e/cleanc/scenarios/*/` are extracted.

### Scenarios

```gherkin
Background:
  Given the source tree is on the current branch
    And the test job has `make`, `go`, and `golangci-lint` on `$PATH`
    And no production-service env-vars (`CLEAN_CODE_PG_URL`, `CLEAN_CODE_KMS_URL`, `CLEAN_CODE_OIDC_*`) are set
```

```gherkin
@invariant @security
Feature: Build-tag matrix produces two binaries with mutually exclusive bypass [impl-plan Stage 5.1] [tech-spec Sec 8.9]
  Scenario: Dev build compiles and includes the bypass
    When CI runs `make build`
    Then exit code is 0
     And `services/clean-code/bin/cleanc` exists and is executable
     And `go list -f '{{.GoFiles}}' ./internal/cli/devpolicy/...` lists `unsigned_dev.go`

  Scenario: Prod build compiles and excludes the bypass
    When CI runs `make build-prod`
    Then exit code is 0
     And `services/clean-code/bin/cleanc-prod` exists and is executable
     And `go list -tags prod -f '{{.GoFiles}}' ./internal/cli/devpolicy/...` does NOT list `unsigned_dev.go`
     And it DOES list `unsigned_prod.go`

  Scenario: Prod build excludes bypass via unit test
    When CI runs `go test -tags prod -run TestProdBuildExcludesDevBypass ./internal/cli/devpolicy/...`
    Then exit code is 0
     And the test asserts `devpolicy.LoadUnsignedBundle(...)` returns `devpolicy.ErrDevModeUnavailable` whose `Error()` is exactly `dev-mode policy bypass not available in prod build`
```

```gherkin
@invariant @security
Feature: Custom lint rules block PRs that smuggle DB imports or untagged bypasses [impl-plan Stage 5.2] [tech-spec Sec 8.10]
  Scenario: Clean tree passes both lints
    Given the actual CLI source tree (no fixtures)
    When `make lint-cli` runs
    Then exit code is 0

  Scenario: no-production-sql-import fires on a fixture violation
    Given a test fixture file `services/clean-code/internal/cli/foo_dirty.go` importing `database/sql`
    When `make lint-cli` runs
    Then exit code is non-zero
     And stderr names the file `foo_dirty.go` AND the rule `no-production-sql-import`

  Scenario: no-production-build-tag-bypass fires on a fixture violation
    Given a test fixture file `services/clean-code/internal/cli/devpolicy/bypass_dirty.go` constructing `steward.PolicyVersion{Signature: nil}` without a `//go:build !prod` constraint
    When `make lint-cli` runs
    Then exit code is non-zero
     And stderr names the file `bypass_dirty.go` AND the rule `no-production-build-tag-bypass`
```

```gherkin
@happy
Feature: End-to-end scenario harness drives the real binary against fixture repos [impl-plan Stage 5.3]
  Scenario: p0-go-cycle scenario passes
    Given the scenario directory `tests/e2e/cleanc/scenarios/p0-go-cycle/`
    When the scenario's `run.sh` invokes `$CLEANC_BINARY_PATH analyze . --out report.md --findings findings.json --diagnostics diag.json`
    Then exit code matches the scenario's `expected_exit_code`
     And `report.md` byte-matches `tests/e2e/cleanc/scenarios/p0-go-cycle/golden/report.md`
     And `findings.json` byte-matches `tests/e2e/cleanc/scenarios/p0-go-cycle/golden/findings.json`

  Scenario: p0-mixed-langs scenario covers all four pinned languages
    Given the scenario directory `tests/e2e/cleanc/scenarios/p0-mixed-langs/`
    When `run.sh` executes
    Then `findings.json.Files` length is `>= 4`
     And the distinct set of `Files[*].language` values is exactly `{go, python, typescript, java}`

  Scenario: p1-prompts scenario produces the expected JSONL row count
    Given the scenario directory `tests/e2e/cleanc/scenarios/p1-prompts/`
    When `run.sh` executes
    Then `prompts.jsonl` line count equals the scenario's `expected_task_count`
     And every line is valid JSON with `prompt_format_version == "v1.2026.05"`

  Scenario Outline: Exit-codes scenario covers the entire matrix
    Given the scenario directory `tests/e2e/cleanc/scenarios/exit-codes/<case>/`
    When `run.sh` executes
    Then the observed exit code equals <expected_code>
    Examples:
      | case                             | expected_code |
      | clean-run                        | 0             |
      | severity-trigger                 | 1             |
      | missing-root                     | 2             |
      | invalid-flag                     | 64            |
      | reserved-apply                   | 64            |
      | reserved-telemetry               | 64            |
      | reserved-churn                   | 64            |
      | injected-engine-error            | 70            |
```

```gherkin
@happy @cross-platform
Feature: Golden snapshots are byte-identical across Linux and Windows [impl-plan Stage 5.3] [tech-spec C11]
  Scenario: Linux and Windows runners produce the same report.md bytes
    Given the `p0-go-cycle` scenario is checked out on BOTH `ubuntu-latest` and `windows-latest` CI runners
    When `run.sh` (or its `run.ps1` Windows equivalent) executes on each runner
    Then both runners' `report.md` outputs are byte-identical
     And the line endings are `\n` (LF) on both platforms (the markdown renderer normalises CRLF to LF before writing)
     And the SHA-256 of the two outputs is equal
```

```gherkin
@happy
Feature: Documentation surfaces the cleanc shipment [impl-plan Stage 5.4]
  Scenario: README has the cleanc CLI section
    Given the updated `services/clean-code/README.md`
    When `grep -F "## cleanc CLI" services/clean-code/README.md` runs
    Then exit code is 0 and the match count is exactly 1

  Scenario: USAGE.md references the --emit-prompts flag
    Given `docs/cleanc/USAGE.md`
    When `grep -F "--emit-prompts" docs/cleanc/USAGE.md` runs
    Then exit code is 0 and the match count is `>= 1`

  Scenario: PROMPT-FORMAT.md pins the format version
    Given `docs/cleanc/PROMPT-FORMAT.md`
    When `grep -F "v1.2026.05" docs/cleanc/PROMPT-FORMAT.md` runs
    Then exit code is 0 and the match count is `>= 1`

  Scenario: CHANGELOG records the cleanc shipment
    Given the repo-root `CHANGELOG.md`
    When `grep -F "cleanc" CHANGELOG.md` runs
    Then exit code is 0 and the match count is `>= 1`
     And the match appears under an `## [Unreleased]` heading (verified by reading the file and asserting the first `## [` heading above the match line is `## [Unreleased]`)
```

```gherkin
@invariant
Feature: Release gate fails closed when any Phase 5 stage regresses [impl-plan Stage 5.3] [impl-plan Stage 5.4]
  Scenario: CI job summary
    Given a PR branch
    When CI runs the full Phase 5 matrix (`make build`, `make build-prod`, `make lint-cli`, `make test-prod`, `make e2e-cleanc`, docs `grep` checks)
    Then merging is blocked unless ALL of the following are green:
        | check                                          |
        | make build                                     |
        | make build-prod                                |
        | make lint-cli                                  |
        | make test-prod                                 |
        | make e2e-cleanc                                |
        | docs grep: ## cleanc CLI                       |
        | docs grep: --emit-prompts in USAGE.md          |
        | docs grep: v1.2026.05 in PROMPT-FORMAT.md      |
        | docs grep: cleanc under [Unreleased]           |
     And the merge gate is enforced via the existing `.github/workflows/clean-code-ci.yml` required-checks list extended for the `cleanc` jobs ([impl-plan Stage 5.1] `build-prod` job; [impl-plan Stage 5.2] `lint-cli` step)
```

---

## Coverage matrix -- scenarios x story-brief layers

The story brief's L1 - L9 gap analysis is the spine. Every layer
has at least one happy-path and at least one edge / invariant
scenario among the phases above.

| Layer | Disposition (story brief) | Phase + scenario tags that exercise it |
| --- | --- | --- |
| L1 Walker | missing | Phase 2 `@happy/@edge` skip rules, gitignore, size cap, missing root, `@cross-platform` symlink-loop |
| L2 Parser | reusable | Phase 2 `@happy` four-language parse fan-out; Phase 1 `@happy/@invariant` `cleanc version` parsers CSV |
| L3 Recipes | reusable, parser-gap-limited | Phase 2 `@happy` `loc` / `cycle_member` / `duplication_ratio`; Phase 2 `@dark` `cyclo` / `cognitive_complexity` / `fan_in` / `fan_out` / `lcom4`; Phase 3 `@dark` header surfaces it |
| L4 Rule engine | reusable | Phase 2 `@happy/@invariant` batched `InsertSamples` + root-commit registration; Phase 2 `@edge/@invariant` engine error -> exit 70 |
| L5 Planner / TaskPlanner | reusable | Phase 2 `@happy/@determinism` hot-spot ordering + canonical TaskKinds; Phase 2 `@happy/@invariant` effort fallback wired |
| L6 CLI composition root | missing | Phase 1 `@happy/@invariant` sub-command surface; Phase 3 `@happy` analyze E2E; Phase 3 `@edge` exit-code matrix |
| L7 Suggestion writer | architectural blocker (Option A only in v1) | Phase 4 `@happy/@invariant` `RefactorPromptRecord` shape, snippet cap, byte-stable emission; Phase 4 `@reserved` `apply` stub |
| L8 Policy signing | friction | Phase 1 `@happy/@invariant` dev-mode embedded packs; Phase 1 `@invariant/@security` `-tags prod` bypass exclusion; Phase 1 `@invariant/@security` banner text exact; Phase 5 `@invariant/@security` build-tag matrix + custom lints |
| L9 Effort ONNX | friction | Phase 1 `@happy/@determinism` formula table + clamps; Phase 1 `@invariant` `Mode()` returns `fallback`; Phase 2 `@happy/@invariant` `EffortHours > 0` and diagnostics record `fallback`; Phase 4 `@happy/@invariant` `effort_source` field in `RefactorPromptRecord` |

## Coverage matrix -- scenarios x tech-spec hard constraints

| Constraint | Anchor | Scenario reference |
| --- | --- | --- |
| C1 No PostgreSQL, no HTTP, no docker | tech-spec Sec 7 | Phase 1 / 2 / 3 / 4 Background lines ("no `CLEAN_CODE_PG_URL` set"); Phase 5 lint rule `no-production-sql-import` |
| C5 Canonical TaskKind enum is the only enum the emitter accepts | tech-spec Sec 7 | Phase 2 `@happy/@invariant` all TaskKinds canonical; Phase 4 `@invariant` non-canonical Kind exits 70 |
| C6 Production build excludes unsigned-policy bypass | tech-spec Sec 7 | Phase 1 `@invariant/@security` prod build Load returns sentinel; Phase 5 `@invariant/@security` `go list -tags prod` excludes `unsigned_dev.go` |
| C8 CLI imports zero Postgres / SQL packages | tech-spec Sec 7 | Phase 2 `@invariant` `make lint-cli` passes; Phase 5 `@invariant/@security` lint fixture violations refused |
| C9 Exit-code matrix is closed at `{0, 1, 2, 64, 70}` | tech-spec Sec 7 / 8.6 | Phase 3 `@happy` exit 1, Phase 3 `@edge` exit 2, Phase 3 `@edge` exit 64, Phase 2 `@edge/@invariant` exit 70; Phase 5 `@happy` exit-codes scenario covers the full matrix |
| C10 Dev banner text is byte-exact, stderr-only, structural | tech-spec Sec 7 | Phase 1 `@invariant/@security` banner text; Phase 3 `@invariant/@security` banner on stderr only |
| C11 All outputs are byte-stable across re-runs | tech-spec Sec 7 | Phase 3 `@determinism` markdown / JSON; Phase 4 `@happy/@determinism` JSONL ordering; Phase 5 `@cross-platform` golden bytes |
| C12 Snippet preserves raw file bytes (no parser normalisation) | tech-spec Sec 7 | Phase 4 `@invariant` UTF-8 + tab preserved |
| C15 Effort estimator mode surfaced in every output | tech-spec Sec 7 | Phase 1 `@invariant` `Mode()` returns `fallback`; Phase 2 `@happy/@invariant` diagnostics record `fallback`; Phase 4 `@happy/@invariant` `effort_source` in `RefactorPromptRecord` |

## Cross-doc consistency notes

The following minor inconsistencies between the parallel-authored
sibling docs were observed during this iteration and surfaced
here so the next iteration of either doc can align without
re-litigating:

1. **`//go:embed` pattern path.** [arch Sec 3.8] lines 604 - 606
   and [tech-spec Sec 8.4] lines 940 - 942 describe the embed
   pattern as `../../policy/rulepacks/...` for prose readability;
   [impl-plan Stage 1.4] line 94 - 95 correctly notes that Go's
   `//go:embed` directive does NOT support `..` and pins the
   actual file to live IN the `rulepacks` package as
   `services/clean-code/policy/rulepacks/embedded_fs.go`. The
   scenarios in Phase 1 ("Embedded rule packs load with a stable
   PolicyVersionID") test the impl-plan-correct location; no
   change is required to the test scenarios, but
   `architecture.md` / `tech-spec.md` would benefit from a
   one-line clarification.
2. **`HotSpot.Breakdown` is z-scores only.** [arch Sec 3.5] line
   674 lists `ScopeInputs.Loc`, `ScopeInputs.Cyclo`,
   `ScopeInputs.FanIn` as the inputs to the effort fallback and
   notes "the values are already on the `Breakdown` carried by
   `PlanResult`". [impl-plan Stage 1.3] line 74 corrects that:
   `HotSpot.Breakdown` carries only z-scores
   (`ComplexityZ`/`ChurnZ`/`CouplingZ`) per
   `services/clean-code/internal/refactor/hotspot.go:264-284`,
   not raw `loc`/`fan_in`. The Phase 1 effort scenarios test the
   impl-plan-correct path (the `EffortInputProvider` indexes the
   `InMemoryMetricSample` rows by `(scope_id, metric_kind)`); no
   change is required to the test scenarios.

These notes are repeated from `implementation-plan.md` so the
QA contract does not silently drift from the as-built behaviour.

## Iteration summary anchor

This document is the QA contract for `cleanc`. Each Phase H1
mirrors a `implementation-plan.md` Phase H1 one-for-one. Each
scenario is anchored to the spec lines it enforces. The
coverage matrices above let the evaluator confirm that every
L1 - L9 layer and every `C1 - C15` constraint has at least one
scenario.
