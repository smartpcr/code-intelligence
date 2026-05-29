package isolation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// ChildEnvVar is the env var name the parent sets on a
// subprocess child to switch the binary into "parser child"
// mode. The host binary's `main()` (or a `TestMain` in test
// builds) MUST observe this var and call [RunChild] to handle
// the parse request from stdin, write the response to stdout,
// and exit. The value's expected shape is a free-form string
// the child interprets (typically just "1" to indicate "act
// as a parser child").
const ChildEnvVar = "__ISOLATION_PARSER_CHILD"

// ChildMemoryLimitEnvVar communicates the parent's
// [SubprocessConfig.MemoryLimitBytes] to the child so the
// child can call `Setrlimit(RLIMIT_AS, …)` on itself before
// any allocation. Go's `syscall.SysProcAttr` doesn't expose
// rlimit on every platform; the env-var-then-self-setrlimit
// pattern works portably and is testable.
const ChildMemoryLimitEnvVar = "__ISOLATION_PARSER_CHILD_MEM_BYTES"

// ExecConfig pins the command shape used by [ExecWorker] to
// spawn a parser child process. The defaults use the host
// binary (`os.Args[0]`) with [ChildEnvVar] set so the host's
// `main` re-routes to parser-child mode.
type ExecConfig struct {
	// Program is the executable path. Defaults to
	// `os.Executable()`.
	Program string
	// Args are the args passed to the child (after Program).
	// Defaults to `[]string{}` -- the child detects mode via
	// [ChildEnvVar], not flags.
	Args []string
	// ExtraEnv lets tests inject additional env (notably a
	// child-mode discriminator beyond [ChildEnvVar]).
	ExtraEnv []string
}

// resolve fills in defaults. Returns the resolved config and
// any error from `os.Executable()`.
func (c ExecConfig) resolve() (ExecConfig, error) {
	out := c
	if out.Program == "" {
		exe, err := os.Executable()
		if err != nil {
			return out, fmt.Errorf("isolation: ExecConfig.resolve: os.Executable: %w", err)
		}
		out.Program = exe
	}
	return out, nil
}

// ExecWorker is the default [Worker] implementation. Each
// Execute call spawns a fresh subprocess via `exec.CommandContext`,
// writes the request to stdin, and reads the response from
// stdout. Stderr is captured for diagnostic inclusion in
// [*ParserCrashError]. Memory cap is communicated via
// [ChildMemoryLimitEnvVar] so the child can self-apply
// RLIMIT_AS (Unix); on Windows the cap is recorded but not
// enforced at the OS layer.
//
// One ExecWorker = one language. The [Pool] holds the
// (language -> ExecWorker) registry for the lifetime of the
// process; the ExecWorker is long-lived but the CHILD process
// it spawns is ephemeral (one per [Execute]). This shape
// gives the Stage 9.3 brief's strongest crash-isolation
// guarantee: a parser crash, segfault, or OOM cannot corrupt
// the next parse's state because the next parse runs in a
// fresh child with a fresh address space. The performance
// cost is one `exec.CommandContext` startup per parse; the
// metric-ingestor amortises this by batching many parses
// inside a single [ModeCoordinator.BeginScan] window so the
// admission/drain accounting reflects one scan rather than
// one-per-file (see [Pool.ParseInScan]). A long-lived
// per-language child process variant that reuses a single
// `exec.Cmd` across many parses is a future perf workstream;
// it would substitute a new [Worker] implementation via
// [Pool.RegisterFactory] without changing the pool's
// admission contract.
type ExecWorker struct {
	language string
	cfg      SubprocessConfig
	exec     ExecConfig
}

// NewExecWorker constructs an [ExecWorker]. The factory shape
// [WorkerFactory] wraps this with the language/cfg arguments.
func NewExecWorker(language string, cfg SubprocessConfig, exec ExecConfig) (*ExecWorker, error) {
	resolved, err := exec.resolve()
	if err != nil {
		return nil, err
	}
	return &ExecWorker{
		language: language,
		cfg:      cfg.resolve(),
		exec:     resolved,
	}, nil
}

