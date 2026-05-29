package isolation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// DefaultMemoryLimitBytes is the default per-worker memory cap
// applied when [SubprocessConfig.MemoryLimitBytes] is zero.
// 512 MiB is a generous-but-bounded budget for tree-sitter
// parsing of a single file; well below what would meaningfully
// impact a host pod sized per architecture Sec 8.3.
const DefaultMemoryLimitBytes uint64 = 512 << 20

// DefaultTimeout is the default per-Execute wall-clock budget
// applied when [SubprocessConfig.Timeout] is zero. 30 seconds
// is more than two orders of magnitude above the p99 parse
// time observed in Stage 2.1 benchmarks; lower than any
// reasonable HTTP scrape window.
const DefaultTimeout = 30 * time.Second

// SubprocessConfig pins the per-worker resource budget. Both
// fields are clamped to documented defaults when set to zero.
type SubprocessConfig struct {
	// MemoryLimitBytes is the soft RLIMIT_AS cap applied to
	// each subprocess (Unix only; Windows is a documented
	// no-op enforced at process scheduling layer in v1).
	// Zero -> [DefaultMemoryLimitBytes].
	MemoryLimitBytes uint64

	// Timeout is the hard wall-clock budget per Execute call.
	// Enforced via the derived context passed to the worker
	// (`exec.CommandContext` honours it). Zero ->
	// [DefaultTimeout]. The context.DeadlineExceeded that
	// fires distinguishes [ErrParserTimeout] from
	// `context.Canceled` (caller-cancel).
	Timeout time.Duration
}

// resolve returns a copy of `cfg` with zero fields filled in
// with the documented defaults.
func (cfg SubprocessConfig) resolve() SubprocessConfig {
	out := cfg
	if out.MemoryLimitBytes == 0 {
		out.MemoryLimitBytes = DefaultMemoryLimitBytes
	}
	if out.Timeout == 0 {
		out.Timeout = DefaultTimeout
	}
	return out
}

// ParseRequest is the input shape forwarded to a [Worker].
type ParseRequest struct {
	// Language is one of the v1-pinned parser tags (`go`,
	// `python`, `typescript`, `java`).
	Language string
	// Path is the repo-relative source path. Preserved on
	// failure diagnostics ([ParserCrashError.Path]).
	Path string
	// Content is the source bytes. Workers MUST NOT mutate.
	Content []byte
}

// ParseResult is the canonical output shape returned by a
// [Worker] on success.
type ParseResult struct {
	// AstFileBytes is the serialised AST payload. In v1 the
	// payload is a JSON-encoded `parser.AstFile` (see
	// [inProcessWorker.Execute] which encodes via
	// `json.NewEncoder`, and [WrapParser] /
	// `metric_ingestor.DirectoryAstFileSource` which decode
	// via `json.Unmarshal`). The isolation layer treats the
	// bytes as opaque -- the codec lives in the parser
	// adapter so swapping it (e.g. to the existing
	// `internal/ast/v1` proto types) is a forward-compatible
	// change at the adapter boundary, not a wire-protocol
	// break.
	//
	// Length-bounded by the worker; callers are responsible
	// for honouring any size limit upstream.
	AstFileBytes []byte

	// DegradedReason mirrors `AstFile.degraded_reason` (see
	// `parser.Parser`). Non-empty indicates a partial parse
	// the caller should surface but not treat as a hard
	// failure.
	DegradedReason string
}

// Worker executes a single parse on behalf of [Pool]. The
// production implementation is an `exec.Cmd`-backed worker
// (see [NewExecWorker]); tests inject fakes via
// [WorkerFactory].
//
// Workers are stateful only in the sense of carrying their
// own configuration; concurrent Execute calls are safe.
// Callers serialise admission via [Pool], not via the worker
// itself.
type Worker interface {
	// Language returns the canonical language tag the
	// worker handles. MUST match the factory registration
	// key in [Pool.RegisterFactory].
	Language() string

	// Execute runs the parse and returns the result or a
	// typed error. The implementation MUST honour
	// `ctx.Done()` and surface any subprocess crash via
	// [*ParserCrashError]; the host process MUST NOT
	// panic on a child crash.
	Execute(ctx context.Context, req ParseRequest) (*ParseResult, error)

	// Close releases any persistent resources (long-lived
	// subprocesses, file descriptors). Idempotent; safe
	// to call multiple times.
	Close() error
}

// WorkerFactory builds a fresh [Worker] for `language` under
// the supplied [SubprocessConfig]. The factory is the
// dependency-injection seam tests use to substitute a fake
// worker without touching `exec.Cmd`.
type WorkerFactory func(language string, cfg SubprocessConfig) (Worker, error)

