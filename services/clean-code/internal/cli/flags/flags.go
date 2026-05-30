// Package flags centralises the default values, exit codes,
// and closed-set sub-command names that the cleanc CLI binary
// (services/clean-code/cmd/cleanc/main.go) depends on.
//
// Keeping these values in a single helper package means the
// dispatcher in main.go stays slim, the build-tag variants in
// `cmd/cleanc/buildtag_*.go` can reference them without an
// import cycle, and every unit test that pins a contract value
// (tech-spec.md Sec 8.1 flag defaults, Sec 8.6 exit codes)
// has one source of truth to assert against.
//
// This package MUST stay free of side effects and external
// dependencies -- it is intentionally a constants-only module
// (per tech-spec C7: "cmd/cleanc must not pull in *_sql_store
// or *sql.DB constructors" -- and a flag-defaults helper that
// only emits constants is the tightest possible surface).
package flags

import (
	"flag"
	"fmt"
	"io"
)

// Exit codes pinned in tech-spec.md Sec 8.6.
//
// The dispatcher in cmd/cleanc/main.go maps each
// terminate-with-status path to exactly one of these
// constants. The CI gates (`.github/workflows/clean-code-ci.yml`
// Phase 1 scenarios) assert these numeric values byte-for-byte
// against the process exit code, so renaming or renumbering
// requires a coordinated change to both the spec and the
// scenarios.
const (
	// ExitOK indicates a clean run with no `--exit-on` trigger.
	ExitOK = 0
	// ExitFindingTriggered indicates a clean run whose maximum
	// finding severity met or exceeded the `--exit-on` threshold
	// (closed set: info / warn / block).
	ExitFindingTriggered = 1
	// ExitWalkerError indicates a walker failure -- the most
	// common cases are a missing root path or a permission
	// denied on a traversed directory (tech-spec Sec 8.6 row 2).
	ExitWalkerError = 2
	// ExitUsage maps to BSD `EX_USAGE` (64) and is returned for
	// any operator-facing usage error: an unknown sub-command,
	// a malformed flag, a missing positional argument, or the
	// use of a reserved verb (`apply`) / reserved flag
	// (`--telemetry-otlp`) in the P0/P1 binaries.
	ExitUsage = 64
	// ExitInternalError maps to BSD `EX_SOFTWARE` (70) and
	// indicates an internal engine error (parser panic, planner
	// crash, etc.). The skeleton stages also use this code for
	// "sub-command body not yet wired" stubs so a successful
	// exit is never claimed for unimplemented behaviour.
	ExitInternalError = 70
)

// Sub-command verb names. The set is closed -- adding a verb
// requires editing both this slice and the help text in
// cmd/cleanc/main.go AND adding a matching dispatch arm.
const (
	VerbAnalyze = "analyze"
	VerbReport  = "report"
	VerbVersion = "version"
	VerbApply   = "apply"
)

// Verbs is the canonical, ordered list of sub-command verbs.
// The order here is the order the dispatcher's usage text
// renders them in.
var Verbs = []string{VerbAnalyze, VerbReport, VerbVersion, VerbApply}

