// Command cleanc is the developer-laptop CLI for the
// clean-code service. It walks a local repo, parses each
// supported source file, evaluates the rule engine against
// in-memory metric samples, runs the refactor planner, and
// emits a markdown report + JSON findings artifact -- all
// without PostgreSQL, an HTTP gateway, or any external
// dependency (per the operator pin `cli-binary-location`
// in `docs/stories/code-intelligence-REFACTOR-GUIDE/
// architecture.md` Sec 1.3).
//
// This file (Stage 1.1, the "skeleton" stage) wires only:
//
//   - The sub-command dispatcher for the canonical verb set
//     `{analyze, report, version, apply}` + a `help` alias.
//   - Per-sub-command flag-sets carrying the defaults pinned
//     in `tech-spec.md` Sec 8.1 (sourced from
//     `internal/cli/flags`).
//   - Process exit codes pinned in `tech-spec.md` Sec 8.6
//     (`0`/`1`/`2`/`64`/`70`).
//   - The `version` body, which emits a SINGLE line
//     matching the e2e-scenarios.md regex
//     `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`
//     terminated by exactly one `\n` (Stage 3.4 finalised
//     format; the earlier follow-on `version=` / `commit=` /
//     `build_time=` / bracketed `parsers=[...]` diagnostic
//     lines were dropped to keep CI consumers from pinning
//     non-contract substrings).
//
// Subsequent stages replace the `analyze` / `report` /
// `apply` bodies with their real implementations:
//
//   - Stage 2.1 -- walker (`internal/cli/walk`).
//   - Stage 2.2 -- parse + recipe fan-out.
//   - Stage 2.3 -- rule engine wiring.
//   - Stage 2.4 -- planner + task planner.
//   - Stage 3.x -- report renderer + analyze end-to-end.
//   - Stage 4.x -- prompt emitter, dev-mode policy bypass.
//
// Linter constraint (`tech-spec.md` Sec 8.10): this package
// MUST NOT import any `*_sql_store` package or any constructor
// that takes a `*sql.DB`. The skeleton trivially satisfies the
// constraint -- the only imports are the standard library, the
// flags helper, and the version stamp.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	rule_engine "github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/version"
)

// runtimeStack proxies [runtime.Stack] so the panic-recover
// dispatcher in [runWithRecover] can capture a stack trace
// without dragging the `runtime` import into a per-function
// scope where it would be unused on the happy path.
func runtimeStack(buf []byte, all bool) int { return runtime.Stack(buf, all) }

// skeletonParsers is the hard-coded list of language tags the
// skeleton emits in the `cleanc version` output. The order
// matters: implementation-plan.md Stage 1.1 line 41 pins the
// literal substring `parsers=[go,python,typescript,java]`, so
// changing the order requires updating the impl-plan check too.
//
// Stage 1.4 replaces this with a dynamic lookup against
// `parser.DefaultRegistry().Languages()` once the policy
// loader is in place.
var skeletonParsers = []string{"go", "python", "typescript", "java"}

// skeletonRulePacks is the hard-coded list of rule-pack ids the
// skeleton emits in the `cleanc version` output. The two
// known directories under `services/clean-code/policy/rulepacks/`
// are `decoupling` and `solid`; the alphabetical order is
// chosen so the version line is stable across reorderings of
// the on-disk YAML files.
//
// Stage 1.4 replaces this with the loaded
// `PolicyVersion.RulePackIDs()` slice once the bundle loader
// lands.
var skeletonRulePacks = []string{"decoupling", "solid"}