// Language implements [Worker].
func (w *ExecWorker) Language() string { return w.language }

// Close implements [Worker]. ExecWorker is stateless across
// Execute calls; Close is a no-op.
func (w *ExecWorker) Close() error { return nil }

// Execute spawns the child, writes the request, reads the
// response, and maps any failure to a typed [*ParserCrashError].
//
// The request/response wire format is the package's own
// length-prefixed protocol (see [encodeRequest] / [decodeResponse]):
// the isolation package does not depend on proto wire so that
// tests can run without protobuf codegen. A production
// integration would substitute a real proto-backed worker by
// implementing [Worker] directly.
func (w *ExecWorker) Execute(ctx context.Context, req ParseRequest) (*ParseResult, error) {
	cmd := exec.CommandContext(ctx, w.exec.Program, w.exec.Args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=1", ChildEnvVar),
		fmt.Sprintf("%s=%d", ChildMemoryLimitEnvVar, w.cfg.MemoryLimitBytes),
	)
	cmd.Env = append(cmd.Env, w.exec.ExtraEnv...)

	// Stdin: the encoded request.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &ParserCrashError{
			Sentinel:      ErrParserCrash,
			Language:      w.language,
			Path:          req.Path,
			StderrSnippet: fmt.Sprintf("stdin pipe: %v", err),
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, &ParserCrashError{
			Sentinel:      ErrParserCrash,
			Language:      w.language,
			Path:          req.Path,
			StderrSnippet: fmt.Sprintf("start: %v", err),
		}
	}

	// Stream the request into the child's stdin. Closing
	// stdin signals "end of request" so the child can return.
	encErr := encodeRequest(stdin, req)
	_ = stdin.Close()
	if encErr != nil && !errors.Is(encErr, io.ErrClosedPipe) {
		// Wait so the child doesn't become a zombie; we'll
		// still surface the encode error as the cause.
		_ = cmd.Wait()
		return nil, &ParserCrashError{
			Sentinel:      ErrParserCrash,
			Language:      w.language,
			Path:          req.Path,
			ExitCode:      cmd.ProcessState.ExitCode(),
			StderrSnippet: snippet(stderr.String()),
		}
	}

	waitErr := cmd.Wait()
	stderrText := snippet(stderr.String())

	if waitErr != nil {
		exitCode := -1
		signalName := ""
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
			signalName = exitSignalName(cmd.ProcessState)
		}
		sentinel := classifyExitFailure(exitCode, signalName, stderrText)
		return nil, &ParserCrashError{
			Sentinel:      sentinel,
			Language:      w.language,
			Path:          req.Path,
			ExitCode:      exitCode,
			Signal:        signalName,
			StderrSnippet: stderrText,
		}
	}

	res, decErr := decodeResponse(&stdout)
	if decErr != nil {
		return nil, &ParserCrashError{
			Sentinel:      ErrParserCrash,
			Language:      w.language,
			Path:          req.Path,
			ExitCode:      0,
			StderrSnippet: fmt.Sprintf("decode response: %v; stderr=%s", decErr, stderrText),
		}
	}
	return res, nil
}

// classifyExitFailure picks a sentinel for a non-zero exit
// based on exit code, terminating signal, and stderr text.
// Rules:
//
//   - 137 (SIGKILL on Unix) or stderr matches an OOM marker ->
//     [ErrParserOOM]. SIGKILL from a self-applied RLIMIT_AS
//     is the canonical OOM signature.
//   - 139 (SIGSEGV), 134 (SIGABRT) -> [ErrParserCrash].
//   - Stderr contains "out of memory" / "runtime: out of memory" ->
//     [ErrParserOOM] regardless of exit code (Go's runtime panic
//     for OOM exits with code 2).
//   - Anything else with a non-zero exit -> [ErrParserCrash].
func classifyExitFailure(exitCode int, signalName, stderr string) error {
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "out of memory") ||
		strings.Contains(lower, "runtime: out of memory") ||
		strings.Contains(lower, "cannot allocate memory") ||
		strings.Contains(lower, "fatal error: runtime: out of memory") {
		return ErrParserOOM
	}
	switch exitCode {
	case 137: // SIGKILL — most likely OOM-killer or rlimit
		return ErrParserOOM
	}
	switch signalName {
	case "killed", "SIGKILL":
		return ErrParserOOM
	}
	return ErrParserCrash
}

