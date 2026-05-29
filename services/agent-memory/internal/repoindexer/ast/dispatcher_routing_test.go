//go:build cgo && !canonical_dispatcher

// dispatcher_routing_test.go pins the Stage 7.1 cross-language
// dispatcher routing contract from
// `docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/implementation-plan.md`.
//
// The three tests exercise `*Dispatcher.selectParser` (the
// single-source-of-truth routing decision the dispatcher's
// `EmitFile` calls before opening the file) plus the
// `ast.dispatch.skip{reason=no_parser}` slog branch in `EmitFile`.
//
// Build-tag rationale: the helpers, fakes, and the legacy
// `TestDispatcher_RoutesByExtension` in `dispatcher_test.go`
// live under `//go:build canonical_dispatcher` (a tag the
// default `make test` path does NOT pass). This file is gated
// with `cgo && !canonical_dispatcher` so the two TestDispatcher_*
// names never coexist in one binary: under default build the new
// tests compile and the legacy ones are excluded; under
// `-tags canonical_dispatcher` the legacy tests compile and the
// new ones are excluded. The `cgo` half of the tag is required
// because the dispatcher's default parser set (`defaultParsers`
// in `parsers_cgo.go`) only registers C, C++, C#, Go, Rust, and
// PowerShell when CGO is on; under `!cgo` the routing table is
// empty for every extension this test asserts on.

package ast

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// routingFakeWriter is a counting `NodeEdgeWriter` used by the
// `EmitFile` path of `TestDispatcher_NoParserForUnknown`. The
// no-parser branch must NOT touch the writer; the test asserts
// both counters stay at zero after `EmitFile` returns.
//
// Named uniquely (vs the cgo-tagged `cFakeWriter`,
// `goFakeWriter`, `rustStage53Writer` in this package) so the
// default `go test ./internal/repoindexer/ast/...` build does
// not produce a duplicate-symbol error.
type routingFakeWriter struct {
	nodeCalls int64
	edgeCalls int64
}

func (w *routingFakeWriter) InsertNode(_ context.Context, _ graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	atomic.AddInt64(&w.nodeCalls, 1)
	return graphwriter.NodeRecord{}, errors.New("routingFakeWriter: InsertNode must not be called for unknown extension")
}

func (w *routingFakeWriter) InsertEdge(_ context.Context, _ graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	atomic.AddInt64(&w.edgeCalls, 1)
	return graphwriter.EdgeRecord{}, errors.New("routingFakeWriter: InsertEdge must not be called for unknown extension")
}

// routingStringRC wraps a string as a `repoindexer.ReadCloser`
// for `EmitFileEvent.Open`. Unique name vs the canonical_dispatcher
// `stringReadCloser` so the two never collide under any tag set.
type routingStringRC struct {
	r *strings.Reader
}

func newRoutingStringRC(s string) *routingStringRC {
	return &routingStringRC{r: strings.NewReader(s)}
}

func (s *routingStringRC) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *routingStringRC) Close() error               { return nil }

// makeRoutingEvent builds a minimal `repoindexer.EmitFileEvent`
// whose `Open` callback increments `openCalls` each invocation.
// Callers pass `openCalls` so the test can assert the dispatcher
// short-circuited BEFORE opening the file (the `selectParser`
// return-nil branch in `EmitFile`).
func makeRoutingEvent(relPath, src string, openCalls *int64) repoindexer.EmitFileEvent {
	return repoindexer.EmitFileEvent{
		RepoID:     fingerprint.MustParseRepoID("11111111-2222-3333-4444-555555555555"),
		RepoURL:    "https://git.example/acme/svc",
		SHA:        "shaABC",
		FileNodeID: "file-node-id",
		RelPath:    relPath,
		Open: func() (repoindexer.ReadCloser, error) {
			if openCalls != nil {
				atomic.AddInt64(openCalls, 1)
			}
			return newRoutingStringRC(src), nil
		},
	}
}

