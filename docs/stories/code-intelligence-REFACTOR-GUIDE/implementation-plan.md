---
title: "refactor guide"
storyId: "code-intelligence:REFACTOR-GUIDE"
---

> Livedoc -- ticks track delivery of the `cleanc analyze <path>` CLI
> defined in `architecture.md` Sec 3 (L1 - L9 components) and bound
> by the hard constraints C1 - C15 in `tech-spec.md` Sec 7. Anchors:
> phase slugs derive from the Phase H1 name; stage slugs derive from
> the Stage H2 name. Every Stage's `### Implementation Steps` maps
> back to a labelled cell in the story brief's "Concrete file plan"
> or "Phased roadmap" table. The plan covers the story brief's P0
> (Phases 1 - 3) and P1 (Phase 4) scopes; P2 (parser-attr expansion)
> and P3 (mechanical patches) are explicitly deferred to follow-up
> stories per `architecture.md` Sec 9 operator pin `cli-l7-authority`
> -- this plan only ships the reserved-verb plumbing for them
> (Stage 4.4).

# Phase 1: Foundations

## Dependencies

- _none -- start phase_

## Stage 1.1: CLI Binary Skeleton

### Implementation Steps

- [ ] Create `services/clean-code/cmd/cleanc/main.go` with a sub-command dispatcher (`analyze`, `report`, `version`, `apply`) returning the exit codes pinned in `tech-spec.md` Sec 8.6 (`0` / `1` / `2` / `64` / `70`).
- [ ] Add `cleanc` to the Makefile `CMD_DIRS` discovery so `make build` emits `bin/cleanc` alongside the six existing service binaries (per `cli-binary-location` pin, `architecture.md` Sec 1.3).
- [ ] Wire global flags (`--out`, `--findings`, `--emit-prompts`, `--policy`, `--with-churn`, `--top-n`, `--exit-on`, `--diagnostics`, `--dev-mode`, `--telemetry-otlp`) per `tech-spec.md` Sec 8.1; defaults set via constants in a `internal/cli/flags` helper file.
- [ ] Implement `cleanc version` to print binary version + `parser.DefaultRegistry().Languages()` + the embedded rule-pack ids and versions (placeholder values for the pack ids until Stage 1.4 lands a real loader).
- [ ] Add `cleanc help <subcommand>` text scraped from a per-sub-command `usage` constant; reject unknown sub-commands with exit code 64.

### Dependencies

- _none -- start stage_

### Test Scenarios

- [ ] Scenario: version sub-command -- Given a built `cleanc` binary, When the user runs `cleanc version`, Then stdout includes `version=`, `parsers=[go,python,typescript,java]`, and exit code is 0.
- [ ] Scenario: unknown sub-command -- Given a built `cleanc` binary, When the user runs `cleanc frobnicate`, Then stderr includes `unknown sub-command`, exit code is 64, and no output is emitted to `--out`.
- [ ] Scenario: help on missing args -- Given a built `cleanc` binary, When the user runs `cleanc analyze` with no path argument, Then stderr prints the analyze usage block and exit code is 64.
- [ ] Scenario: makefile discovery -- Given a clean checkout, When the developer runs `make -C services/clean-code build`, Then `services/clean-code/bin/cleanc` exists and is executable.

## Stage 1.2: Repo Context and Scope Binding

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/repocontext/repocontext.go` with a `RepoContext` struct matching `architecture.md` Sec 4.1 fields (`RootPath`, `RepoID`, `HeadSHA`, `ModulePath`, `IsGitRepo`).
- [ ] Implement `MintRepoID(rootPath string) uuid.UUID` as `uuid.NewSHA1(namespaceURL, []byte("cleanc.local-repo/"+normalisedPath))` so re-runs on the same path yield the same id (CLEAN-CODE arch G2; `tech-spec.md` C3).
- [ ] Implement `DetectHeadSHA(rootPath string) (string, bool)` that shells out to `git rev-parse HEAD` (no go-git dep), returns `"working-copy"` when the path is not a git repo (per `architecture.md` Sec 4.1 note).
- [ ] Implement `DetectModulePath(rootPath, language string) string` with per-language strategies: Go reads `go.mod` `module` line; TS reads `package.json` `name`; Java reads top-level `package` declaration of the first source file; Python reads `pyproject.toml` PEP 621 `name`.
- [ ] Create `services/clean-code/internal/cli/scopebinding/scopebinding.go` exposing `Table` (a `sync.Map` keyed on `ScopeID`) and `Insert(ScopeBinding) / Get(ScopeID) (ScopeBinding, bool)` per `architecture.md` Sec 4.3.
- [ ] Add a `ScopeBinding.MintScopeID(repoID uuid.UUID, scopeKind, canonicalSignature, firstSeenSHA string) uuid.UUID` helper using the same UUID-v5 derivation pinned in CLEAN-CODE arch G2 so identity is stable across re-runs (`tech-spec.md` C3, R2 mitigation).

### Dependencies

- _none -- start stage_

### Test Scenarios

- [ ] Scenario: stable repo id -- Given an absolute root path `/tmp/foo`, When `MintRepoID` is called twice, Then both calls return the same UUID-v5 value byte-for-byte.
- [ ] Scenario: working-copy fallback -- Given a directory that is not a git repo, When `DetectHeadSHA` runs, Then it returns the literal string `working-copy` and `IsGitRepo == false`.
- [ ] Scenario: module path from go.mod -- Given a `go.mod` file containing `module github.com/example/foo`, When `DetectModulePath(root, "go")` runs, Then it returns `github.com/example/foo`.
- [ ] Scenario: scope binding round trip -- Given a `ScopeBinding` inserted with `ScopeID = X`, When `Get(X)` runs, Then it returns the same struct with `FilePath`, `StartLine`, `EndLine`, and `Signature` intact.

## Stage 1.3: Effort Estimator Fallback

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/effort/effort.go` with a `FallbackModel` struct implementing the `refactor.EffortModel` interface (`services/clean-code/internal/refactor/effort_model.go:140-155`).
- [ ] Implement the deterministic formula pinned in `tech-spec.md` Sec 8.5: `base = 0.02*loc + 0.10*cyclo + 0.05*fan_in + 1.0`, multiply by `taskKindFactor[TaskKind]` (1.5 / 1.3 / 1.4 / 0.7 / 1.0), clamp to `[0.1, 80.0]`, round half-up to 1 decimal.
- [ ] Implement input extraction from `refactor.HotSpot.Breakdown` z-scores reversed against the policy weights so `loc`, `cyclo`, `fan_in` are read without re-querying the metric reader; treat missing dark metrics as `0` per `tech-spec.md` Sec 8.5 note.
- [ ] Expose `func New(logger *slog.Logger) refactor.EffortModel` that logs one WARNING line on first invocation per run: `"effort estimator using deterministic fallback formula <version> (no ONNX model loaded)"` (per `architecture.md` Sec 3.5).
- [ ] Add an `EffortSource` enum (`ml` / `fallback`) and a constructor option `effort.WithSourceTag(string)` so callers can stamp every produced `EffortHours` with provenance (consumed by Phase 4 prompt emitter).
- [ ] Add a single-line opt-in `Mode() string` method so the orchestrator's diagnostics writer can surface the effort mode in every `RunArtifact` (`tech-spec.md` C15).

