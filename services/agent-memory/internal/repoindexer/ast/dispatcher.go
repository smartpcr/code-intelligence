// Package ast — minimal canonical Dispatcher.
//
// This file ships the v1 surface the `cmd/repoindexer/main.go`
// wiring already calls (`NewDispatcher(writer, opts...)`,
// `WithEmbeddingPublisher`) plus the option helpers the Stage
// 3.2 dispatcher-landing workstream's tests assume
// (`WithLanguageHints`, `WithParsers`, `WithLogger`). The
// node/edge emission pipeline that pinned tests under
// `//go:build canonical_dispatcher` exercise (extends /
// implements / static_calls / contains / imports / reads /
// writes edges, block subdivision, multimap collision rules,
// trait/override resolution, embedding publish ordering)
// remains the responsibility of the Stage 3.2 dispatcher-
// landing workstream and is intentionally NOT implemented
// here -- this file is the minimum surface that (a) keeps the
// service binary compiling, (b) registers the default parser
// set returned by `defaultParsers()` (CGO-on includes the
// tree-sitter Go parser from parser_treesitter_go.go), and
// (c) routes EmitFile by file extension so a parser-routing
// test can prove `.go` reaches `goTreeSitterParser` end-to-
// end through the same code path production uses.
//
// When the Stage 3.2 dispatcher-landing workstream lands the
// full pipeline, it should swap THIS file's body in place
// (preserving the public surface) and unset the
// `//go:build canonical_dispatcher` tag on the test files
// that already encode the full contract.
package ast

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// nodeEdgeWriter is the small consumer-side interface the
// dispatcher uses to write Nodes/Edges. `*graphwriter.Writer`
// satisfies it in production wiring; tests inject a
// `*fakeNodeEdgeWriter` that captures calls without touching
// PostgreSQL (see doc.go "Test-only seam"). The interface is
// declared on the consumer side so the ast package does not
// import graphwriter directly -- a future test landing the
// full canonical dispatcher will add the captured Insert
// signatures here without touching production wiring.
type nodeEdgeWriter interface{}

// Dispatcher routes EmitFile calls to the LanguageParser whose
// `Extensions()` claim the file's suffix and orchestrates the
// resulting Class / Method / Block Nodes plus static Edges
// through `nodeEdgeWriter`. See doc.go for the architectural
// contract.
type Dispatcher struct {
	writer    nodeEdgeWriter
	parsers   map[string]LanguageParser
	hints     []string
	logger    *slog.Logger
	publisher NodeEmbeddingPublisher
	mu        sync.Mutex
}

// DispatcherOption configures a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithParsers replaces the default parser set returned by
// `defaultParsers()`. Useful in tests that want to drive a
// specific scenario (e.g. a `panickingParser` or a
// `fakeStaticParser` that returns a canned ParseResult)
// without dragging in the full v1 parser surface.
func WithParsers(parsers ...LanguageParser) DispatcherOption {
	return func(d *Dispatcher) {
		d.parsers = map[string]LanguageParser{}
		for _, p := range parsers {
			for _, ext := range p.Extensions() {
				d.parsers[strings.ToLower(ext)] = p
			}
		}
	}
}

// WithLanguageHints supplies the dispatcher-global hint
// fallback for files whose extension does not map to a
// registered parser. Per-event hints
// (`EmitFileEvent.LanguageHints`) always win when both are
// set; see doc.go "Per-event language hints".
func WithLanguageHints(hints []string) DispatcherOption {
	return func(d *Dispatcher) {
		d.hints = append([]string{}, hints...)
	}
}

// WithLogger sets the structured logger. The default is
// `slog.Default()`.
func WithLogger(l *slog.Logger) DispatcherOption {
	return func(d *Dispatcher) { d.logger = l }
}

// WithEmbeddingPublisher wires the optional embedding
// publisher that receives Method / Block emissions for
// downstream fan-out to the EmbeddingIndex writer. The
// default is a no-op publisher so existing callers that do
// not configure embeddings compile and run unchanged.
func WithEmbeddingPublisher(p NodeEmbeddingPublisher) DispatcherOption {
	return func(d *Dispatcher) { d.publisher = p }
}