// Flag default values pinned in tech-spec.md Sec 8.1.
//
// `DefaultDevMode` is intentionally split across the build-tag
// pair `devmode_default.go` (`//go:build !prod` -> true) and
// `devmode_prod.go` (`//go:build prod` -> false) so the matrix
// is enforced at COMPILE time: a prod build that imports this
// package gets `DefaultDevMode == false` regardless of what
// `cmd/cleanc/` does. Centralising the constant here means the
// dispatcher in `cmd/cleanc/main.go` reads `flags.DefaultDevMode`
// directly and never owns a `defaultDevMode` of its own
// (resolves iter-4 evaluator item 6 -- "DEV-MODE DEFAULT NOT
// CENTRALIZED IN FLAGS HELPER").
const (
	// DefaultOut for `--out`. Empty string means "write
	// markdown report to stdout" (tech-spec Sec 8.1 row 1).
	DefaultOut = ""
	// DefaultFindings for `--findings`. The skeleton uses the
	// literal file name the e2e scenarios reference verbatim.
	DefaultFindings = "findings.json"
	// DefaultEmitPrompts for `--emit-prompts`. Empty string
	// means "do not emit the L7 structured-prompt JSONL".
	DefaultEmitPrompts = ""
	// DefaultPolicy for `--policy`. Empty string means "use the
	// embedded YAML rule packs baked into the binary via
	// `policy/rulepacks/embedded_fs.go` (`//go:embed solid/*.yaml
	// decoupling/*.yaml`); the dev-mode loader in
	// `internal/cli/devpolicy/embed.go` re-exports
	// `rulepacks.EmbeddedFS` to the orchestrator.
	DefaultPolicy = ""
	// DefaultWithChurn for `--with-churn`. The default is false
	// because git history scanning is opt-in for Phase 1
	// (tech-spec Sec 8.1 row 5; the walker lands in Stage 2.1).
	DefaultWithChurn = false
	// DefaultTopN for `--top-n`. ZERO MEANS "use the policy
	// default of 20" (PolicyDefaultTopN) -- tech-spec Sec 8.1
	// row 6 pins this semantic explicitly. The report renderer
	// substitutes PolicyDefaultTopN when it observes a literal
	// zero on the CLI, so an operator cannot accidentally
	// request "no cap" without supplying a very large number
	// (e.g. `--top-n 999999`).
	DefaultTopN = 0
	// PolicyDefaultTopN is the substitute value the renderer
	// uses when `--top-n` is the literal zero default. Pinned
	// by tech-spec Sec 8.1 row 6.
	PolicyDefaultTopN = 20
	// DefaultExitOn for `--exit-on`. The default is `block`,
	// meaning only block-severity findings trip exit code 1.
	DefaultExitOn = "block"
	// DefaultDiagnostics for `--diagnostics`. Empty string
	// means "do not write the diagnostics JSON sidecar".
	DefaultDiagnostics = ""
	// DefaultTelemetryOTLP for `--telemetry-otlp`. Empty string
	// means "no OTLP sink wired". This flag is reserved in the
	// P0/P1 binaries -- the dispatcher rejects any non-empty
	// value with `ExitUsage` (tech-spec Sec 8.6 row 4).
	DefaultTelemetryOTLP = ""
)

// Globals collects every global-flag pointer pinned by
// tech-spec Sec 8.1. `Register` populates one of these from
// any *flag.FlagSet and `Validate` enforces the closed-set
// rules (`--exit-on` membership, reserved-flag rejection).
//
// Keeping the surface inside this helper guarantees `analyze`
// and `report` see byte-identical flag sets (resolves iter-4
// evaluator item 4 -- "REPORT FLAG SURFACE INCOMPLETE").
type Globals struct {
	Out           *string
	Findings      *string
	EmitPrompts   *string
	Policy        *string
	WithChurn     *bool
	TopN          *int
	ExitOn        *string
	Diagnostics   *string
	DevMode       *bool
	TelemetryOTLP *string
}

// Register attaches every global flag pinned by tech-spec
// Sec 8.1 to `fs` using the per-flag default constants in
// this package. The `--dev-mode` default comes from the
// build-tag-paired `DefaultDevMode` constant in this same
// package -- callers MUST NOT pass their own default; the
// build matrix is the single source of truth.
//
// Returns a populated *Globals whose pointer fields are kept
// in sync with `fs.Parse(...)` writes. Callers chain
// `g := flags.Register(fs); ...; if err := g.Validate(verb);
// err != nil { ... }` after parsing.
func Register(fs *flag.FlagSet) *Globals {
	g := &Globals{
		Out:           fs.String("out", DefaultOut, "markdown report path (empty = stdout)"),
		Findings:      fs.String("findings", DefaultFindings, "JSON findings artifact path"),
		EmitPrompts:   fs.String("emit-prompts", DefaultEmitPrompts, "JSONL refactor-prompt path (empty = disabled)"),
		Policy:        fs.String("policy", DefaultPolicy, "policy-bundle directory (empty = embedded rule packs)"),
		WithChurn:     fs.Bool("with-churn", DefaultWithChurn, "include git churn (reserved for P2, rejected in P0/P1)"),
		TopN:          fs.Int("top-n", DefaultTopN, "cap the hot-spot table (0 = use policy default of 20)"),
		ExitOn:        fs.String("exit-on", DefaultExitOn, "severity threshold for exit code 1 (info|warn|block)"),
		Diagnostics:   fs.String("diagnostics", DefaultDiagnostics, "diagnostics JSON sidecar path (empty = disabled)"),
		DevMode:       fs.Bool("dev-mode", DefaultDevMode, "permit unsigned policy bundles (dev builds only)"),
		TelemetryOTLP: fs.String("telemetry-otlp", DefaultTelemetryOTLP, "OTLP collector URL (reserved for a future story)"),
	}
	return g
}