### Dependencies

- _none -- start stage_

### Test Scenarios

- [ ] Scenario: deterministic output -- Given fixture inputs `{loc: 500, cyclo: 20, fan_in: 8, TaskKind: split_class}`, When `FallbackModel.EstimateEffort` runs, Then result is `(0.02*500 + 0.10*20 + 0.05*8 + 1.0) * 1.5 = 19.05` rounded to `19.1` hours.
- [ ] Scenario: clamp upper bound -- Given fixture inputs that would compute to 120.0 hours, When estimation runs, Then result is exactly `80.0`.
- [ ] Scenario: clamp lower bound -- Given fixture inputs that would compute to 0.05 hours, When estimation runs, Then result is exactly `0.1`.
- [ ] Scenario: task-kind multiplier -- Given identical metric inputs, When estimation runs for `split_class` vs `extract_method`, Then the `split_class` output is exactly `1.5 / 0.7` times the `extract_method` output (modulo rounding).

## Stage 1.4: Dev Policy Loader

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/devpolicy/embed.go` (no build tag) declaring `//go:embed ../../../policy/rulepacks/solid/*.yaml ../../../policy/rulepacks/decoupling/*.yaml` into a `var embeddedRulePacks embed.FS` (per `cli-policy-distribution` pin, `architecture.md` Sec 1.3, Sec 3.8).
- [ ] Create `services/clean-code/internal/cli/devpolicy/loader.go` with a `Loader` interface and `Bundle` struct matching `architecture.md` Sec 5.8 (`PolicyVersion`, `Rules`, `RulePacks`).
- [ ] Implement YAML decoding into canonical `steward.RulePack` / `steward.Rule` shapes (`services/clean-code/internal/policy/steward/types.go:112-200`) -- one `Rule` per `rules:` entry, one `RulePack` per file.
- [ ] Implement `synthesisePolicyVersion(rules []steward.Rule, weights refactor.PolicyWeights) steward.PolicyVersion` returning a `PolicyVersion` whose `Signature == nil` and whose `PolicyVersionID` is `UUID-v5(namespace=cleanc.dev-policy, name=sha256(sorted_rule_ids)+effort_model_version)` so it is stable per `(loaded packs, effort model)` (`architecture.md` Sec 4.5; `tech-spec.md` C11).
- [ ] Create `services/clean-code/internal/cli/devpolicy/unsigned_dev.go` carrying the `//go:build !prod` constraint; it contains the only function in the tree that constructs the unsigned `PolicyVersion` (`tech-spec.md` C6).
- [ ] Create `services/clean-code/internal/cli/devpolicy/unsigned_prod.go` carrying `//go:build prod`; the function body is `return Bundle{}, errors.New("dev-mode policy bypass not available in prod build")` so a prod build fails fast (`architecture.md` Sec 7.2).
- [ ] Implement filesystem override `LoadFromDir(path string) (Bundle, error)` for `--policy <path>`; only callable from the `!prod` file so prod builds reject the flag at compile time (`tech-spec.md` Sec 8.4).
- [ ] Add a `EmitBanner(w io.Writer)` helper that writes the exact text `"WARNING: dev-mode policy is unsigned. Do NOT use cleanc output as the source of truth for a production gate."` and is called unconditionally at the start of every `analyze` run when the active build allows the bypass (`tech-spec.md` C10).

### Dependencies

- _none -- start stage_

### Test Scenarios

- [ ] Scenario: embedded packs loaded -- Given a no-tag build, When `Loader.Load` is called with `LoaderSource{UseEmbedded: true}`, Then the returned `Bundle.Rules` contains at least one rule for every YAML under `services/clean-code/policy/rulepacks/{solid,decoupling}/`.
- [ ] Scenario: stable policy id -- Given the same embedded pack set twice, When `synthesisePolicyVersion` is called both times, Then the returned `PolicyVersionID` is byte-for-byte identical.
- [ ] Scenario: filesystem override -- Given a temp directory with `custom.yaml` matching the embedded shape, When `Loader.Load(LoaderSource{UseEmbedded: false, DirPath: tmp})` runs in a dev build, Then the returned `Bundle.RulePacks` length is 1 and the rule ids match the YAML.
- [ ] Scenario: prod build refuses bypass -- Given a `go build -tags prod` of `internal/cli/devpolicy/`, When the test calls `Loader.Load`, Then it returns the error `dev-mode policy bypass not available in prod build`.
- [ ] Scenario: banner text exact -- Given a dev build, When `EmitBanner` writes to a `bytes.Buffer`, Then the buffer contents exactly equal the C10 banner string (byte-for-byte).

# Phase 2: Pipeline

## Dependencies

- phase-foundations