// Pool routes parse requests through a per-language [Worker]
// while honouring the [ModeCoordinator] drain contract. The
// pool's invariants:
//
//   - One long-lived [Worker] per language (created lazily on
//     first [Parse] for that language and cached for the
//     lifetime of the pool). The default [ExecWorker]
//     implementation spawns a FRESH child process per parse;
//     a long-running worker that reuses a child across parses
//     can be substituted via [RegisterFactory] without
//     touching the pool's admission / drain contract. The
//     ephemeral-child default is deliberate: a parser crash
//     or OOM cannot corrupt the next parse's state, which is
//     the Stage 9.3 brief's strongest crash-isolation
//     guarantee. A long-lived child process variant is a
//     future workstream (perf optimisation; tracked as a
//     follow-up because it adds a child-restart state machine
//     the current contract does not need).
//   - All [Parse] calls go through [ModeCoordinator.BeginScan]
//     so a `mgmt.set_mode` flip drains in-flight parses for
//     the targeted repo.
//   - All errors from the underlying [Worker] are propagated
//     verbatim; the pool does not wrap [*ParserCrashError]
//     a second time.
type Pool struct {
	cfg          SubprocessConfig
	coordinator  *ModeCoordinator
	mu           sync.Mutex
	factories    map[string]WorkerFactory
	workers      map[string]Worker
	closed       bool
}

// NewPool constructs a Pool with the supplied config and
// coordinator. `coordinator` MUST be non-nil; the brief
// (Stage 9.3) makes drain-on-flip a hard requirement and the
// pool refuses to operate without the primitive that enforces
// it.
func NewPool(cfg SubprocessConfig, coordinator *ModeCoordinator) (*Pool, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("isolation: NewPool: coordinator is nil; mode-flip drain is mandatory per Stage 9.3 brief")
	}
	return &Pool{
		cfg:         cfg.resolve(),
		coordinator: coordinator,
		factories:   make(map[string]WorkerFactory),
		workers:     make(map[string]Worker),
	}, nil
}

// RegisterFactory installs `factory` for `language`. Returns
// an error if `language` is already registered (loud
// build-time failure rather than silent last-writer-wins). A
// nil factory is rejected.
func (p *Pool) RegisterFactory(language string, factory WorkerFactory) error {
	if factory == nil {
		return fmt.Errorf("isolation: Pool.RegisterFactory: nil factory for language %q", language)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("isolation: Pool.RegisterFactory: pool is closed")
	}
	if _, exists := p.factories[language]; exists {
		return fmt.Errorf("isolation: Pool.RegisterFactory: language %q already registered", language)
	}
	p.factories[language] = factory
	return nil
}

// Languages returns the registered language tags. Test
// helper; production health checks use the registered set.
func (p *Pool) Languages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.factories))
	for lang := range p.factories {
		out = append(out, lang)
	}
	return out
}

// workerFor returns the [Worker] for `language`, creating it
// from the registered factory on first touch. Returns
// [ErrUnknownLanguage] if no factory is registered.
func (p *Pool) workerFor(language string) (Worker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("isolation: Pool.workerFor: pool is closed")
	}
	if w, ok := p.workers[language]; ok {
		return w, nil
	}
	factory, ok := p.factories[language]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownLanguage, language)
	}
	w, err := factory(language, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("isolation: Pool.workerFor: factory(%q): %w", language, err)
	}
	p.workers[language] = w
	return w, nil
}

// Parse routes `req` through the coordinator (drain-safe
// admission) and the per-language worker.
//
//   - The repo MUST have been hydrated via
//     [ModeCoordinator.HydrateMode] OR a [WithModeHydrator]
//     callback MUST be wired on the coordinator; otherwise
//     [ErrModeNotHydrated] is returned.
//   - The call honours `ctx.Done()` AND the configured
//     [SubprocessConfig.Timeout]; whichever fires first
//     terminates the parse. A timeout returns a
//     [*ParserCrashError] with [ErrParserTimeout]; a caller
//     cancel returns `context.Canceled`.
//   - A subprocess crash (OOM, segfault, panic) returns a
//     [*ParserCrashError] with the appropriate sentinel; the
//     host process REMAINS RUNNING.
//
// Callers that perform many parses inside a single
// scan-admission window (notably the metric-ingestor's
// [DirectoryAstFileSource] walking a checkout) should use
// [ParseInScan] with a held token instead so the coordinator's
// in-flight counter reflects ONE scan, not one-per-file.
func (p *Pool) Parse(ctx context.Context, repoID uuid.UUID, req ParseRequest) (*ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tok, err := p.coordinator.BeginScan(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer p.coordinator.EndScan(tok)
	return p.executeInScan(ctx, tok, req)
}

// ParseInScan runs the parse using an already-admitted scan
// token. The caller MUST hold a valid, still-active token from
// [ModeCoordinator.BeginScan]; an after-life token
// (`EndScan` already called) or the zero-token is rejected
// with [ErrScanTokenInvalid].
//
// Use this from callers that bracket many parses with ONE
// BeginScan/EndScan pair (the [DirectoryAstFileSource] pattern)
// so the coordinator's in-flight counter tracks scans -- not
// files. The drain contract still holds because the held token
// keeps `inFlight > 0` until EndScan.
func (p *Pool) ParseInScan(ctx context.Context, tok ScanToken, req ParseRequest) (*ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !tok.Active() {
		return nil, fmt.Errorf("%w: token is zero or already ended; ParseInScan MUST be called between BeginScan and EndScan", ErrScanTokenInvalid)
	}
	return p.executeInScan(ctx, tok, req)
}

// executeInScan is the shared body of [Parse] and
// [ParseInScan]. It owns the per-call timeout context and the
// worker-error classification. It does NOT touch the
// coordinator -- the caller is responsible for admission.
func (p *Pool) executeInScan(ctx context.Context, tok ScanToken, req ParseRequest) (*ParseResult, error) {
	worker, err := p.workerFor(req.Language)
	if err != nil {
		return nil, err
	}

	parseCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	res, execErr := worker.Execute(parseCtx, req)
	if execErr != nil {
		return nil, classifyWorkerError(parseCtx, ctx, req, execErr)
	}
	_ = tok // referenced for godoc / future drain-aware diagnostics
	return res, nil
}

// Close releases every registered worker. Idempotent.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var firstErr error
	for lang, w := range p.workers {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("isolation: Pool.Close: worker(%q): %w", lang, err)
		}
	}
	p.workers = map[string]Worker{}
	return firstErr
}