// snippet trims a long stderr to [StderrSnippetMax] bytes for
// inclusion in [*ParserCrashError].
func snippet(s string) string {
	if len(s) <= StderrSnippetMax {
		return s
	}
	return s[:StderrSnippetMax] + "...(truncated)"
}

// --- request/response wire ---
//
// The wire is a tiny binary frame: a 4-byte big-endian length
// followed by the field bytes, repeated for each field. The
// payload fields in order are: language, path, content. The
// response shape is: degraded_reason, ast_file_bytes.
//
// This package-local protocol avoids pulling protobuf into the
// isolation layer and keeps the tests self-contained. Pool
// callers serialise their richer AstFile payload into
// ast_file_bytes upstream.

// encodeRequest writes the request to `w`.
func encodeRequest(w io.Writer, req ParseRequest) error {
	if err := writeFrame(w, []byte(req.Language)); err != nil {
		return err
	}
	if err := writeFrame(w, []byte(req.Path)); err != nil {
		return err
	}
	if err := writeFrame(w, req.Content); err != nil {
		return err
	}
	return nil
}

// decodeRequest reads a request from `r` (used by [RunChild]).
func decodeRequest(r io.Reader) (ParseRequest, error) {
	lang, err := readFrame(r)
	if err != nil {
		return ParseRequest{}, fmt.Errorf("read language: %w", err)
	}
	path, err := readFrame(r)
	if err != nil {
		return ParseRequest{}, fmt.Errorf("read path: %w", err)
	}
	content, err := readFrame(r)
	if err != nil {
		return ParseRequest{}, fmt.Errorf("read content: %w", err)
	}
	return ParseRequest{
		Language: string(lang),
		Path:     string(path),
		Content:  content,
	}, nil
}

// encodeResponse writes a [ParseResult] to `w`. A nil `res`
// is encoded as two zero-length frames so the wire shape is
// invariant: [decodeResponse] always reads exactly two frames
// (degraded_reason, ast_file_bytes). A single-frame nil
// encoding would desync the reader and surface as a confusing
// `decode response: read ast_file_bytes: unexpected EOF`
// wrapped in [ErrParserCrash]. Downstream consumers
// (e.g. [WrapParser]) already treat `len(AstFileBytes) == 0`
// as the "no AST produced" signal, so the round-tripped
// `&ParseResult{AstFileBytes: []byte{}, DegradedReason: ""}`
// is semantically equivalent to the nil the child handler
// returned.
func encodeResponse(w io.Writer, res *ParseResult) error {
	if res == nil {
		if err := writeFrame(w, nil); err != nil {
			return err
		}
		return writeFrame(w, nil)
	}
	if err := writeFrame(w, []byte(res.DegradedReason)); err != nil {
		return err
	}
	if err := writeFrame(w, res.AstFileBytes); err != nil {
		return err
	}
	return nil
}

// decodeResponse reads a [ParseResult] from `r`.
func decodeResponse(r io.Reader) (*ParseResult, error) {
	reason, err := readFrame(r)
	if err != nil {
		return nil, fmt.Errorf("read degraded_reason: %w", err)
	}
	ast, err := readFrame(r)
	if err != nil {
		return nil, fmt.Errorf("read ast_file_bytes: %w", err)
	}
	return &ParseResult{
		AstFileBytes:   ast,
		DegradedReason: string(reason),
	}, nil
}