## Stage 2.1: Repo Walker

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/walk/walker.go` with a `Walker` interface returning `(<-chan WalkedFile, <-chan WalkSkip, <-chan error)` per `architecture.md` Sec 5.1.
- [ ] Implement `DefaultWalker` using `filepath.WalkDir` with the hard-coded skip directory list (`.git/`, `node_modules/`, `vendor/`, `target/`, `dist/`, `build/`, `.next/`, `__pycache__/`, `.venv/`, `venv/`) per `architecture.md` Sec 3.1.
- [ ] Integrate the `github.com/go-git/go-git/v5/plumbing/format/gitignore` matcher to honour `.gitignore` / `.git/info/exclude`; surface skipped files as `WalkSkip{Reason: "gitignore"}`.
- [ ] Enforce the 2 MiB per-file size cap from `tech-spec.md` Sec 8.3; oversize files emit `WalkSkip{Reason: "size_cap"}` and the byte content is never read.
- [ ] Implement language detection via `parser.DetectLanguage`; files whose language is not in `{go, python, typescript, java}` emit `WalkSkip{Reason: "unsupported_language"}`.
- [ ] Implement symlink-loop guard: a `visited` set keyed on `(device_id, inode)` on POSIX and the canonical path string on Windows (`architecture.md` Sec 3.1 "Failure modes").
- [ ] Sort `WalkedFile` emission lexicographically by `RepoRelPath` so re-runs of the orchestrator produce identical sample ordering (`architecture.md` Sec 10 "Determinism" bullet; `tech-spec.md` C11).
- [ ] Return `walk.ErrRootNotFound` when the root path does not exist; the orchestrator translates this to exit code 2 (`tech-spec.md` Sec 8.6).

### Dependencies

- phase-foundations/stage-cli-binary-skeleton

### Test Scenarios

- [ ] Scenario: skip list honoured -- Given a fixture repo containing `node_modules/foo.js`, When the walker traverses, Then `node_modules/foo.js` does not appear in `WalkedFile` and a `WalkSkip{Reason: "directory_skip"}` row is emitted for the directory.
- [ ] Scenario: gitignore honoured -- Given a fixture repo whose `.gitignore` lists `secret.txt`, When the walker traverses, Then `secret.txt` produces a `WalkSkip{Reason: "gitignore"}` and zero `WalkedFile` rows for that path.
- [ ] Scenario: size cap -- Given a 3 MiB `.go` file under root, When the walker traverses, Then it emits `WalkSkip{Reason: "size_cap"}` and never reads the file's bytes (verified via a `fs.FS` test double whose `Open` panics on the path).
- [ ] Scenario: missing root -- Given a path that does not exist, When `Walk` runs, Then the error channel yields `walk.ErrRootNotFound` and the file channel closes with zero rows.
- [ ] Scenario: deterministic ordering -- Given a fixture repo with files `b.go`, `a.go`, `c.go`, When the walker traverses twice, Then both runs emit `WalkedFile` in identical order (`a.go`, `b.go`, `c.go`).

## Stage 2.2: Parse and Recipe Fanout

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/orchestrator/orchestrator.go` with an `Orchestrator` struct holding the walker, parser registry, recipe registry, and a `GOMAXPROCS`-sized worker pool (`architecture.md` Sec 10).
- [ ] Implement parse fan-out: for each `WalkedFile`, dispatch to a worker that calls `parser.DefaultRegistry().Parse(ctx, file.RepoRelPath, file.Content)` and pushes the resulting `*parser.AstFile` to a result channel.
- [ ] Implement recipe fan-out: for each `*AstFile`, iterate `recipes.DefaultProjectRegistry().Recipes()` and call `Recipe.AppliesTo(file)` then `Recipe.Compute(file)`; collect `MetricSampleDraft` rows into a slice keyed by `(metric_kind, scope_id)`.
- [ ] Stamp `AttrModulePath` on each `*AstFile` from the `RepoContext.ModulePath` before calling recipes so `cycle_member` can resolve intra-repo imports (`architecture.md` Sec 4.1 note).
- [ ] For each parsed `*AstFile`, walk its scope tree and `scopebinding.Table.Insert` one `ScopeBinding` per encountered scope so Phase 4's prompt emitter can resolve `ScopeID` -> `(file_path, start_line, end_line)`.
- [ ] Surface parser panics (defensive `recover()` in the worker) as `WalkSkip{Reason: "parser_panic"}` rows instead of crashing the orchestrator; the orchestrator's exit code is `70` only when the panic happens outside per-file parsing (`tech-spec.md` Sec 8.6).
- [ ] Sort the collected `MetricSampleDraft` slice by `(metric_kind, scope_id)` before handing it to Stage 2.3 so engine input ordering is deterministic (`tech-spec.md` C11).

### Dependencies

- phase-foundations/stage-repo-context-and-scope-binding
- phase-pipeline/stage-repo-walker

### Test Scenarios

