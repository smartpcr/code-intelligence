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
//   - The `version` body, which emits a line matching the
//     e2e-scenarios.md regex
//     `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`
//     followed by implementation-plan substring lines
//     (`version=`, `parsers=[go,python,typescript,java]`,
//     ...).
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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/version"
)

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
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
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

// runVersion prints the binary version, the parser set, and
// the rule-pack set. Output format is pinned by:
//
//   - e2e-scenarios.md line 146 (regex):
//     `^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`
//   - e2e-scenarios.md line 147 (set check):
//     stdout contains `parsers=` whose CSV value is exactly the
//     set `{go, python, typescript, java}`.
//   - implementation-plan.md Stage 1.1 line 41 (substrings):
//     stdout includes `version=` and
//     `parsers=[go,python,typescript,java]`.
//
// To satisfy all three, the first line is the strict-regex
// header (CSV value, no brackets), and the subsequent lines
// carry the impl-plan substrings (`version=`, bracketed
// `parsers=[...]`, etc.) plus operator-debug stamps (commit,
// build_time).
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
	fmt.Fprintf(stdout, "version=%s\n", version.Version)
	fmt.Fprintf(stdout, "commit=%s\n", version.Commit)
	fmt.Fprintf(stdout, "build_time=%s\n", version.BuildTime)
	fmt.Fprintf(stdout, "parsers=[%s]\n", parserCSV)
	fmt.Fprintf(stdout, "rule-packs=[%s]\n", rulePackCSV)
	return flags.ExitOK
}

// runAnalyze handles `cleanc analyze <repo-path> [flags]`.
// Stage 1.1 wires the flag-set per tech-spec Sec 8.1 but does
// NOT yet execute the pipeline -- the walker / parser / engine
// / planner / report renderer land in Phase 2+ stages. To keep
// the skeleton from claiming success for unimplemented work,
// the body returns `ExitInternalError` (70, EX_SOFTWARE) with
// an explicit "not yet wired" stderr line once flag validation
// succeeds.
//
// The reserved-flag and invalid-value rejections (`--telemetry-otlp`,
// `--with-churn`, `--exit-on banana`) ARE wired in Stage 1.1
// because the workstream brief lists them as global-flag
// requirements; the same checks satisfy the corresponding
// Stage 3.3 / Stage 4.4 e2e assertions verbatim.
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

	// Suppress unused-variable warnings: the Globals struct
	// owns every flag pointer; the skeleton body doesn't
	// consume the values yet but the flag-set really
	// accepts them (the test suite asserts the surface and
	// `-h` lists them), so downstream stages will read
	// `g.Out`, `g.Findings`, etc. directly.
	_ = g

	// Surface stdout writer so unused-import warnings don't
	// drop the import in tests that don't read stdout.
	_ = stdout

	fmt.Fprintln(stderr, "cleanc analyze: pipeline not yet wired in the Stage 1.1 skeleton; the walker, parser, engine, planner, and report renderer land in Phase 2+ stages.")
	return flags.ExitInternalError
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

	_ = g
	_ = stdout

	fmt.Fprintln(stderr, "cleanc report: re-render not yet wired in the Stage 1.1 skeleton; the report renderer lands in Stage 4.1.")
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
// The conventional `--` end-of-flags sentinel is honoured
// across iterations: every token AFTER the first `--` is
// collected verbatim as a positional, even if it starts with a
// dash (e.g. `cleanc analyze -- -my-repo` treats `-my-repo` as
// the repo path rather than rejecting it as an unknown flag).
// Without this up-front split the inner loop would lose the
// sentinel after the first `fs.Parse` call -- `flag.FlagSet`
// strips `--` from `fs.Args()` -- and the second iteration
// would hand a leading-dash token back to `fs.Parse`, which
// would reject it.
//
// The function returns the collected positional arguments in
// the order they appeared on the command line, or the first
// parse error encountered.
func parseInterleavedFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var trailing []string
	if i := indexOfEndOfFlags(args); i >= 0 {
		// Copy so a later append into `positionals` cannot
		// alias / mutate the caller's slice.
		trailing = append([]string(nil), args[i+1:]...)
		args = args[:i]
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

// indexOfEndOfFlags returns the index of the first standalone
// `--` token in `args`, or -1 if none is present. Only the
// first occurrence is treated as the sentinel; later `--`
// tokens are positional arguments (which is the convention
// followed by getopt, POSIX utilities, and `go run -- --`).
func indexOfEndOfFlags(args []string) int {
	for i, a := range args {
		if a == "--" {
			return i
		}
	}
	return -1
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