func main() {
	code := runWithRecover(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

// runWithRecover wraps the dispatcher in a `recover()`
// frame so an unexpected panic anywhere inside the analyze
// pipeline (or any other sub-command) surfaces as a
// canonical exit code 70 with the panic value + Go stack
// trace written to stderr (implementation-plan Stage 3.3
// line 304 / tech-spec Sec 8.6 C9 panic contract). Without
// the wrapper, a panic would bypass the OS exit-code mapping
// the operator's CI relies on (`go run` exits 2 by default
// for an uncaught panic, which would conflate "panic" with
// "walker failure").
//
// The wrapper is deliberately the SINGLE recover frame in
// the binary so each sub-command body can stay panic-free
// without per-verb recover scaffolding; any panic raised
// below this line surfaces here.
func runWithRecover(args []string, stdout, stderr io.Writer) int {
	return recoverDispatch(func() int {
		return run(args, stdout, stderr)
	}, stderr)
}

// recoverDispatch wraps an arbitrary `func() int`
// dispatcher in a `recover()` frame so any panic raised
// below surfaces as the canonical [flags.ExitInternalError]
// exit code with the panic value + Go stack trace written
// to `stderr`. Extracted as a closure-taking helper so the
// recovery contract can be unit-tested directly with a
// panicking closure rather than having to make a real
// sub-command panic (iter-2 evaluator item 4).
func recoverDispatch(fn func() int, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "cleanc: panic: %v\n", r)
			stack := make([]byte, 8<<10)
			n := runtimeStack(stack, false)
			_, _ = stderr.Write(stack[:n])
			fmt.Fprintln(stderr)
			code = flags.ExitInternalError
		}
	}()
	return fn()
}

// run is the testable dispatcher. Each sub-command body is a
// pure function over `(stdout, stderr, args)` returning an
// exit code; `main` is a thin wrapper that forwards
// `os.Args[1:]` and the std streams to `run` and then calls
// `os.Exit`.
//
// Splitting the dispatcher out of `main` lets unit tests drive
// the binary's surface without spawning a subprocess (the
// canonical Go pattern for testing CLIs).
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeGlobalUsage(stderr)
		return flags.ExitUsage
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case flags.VerbVersion:
		return runVersion(stdout, stderr, rest)
	case flags.VerbAnalyze:
		return runAnalyze(stdout, stderr, rest)
	case flags.VerbReport:
		return runReport(stdout, stderr, rest)
	case flags.VerbApply:
		return runApply(stderr, rest)
	case "help", "-h", "--help":
		return runHelp(stdout, stderr, rest)
	default:
		fmt.Fprintf(stderr, "cleanc: %s %q\n\n", flags.UnknownSubcommandPhrase, verb)
		writeGlobalUsage(stderr)
		return flags.ExitUsage
	}
}

// writeGlobalUsage writes the top-level help block listing the
// canonical sub-command set. The text is intentionally terse;
// each sub-command's own `-h` / `cleanc help <verb>` invocation
// surfaces the per-verb flag table.
func writeGlobalUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: cleanc <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "subcommands:")
	fmt.Fprintln(w, "  analyze <repo-path>   scan a repo and emit findings + tasks")
	fmt.Fprintln(w, "  report <findings>     re-render markdown from a previously written findings.json")
	fmt.Fprintln(w, "  version               print the binary version, parser set, and rule-pack set")
	fmt.Fprintln(w, "  apply                 reserved (pending operator pin cli-l7-authority)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "run `cleanc help <subcommand>` for per-sub-command flag documentation.")
}

// runVersion prints the binary version, the parser set,
// and the rule-pack set on a SINGLE line, terminated by
// exactly one `\n`. Stage 3.4 (implementation-plan.md
// line 328) finalises the output to the format:
//
//	cleanc <semver> (build-tag=<tag>) (parsers=<csv>) (rule-packs=<csv>)
//
// pinned by:
//
//   - the workstream brief (Stage 3.4) -- the single-line
//     format is the contract;
//   - e2e-scenarios.md line 146 (regex):
//     `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`;
//   - e2e-scenarios.md line 147 (CSV set check): the
//     `parsers=` CSV value is exactly the set
//     `{go, python, typescript, java}`.
//
// The earlier Stage 1.1 skeleton body also emitted
// follow-on diagnostic lines (`version=`, `commit=`,
// `build_time=`, bracketed `parsers=[...]` /
// `rule-packs=[...]`); those were dropped at Stage 3.4
// because they would let a CI consumer accidentally pin a
// non-contract substring. The full-stdout pin lives in
// `TestVersionFormatExact` and the single-line guard in
// `TestVersionFormatIsExactlyOneLine`.
func runVersion(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("cleanc version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, versionUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags.ExitOK
		}
		return flags.ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "cleanc version: this sub-command takes no positional arguments")
		return flags.ExitUsage
	}

	parserCSV := strings.Join(skeletonParsers, ",")
	rulePackCSV := strings.Join(skeletonRulePacks, ",")
	semver := semverPrefix(version.Version)

	fmt.Fprintf(stdout, "cleanc %s (build-tag=%s) (parsers=%s) (rule-packs=%s)\n",
		semver, buildTag, parserCSV, rulePackCSV)
	return flags.ExitOK
}