// classifyWorkerError disambiguates the failure modes the
// worker may surface. Order matters:
//
//  1. If the caller's outer context cancelled (context.Canceled
//     and NOT DeadlineExceeded), the failure is a caller-cancel.
//     We surface ctx.Err() verbatim so callers can match
//     `errors.Is(err, context.Canceled)`.
//  2. If the parseCtx (timeout-derived) hit DeadlineExceeded,
//     the failure is a timeout. We wrap with [ErrParserTimeout].
//  3. If the error already carries a typed [*ParserCrashError]
//     sentinel (workers may return one directly), pass it
//     through unchanged.
//  4. Otherwise wrap the error with [ErrParserCrash] so the
//     caller still gets a typed sentinel.
//
// `outerCtx` is the original context the caller supplied;
// `parseCtx` is the per-call timeout-derived ctx. Distinguishing
// the two lets us tell "caller cancelled" (Canceled on outer)
// from "we timed out" (DeadlineExceeded on parseCtx).
func classifyWorkerError(parseCtx, outerCtx context.Context, req ParseRequest, execErr error) error {
	// Caller-cancel takes precedence: if outerCtx is done with
	// Canceled, attribute the failure to the caller regardless
	// of which signal the subprocess died from.
	if outerErr := outerCtx.Err(); outerErr != nil && errors.Is(outerErr, context.Canceled) {
		return outerErr
	}
	// Per-call timeout: parseCtx hit its deadline.
	if errors.Is(parseCtx.Err(), context.DeadlineExceeded) {
		// Preserve any worker-side diagnostic on the
		// ParserCrashError; fall back to a minimal one.
		var pce *ParserCrashError
		if errors.As(execErr, &pce) {
			pce.Sentinel = ErrParserTimeout
			pce.Language = req.Language
			pce.Path = req.Path
			return pce
		}
		return &ParserCrashError{
			Sentinel:      ErrParserTimeout,
			Language:      req.Language,
			Path:          req.Path,
			ExitCode:      -1,
			Signal:        "timeout",
			StderrSnippet: execErr.Error(),
		}
	}
	// Already a typed ParserCrashError? Pass through but
	// guarantee Language/Path are populated.
	var pce *ParserCrashError
	if errors.As(execErr, &pce) {
		if pce.Language == "" {
			pce.Language = req.Language
		}
		if pce.Path == "" {
			pce.Path = req.Path
		}
		return pce
	}
	// Bare sentinel returned by a [Worker] (notably the
	// fake worker used in unit tests, but also any real
	// worker that bubbles up [ErrParserOOM] /
	// [ErrParserTimeout] / [ErrParserCrash] directly).
	// Wrap so the caller still gets [*ParserCrashError]
	// context.
	for _, sentinel := range []error{ErrParserOOM, ErrParserTimeout, ErrParserCrash} {
		if errors.Is(execErr, sentinel) {
			return &ParserCrashError{
				Sentinel:      sentinel,
				Language:      req.Language,
				Path:          req.Path,
				ExitCode:      -1,
				StderrSnippet: execErr.Error(),
			}
		}
	}
	// Generic worker error: wrap with ErrParserCrash.
	return &ParserCrashError{
		Sentinel:      ErrParserCrash,
		Language:      req.Language,
		Path:          req.Path,
		ExitCode:      -1,
		StderrSnippet: execErr.Error(),
	}
}