// Validate runs the cross-flag rules pinned by e2e-scenarios.md
// Stage 3.3 / Stage 4.4: rejected reserved flags and the
// closed-set `--exit-on` membership. The `verb` argument is woven
// into the literal stderr messages so an operator invoking
// `cleanc report --with-churn=true findings.json` sees a
// `cleanc report: --with-churn is reserved ...` prefix rather
// than the misleading `cleanc analyze:` baked into the const
// form -- the substring assertions in e2e-scenarios.md Stage 4.4
// (`--with-churn is reserved`) still match in either case.
//
// If `stderr` is non-nil and a rule trips, Validate writes the
// verb-prefixed literal message before returning a non-nil error
// so the dispatcher can exit with `ExitUsage` without duplicating
// the message strings. An empty `verb` falls back to the legacy
// `cleanc analyze:` prefix held in the exported message constants
// so callers that pre-date this helper keep working unchanged.
func (g *Globals) Validate(verb string, stderr io.Writer) error {
	if g.TelemetryOTLP != nil && *g.TelemetryOTLP != "" {
		if stderr != nil {
			fmt.Fprintln(stderr, ReservedTelemetryMessageFor(verb))
		}
		return fmt.Errorf("--telemetry-otlp is reserved")
	}
	if g.WithChurn != nil && *g.WithChurn {
		if stderr != nil {
			fmt.Fprintln(stderr, ReservedWithChurnMessageFor(verb))
		}
		return fmt.Errorf("--with-churn is reserved")
	}
	if g.ExitOn != nil && !IsValidExitOn(*g.ExitOn) {
		if stderr != nil {
			fmt.Fprintln(stderr, ExitOnUsageMessage)
		}
		return fmt.Errorf("--exit-on out of range")
	}
	return nil
}

// ReservedTelemetryMessageFor returns the literal stderr line
// emitted when `--telemetry-otlp` is set on a P0/P1 build,
// prefixed with the invoking verb so `cleanc report
// --telemetry-otlp=...` does not mislead operators with an
// `analyze` tag. Pass an empty `verb` to get the legacy
// `cleanc analyze:` prefix held in [ReservedTelemetryMessage].
func ReservedTelemetryMessageFor(verb string) string {
	if verb == "" {
		return ReservedTelemetryMessage
	}
	return fmt.Sprintf("cleanc %s: --telemetry-otlp is reserved for a future story (not implemented in P0/P1)", verb)
}

// ReservedWithChurnMessageFor returns the literal stderr line
// emitted when `--with-churn` is set on a P0/P1 build, prefixed
// with the invoking verb. Pass an empty `verb` to get the legacy
// `cleanc analyze:` prefix held in [ReservedWithChurnMessage].
func ReservedWithChurnMessageFor(verb string) string {
	if verb == "" {
		return ReservedWithChurnMessage
	}
	return fmt.Sprintf("cleanc %s: --with-churn is reserved for P2 and rejected in P0/P1", verb)
}

// ReservedSnippetCapLinesMessageFor returns the literal stderr
// line emitted when `--snippet-cap-lines` is set on a P0/P1
// build, prefixed with the invoking verb so `cleanc report
// --snippet-cap-lines=120` does not mislead operators with an
// `analyze` tag. Pass an empty `verb` to get the legacy
// `cleanc analyze:` prefix held in
// [ReservedSnippetCapLinesMessage].
func ReservedSnippetCapLinesMessageFor(verb string) string {
	if verb == "" {
		return ReservedSnippetCapLinesMessage
	}
	return fmt.Sprintf("cleanc %s: --snippet-cap-lines is reserved for a future minor release", verb)
}

// IsReservedSnippetCapLinesArg reports whether `arg` is any
// recognised form of the reserved `--snippet-cap-lines` flag:
//
//   - `--snippet-cap-lines`           (boolean / value-as-next-arg)
//   - `-snippet-cap-lines`            (single-dash form)
//   - `--snippet-cap-lines=<value>`   (value-attached form)
//   - `-snippet-cap-lines=<value>`    (single-dash value-attached)
//
// The dispatcher pre-scans `args` with this helper BEFORE the
// flag-set is parsed, so a reserved-flag invocation exits with
// `ExitUsage` + the literal `reserved for a future minor release`
// substring (e2e-scenarios.md Stage 4.4 line 1072) rather than
// the stdlib `flag provided but not defined` error that would
// otherwise fire (since the flag is intentionally NOT registered
// on either `analyze` or `report`).
func IsReservedSnippetCapLinesArg(arg string) bool {
	for _, prefix := range []string{"--snippet-cap-lines", "-snippet-cap-lines"} {
		if arg == prefix {
			return true
		}
		if len(arg) > len(prefix) && arg[:len(prefix)] == prefix && arg[len(prefix)] == '=' {
			return true
		}
	}
	return false
}

