package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
)

// InProcessWorkerFactory returns a [WorkerFactory] that, instead
// of spawning a subprocess, runs the supplied
// `parser.Registry.For(language)` parser INSIDE the host
// process but with two safety nets:
//
//  1. A `recover()` around the parser call so a panic in the
//     parser does NOT propagate up; instead the worker returns
//     a [*ParserCrashError] with [ErrParserCrash].
//  2. The per-call [SubprocessConfig.Timeout] enforced by the
//     supplied `ctx`.
//
// This is the "in-pod, in-thread" fallback used in
// environments where a real subprocess pool is not viable
// (Windows dev box, CI without exec privileges). The
// architecture (Sec 9.2) flags this as a degraded mode --
// production deployments wire [NewExecWorker] for true
// crash isolation.
//
// The factory implements the [WorkerFactory] shape, so callers
// register it directly into [Pool.RegisterFactory]:
//
//	pool.RegisterFactory(parser.LanguageGo,
//	    isolation.InProcessWorkerFactory(parser.DefaultRegistry()))
func InProcessWorkerFactory(registry *parser.Registry) WorkerFactory {
	return func(language string, _ SubprocessConfig) (Worker, error) {
		p, err := registry.For(language)
		if err != nil {
			return nil, fmt.Errorf("isolation: InProcessWorkerFactory(%q): %w", language, err)
		}
		return &inProcessWorker{language: language, parser: p}, nil
	}
}

// inProcessWorker is the [Worker] returned by
// [InProcessWorkerFactory]. It hosts the parser in-process
// behind a panic-safe goroutine; the goroutine writes the
// outcome to a channel the Execute call selects on alongside
// `ctx.Done()`. This pattern preserves the contract that
// Execute returns promptly on ctx-cancel even if the parser
// is mid-call (the parser goroutine may still finish in the
// background; for in-process Go parsers this is harmless
// because they read `ctx.Err()` themselves -- see Stage 2.1
// parser implementations).
type inProcessWorker struct {
	language string
	parser   parser.Parser
}

// Language implements [Worker].
func (w *inProcessWorker) Language() string { return w.language }

// Close implements [Worker]. The parser owns no long-lived
// resources at this layer; nothing to do.
func (w *inProcessWorker) Close() error { return nil }