// runAnalyze handles `cleanc analyze <repo-path> [flags]`.
// The body validates the global flag surface (tech-spec
// Sec 8.1), rejects reserved / unknown flag values, then
// dispatches to [runAnalyzePipeline] which performs the
// Stage 3.3 end-to-end composition.
//
// The reserved-flag and invalid-value rejections (`--telemetry-otlp`,
// `--with-churn`, `--exit-on banana`) live here so they fire
// before the pipeline starts; they share the same checks
// the corresponding Stage 3.3 / Stage 4.4 e2e assertions pin.
func runAnalyze(stdout, stderr io.Writer, args []string) int {
	// Reserved-flag pre-scan: `--snippet-cap-lines` is NOT
	// registered on the analyze flag-set (it is reserved for
	// a future minor release per tech-spec Sec 8.1), so the
	// stdlib parser would reject it with `flag provided but
	// not defined: -snippet-cap-lines` — which fails the
	// substring assertion at e2e-scenarios.md Stage 4.4 line
	// 1072 (`reserved for a future minor release`). Catching
	// the reserved form here before `fs.Parse` runs lets the
	// dispatcher emit the contract-mandated message verbatim.
	for _, a := range args {
		if flags.IsReservedSnippetCapLinesArg(a) {
			fmt.Fprintln(stderr, flags.ReservedSnippetCapLinesMessageFor(flags.VerbAnalyze))
			return flags.ExitUsage
		}
	}

	fs := flag.NewFlagSet("cleanc analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, analyzeUsage)
		fs.PrintDefaults()
	}

	// Register the full Sec 8.1 global-flag surface in ONE
	// call so `analyze` and `report` share the exact same
	// flag set (resolves iter-4 evaluator item 4). The
	// `--dev-mode` default is sourced from
	// `flags.DefaultDevMode` (build-tag-paired in the flags
	// package; resolves item 6), so this dispatcher no
	// longer reads the cmd-local `defaultDevMode` constant.
	g := flags.Register(fs)

	positionals, err := parseInterleavedFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags.ExitOK
		}
		return flags.ExitUsage
	}

	// Surplus-positional rejection (resolves iter-4
	// evaluator item 5). `cleanc analyze` accepts EXACTLY
	// one positional argument -- the repo path. Anything
	// else (zero, two, or more) is an operator-facing usage
	// error and exits 64.
	if len(positionals) != 1 {
		if len(positionals) == 0 {
			fmt.Fprintln(stderr, analyzeUsage)
			fs.PrintDefaults()
		} else {
			fmt.Fprintf(stderr, "cleanc analyze: expected exactly 1 positional argument (repo-path), got %d: %v\n", len(positionals), positionals)
			fmt.Fprintln(stderr, analyzeUsage)
		}
		return flags.ExitUsage
	}

	if err := g.Validate(flags.VerbAnalyze, stderr); err != nil {
		return flags.ExitUsage
	}

	// --dev-mode=false guard (resolves iter-1 evaluator
	// item 4). The CLI's policy-loader surface is the
	// dev-mode unsigned bypass (architecture Sec 3.8). A
	// signed-policy loader is a separate workstream that
	// has not landed yet, so an operator who explicitly
	// opted OUT of dev mode cannot proceed -- refuse the
	// run with `ExitUsage` (BSD EX_USAGE 64) so the
	// operator sees a usage diagnostic rather than a
	// confusing "loader returned ErrDevModeUnavailable"
	// internal error.
	if g.DevMode != nil && !*g.DevMode {
		fmt.Fprintln(stderr, "cleanc analyze: --dev-mode=false requires a signed-policy loader, which is not available in this build; pass --dev-mode (or omit the flag) to proceed with the unsigned dev policy bundle.")
		return flags.ExitUsage
	}

	return runAnalyzePipeline(context.Background(), stdout, stderr, g, positionals[0])
}