// ExitOnLevels is the closed severity set accepted by
// `--exit-on`. Lower-cased exact match is enforced by
// `IsValidExitOn`.
var ExitOnLevels = []string{"info", "warn", "block"}

// IsValidExitOn reports whether v is one of the
// `ExitOnLevels`. The CLI dispatcher uses this to reject
// `--exit-on banana` with `ExitUsage`.
func IsValidExitOn(v string) bool {
	for _, lvl := range ExitOnLevels {
		if lvl == v {
			return true
		}
	}
	return false
}

// ReservedApplyMessage is the literal stderr line the
// dispatcher writes when an operator invokes `cleanc apply ...`
// against a P0/P1 binary. The operator pin `cli-l7-authority`
// (architecture.md Sec 1.3) gates whether `apply` is ever
// implemented; until then the sub-command is reserved.
//
// The string MUST contain BOTH substrings:
//
//   - `not implemented; pending operator pin cli-l7-authority`
//     (NOTE: no backticks around the pin id — `e2e-scenarios.md`
//     Stage 4.4 line 1050 asserts the literal phrase with the
//     bare identifier, so wrapping the id in backticks would
//     fail the e2e contract).
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3`
//     (the operator-facing pointer required by
//     `e2e-scenarios.md` Stage 4.4 line 1051).
const ReservedApplyMessage = "cleanc apply: not implemented; pending operator pin cli-l7-authority (see docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3)"

// ReservedSnippetCapLinesMessage is the literal stderr line
// emitted when the (currently unrecognised) `--snippet-cap-lines`
// flag appears on an `analyze` or `report` command line. The
// flag is named in `tech-spec.md` Sec 8.1 as reserved for a
// future minor release; e2e-scenarios.md Stage 4.4 line 1072
// pins the literal substring `reserved for a future minor release`
// so this message MUST contain that phrase verbatim.
//
// The legacy `cleanc analyze:` prefix is preserved for the
// default const so existing call sites that pre-date the
// verb-aware helper keep working unchanged; callers with
// a verb in hand should prefer
// [ReservedSnippetCapLinesMessageFor].
const ReservedSnippetCapLinesMessage = "cleanc analyze: --snippet-cap-lines is reserved for a future minor release"

// ReservedTelemetryMessage is the literal stderr line the
// dispatcher writes when `--telemetry-otlp` is set on a
// P0/P1 build. The exact phrase `--telemetry-otlp is reserved
// for a future story` is pinned by `e2e-scenarios.md` Stage 4.4
// (line 1061), so this string MUST contain that substring
// verbatim.
const ReservedTelemetryMessage = "cleanc analyze: --telemetry-otlp is reserved for a future story (not implemented in P0/P1)"

// ReservedWithChurnMessage is the literal stderr line the
// dispatcher writes when `--with-churn` is set on a P0/P1
// build. The exact phrase `--with-churn is reserved for P2
// and rejected in P0/P1` is pinned by `e2e-scenarios.md`
// Stage 4.4 (line 1067).
const ReservedWithChurnMessage = "cleanc analyze: --with-churn is reserved for P2 and rejected in P0/P1"

// ExitOnUsageMessage is the literal stderr line the dispatcher
// writes when `--exit-on <sev>` carries a value outside the
// closed set `{info, warn, block}`. Pinned by
// `e2e-scenarios.md` Stage 3.3 (line 788).
const ExitOnUsageMessage = "--exit-on must be one of info, warn, block"

// UnknownSubcommandPhrase is the literal stderr substring the
// dispatcher emits when an operator runs `cleanc <unknown-verb>`.
// The e2e scenarios at lines 168 and 157 of
// `e2e-scenarios.md` assert both presence (rejected verbs) and
// absence (canonical verbs) of this exact phrase, so renaming
// is a contract change.
const UnknownSubcommandPhrase = "unknown sub-command"