// NewDispatcher constructs a Dispatcher with the default
// parser set returned by `defaultParsers()` routed by file
// extension (lower-case). Panics if writer is nil --
// production wiring always supplies `graphwriter.New(...)`
// and a nil writer indicates a misconfigured DI graph rather
// than a recoverable user error.
func NewDispatcher(writer nodeEdgeWriter, opts ...DispatcherOption) *Dispatcher {
	if writer == nil {
		panic("ast.NewDispatcher: writer must not be nil")
	}
	d := &Dispatcher{
		writer:    writer,
		parsers:   map[string]LanguageParser{},
		publisher: noopNodeEmbeddingPublisher{},
	}
	for _, p := range defaultParsers() {
		for _, ext := range p.Extensions() {
			d.parsers[strings.ToLower(ext)] = p
		}
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// EmitFile is the v1 minimal pass-through: it picks a parser
// by file extension (falling back to language hints when no
// extension matches), opens and reads the file via
// `ev.Open()`, hands the bytes to the parser, and returns an
// empty `repoindexer.EmitResult`. Production node/edge
// emission (and the Stage 3.4 TouchedNodes population) is the
// responsibility of the Stage 3.2 dispatcher-landing
// workstream. Errors:
//
//   - `ev.Open()` failure is wrapped and returned (the
//     surrounding worker marks the ingest failed).
//   - Parser panics are recovered and logged at error level;
//     the call returns nil so one malformed file does NOT
//     abort the ingest.
//   - `ErrParserUnavailable` is treated as an info-level
//     skip event (parser cannot run because a required
//     runtime dependency is missing) and is NOT propagated.
//   - Other parser errors are logged at warn level and
//     swallowed (the parser already did partial extraction).
//
// Unknown extensions with no language-hint match return
// `(EmitResult{}, nil)` silently -- the contract documented on
// `repoindexer.NoopASTEmitter.EmitFile`.
func (d *Dispatcher) EmitFile(ctx context.Context, ev repoindexer.EmitFileEvent) (repoindexer.EmitResult, error) {
	parser := d.pickParser(ev.RelPath, ev.LanguageHints)
	if parser == nil {
		d.log().Debug("ast.dispatch.skip",
			slog.String("rel_path", ev.RelPath),
			slog.String("reason", "no_parser"),
		)
		return repoindexer.EmitResult{}, nil
	}
	rc, err := ev.Open()
	if err != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast.dispatcher: open %q: %w", ev.RelPath, err)
	}
	defer func() { _ = rc.Close() }()
	src, err := io.ReadAll(rc)
	if err != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast.dispatcher: read %q: %w", ev.RelPath, err)
	}
	if _, err := d.safeParse(parser, ev.RelPath, src); err != nil {
		return repoindexer.EmitResult{}, nil
	}
	// Stage 3.2 dispatcher-landing workstream lands the
	// node/edge emission pipeline that fills TouchedNodes.
	// The v1 surface returns an empty result so the worker
	// continues without partial-progress noise.
	_ = ctx
	return repoindexer.EmitResult{}, nil
}

// safeParse wraps `parser.Parse` with a panic recovery and the
// `ErrParserUnavailable` classification documented on
// `parser.go::ErrParserUnavailable`. The contract matches the
// pinning in `dispatcher_pass2bd_test.go` (gated behind
// `//go:build canonical_dispatcher` until the Stage 3.2
// landing workstream lands the full pipeline).
func (d *Dispatcher) safeParse(parser LanguageParser, relPath string, src []byte) (res ParseResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			d.log().Error("ast.parse.panic",
				slog.String("rel_path", relPath),
				slog.String("language", parser.Language()),
				slog.Any("panic", r),
			)
			err = fmt.Errorf("ast.dispatcher: parser panic for %q: %v", relPath, r)
		}
	}()
	res, err = parser.Parse(relPath, src)
	if err != nil {
		// `ErrParserUnavailable` is a SKIP, not a parse
		// failure -- match the wrap convention documented
		// on parser.go.
		if isParserUnavailable(err) {
			d.log().Info("ast.dispatch.skip",
				slog.String("rel_path", relPath),
				slog.String("language", parser.Language()),
				slog.String("reason", extractReasonSlug(err)),
			)
			return ParseResult{}, err
		}
		d.log().Warn("ast.parse.error",
			slog.String("rel_path", relPath),
			slog.String("language", parser.Language()),
			slog.String("error", err.Error()),
		)
		return ParseResult{}, err
	}
	return res, nil
}

// pickParser selects the LanguageParser for relPath. Extension
// match wins; the hint fallback (event hints first, then
// dispatcher-global hints) picks the FIRST registered parser
// whose `Language()` matches a hint slug.
func (d *Dispatcher) pickParser(relPath string, eventHints []string) LanguageParser {
	ext := strings.ToLower(path.Ext(relPath))
	if p, ok := d.parsers[ext]; ok {
		return p
	}
	hints := eventHints
	if len(hints) == 0 {
		hints = d.hints
	}
	for _, lang := range hints {
		for _, p := range d.parsers {
			if p.Language() == lang {
				return p
			}
		}
	}
	return nil
}

// log returns the dispatcher's logger or slog.Default().
func (d *Dispatcher) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// isParserUnavailable reports whether err wraps
// `ErrParserUnavailable`.
func isParserUnavailable(err error) bool {
	for cur := err; cur != nil; {
		if cur == ErrParserUnavailable {
			return true
		}
		u, ok := cur.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}

// extractReasonSlug pulls the `(reason=<slug>)` annotation
// from the wrapped error string per the parser.go convention.
// Falls back to `runtime_unavailable` when the wrapper did
// not embed a slug.
func extractReasonSlug(err error) string {
	if err == nil {
		return "runtime_unavailable"
	}
	msg := err.Error()
	idx := strings.Index(msg, "(reason=")
	if idx < 0 {
		return "runtime_unavailable"
	}
	tail := msg[idx+len("(reason="):]
	end := strings.IndexByte(tail, ')')
	if end < 0 {
		return "runtime_unavailable"
	}
	slug := tail[:end]
	if slug == "" {
		return "runtime_unavailable"
	}
	return slug
}

// dispatcherParsersForTest exposes the dispatcher's internal
// extension→parser map for tests in the same package. It
// returns a defensive copy so a caller mutating the map
// cannot affect dispatcher state.
func (d *Dispatcher) dispatcherParsersForTest() map[string]LanguageParser {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]LanguageParser, len(d.parsers))
	for k, v := range d.parsers {
		out[k] = v
	}
	return out
}