- [ ] Scenario: parse all four languages -- Given a fixture repo with one file each of Go/Python/TypeScript/Java, When the orchestrator runs the parse stage, Then four `*AstFile` rows are collected and zero `WalkSkip{Reason: "unsupported_language"}` rows are emitted.
- [ ] Scenario: loc recipe lights up -- Given a fixture Go file of known line count `N`, When recipes run, Then a `MetricSampleDraft{MetricKind: "loc", Value: N}` is collected.
- [ ] Scenario: dark cyclo recipe -- Given a fixture Go file (today's parser does not stamp `decision_blocks`), When recipes run, Then `Recipe.AppliesTo` returns false for `cyclo` and zero `MetricSampleDraft` rows for `metric_kind == "cyclo"` are emitted.
- [ ] Scenario: scope binding populated -- Given a fixture Go file with a function `Foo`, When parse + recipe fan-out completes, Then `scopebinding.Table.Get(scopeIDFor("Foo"))` returns a row whose `Signature` ends with `::Foo` and whose `StartLine`/`EndLine` enclose the function body.
- [ ] Scenario: parser panic is non-fatal -- Given a fixture file that triggers a panic in the parser stub (via test double), When the orchestrator runs, Then `WalkSkip{Reason: "parser_panic", Path: <file>}` is emitted and the orchestrator exits cleanly through the remainder of the corpus.

## Stage 2.3: Rule Engine Wiring

### Implementation Steps

- [ ] In `internal/cli/orchestrator`, add `loadStore(bundle devpolicy.Bundle, samples []rule_engine.Sample) *rule_engine.InMemoryStore` that calls `InsertPolicyVersion(bundle.PolicyVersion)` and one `InsertRule` per `bundle.Rules` entry (`architecture.md` Sec 3.4 steps 1 - 2).
- [ ] Convert each `MetricSampleDraft` from Stage 2.2 into a `rule_engine.Sample` per `architecture.md` Sec 4.4 field mapping; stamp `ScopeSignature` from `scopebinding.Table.Get`.
- [ ] Call `store.InsertSamples(repoCtx.RepoID, repoCtx.HeadSHA, samples)` (the canonical plural / batched API at `services/clean-code/internal/rule_engine/inmem_store.go:144-151`); fail-fast on any returned error (exit code 70).
- [ ] Call `store.RegisterCommit(repoCtx.RepoID, repoCtx.HeadSHA, "")` (empty parent SHA -> root commit, per `inmem_store.go:412-423`) so every firing rule emits a `delta=new` finding.
- [ ] Construct the engine via `rule_engine.New(rule_engine.Config{Store: store})` (canonical constructor name; `engine.go:130-162`) and call `engine.RunBatch(ctx, repoCtx.RepoID, repoCtx.HeadSHA, bundle.PolicyVersion.PolicyVersionID)`.
- [ ] Read back `EvaluationRun`, `EvaluationVerdict`, and `[]Finding` via the store's `Runs()` / `Verdicts()` / `Findings()` accessors (`inmem_store.go:700-720`); attach them to a partially-populated `RunArtifact` (Section 4.7).
- [ ] When `engine.RunBatch` returns a non-nil error, log it with the run id (if any) and exit code 70; do NOT swallow the error into the artifact.

### Dependencies

- phase-foundations/stage-dev-policy-loader
- phase-pipeline/stage-parse-and-recipe-fanout

### Test Scenarios

- [ ] Scenario: smoke run on fixture -- Given a fixture with one Go file containing a 2000-line class (triggers `loc >= 1500` rule), When the orchestrator runs the engine stage, Then `RunArtifact.Findings` has at least one entry with `RuleID` from the SRP / cohesion pack and `Delta == "new"`.
- [ ] Scenario: empty corpus -- Given a fixture repo with zero source files, When the engine stage runs, Then `Findings` is empty, `Verdict.Verdict == "pass"`, and exit code is 0.
- [ ] Scenario: store wiring uses plural insert -- Given a test double recording calls on `InsertSamples`, When the orchestrator's engine stage runs, Then `InsertSamples` is called exactly once with the full batch (no per-row `InsertSample` calls).
- [ ] Scenario: engine error surfaces -- Given a test double `Store` whose `InsertPolicyVersion` returns an error, When the orchestrator runs, Then the binary exits with code 70 and stderr contains the engine error string.

## Stage 2.4: Planner and Task Planner Wiring

### Implementation Steps

- [ ] In `internal/cli/orchestrator`, add a `cliPolicyReader` struct satisfying `refactor.PolicyReader` (`services/clean-code/internal/refactor/planner.go:39`) by returning `(PolicySnapshot{PolicyVersionID: bundle.PolicyVersion.PolicyVersionID, Weights: bundle.PolicyVersion.RefactorWeights}, true, nil)`.
- [ ] Populate `refactor.NewInMemoryMetricSampleReader()` with the per-scope foundation-tier samples filtered to `refactor.HotSpotInputMetricKinds`.
- [ ] Populate `refactor.NewInMemoryFindingReader()` from `RunArtifact.Findings` filtered to `delta IN ('new','newly_failing')` (per `architecture.md` Sec 3.5).
- [ ] Construct the Planner via `refactor.NewPlanner(policyReader, metricReader, findingReader, hotSpotWriter)` and call `planner.Plan(ctx, repoCtx.RepoID, repoCtx.HeadSHA)`.
- [ ] Populate `refactor.NewInMemoryHotSpotReader()` from the just-written hot-spot batch and `refactor.NewInMemoryFindingDetailReader()` by joining each `Finding` to its `Rule` from the policy bundle.
- [ ] Construct the TaskPlanner via `refactor.NewTaskPlanner(policyReader, hotSpotReader, findingDetailReader, planTaskWriter, refactor.WithEffortModel(effort.New(logger)))` so the Stage 1.3 fallback is the active estimator unless the future ONNX loader pins something else.
- [ ] Call `taskPlanner.Plan(ctx, repoCtx.RepoID, repoCtx.HeadSHA)` and attach the resulting `PlanAndTasksResult.Plan` + `Tasks` to `RunArtifact`.
- [ ] Pin `tp.TopN` from `--top-n` flag when non-zero, else from `bundle.PolicyVersion.RefactorWeights.TopN`, else default `20` (per `architecture.md` Sec 3.6 flag table).

### Dependencies

- phase-foundations/stage-effort-estimator-fallback
- phase-pipeline/stage-rule-engine-wiring

### Test Scenarios

- [ ] Scenario: hot-spot ranking populated -- Given a fixture run with three findings on three different scopes, When the planner stage runs, Then `RunArtifact.HotSpots` length is 3 and rows are sorted by `Score` descending then `ScopeID` ascending.
- [ ] Scenario: task kinds canonical -- Given a fixture run producing one task per kind, When the task planner stage runs, Then every `Tasks[i].Kind` satisfies `refactor.IsCanonicalTaskKind` (no rejected aliases reach the output).
- [ ] Scenario: effort fallback wired -- Given a fixture where no ONNX model is configured, When the task planner runs, Then every `Tasks[i].EffortHours > 0` and the diagnostics record `effort_source == "fallback"`.
- [ ] Scenario: top-n flag override -- Given a fixture with 50 hot-spots and `--top-n 5`, When the analyze command runs, Then `RunArtifact.Tasks` length is at most 5.

## Stage 2.5: Dark Metric Diagnostics

### Implementation Steps

- [ ] In `internal/cli/orchestrator`, add a constant slice `metricAttrRequirements []metricAttrRow{ {Kind: "cyclo", Attrs: []string{"decision_blocks"}}, {Kind: "cognitive_complexity", Attrs: []string{"decision_blocks"}}, {Kind: "fan_in", Attrs: []string{"call_edges"}}, {Kind: "fan_out", Attrs: []string{"call_edges"}}, {Kind: "lcom4", Attrs: []string{"call_edges","field_accesses"}}, ... }` mirroring the `AppliesTo` gates in `recipes/recipe.go:55-122` (`architecture.md` Sec 3.3, Sec 5.3).
- [ ] When a recipe returns `AppliesTo(file) == false`, look up the recipe's `MetricKind` in `metricAttrRequirements`; if matched, increment a `darkMetrics[(metric_kind, language)].affected_scope_count` counter.
- [ ] Emit one `DarkMetric` row per `(metric_kind, language)` pair into `RunArtifact.Diagnostics.DarkMetrics` with `closure_phase: "P2"` (per `tech-spec.md` Sec 8.7).
- [ ] Fail-closed when an unknown `missing_attrs` value is computed (i.e. the lookup table is stale relative to the recipes): exit code 70 with stderr `"orchestrator: unknown parser attr in dark-metric table: <attr>"` (`tech-spec.md` Sec 8.7 last paragraph).
- [ ] Add a `Diagnostics.EffortSource string` field stamped by the active `effort.EffortModel.Mode()` so every run advertises whether `ml` or `fallback` was used (`tech-spec.md` C15).
- [ ] Add a `Diagnostics.SkippedFiles []WalkSkip` slice populated from the walker's skip channel so the report renderer (Stage 3.1) can surface the count and reasons.

### Dependencies

- phase-pipeline/stage-parse-and-recipe-fanout

### Test Scenarios

- [ ] Scenario: cyclo dark on Go -- Given a fixture Go file with one function (today's parser does not stamp `decision_blocks`), When the orchestrator runs, Then `Diagnostics.DarkMetrics` includes `{metric_kind: cyclo, language: go, missing_attrs: [decision_blocks], affected_scope_count: 1, closure_phase: P2}`.
- [ ] Scenario: loc not flagged dark -- Given the same fixture, When the orchestrator runs, Then `Diagnostics.DarkMetrics` does NOT include any row with `metric_kind == "loc"`.
- [ ] Scenario: unknown attr fails closed -- Given a recipe registered with a `MetricKind` not present in `metricAttrRequirements` and a fake `AppliesTo` returning false, When the orchestrator runs in a test that adds a `bogus_attr` to the table, Then the binary exits code 70 with the stderr error string from `tech-spec.md` Sec 8.7.
- [ ] Scenario: effort source recorded -- Given a fixture where the ONNX path resolves to a missing file, When the orchestrator runs, Then `Diagnostics.EffortSource == "fallback"`.

# Phase 3: P0 Reports and Delivery

## Dependencies

- phase-pipeline

## Stage 3.1: Markdown Report Renderer

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/report/markdown.go` exposing `type Markdown struct{}` that satisfies `Renderer` (`architecture.md` Sec 5.7).
- [ ] Render the header block: repo path, head SHA, policy id + version, active parser fleet from `parser.DefaultRegistry().Languages()`, dark-metric summary count (`architecture.md` Sec 3.7.1 step 1).
- [ ] Render the verdict block: a single line `"Verdict: <pass|warn|block>"` echoing `RunArtifact.Verdict.Verdict`.
- [ ] Render findings grouped by `Severity` (`info` / `warn` / `block`) with per-row `(scope_signature, rule_id, metric_kind, value, threshold)` and the rule's `DescriptionMD` "Suggested refactor:" excerpt extracted via a `extractSuggestedRefactor(descriptionMD string) string` helper that scans for the literal marker.
- [ ] Render the hot-spot ranking table: top-N rows (using policy `TopN`) in `(Score DESC, ScopeID ASC)` order with `(scope_signature, score, breakdown_z_scores, finding_count)`.
- [ ] Render the refactor plan block: `RefactorPlan.SummaryMD` then a `RefactorTask` table grouped by `TaskKind` with `(scope_signature, kind, effort_hours, rule_id, description_md)`.
- [ ] Render the diagnostics block: skipped files count, per-`(metric_kind, language)` dark-metric rows, effort estimator mode tag.
- [ ] Honour stable ordering everywhere: sort findings by `(severity DESC, rule_id ASC, scope_id ASC)` and tasks by `(task_kind ASC, scope_id ASC)` so two runs of `cleanc analyze` on the same repo produce byte-identical markdown (`tech-spec.md` C11).

### Dependencies

- phase-pipeline/stage-planner-and-task-planner-wiring
- phase-pipeline/stage-dark-metric-diagnostics

### Test Scenarios

- [ ] Scenario: empty corpus renders pass -- Given a `RunArtifact` with zero findings, When `Markdown.Render` runs, Then output contains `Verdict: pass` and a non-empty diagnostics block.
- [ ] Scenario: byte-identical re-render -- Given the same `RunArtifact` rendered twice, When the outputs are compared, Then they are byte-identical.
- [ ] Scenario: dark-metric surfaced -- Given a `RunArtifact` whose `Diagnostics.DarkMetrics` includes a `cyclo` row, When `Markdown.Render` runs, Then output contains the literal phrase `metric dark: cyclo` (or equivalent unambiguous tag).
- [ ] Scenario: suggested refactor excerpt -- Given a rule with `DescriptionMD` containing `"Suggested refactor: split the class..."`, When the renderer extracts the excerpt, Then the output's finding row carries the suffix `split the class...`.

## Stage 3.2: JSON Findings Artifact

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/report/json.go` exposing `type JSON struct{}` that satisfies `Renderer`.
- [ ] Marshal the full `RunArtifact` (`architecture.md` Sec 4.7) one-for-one; do NOT collapse fields or add new ones so downstream consumers can `json.Unmarshal` into the same struct.
- [ ] Use `encoding/json` with `SetIndent("", "  ")` and `SetEscapeHTML(false)`; UUIDs serialise via their canonical RFC 4122 string form.
- [ ] Sort every slice in the artifact before marshalling (findings, hot-spots, tasks, samples, walk-skips, dark-metrics) so JSON output is also byte-stable across runs.
- [ ] Stamp `RunArtifact.SchemaVersion: "v1.2026.05"` so future shape changes are detectable (mirrors `prompt_format_version` from Stage 4.1).
- [ ] Add a `JSON.RenderFromBytes(prevFindings []byte, w io.Writer) error` helper used by `cleanc report` (Stage 3.4) to re-emit a previously written `findings.json` without re-running the pipeline.

### Dependencies

- phase-p0-reports-and-delivery/stage-markdown-report-renderer

### Test Scenarios

- [ ] Scenario: round-trip via Unmarshal -- Given a `RunArtifact` rendered by `JSON.Render` and then `json.Unmarshal`'d back, When the resulting struct is compared to the original, Then all fields match.
- [ ] Scenario: byte-stable across runs -- Given the same `RunArtifact` marshalled twice, When the outputs are compared, Then they are byte-identical.
- [ ] Scenario: schema version stamped -- Given any non-empty `RunArtifact`, When `JSON.Render` runs, Then the output JSON contains `"schemaVersion": "v1.2026.05"` (or the camelCase equivalent if the marshaller tag pins it).

## Stage 3.3: Analyze End To End Wiring

### Implementation Steps

- [ ] In `cmd/cleanc/main.go`, implement `runAnalyze(ctx, flags) int` that calls `devpolicy.EmitBanner(stderr)` (when dev build), constructs `repocontext.RepoContext`, runs the orchestrator, and dispatches the assembled `RunArtifact` to the requested renderers.
- [ ] Wire `--out` (markdown), `--findings` (JSON), `--diagnostics` (JSON) outputs through `os.Create` + a `defer w.Close()` pattern; stdout default for `--out`.
- [ ] Translate `RunArtifact.Verdict.Verdict` + `--exit-on <sev>` into the exit code: `0` when no finding meets the threshold, `1` when at least one does (`tech-spec.md` Sec 8.6, C9).
- [ ] Translate walker errors / parser internal errors / engine errors into the exit-code matrix per `tech-spec.md` Sec 8.6 (`2` / `70`); a `panic` anywhere in the pipeline is recovered in `main` and surfaced as exit code 70 with the stack trace on stderr.
- [ ] Honour `--dev-mode=false` by refusing to start: print `"dev-mode disabled but no signed-policy loader available; rebuild with -tags prod"` and exit 64.
- [ ] Reject unknown flag values for `--exit-on` (must be one of `info`/`warn`/`block`) at flag-parse time with exit 64.
- [ ] Add a `--profile <cpu|mem|trace>` reserved flag that exits 64 in P0/P1 with a `not yet implemented` message so the surface is locked.

### Dependencies

- phase-p0-reports-and-delivery/stage-markdown-report-renderer
- phase-p0-reports-and-delivery/stage-json-findings-artifact

### Test Scenarios

- [ ] Scenario: happy path -- Given a fixture repo with one block-severity finding, When `cleanc analyze <fixture> --out report.md --findings findings.json --exit-on block` runs, Then `report.md` and `findings.json` are written and the process exits with code 1.
- [ ] Scenario: walker error exit code -- Given a non-existent root path, When `cleanc analyze /no/such/path` runs, Then the process exits with code 2 and stderr contains `ErrRootNotFound`.
- [ ] Scenario: invalid exit-on -- Given `--exit-on critical`, When `cleanc analyze . --exit-on critical` runs, Then the process exits with code 64 before any pipeline stage starts.
- [ ] Scenario: dev banner emitted -- Given a dev build, When `cleanc analyze .` runs, Then stderr begins with the exact C10 banner string.
- [ ] Scenario: stdout default -- Given a fixture repo and no `--out` flag, When `cleanc analyze <fixture>` runs, Then markdown is written to stdout and exit code is 0 or 1 (severity dependent).

## Stage 3.4: Report And Version Subcommands

### Implementation Steps

- [ ] Implement `cleanc report <findings.json> [--out report.md]` in `cmd/cleanc/main.go` that calls `report.JSON.RenderFromBytes` to unmarshal a previously written artifact and re-render markdown without re-running the pipeline.
- [ ] Refuse to read a `findings.json` whose `schemaVersion` does not match the current binary's version constant; exit 64 with a clear message naming both versions.
- [ ] Finalise `cleanc version` output: pin the format `"cleanc <semver> (build-tag=<tag>) (parsers=<csv>) (rule-packs=<csv>)"` and assert it in a test.
- [ ] Add `cleanc help` (with and without sub-command) that prints either the global usage block or the sub-command's `usage` constant; exit 0.
- [ ] Add a hidden `cleanc internal-self-check` sub-command that runs the dev-policy loader against the embedded packs and exits 0; used by Stage 5.3 e2e tests to verify the prod build excludes the bypass.

### Dependencies

- phase-p0-reports-and-delivery/stage-json-findings-artifact

### Test Scenarios

- [ ] Scenario: report re-render -- Given a `findings.json` previously written by an analyze run, When `cleanc report findings.json --out replay.md` runs, Then `replay.md` is byte-identical to the markdown that the analyze run emitted.
- [ ] Scenario: schema mismatch refused -- Given a `findings.json` whose `schemaVersion` is `"v0.0.0"`, When `cleanc report findings.json` runs, Then exit code is 64 and stderr names both schema versions.
- [ ] Scenario: version format -- Given a built binary, When `cleanc version` runs, Then stdout matches the regex `^cleanc \d+\.\d+\.\d+ \(build-tag=.+\) \(parsers=.+\) \(rule-packs=.+\)$`.
- [ ] Scenario: prod build self-check refuses -- Given a `-tags prod` build, When `cleanc internal-self-check` runs, Then exit code is 70 and stderr contains `dev-mode policy bypass not available in prod build`.

## Stage 3.5: P0 Fixture Corpus And Golden Snapshots

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/testdata/fixtures/<lang>/` directories with one self-contained, license-clean sample project per language `{go, python, typescript, java}` that exercises the metrics lit-up today (`loc`, `duplication_ratio`, `cycle_member`, `interface_width`, `depth_of_inheritance`).
- [ ] Author one fixture per language with a deliberate dependency cycle to drive `cycle_member` and `break_cycle` task generation.
- [ ] Author one fixture with high-LOC files (>2000 lines) to drive `loc` SRP findings and `split_class` tasks.
- [ ] Snapshot the expected markdown / JSON outputs under `services/clean-code/internal/cli/testdata/golden/<scenario>/{report.md,findings.json,diagnostics.json}`.
- [ ] Add a `TestAnalyzeGolden` table-driven test in `internal/cli/orchestrator/orchestrator_golden_test.go` that runs the orchestrator end-to-end (via the CLI's `runAnalyze` seam exposed for testing) and diff-compares against golden files; failures dump the diff.
- [ ] Add a `make update-cli-golden` target that re-runs the test with an `UPDATE=1` env var to regenerate snapshots when the orchestrator output legitimately changes (gated by code review).

### Dependencies

- phase-p0-reports-and-delivery/stage-analyze-end-to-end-wiring

### Test Scenarios

- [ ] Scenario: golden match Go corpus -- Given the Go fixture, When `runAnalyze` runs, Then `report.md` and `findings.json` byte-match the golden files.
- [ ] Scenario: cycle detected -- Given the Go cycle fixture, When `runAnalyze` runs, Then `findings.json.Findings` contains at least one row with `RuleID` matching `decoupling.cycle_member` and at least one `RefactorTask` with `Kind == "break_cycle"`.
- [ ] Scenario: cross-language coverage -- Given the four-language fixture set, When `runAnalyze` runs sequentially per language, Then each language's `report.md` byte-matches its golden file.
- [ ] Scenario: deterministic re-run -- Given any fixture, When `runAnalyze` runs twice back-to-back in the same process, Then both runs produce byte-identical `report.md` and `findings.json` outputs.

# Phase 4: P1 Structured Prompt Emitter

## Dependencies

- phase-p0-reports-and-delivery

## Stage 4.1: Prompt Record And Source Snippet Extractor

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/suggest/record.go` declaring the `RefactorPromptRecord` struct with the exact field set in `architecture.md` Sec 4.6 (`task_id`, `plan_id`, `repo_id`, `head_sha`, `policy_version_id`, `task_kind`, `rule_id`, `rule_version`, `severity`, `scope.*`, `source_snippet`, `source_snippet_truncated`, `metric_evidence[]`, `prose_suggestion`, `effort_hours`, `effort_source`, `prompt_format_version`).
- [ ] Add the constant `PromptFormatVersion = "v1.2026.05"`; embed it in every emitted record.
- [ ] Create `services/clean-code/internal/cli/suggest/snippet.go` with `ExtractSnippet(absFilePath string, startLine, endLine int, maxLines int) (snippet string, truncated bool, err error)` that reads raw bytes from disk (NOT the parser's normalised form, per `tech-spec.md` C12, R4 mitigation).
- [ ] Default `maxLines` to 200 (per `tech-spec.md` Sec 8.2); when the snippet exceeds the cap, truncate to the first `maxLines` lines and append `... [truncated <N> lines]` sentinel as the last retained line.
- [ ] Add a `MetricEvidence` struct (`metric_kind`, `value`, `threshold`, `op`) and an aggregator that resolves the originating samples for a `RefactorTask` by joining `Task.ScopeID + Task.RuleID` against the in-memory rule engine's `Verdicts()`.
- [ ] Stamp `effort_source` from the active `effort.EffortModel.Mode()` (Stage 1.3) on every record so the consumer knows whether the hours value is ML- or fallback-derived.

### Dependencies

- phase-foundations/stage-effort-estimator-fallback
- phase-foundations/stage-repo-context-and-scope-binding

### Test Scenarios

- [ ] Scenario: snippet capped -- Given a 500-line scope and `maxLines=200`, When `ExtractSnippet` runs, Then the returned string has exactly 200 lines, `truncated == true`, and the last line is `... [truncated 300 lines]`.
- [ ] Scenario: snippet not truncated for small scope -- Given a 50-line scope and `maxLines=200`, When `ExtractSnippet` runs, Then `truncated == false` and the snippet contains exactly 50 lines.
- [ ] Scenario: raw bytes -- Given a fixture file containing a literal tab character followed by a multi-byte UTF-8 sequence, When `ExtractSnippet` runs, Then the returned snippet preserves the exact byte sequence (no parser normalisation).
- [ ] Scenario: metric evidence join -- Given a fixture run producing one task firing on `metric_kind=loc` at value 2000 against threshold 1500, When the aggregator runs for that task, Then `metric_evidence` contains exactly one entry `{metric_kind:"loc", value:2000, threshold:1500, op:">="}`.

## Stage 4.2: JSONL Prompt Emitter

### Implementation Steps

- [ ] Create `services/clean-code/internal/cli/suggest/emitter.go` exposing `type PromptEmitter interface { Emit(ctx, art RunArtifact, w io.Writer) error }` and a default `JSONL` implementation per `architecture.md` Sec 5.7.
- [ ] For each `RefactorTask` in `art.Tasks`, assemble the `RefactorPromptRecord` (Stage 4.1), `json.Marshal` it onto a single line, and append `"\n"` to `w`.
- [ ] Resolve `scope.signature` / `scope.kind` / `scope.file_path` / `scope.start_line` / `scope.end_line` by looking up `Task.ScopeID` in `scopebinding.Table`; fail-closed with a non-nil error if the binding is absent (consistency check, never expected in a healthy run).
- [ ] Call `refactor.ValidateTaskKind(task.Kind)` before emitting; if it returns `ErrRejectedTaskKindAlias`, exit code 70 with a clear message naming the offending task id (`tech-spec.md` C5).
- [ ] Sort tasks before emission by `(severity DESC, score DESC, scope_id ASC)` so two runs produce byte-identical JSONL output (`tech-spec.md` C11).
- [ ] Add a `--emit-prompts-pretty` reserved flag (defaults off) that pretty-prints each record across multiple lines for human inspection; mutually exclusive with the JSONL contract.

### Dependencies

- phase-p1-structured-prompt-emitter/stage-prompt-record-and-source-snippet-extractor
- phase-pipeline/stage-planner-and-task-planner-wiring

### Test Scenarios

- [ ] Scenario: one line per task -- Given a `RunArtifact` with 10 tasks, When `JSONL.Emit` runs, Then the output has exactly 10 lines, each parseable as a standalone JSON object.
- [ ] Scenario: prompt format version stamped -- Given any emitted record, When the JSON is parsed, Then `prompt_format_version == "v1.2026.05"`.
- [ ] Scenario: rejected task kind -- Given a fixture run injecting a `RefactorTask{Kind: "refactor_everything"}` directly into the artifact (test-only seam), When `JSONL.Emit` runs, Then exit code is 70 and stderr names the offending task id.
- [ ] Scenario: byte-stable emission -- Given the same `RunArtifact` emitted twice, When the outputs are compared, Then they are byte-identical.

## Stage 4.3: Emit Prompts Flag Wiring

### Implementation Steps

- [ ] In `cmd/cleanc/main.go` `runAnalyze`, when `--emit-prompts <path>` is non-empty, construct `suggest.NewJSONL()` and call `Emit(ctx, art, w)` after the report + findings writers complete.
- [ ] When `--emit-prompts` is set with no value or to `-`, write to stdout (mutually exclusive with stdout-default `--out`; the analyze command refuses both at flag-parse time with exit 64).
- [ ] Append a `Diagnostics.PromptCount int` field to `RunArtifact` and stamp it from the emitter's row count; the markdown / JSON renderers surface this in the diagnostics block.
- [ ] When `--emit-prompts` is set, the markdown report's diagnostics block adds a line `"Prompts emitted: N to <path>"`.
- [ ] Add a `--emit-prompts` exit-code contract: an emitter error is exit code 70 (internal). A successful emission with zero tasks is exit code 0 (not 1) and the file is created empty.

### Dependencies

- phase-p1-structured-prompt-emitter/stage-jsonl-prompt-emitter
- phase-p0-reports-and-delivery/stage-analyze-end-to-end-wiring

### Test Scenarios

- [ ] Scenario: file written -- Given `--emit-prompts prompts.jsonl` on a fixture with 5 tasks, When `cleanc analyze` runs, Then `prompts.jsonl` exists with exactly 5 lines and exit code is 0.
- [ ] Scenario: stdout sink -- Given `--emit-prompts -` and no `--out`, When `cleanc analyze` runs, Then stdout receives the JSONL stream (one line per task) and exit code matches the severity threshold.
- [ ] Scenario: zero tasks -- Given a fixture producing zero tasks, When `--emit-prompts prompts.jsonl` is set, Then `prompts.jsonl` exists, is zero bytes, and exit code is 0.
- [ ] Scenario: ambiguous stdout refused -- Given both `--emit-prompts -` AND no `--out` flag (so markdown defaults to stdout), When `cleanc analyze` runs, Then exit code is 64 before any pipeline stage runs.
- [ ] Scenario: diagnostics count -- Given `--emit-prompts prompts.jsonl` with 7 tasks, When the markdown report is generated, Then the diagnostics block contains `"Prompts emitted: 7 to prompts.jsonl"`.

## Stage 4.4: Reserved Verbs And Flags

### Implementation Steps

- [ ] Implement `cleanc apply <task_id>` as a stub: print `"not implemented; pending operator pin cli-l7-authority (see docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3)"` and exit 64 (`architecture.md` Sec 3.6 last bullet).
- [ ] Implement `--telemetry-otlp <url>` flag rejection: when set, print `"--telemetry-otlp is reserved for a future story; rejected in P0/P1"` and exit 64 BEFORE the pipeline starts (`tech-spec.md` Sec 8.1 last row).
- [ ] Implement `--with-churn` flag handling: accept the flag (so its surface is locked) but emit a stderr warning `"--with-churn requires the P2 parser-attr extension; modification_count_in_window will not light up"` and continue without invoking any git-history walk (`architecture.md` Sec 3.6 flag table).
- [ ] Implement `--snippet-cap-lines <int>` as a reserved flag that exits 64 in P0/P1 with `"reserved for a future minor release"`; the surface is locked but not honoured (`tech-spec.md` Sec 8.2).
- [ ] Add a `TestReservedSurface` table-driven test that exercises every reserved verb/flag and asserts the exact exit code + stderr message string; the test acts as a regression guard against accidentally activating a reserved surface.

### Dependencies

- phase-p1-structured-prompt-emitter/stage-emit-prompts-flag-wiring

### Test Scenarios

- [ ] Scenario: apply not implemented -- Given a built binary, When `cleanc apply 00000000-0000-0000-0000-000000000000` runs, Then exit code is 64 and stderr contains `pending operator pin cli-l7-authority`.
- [ ] Scenario: telemetry flag rejected -- Given a fixture repo, When `cleanc analyze . --telemetry-otlp http://localhost:4317` runs, Then exit code is 64 and no `--out` / `--findings` file is created.
- [ ] Scenario: churn warning -- Given a fixture repo, When `cleanc analyze . --with-churn` runs, Then exit code is 0 or 1 (severity dependent) AND stderr contains the with-churn warning string.
- [ ] Scenario: snippet cap reserved -- Given a fixture repo, When `cleanc analyze . --snippet-cap-lines 100` runs, Then exit code is 64 before any pipeline stage starts.

# Phase 5: Hardening And Release

## Dependencies

- phase-p1-structured-prompt-emitter

## Stage 5.1: Build Tag Matrix

### Implementation Steps

- [ ] Add `make build-prod` target invoking `go build -tags prod -o bin/cleanc-prod ./cmd/cleanc`; the existing `make build` continues to produce the no-tag dev binary.
- [ ] Verify `internal/cli/devpolicy/unsigned_dev.go` and `internal/cli/devpolicy/unsigned_prod.go` mutually exclude via `//go:build !prod` / `//go:build prod` constraints so the bypass cannot be smuggled into a prod build.
- [ ] Extend `.github/workflows/clean-code-ci.yml` to add a `build-prod` job that runs `make build-prod`, then runs `cleanc-prod internal-self-check` and asserts exit code 70 (the bypass MUST refuse to load).
- [ ] Add a `make test-prod` target that runs `go test -tags prod ./...` so the prod-only code paths get unit-test coverage; CI invokes this as a separate matrix entry (`tech-spec.md` Sec 8.9).
- [ ] Document the build-tag matrix in `services/clean-code/README.md` (one paragraph) so a future maintainer reading the README does not need to chase the tag constraints across files.

### Dependencies

- phase-foundations/stage-dev-policy-loader

### Test Scenarios

- [ ] Scenario: prod build compiles -- Given the source tree, When CI runs `make build-prod`, Then it exits 0 and produces `bin/cleanc-prod`.
- [ ] Scenario: prod self-check refuses bypass -- Given the `cleanc-prod` binary, When `cleanc-prod internal-self-check` runs, Then exit code is 70 and stderr contains `dev-mode policy bypass not available in prod build`.
- [ ] Scenario: prod tests pass -- Given the source tree, When CI runs `make test-prod`, Then it exits 0.

## Stage 5.2: Custom Lint Rules

### Implementation Steps

- [ ] Add a custom `forbidigo` config rule under `.golangci.yml` named `no-production-sql-import` that fires on any `import` whose path matches `*.sql_store` or `database/sql` from within `cmd/cleanc/...` / `internal/cli/...` (`tech-spec.md` Sec 8.10).
- [ ] Add a custom `go vet` analyser at `services/clean-code/tools/buildtaglint/main.go` that parses build tags on files under `internal/cli/devpolicy/` and asserts any file constructing a `steward.PolicyVersion` with nil `Signature` carries `//go:build !prod` (the `no-production-build-tag-bypass` rule, `tech-spec.md` Sec 8.10).
- [ ] Wire both linters into `make lint` via a new `make lint-cli` target invoked by the existing `lint` target so existing developer workflows pick them up automatically.
- [ ] Add a `tools/buildtaglint/README.md` documenting the rule and how to extend it; reference `tech-spec.md` Sec 8.10 as the source of truth.
- [ ] Add a `lint-cli` step to `.github/workflows/clean-code-ci.yml` so PRs that smuggle an unconstrained bypass file fail before review.

### Dependencies

- phase-hardening-and-release/stage-build-tag-matrix

### Test Scenarios

- [ ] Scenario: SQL import refused -- Given a test fixture file under `internal/cli/foo.go` importing `database/sql`, When `make lint-cli` runs, Then it exits non-zero and stderr names the file and the `no-production-sql-import` rule.
- [ ] Scenario: missing build tag refused -- Given a test fixture file under `internal/cli/devpolicy/bypass.go` that constructs `steward.PolicyVersion{Signature: nil}` without `//go:build !prod`, When `make lint-cli` runs, Then it exits non-zero and stderr names the file and the `no-production-build-tag-bypass` rule.
- [ ] Scenario: clean tree passes -- Given the actual CLI source tree (no fixtures), When `make lint-cli` runs, Then it exits 0.

## Stage 5.3: End To End Golden Tests

### Implementation Steps

- [ ] Create `tests/e2e/cleanc/` directory mirroring the existing Phase-09 audit-WAL layout so the CI scaffolding (compose-less, since the CLI needs no DB) plugs in naturally.
- [ ] Add `tests/e2e/cleanc/scenarios/p0-go-cycle/` scenario: a tarball-able sample repo + a `run.sh` invoking `cleanc analyze . --out report.md --findings findings.json --diagnostics diag.json` + a `golden/` directory; the scenario asserts the produced files byte-match golden.
- [ ] Add `tests/e2e/cleanc/scenarios/p0-mixed-langs/` covering one file each of Go / Python / TypeScript / Java; assertion checks the four languages all show up in `RunArtifact.Files`.
- [ ] Add `tests/e2e/cleanc/scenarios/p1-prompts/` exercising `--emit-prompts prompts.jsonl` on a fixture with a known task count; assertion checks `prompts.jsonl` line count + each line parses + `prompt_format_version == "v1.2026.05"`.
- [ ] Add `tests/e2e/cleanc/scenarios/exit-codes/` covering the `0` / `1` / `2` / `64` matrix from `tech-spec.md` Sec 8.6 via deliberate failure injections (non-existent root, invalid flag, severity-crossing finding).
- [ ] Add a `make e2e-cleanc` target in `services/clean-code/Makefile` that iterates `tests/e2e/cleanc/scenarios/*/run.sh` and aggregates pass/fail; CI invokes this in a dedicated job.

### Dependencies

- phase-p1-structured-prompt-emitter/stage-emit-prompts-flag-wiring
- phase-p0-reports-and-delivery/stage-p0-fixture-corpus-and-golden-snapshots

### Test Scenarios

- [ ] Scenario: Go cycle e2e -- Given the `p0-go-cycle` scenario, When `run.sh` executes, Then exit code matches the scenario's `expected_exit_code` and `report.md` byte-matches golden.
- [ ] Scenario: mixed langs e2e -- Given the `p0-mixed-langs` scenario, When `run.sh` executes, Then `findings.json` lists exactly four `Files` entries with distinct `language` values.
- [ ] Scenario: prompt emission e2e -- Given the `p1-prompts` scenario, When `run.sh` executes, Then `prompts.jsonl` line count equals the scenario's `expected_task_count` and every line is valid JSON with `prompt_format_version == "v1.2026.05"`.
- [ ] Scenario: exit codes matrix -- Given the `exit-codes` scenario, When each sub-case runs, Then the observed exit code matches the expected code listed in `tech-spec.md` Sec 8.6 row-by-row.

## Stage 5.4: Documentation And Release Notes

### Implementation Steps

- [ ] Update `services/clean-code/README.md` with a `## cleanc CLI` section: install (`make build`), basic usage (`bin/cleanc analyze <path>`), the flag table from `tech-spec.md` Sec 8.1, and the exit-code matrix from Sec 8.6.
- [ ] Add `docs/cleanc/USAGE.md` for end-user-facing documentation: walkthrough of P0 (analyze + report), P1 (`--emit-prompts` workflow with AI coder), the dev-mode banner, and the build-tag matrix.
- [ ] Add `docs/cleanc/PROMPT-FORMAT.md` documenting the `RefactorPromptRecord` JSON shape (Stage 4.1) with field-by-field semantics and a worked example; this is the consumer contract for AI coders.
- [ ] Add an entry to the repo-root `CHANGELOG.md` (create if absent) summarising the `cleanc` shipment under `## [Unreleased]` and listing the deferred P2 / P3 follow-up story ids.
- [ ] Cross-link `architecture.md` Sec 9 ("Phased roadmap mapping") from the README's `cleanc` section so a reader can navigate from the binary back to the design rationale.

### Dependencies

- phase-hardening-and-release/stage-end-to-end-golden-tests

### Test Scenarios

- [ ] Scenario: README has cleanc section -- Given the updated `services/clean-code/README.md`, When `grep -F "## cleanc CLI" services/clean-code/README.md` runs, Then it returns one match.
- [ ] Scenario: usage doc references flags -- Given `docs/cleanc/USAGE.md`, When `grep -F "--emit-prompts" docs/cleanc/USAGE.md` runs, Then it returns at least one match.
- [ ] Scenario: prompt format doc has version -- Given `docs/cleanc/PROMPT-FORMAT.md`, When `grep -F "v1.2026.05" docs/cleanc/PROMPT-FORMAT.md` runs, Then it returns at least one match.
- [ ] Scenario: changelog updated -- Given `CHANGELOG.md`, When `grep -F "cleanc" CHANGELOG.md` runs, Then it returns at least one match under an `## [Unreleased]` heading.