// TestDispatcher_RoutesByExtension pins the cross-language
// extension→parser mapping for the v1 fleet (C, C++, C#, Go,
// Rust, PowerShell) listed in `parsers_cgo.go::defaultParsers`.
//
// The assertion runs against `*Dispatcher.selectParser` directly
// (rather than the full `EmitFile` pipeline) so the routing
// decision is exercised independently of parser execution. The
// `.h` case is intentionally included here AND in a dedicated
// pin (`TestDispatcher_DotHRoutesToC_EvenWithCppHint`) — this
// table guards the no-hint default, the dedicated test guards
// the dot-h-extension-routing pin against the C++ hint regression.
func TestDispatcher_RoutesByExtension(t *testing.T) {
	cases := []struct {
		name     string
		relPath  string
		wantLang string
	}{
		{name: "dot-c routes to C", relPath: "src/foo.c", wantLang: "c"},
		{name: "dot-h routes to C (default)", relPath: "include/foo.h", wantLang: "c"},
		{name: "dot-cpp routes to C++", relPath: "src/foo.cpp", wantLang: "cpp"},
		{name: "dot-cs routes to C#", relPath: "src/foo.cs", wantLang: "csharp"},
		{name: "dot-go routes to Go", relPath: "src/foo.go", wantLang: "go"},
		{name: "dot-rs routes to Rust", relPath: "src/foo.rs", wantLang: "rust"},
		{name: "dot-ps1 routes to PowerShell", relPath: "scripts/foo.ps1", wantLang: "powershell"},
		{name: "dot-psm1 routes to PowerShell", relPath: "modules/foo.psm1", wantLang: "powershell"},
	}
	d := NewDispatcher(&routingFakeWriter{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := d.selectParser(tc.relPath, nil)
			if p == nil {
				t.Fatalf("selectParser(%q, nil) = nil; want parser with Language()=%q", tc.relPath, tc.wantLang)
			}
			if got := p.Language(); got != tc.wantLang {
				t.Errorf("selectParser(%q, nil).Language() = %q; want %q", tc.relPath, got, tc.wantLang)
			}
		})
	}
}

// TestDispatcher_DotHRoutesToC_EvenWithCppHint pins the
// `dot-h-extension-routing` decision (architecture §4.1 + the
// `parser_treesitter_c.go` comment block): a `.h` file MUST
// route to the C parser even when the caller supplies a `cpp`
// language hint. The hint is a tie-breaker for unknown
// extensions, not an override of an extension that IS registered.
//
// The C parser claims `.c` and `.h`; the C++ parser deliberately
// omits `.h` from its `Extensions()`. Because `.h` is already
// in `extMap`, `selectParser` returns the C parser on the
// extension-wins path and never consults the hint list.
func TestDispatcher_DotHRoutesToC_EvenWithCppHint(t *testing.T) {
	d := NewDispatcher(&routingFakeWriter{})
	p := d.selectParser("include/a.h", []string{"cpp"})
	if p == nil {
		t.Fatalf("selectParser(\"include/a.h\", [\"cpp\"]) = nil; want C parser (extension wins over hint)")
	}
	if got := p.Language(); got != "c" {
		t.Errorf("selectParser(\"include/a.h\", [\"cpp\"]).Language() = %q; want %q (dot-h-extension-routing pin)", got, "c")
	}
}

// TestDispatcher_NoParserForUnknown pins the unknown-extension
// fallthrough contract in two halves: the routing-decision half
// asserts `selectParser` returns nil (the input the `EmitFile`
// skip branch keys off), and the dispatcher-side half asserts
// the skip emits the canonical `ast.dispatch.skip{reason=no_parser}`
// slog event AND short-circuits before touching the writer or
// invoking the event's `Open` callback.
//
// The two subtests share an unknown extension (`.foo`) so the
// pin is unambiguous if anyone later adds `.foo` to a parser's
// `Extensions()`.
func TestDispatcher_NoParserForUnknown(t *testing.T) {
	t.Run("selectParser returns nil", func(t *testing.T) {
		d := NewDispatcher(&routingFakeWriter{})
		if p := d.selectParser("src/x.foo", nil); p != nil {
			t.Fatalf("selectParser(\"src/x.foo\", nil) = %T (Language=%q); want nil", p, p.Language())
		}
	})

	t.Run("EmitFile logs skip and writes nothing", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		writer := &routingFakeWriter{}
		d := NewDispatcher(writer, WithLogger(logger))

		var openCalls int64
		ev := makeRoutingEvent("src/x.foo", "unused", &openCalls)
		res, err := d.EmitFile(context.Background(), ev)
		if err != nil {
			t.Fatalf("EmitFile returned error for unknown extension; want nil. err=%v", err)
		}
		if len(res.TouchedNodes) != 0 {
			t.Errorf("EmitFile returned %d TouchedNodes for unknown extension; want 0", len(res.TouchedNodes))
		}

		got := logBuf.String()
		if !strings.Contains(got, "ast.dispatch.skip") {
			t.Errorf("log output missing canonical skip event %q; got=%q", "ast.dispatch.skip", got)
		}
		if !strings.Contains(got, "reason=no_parser") {
			t.Errorf("log output missing %q attribute; got=%q", "reason=no_parser", got)
		}

		if n := atomic.LoadInt64(&writer.nodeCalls); n != 0 {
			t.Errorf("writer.InsertNode called %d times for unknown extension; want 0", n)
		}
		if n := atomic.LoadInt64(&writer.edgeCalls); n != 0 {
			t.Errorf("writer.InsertEdge called %d times for unknown extension; want 0", n)
		}
		if n := atomic.LoadInt64(&openCalls); n != 0 {
			t.Errorf("EmitFileEvent.Open invoked %d times for unknown extension; want 0 (skip must short-circuit before opening the file)", n)
		}
	})
}