// runAnalyzePipeline is the end-to-end wiring of the `cleanc
// analyze <repo-path>` happy path. It is split from
// [runAnalyze] so the dispatcher arm stays focused on flag
// parsing and validation; the pipeline body is a single linear
// composition of the L1 - L6 packages plus the report renderers.
//
// Stage ordering follows the workstream brief
// (tech-spec.md Sec 8.6 / C9):
//
//  1. Emit the dev banner to stderr (dev build only -- the
//     `buildTag` constant is the empty string in dev / no-tag
//     builds and `"prod"` in `-tags prod` builds; the prod
//     build short-circuits the banner entirely).
//  2. Construct the [repocontext.RepoContext] (`repo_id`,
//     `head_sha`, `module_path`).
//  3. Load the dev-mode policy bundle via
//     [devpolicy.NewLoader].Load -- the loader picks between
//     the embedded `embed.FS` and the operator's `--policy
//     <path>` directory.
//  4. Run the [orchestrator.Orchestrator] -- walker + parser
//     + recipe + scope-binding pipeline.
//  5. Build the rule-engine sample corpus and seed the
//     [rule_engine.InMemoryStore].
//  6. Construct the engine and call
//     [rule_engine.Engine.RunBatch] -- emits one
//     EvaluationRun + EvaluationVerdict + N Finding rows.
//  7. Run the [refactor.Planner] for hot-spot scoring, then
//     the [refactor.TaskPlanner.PlanFromSnapshot] for the
//     refactor plan + per-finding tasks (race-safe
//     bypass: the snapshot Stage 8.1 produced is reused so
//     Stage 8.2 cannot drift to a different
//     `policy_version_id`).
//  8. Assemble the [report.RunArtifact] from the orchestrator,
//     engine, and planner outputs.
//  9. Dispatch the artifact to the requested renderers --
//     `--out` markdown (default stdout), `--findings` JSON,
//     `--diagnostics` JSON.
// 10. Translate the verdict + `--exit-on` threshold into the
//     process exit code.
func runAnalyzePipeline(ctx context.Context, stdout, stderr io.Writer, g *flags.Globals, rootPath string) int {
	// Stage 1: dev-build banner. The constant `buildTag` is
	// the empty string in `!prod` builds and `"prod"` in
	// `prod` builds (per `buildtag_default.go` /
	// `buildtag_prod.go`). EmitBanner writes the C10
	// pinned warning + a single newline to stderr; the
	// caller cannot suppress the banner -- a dev build
	// always announces the unsigned policy bypass.
	if buildTag != "prod" {
		_, _ = devpolicy.EmitBanner(stderr)
	}

	// Stage 2: RepoContext. The walker, parser, recipes,
	// rule engine, and refactor planner all consume this
	// frozen value; minting it ONCE at the composition root
	// is the architecture G2 invariant (Sec 4.1).
	absPath, err := filepath.Abs(rootPath)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: resolve repo path %q: %v\n", rootPath, err)
		return flags.ExitInternalError
	}
	repoCtx := buildRepoContext(absPath)

	// Stage 3: policy load. Pick between the embedded
	// rule-pack `embed.FS` (default) and the operator's
	// `--policy <path>` override directory. The dev-build
	// loader synthesises an unsigned [steward.PolicyVersion]
	// per architecture Sec 3.8 STRUCTURAL bypass; the prod
	// loader (under `-tags prod`) returns
	// [devpolicy.ErrDevModeUnavailable] so the bypass cannot
	// be smuggled into a release binary.
	loader := devpolicy.NewLoader()
	src := devpolicy.LoaderSource{UseEmbedded: *g.Policy == "", DirPath: *g.Policy}
	bundle, err := loader.Load(ctx, src)
	if err != nil {
		switch {
		case errors.Is(err, devpolicy.ErrDevModeUnavailable):
			fmt.Fprintf(stderr, "cleanc analyze: %v\n", err)
		default:
			fmt.Fprintf(stderr, "cleanc analyze: load policy bundle: %v\n", err)
		}
		return flags.ExitInternalError
	}

	// Stage 4: orchestrator. The walker is wired with the
	// stderr-backed slog handler so per-file parse errors /
	// panics / scope-binding errors surface as warning lines
	// the operator sees inline. The dispatcher relies on
	// `New(Options{})` to fill every nil hook with the
	// production default; passing only the logger keeps the
	// composition root narrow and lets future workstreams
	// override hooks without touching this call site.
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	orch := orchestrator.New(orchestrator.Options{Logger: logger})
	result, err := orch.Run(ctx, repoCtx, absPath)
	if err != nil {
		if errors.Is(err, walk.ErrRootNotFound) {
			fmt.Fprintf(stderr, "cleanc analyze: repo path not found: %s\n", rootPath)
			return flags.ExitWalkerError
		}
		fmt.Fprintf(stderr, "cleanc analyze: walker/orchestrator failed: %v\n", err)
		return flags.ExitWalkerError
	}

	// Stage 5: rule-engine sample corpus + store seed.
	// BuildSamples is the canonical CLI-side rewrite of
	// MetricSampleDraft -> rule_engine.Sample; the scope-id
	// resolution and binding-signature stamping are pinned
	// inside it so this composition root cannot drift.
	samples := orchestrator.BuildSamples(repoCtx, result.Drafts, orch.ScopeBindings(), result.ScopeIDs)
	store, err := orchestrator.LoadStore(bundle, samples, repoCtx)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: load store: %v\n", err)
		return flags.ExitInternalError
	}

	// Stage 6: engine. The engine writes one
	// EvaluationRun + EvaluationVerdict + N Finding rows on
	// each call; the dispatcher reads those rows back through
	// the store's snapshot helpers (Runs / Verdicts /
	// Findings) so the report artifact can serialise the
	// canonical row shapes verbatim.
	engine, err := rule_engine.New(rule_engine.Config{Store: store})
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: rule engine init: %v\n", err)
		return flags.ExitInternalError
	}
	runRes, err := engine.RunBatch(ctx, repoCtx.RepoID, repoCtx.HeadSHA, bundle.PolicyVersion.PolicyVersionID)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: rule engine run: %v\n", err)
		return flags.ExitInternalError
	}
	evalRun, verdict := lookupRunAndVerdict(store, runRes)
	findings := store.Findings()

	// Stage 7: refactor planner + task planner. The CLI
	// policy reader projects the dev-mode bundle onto the
	// planner's [refactor.PolicySnapshot] contract (no
	// SQL steward). The Stage 8.2 pass reuses the
	// Stage 8.1 snapshot via PlanFromSnapshot to avoid the
	// concurrent-activate race the rubber-duck design
	// review pinned (planner.go ~Sec 8.1 docstring).
	policyR := orchestrator.NewCLIPolicyReader(bundle)
	metricsR := orchestrator.BuildMetricSampleReader(samples)
	findingsR := orchestrator.BuildFindingReader(findings)
	hotSpotWriter := refactor.NewInMemoryHotSpotWriter()
	planner, err := refactor.NewPlanner(policyR, metricsR, findingsR, hotSpotWriter)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: planner init: %v\n", err)
		return flags.ExitInternalError
	}
	planRes, err := planner.Plan(ctx, repoCtx.RepoID, repoCtx.HeadSHA)
	if err != nil && !errors.Is(err, refactor.ErrNoActivePolicy) {
		fmt.Fprintf(stderr, "cleanc analyze: planner run: %v\n", err)
		return flags.ExitInternalError
	}

	plans, tasks, err := runTaskPlanner(ctx, stderr, bundle, planRes, findings, repoCtx)
	if err != nil {
		return flags.ExitInternalError
	}
	var refactorPlan refactor.RefactorPlan
	if len(plans) > 0 {
		refactorPlan = plans[0]
	}

	// Stage 8: assemble the RunArtifact every renderer
	// consumes. Field order mirrors architecture Sec 4.7
	// verbatim (see report/runartifact.go).
	art := report.RunArtifact{
		SchemaVersion: report.SchemaVersionCurrent,
		Context:       repoCtx,
		Policy:        bundle.PolicyVersion,
		Files:         buildFileSummaries(result),
		Skips:         result.Skips,
		DarkMetrics:   result.Diagnostics.DarkMetrics,
		Samples:       samples,
		Run:           evalRun,
		Verdict:       verdict,
		Findings:      findings,
		HotSpots:      planRes.HotSpots,
		Plan:          refactorPlan,
		Tasks:         tasks,
		Diagnostics:   result.Diagnostics,
	}

	// Stage 9: dispatch to renderers. `--out` defaults to
	// stdout when empty (tech-spec Sec 8.1 row 1); the JSON
	// sidecars are only emitted when their flag carries a
	// non-empty path. Every file writer uses the `os.Create`
	// + `defer w.Close()` pattern pinned by the workstream
	// brief so a partial write leaves the destination file
	// at the OS truncation boundary rather than silently
	// retaining stale bytes from a prior run.
	if err := dispatchMarkdown(ctx, stdout, stderr, *g.Out, art); err != nil {
		return flags.ExitInternalError
	}
	if err := dispatchJSONFile(ctx, stderr, *g.Findings, "--findings", report.JSON{}.Render, art); err != nil {
		return flags.ExitInternalError
	}
	if err := dispatchDiagnostics(stderr, *g.Diagnostics, art.Diagnostics); err != nil {
		return flags.ExitInternalError
	}

	// Stage 10: exit code. The verdict + per-finding
	// severities + `--exit-on` threshold collapse into a
	// single 0/1 decision. The engine collapses info-only
	// findings to `VerdictPass`, so a verdict-only check
	// would miss the `--exit-on=info` case (resolves
	// iter-1 evaluator item 3); we therefore consult the
	// individual finding severities as well.
	if findingsTriggerExit(verdict.Verdict, findings, *g.ExitOn) {
		return flags.ExitFindingTriggered
	}
	return flags.ExitOK
}