// Execute runs the parser on the request, recovering from
// panics into [ErrParserCrash]. On success the AstFile is
// serialised to JSON for transport over the wire shape; the
// caller deserialises upstream if needed.
func (w *inProcessWorker) Execute(ctx context.Context, req ParseRequest) (*ParseResult, error) {
	type outcome struct {
		res *ParseResult
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Convert any parser panic into a typed
				// ParserCrashError. The host process stays
				// up because the panic is contained by this
				// goroutine's defer.
				out <- outcome{
					err: &ParserCrashError{
						Sentinel:      ErrParserCrash,
						Language:      w.language,
						Path:          req.Path,
						ExitCode:      -1,
						Signal:        "panic",
						StderrSnippet: fmt.Sprintf("in-process parser panic: %v", r),
					},
				}
			}
		}()
		ast, err := w.parser.Parse(ctx, req.Path, req.Content)
		if err != nil {
			out <- outcome{err: err}
			return
		}
		// Serialise the AstFile to JSON. The brief doesn't
		// pin a wire shape; JSON keeps the test path
		// dependency-free.
		buf := &bytes.Buffer{}
		if ast != nil {
			if err := json.NewEncoder(buf).Encode(ast); err != nil {
				out <- outcome{err: fmt.Errorf("isolation: inProcessWorker: encode AstFile: %w", err)}
				return
			}
		}
		out <- outcome{
			res: &ParseResult{
				AstFileBytes:   buf.Bytes(),
				DegradedReason: ast.GetDegradedReason(),
			},
		}
	}()

	select {
	case o := <-out:
		return o.res, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WrapParser returns a [parser.Parser] facade that routes
// every `Parse` call through the supplied [Pool] so the
// caller benefits from the drain-on-flip contract and the
// per-language worker pool without changing its parse-call
// shape.
//
// `repoID` is the repo the parser's calls belong to; it is
// passed into [Pool.Parse] so the [ModeCoordinator] can scope
// the drain correctly. Callers that parse across multiple
// repos should call WrapParser once per repo or pass repoID
// at a higher layer.
//
// The returned parser's `Parse` returns the same `*parser.AstFile`
// the underlying parser would have returned, decoded from the
// pool's [ParseResult.AstFileBytes]. Errors are passed through
// verbatim (typed [*ParserCrashError] / `context.Canceled` /
// `context.DeadlineExceeded`).
func WrapParser(pool *Pool, repoID uuid.UUID, p parser.Parser) parser.Parser {
	return &wrappedParser{pool: pool, repoID: repoID, inner: p}
}

type wrappedParser struct {
	pool   *Pool
	repoID uuid.UUID
	inner  parser.Parser
}

func (w *wrappedParser) Language() string { return w.inner.Language() }

func (w *wrappedParser) Parse(ctx context.Context, path string, content []byte) (*parser.AstFile, error) {
	res, err := w.pool.Parse(ctx, w.repoID, ParseRequest{
		Language: w.inner.Language(),
		Path:     path,
		Content:  content,
	})
	if err != nil {
		return nil, err
	}
	if len(res.AstFileBytes) == 0 {
		return nil, nil
	}
	var ast parser.AstFile
	if err := json.Unmarshal(res.AstFileBytes, &ast); err != nil {
		return nil, fmt.Errorf("isolation: WrapParser.Parse: decode AstFile: %w", err)
	}
	return &ast, nil
}

// ExecWorkerFactoryFromConfig returns a [WorkerFactory] that
// spawns the host binary as a parser child via [ExecWorker]
// (true subprocess isolation per architecture Sec 9.2). The
// supplied `executablePath` MUST be the host binary's own
// path (typically `os.Args[0]` or `os.Executable()`); the
// child re-enters via the [IsChildProcess] guard in `main()`.
//
// Wire this into a [Pool] in production for crash isolation:
//
//	exe, err := os.Executable()
//	if err != nil { return fmt.Errorf("os.Executable: %w", err) }
//	for _, lang := range parser.SupportedLanguages {
//	    pool.RegisterFactory(lang,
//	        isolation.ExecWorkerFactoryFromConfig(exe))
//	}
//
// The host's `main()` MUST also call
// [ParserRegistryChildHandler]+[RegisterChildHandler]+[RunChild]
// under the [IsChildProcess] guard so the spawned children
// know how to fulfil parse requests.
func ExecWorkerFactoryFromConfig(executablePath string) WorkerFactory {
	return func(language string, cfg SubprocessConfig) (Worker, error) {
		return NewExecWorker(language, cfg, ExecConfig{Program: executablePath})
	}
}

// ParserRegistryChildHandler returns a [ChildHandler] that
// dispatches every parse request to `registry.For(req.Language).Parse(...)`
// and serialises the resulting [parser.AstFile] to JSON in the
// [ParseResult.AstFileBytes] field (matching the wire shape
// that [WrapParser] decodes upstream). Register via
// [RegisterChildHandler] before calling [RunChild] from the
// host binary's `main()` IsChildProcess guard:
//
//	if isolation.IsChildProcess() {
//	    isolation.RegisterChildHandler(
//	        isolation.ParserRegistryChildHandler(parser.DefaultRegistry()))
//	    isolation.RunChild()
//	}
//
// The handler enforces a small input contract:
//
//   - Empty Language is rejected (the parent always sets it).
//   - Unknown languages return the parser package's
//     `ErrUnsupportedLanguage` wrapped error, which the parent
//     can `errors.Is` on.
//   - A nil AstFile from the parser is returned with empty
//     bytes (the upstream WrapParser maps that to nil).
//
// Panics in the underlying parser are NOT recovered here -- a
// panic in the CHILD process is the correct behaviour: the
// child crashes, the parent observes a non-zero exit, and
// [classifyExitFailure] returns [ErrParserCrash]. This is the
// crash-isolation contract the brief mandates.
func ParserRegistryChildHandler(registry *parser.Registry) ChildHandler {
	return func(ctx context.Context, req ParseRequest) (*ParseResult, error) {
		if req.Language == "" {
			return nil, fmt.Errorf("isolation/child: ParserRegistryChildHandler: empty Language in request")
		}
		p, err := registry.For(req.Language)
		if err != nil {
			return nil, fmt.Errorf("isolation/child: registry.For(%q): %w", req.Language, err)
		}
		ast, err := p.Parse(ctx, req.Path, req.Content)
		if err != nil {
			return nil, err
		}
		if ast == nil {
			return &ParseResult{AstFileBytes: nil}, nil
		}
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(ast); err != nil {
			return nil, fmt.Errorf("isolation/child: encode AstFile: %w", err)
		}
		return &ParseResult{
			AstFileBytes:   buf.Bytes(),
			DegradedReason: ast.GetDegradedReason(),
		}, nil
	}
}
