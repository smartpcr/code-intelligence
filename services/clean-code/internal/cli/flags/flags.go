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
// `--dev-mode` is intentionally absent from this set because
// its default flips between dev and prod builds (no-tag -> true,
// `-tags prod` -> false); the per-build-tag default lives in
// `cmd/cleanc/buildtag_default.go` / `buildtag_prod.go` to keep
// the toggle visible to a reader scanning the cmd/ directory.
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
	// go:embed" (the loader lands in Stage 1.4).
	DefaultPolicy = ""
	// DefaultWithChurn for `--with-churn`. The default is false
	// because git history scanning is opt-in for Phase 1
	// (tech-spec Sec 8.1 row 5; the walker lands in Stage 2.1).
	DefaultWithChurn = false
	// DefaultTopN for `--top-n`. Zero means "no cap on the
	// hotspot table" -- the report renderer treats 0 as "show
	// every row".
	DefaultTopN = 0
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
const ReservedApplyMessage = "cleanc apply: not implemented; pending operator pin `cli-l7-authority`"

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
