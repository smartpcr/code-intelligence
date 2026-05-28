package isolation

import (
	"errors"
	"fmt"
)

// Sentinel errors surfaced by the isolation package. All real
// errors returned to callers are wrapped in [*ParserCrashError]
// (or [*ScanAdmissionError]) and unwrap to one of these
// sentinels so callers can use `errors.Is`.
var (
	// ErrParserOOM signals the parser subprocess exceeded its
	// memory budget. The host process REMAINS RUNNING; only
	// the subprocess died. Architecture Sec 9.2 maps this to
	// the `parse_panics_total` counter; threshold may trigger
	// an automatic mode flip.
	ErrParserOOM = errors.New("isolation: parser subprocess exceeded memory limit")

	// ErrParserTimeout signals the parser subprocess exceeded
	// its hard wall-clock budget ([SubprocessConfig.Timeout]).
	// The host received `ctx.DeadlineExceeded`; the subprocess
	// was killed via `exec.CommandContext`. Distinct from a
	// generic ctx-cancel so callers can apply different
	// back-off heuristics.
	ErrParserTimeout = errors.New("isolation: parser subprocess exceeded timeout")

	// ErrParserCrash signals an abnormal subprocess exit that
	// is not specifically OOM or timeout (segfault, panic,
	// non-zero exit with no diagnosable signal). The wrapping
	// [*ParserCrashError] carries the captured stderr snippet
	// and the raw exit code.
	ErrParserCrash = errors.New("isolation: parser subprocess crashed")

	// ErrModeNotHydrated is returned by [ModeCoordinator.BeginScan]
	// when the caller has not yet called
	// [ModeCoordinator.HydrateMode] for the repo_id. The
	// coordinator deliberately does NOT default to `embedded`
	// because doing so would let a coordinator cold-start
	// disagree with the persisted `clean_code.repo.mode`
	// (rubber-duck iter-1 finding #2).
	ErrModeNotHydrated = errors.New("isolation: ModeCoordinator: repo mode not hydrated; call HydrateMode after consulting the catalog")

	// ErrInvalidMode is returned when a caller passes a mode
	// value outside the [AllowedModes] set. Mirrors the
	// management/repo_store invariant; pinned here so the
	// isolation package has no upward dependency on
	// internal/management.
	ErrInvalidMode = errors.New("isolation: invalid mode (allowed: embedded, linked)")

	// ErrModeFlipApplyFailed is returned by [ModeCoordinator.SetMode]
	// when the supplied `applyFn` (the catalog mutation) fails.
	// The coordinator's in-memory mode is left UNCHANGED and
	// the flip flag is cleared so the next call can retry.
	ErrModeFlipApplyFailed = errors.New("isolation: ModeCoordinator: applyFn failed; mode left unchanged")

	// ErrUnknownLanguage is returned by [Pool.Parse] / [Pool.Worker]
	// when no [WorkerFactory] is registered for the requested
	// language tag.
	ErrUnknownLanguage = errors.New("isolation: no worker factory registered for language")
)

// ParserCrashError captures the diagnostic context that
// accompanies an OOM / timeout / crash. Callers match the
// failure class with `errors.Is(err, ErrParserOOM)` (etc.) and
// pull richer context (exit code, signal, stderr snippet) via
// `errors.As(err, &pce *ParserCrashError)`.
type ParserCrashError struct {
	// Sentinel is one of [ErrParserOOM], [ErrParserTimeout],
	// or [ErrParserCrash]. `errors.Is` traverses through
	// [ParserCrashError.Unwrap] to reach it.
	Sentinel error
	// Language is the parser language tag (`go`, `python`,
	// `typescript`, `java`) the failed call targeted.
	Language string
	// Path is the repo-relative source path the parser was
	// invoked on, preserved verbatim so operators can grep
	// logs for a single file.
	Path string
	// ExitCode is the subprocess exit code. -1 if the
	// subprocess was killed before yielding an exit code
	// (timeout / signal).
	ExitCode int
	// Signal is the captured terminating signal name (e.g.
	// "killed", "segmentation fault"). Empty when the
	// subprocess exited normally with a non-zero code.
	Signal string
	// StderrSnippet is up to [StderrSnippetMax] bytes of the
	// subprocess's stderr at termination, trimmed to keep
	// log lines bounded.
	StderrSnippet string
}

// StderrSnippetMax bounds the stderr capture preserved on
// [ParserCrashError]. Stderr beyond this is truncated; the
// snippet is for human diagnosis, not bulk log archival.
const StderrSnippetMax = 4096

// Error implements `error`.
func (e *ParserCrashError) Error() string {
	if e == nil {
		return "<nil isolation.ParserCrashError>"
	}
	parts := fmt.Sprintf("%s: language=%q path=%q exit=%d", e.Sentinel.Error(), e.Language, e.Path, e.ExitCode)
	if e.Signal != "" {
		parts += " signal=" + e.Signal
	}
	if e.StderrSnippet != "" {
		parts += " stderr=" + truncate(e.StderrSnippet, 256)
	}
	return parts
}

// Unwrap surfaces [ParserCrashError.Sentinel] so
// `errors.Is(err, ErrParserOOM)` succeeds.
func (e *ParserCrashError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Sentinel
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