// writeFrame writes a 4-byte big-endian length prefix + `p`.
func writeFrame(w io.Writer, p []byte) error {
	var lenBuf [4]byte
	n := uint32(len(p))
	lenBuf[0] = byte(n >> 24)
	lenBuf[1] = byte(n >> 16)
	lenBuf[2] = byte(n >> 8)
	lenBuf[3] = byte(n)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	if len(p) > 0 {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads a 4-byte big-endian length prefix + payload.
func readFrame(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := uint32(lenBuf[0])<<24 |
		uint32(lenBuf[1])<<16 |
		uint32(lenBuf[2])<<8 |
		uint32(lenBuf[3])
	if n == 0 {
		return []byte{}, nil
	}
	const maxFrame = 64 << 20 // 64 MiB hard cap per frame
	if n > maxFrame {
		return nil, fmt.Errorf("frame exceeds max %d bytes: got %d", maxFrame, n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	return p, nil
}

// --- child-side entrypoint ---

// ChildHandler is the function the child process runs against
// the decoded request. The host binary's `main()` registers a
// real handler (e.g. an `internal/ast/parser` dispatcher) via
// [RegisterChildHandler] before calling [RunChild].
type ChildHandler func(ctx context.Context, req ParseRequest) (*ParseResult, error)

var (
	childHandlerMu sync.RWMutex
	childHandler   ChildHandler
)

// RegisterChildHandler installs the function the child uses
// to fulfill parse requests. Tests register a fake handler
// that exercises OOM / crash / timeout paths.
func RegisterChildHandler(h ChildHandler) {
	childHandlerMu.Lock()
	defer childHandlerMu.Unlock()
	childHandler = h
}

// childHandlerSnapshot returns the registered handler under a
// read lock.
func childHandlerSnapshot() ChildHandler {
	childHandlerMu.RLock()
	defer childHandlerMu.RUnlock()
	return childHandler
}

// IsChildProcess reports whether the current process was
// invoked as a parser child (via [ChildEnvVar]). The host
// binary's `main()` checks this and calls [RunChild] before
// any normal startup.
func IsChildProcess() bool {
	return os.Getenv(ChildEnvVar) != ""
}

// RunChild is the entrypoint the parser child invokes. It:
//
//  1. Applies the memory cap communicated via
//     [ChildMemoryLimitEnvVar] (Unix: `Setrlimit(RLIMIT_AS, …)`;
//     Windows: documented no-op stub). A failure to install
//     the cap is FATAL: the child writes
//     [ErrChildRlimitFailed] to stderr and exits with code 6
//     so the parent's [classifyExitFailure] does NOT confuse
//     the missing cap with a successful parse.
//  2. Reads the request from stdin.
//  3. Invokes the registered [ChildHandler] (defaults to an
//     error if none registered).
//  4. Writes the response to stdout and exits 0.
//
// On any failure the child writes the error to stderr and
// exits with a non-zero code; the parent observes the exit
// code and maps it to [*ParserCrashError]. RunChild does NOT
// return; it always exits.
func RunChild() {
	// Apply rlimit before anything else so a runaway handler
	// can't outrun the cap. A failure is fatal -- silently
	// continuing without the cap would let an OOM-prone
	// parser exhaust host memory while the parent thinks the
	// cap is in force (evaluator iter-1 item #4).
	if memEnv := os.Getenv(ChildMemoryLimitEnvVar); memEnv != "" {
		if err := applyChildMemoryLimit(memEnv); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", ErrChildRlimitFailed.Error(), err)
			os.Exit(6)
		}
	}

	req, err := decodeRequest(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolation/child: decode request: %v\n", err)
		os.Exit(2)
	}

	handler := childHandlerSnapshot()
	if handler == nil {
		fmt.Fprintln(os.Stderr, "isolation/child: no handler registered")
		os.Exit(3)
	}

	ctx := context.Background()
	res, err := handler(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolation/child: handler: %v\n", err)
		os.Exit(4)
	}

	if err := encodeResponse(os.Stdout, res); err != nil {
		fmt.Fprintf(os.Stderr, "isolation/child: encode response: %v\n", err)
		os.Exit(5)
	}
	os.Exit(0)
}