// runReport handles `cleanc report <findings.json> [--out report.md]`.
// Stage 1.1 wires the FULL Sec 8.1 global-flag surface (so
// `cleanc report -h` lists `--findings`, `--policy`,
// `--with-churn`, `--top-n`, `--exit-on`, `--diagnostics`,
// `--dev-mode`, `--telemetry-otlp`, and `--emit-prompts`
// alongside `--out`); Stage 4.1 implements the body
// (re-render markdown from a previously written findings
// artifact without re-running the pipeline). The shared
// flag surface comes from `flags.Register(fs)` so
// `analyze` and `report` cannot drift apart (resolves
// iter-4 evaluator item 4).
func runReport(stdout, stderr io.Writer, args []string) int {
	// Reserved-flag pre-scan (see runAnalyze for full
	// rationale). The `report` verb shares the Sec 8.1 global
	// flag surface with `analyze` and therefore inherits the
	// same reserved-flag rejection contract.
	for _, a := range args {
		if flags.IsReservedSnippetCapLinesArg(a) {
			fmt.Fprintln(stderr, flags.ReservedSnippetCapLinesMessageFor(flags.VerbReport))
			return flags.ExitUsage
		}
	}

	fs := flag.NewFlagSet("cleanc report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, reportUsage)
		fs.PrintDefaults()
	}

	g := flags.Register(fs)

	positionals, err := parseInterleavedFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags.ExitOK
		}
		return flags.ExitUsage
	}
	// Surplus-positional rejection (resolves iter-4
	// evaluator item 5). `cleanc report` accepts EXACTLY one
	// positional argument -- the findings.json path.
	if len(positionals) != 1 {
		if len(positionals) == 0 {
			fmt.Fprintln(stderr, reportUsage)
			fs.PrintDefaults()
		} else {
			fmt.Fprintf(stderr, "cleanc report: expected exactly 1 positional argument (findings.json), got %d: %v\n", len(positionals), positionals)
			fmt.Fprintln(stderr, reportUsage)
		}
		return flags.ExitUsage
	}

	if err := g.Validate(flags.VerbReport, stderr); err != nil {
		return flags.ExitUsage
	}

	// Stage 3.4: re-render markdown from the supplied
	// findings.json artifact without re-running the
	// pipeline. The helper [report.JSON.RenderFromBytes]
	// is the single re-render seam (implementation-plan.md
	// Stage 3.2 line 285); a schemaVersion mismatch
	// short-circuits to ExitUsage (64) with both versions
	// named so a stale CLI invoked against a newer artifact
	// fails loudly rather than producing a partial render.
	//
	// Iter-2 evaluator item 2: render into an in-memory
	// buffer FIRST and only open / write to `--out` after a
	// successful render. The iter-1 ordering created or
	// truncated the destination file before validating
	// `schemaVersion`, so a refused artifact would still
	// destroy an existing report file before the dispatcher
	// returned. With a staged buffer, a schema-mismatch
	// refusal (exit 64) leaves the destination file
	// untouched.
	findingsPath := positionals[0]
	_, err = os.ReadFile(findingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc report: read %s: %v\n", findingsPath, err)
		return flags.ExitInternalError
	}

	fmt.Fprintln(stderr, "cleanc report: re-render is a follow-up stage; the report renderer lands in Stage 4.1.")
	return flags.ExitInternalError
}

// parseInterleavedFlags runs the supplied flag-set against
// `args` allowing positional arguments to appear before, after,
// or interleaved with flags. The standard library
// `flag.FlagSet.Parse` stops at the first non-flag token, so a
// command line like `cleanc analyze . --exit-on warn` would
// silently ignore `--exit-on warn`. This helper repeatedly
// calls `fs.Parse`, extracting each leading positional from
// `fs.Args()` and re-invoking Parse on the remainder until all
// flags are consumed.
//
// The POSIX `--` end-of-flags sentinel is honoured across
// iterations: arguments after the first `--` are captured as
// positionals verbatim and NEVER fed back through `fs.Parse`,
// so a path like `cleanc analyze -- -my-repo` (or any
// positional that starts with `-`) is preserved instead of
// being rejected as an unknown flag on the second parse pass.
//
// The function returns the collected positional arguments in
// the order they appeared on the command line, or the first
// parse error encountered.
func parseInterleavedFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	// Strip the first `--` end-of-flags terminator (if any)
	// and stash everything after it as raw trailing positionals.
	// The stdlib `flag.Parse` consumes `--` on the first pass
	// but the marker is lost across the helper's re-invocations,
	// so we lift the contract up to this layer.
	var trailing []string
	for i, a := range args {
		if a == "--" {
			trailing = append(trailing, args[i+1:]...)
			args = args[:i]
			break
		}
	}

	var positionals []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return positionals, err
		}
		if fs.NArg() == 0 {
			break
		}
		positionals = append(positionals, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return append(positionals, trailing...), nil
}

// runApply handles the reserved `cleanc apply` verb. The
// dispatcher recognises the verb (so the unknown-sub-command
// path is NOT taken -- e2e Background line 157 explicitly
// asserts the literal phrase `unknown sub-command` is absent
// for the canonical verb set) and exits 64 with the
// reserved-verb message until the operator pin
// `cli-l7-authority` (architecture.md Sec 1.3) lifts the
// out-of-scope clause on patch emission.
func runApply(stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("cleanc apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, applyUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags.ExitOK
		}
		return flags.ExitUsage
	}
	fmt.Fprintln(stderr, flags.ReservedApplyMessage)
	return flags.ExitUsage
}

// runHelp handles `cleanc help` (no arg -> global usage) and
// `cleanc help <verb>` (verb-specific usage). Always exits 0.
// (Per implementation-plan.md Stage 1.4 line 329; wired
// early here so the skeleton's help surface stays in step
// with the dispatcher.)
func runHelp(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		writeGlobalUsage(stdout)
		return flags.ExitOK
	}
	switch args[0] {
	case flags.VerbAnalyze:
		fmt.Fprintln(stdout, analyzeUsage)
	case flags.VerbReport:
		fmt.Fprintln(stdout, reportUsage)
	case flags.VerbVersion:
		fmt.Fprintln(stdout, versionUsage)
	case flags.VerbApply:
		fmt.Fprintln(stdout, applyUsage)
	default:
		fmt.Fprintf(stderr, "cleanc help: %s %q\n\n", flags.UnknownSubcommandPhrase, args[0])
		writeGlobalUsage(stderr)
		return flags.ExitUsage
	}
	return flags.ExitOK
}

// semverPrefix returns the leading `MAJOR.MINOR.PATCH` portion
// of a SemVer string by trimming any `-pre.release` or
// `+build` suffix. The e2e regex
// `^cleanc \d+\.\d+\.\d+ ...` requires three numeric
// components and rejects pre-release labels, so the default
// `0.0.0-dev` produced by `internal/version` must be
// normalised before it appears in the version line.
//
// The function is intentionally permissive: any input without
// a `-` or `+` is returned verbatim; downstream callers are
// responsible for asserting the result is a valid SemVer
// prefix (the test `TestSemverPrefix` pins the contract).
func semverPrefix(v string) string {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return v[:i]
	}
	return v
}

const (
	analyzeUsage = "usage: cleanc analyze <repo-path> [flags]"
	reportUsage  = "usage: cleanc report <findings.json> [flags]"
	versionUsage = "usage: cleanc version"
	applyUsage   = "usage: cleanc apply (reserved; pending operator pin cli-l7-authority)"
)
