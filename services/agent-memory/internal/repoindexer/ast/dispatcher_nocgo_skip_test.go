//go:build !cgo

// dispatcher_nocgo_skip_test.go pins the Stage 7.3
// `Validation — targeted and full service suite` no-CGO
// fallthrough contract: under `CGO_ENABLED=0` the dispatcher's
// `defaultParsers()` only registers the scanner-only PowerShell
// parser (see `parsers_nocgo.go`); every other compiled-language
// extension this story added (`.c`, `.h`, `.cc`, `.cpp`,
// `.cxx`, `.c++`, `.hpp`, `.hh`, `.hxx`, `.h++`, `.cs`, `.go`,
// `.rs`) MUST fall through to the
// `ast.dispatch.skip{reason=no_parser}` branch in `EmitFile`
// (dispatcher.go lines 273-281) WITHOUT panicking, opening the
// file, or touching the writer.
//
// This file is gated `!cgo` for two reasons:
//   1. Under CGO=1 the new parsers ARE registered, so the
//      skip-path assertions would falsely fail (the dispatcher
//      would correctly route .c/.cpp/.cs/.go/.rs to a real
//      parser).
//   2. The mandated `CGO_ENABLED=0 go test
//      ./internal/repoindexer/ast -count=1` command compiles
//      with the default tag set, so this file is the only
//      place where the no-CGO skip-path contract can be pinned
//      without depending on the `canonical_dispatcher` tag
//      that the existing `parsers_nocgo_rust_test.go` requires
//      (and which Forge's test commands do not pass).
//
// Helpers (`fallbackFakeWriter`, `newFallbackEvent`,
// `fallbackStringRC`) live in the untagged
// `dispatcher_fallback_test.go` and are reused here — under
// `!cgo` both files compile together and share the helpers.

package ast

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// nocgoSkipExtensions enumerates every file extension this story
// added a CGO-only parser for (parsers_cgo.go::defaultParsers).
// Under `!cgo` NONE of these is registered, so the dispatcher
// MUST short-circuit each one through the no-parser skip path.
//
// The list MUST stay in lock-step with the cgo-side
// `Extensions()` declarations on:
//   - cTreeSitterParser    (.c, .h)
//   - cppTreeSitterParser  (.cc, .cpp, .cxx, .c++, .hpp, .hh, .hxx, .h++)
//   - csTreeSitterParser   (.cs)
//   - goTreeSitterParser   (.go)
//   - rustTreeSitterParser (.rs)
//
// PowerShell (.ps1, .psm1, .psd1) is NOT included because the
// PowerShell parser is registered under BOTH cgo and !cgo
// (parsers_nocgo.go also registers it), so the skip path does
// not apply to it.
var nocgoSkipExtensions = []string{
	".c", ".h",
	".cc", ".cpp", ".cxx", ".c++", ".hpp", ".hh", ".hxx", ".h++",
	".cs",
	".go",
	".rs",
}

// TestDispatcher_NoCGOSkipsCompiledLanguages pins items #2 + #3
// of the Stage 7.3 evaluator feedback. For every extension this
// story claimed (parser_treesitter_*.go::Extensions), the
// no-CGO dispatcher MUST:
//
//   1. Return `(EmitResult{}, nil)` — no error propagated.
//   2. Emit `ast.dispatch.skip` with `reason=no_parser` on the
//      attached structured logger.
//   3. NOT call `InsertNode` or `InsertEdge` on the writer.
//   4. NOT invoke the `EmitFileEvent.Open` callback.
//   5. NOT panic (covered implicitly — a panic would abort the
//      subtest before the assertions run).
//
// The dispatcher constructs its parser set via
// `defaultParsers()` (no explicit `WithParsers`), so this test
// exercises the same wiring `cmd/repo-indexer` uses in
// production under CGO=0.
func TestDispatcher_NoCGOSkipsCompiledLanguages(t *testing.T) {
	for _, ext := range nocgoSkipExtensions {
		ext := ext
		t.Run("ext="+ext, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			writer := &fallbackFakeWriter{}
			d := NewDispatcher(writer, WithLogger(logger))

			// Confirm the underlying routing decision returns nil
			// — the EmitFile skip path keys off this.
			if p := d.selectParser("src/a"+ext, nil); p != nil {
				t.Fatalf("selectParser(\"src/a%s\", nil) = %T (Language=%q); want nil under !cgo",
					ext, p, p.Language())
			}

			var openCalls int64
			ev := newFallbackEvent("src/a"+ext, "unused-source-bytes", &openCalls)
			res, err := d.EmitFile(context.Background(), ev)
			if err != nil {
				t.Fatalf("EmitFile(%s) returned error; want nil. err=%v", ext, err)
			}
			if len(res.TouchedNodes) != 0 {
				t.Errorf("EmitFile(%s) returned %d TouchedNodes; want 0", ext, len(res.TouchedNodes))
			}
			if len(writer.nodes) != 0 {
				t.Errorf("writer.InsertNode called %d times for %s under !cgo; want 0",
					len(writer.nodes), ext)
			}
			if len(writer.edges) != 0 {
				t.Errorf("writer.InsertEdge called %d times for %s under !cgo; want 0",
					len(writer.edges), ext)
			}
			if n := atomic.LoadInt64(&openCalls); n != 0 {
				t.Errorf("EmitFileEvent.Open invoked %d times for %s under !cgo; want 0 "+
					"(skip must short-circuit before opening the file)", n, ext)
			}

			got := logBuf.String()
			if !strings.Contains(got, "ast.dispatch.skip") {
				t.Errorf("log output missing canonical skip event %q for %s; got=%q",
					"ast.dispatch.skip", ext, got)
			}
			if !strings.Contains(got, "reason=no_parser") {
				t.Errorf("log output missing %q attribute for %s; got=%q",
					"reason=no_parser", ext, got)
			}
		})
	}
}

// TestDispatcher_NoCGODefaultParsersOmitsCompiledLanguages pins
// item #2 of the Stage 7.3 evaluator feedback at the
// `defaultParsers()` level: under `!cgo` the parser set MUST
// NOT include any parser whose `Extensions()` claims a
// compiled-language extension (`.c`, `.cpp`, `.cs`, `.go`,
// `.rs`, ...). This is the contract `parsers_nocgo.go` enforces
// by only registering `NewPowerShellParser()`.
//
// This test ports the intent of the
// `canonical_dispatcher`-gated `TestDefaultParsers_NoCGOOmitsRust`
// in `parsers_nocgo_rust_test.go` into the default !cgo test
// set (per the evaluator's "move no-CGO skip coverage into the
// default !cgo test set" directive) and broadens it to every
// compiled-language extension this story touched.
func TestDispatcher_NoCGODefaultParsersOmitsCompiledLanguages(t *testing.T) {
	got := DefaultParsers()
	if len(got) == 0 {
		t.Fatalf("DefaultParsers() returned empty slice under !cgo; want at least PowerShell")
	}

	// Build the inverse map: extension → registered language.
	// Used both to assert NO compiled-language extension is
	// claimed AND to print a helpful failure message naming the
	// offending parser if the contract regresses.
	registered := map[string]string{}
	for _, p := range got {
		for _, ext := range p.Extensions() {
			registered[ext] = p.Language()
		}
	}

	for _, ext := range nocgoSkipExtensions {
		if lang, ok := registered[ext]; ok {
			t.Errorf("DefaultParsers() registers extension %q under !cgo (language=%q); "+
				"want NO registration — these extensions are CGO-only",
				ext, lang)
		}
	}

	// Confirm PowerShell IS still registered — its `.ps1`,
	// `.psm1`, `.psd1` claims are the only extensions
	// parsers_nocgo.go::defaultParsers should produce.
	psFound := false
	for _, p := range got {
		if p.Language() == "powershell" {
			psFound = true
			break
		}
	}
	if !psFound {
		t.Errorf("DefaultParsers() under !cgo missing PowerShell parser; "+
			"parsers_nocgo.go contract is to register exactly PowerShell. got=%v",
			got)
	}
}

// TestDispatcher_NoCGOLanguageHintsDoNotForceCompiledLanguageRouting
// pins the negative-hint contract from the architecture's hint
// rule (architecture §4.1 + `selectParser` doc): a language
// hint can promote an unknown extension to a registered parser,
// but it CANNOT synthesise a parser that wasn't registered at
// all. Under !cgo, a `cpp` / `rust` / `go` / `csharp` hint on
// an unknown extension MUST still produce the no_parser skip
// (the hint table maps to a Language that has no
// corresponding registered parser).
//
// Guards against a regression where the hint fallback silently
// dispatches to a nil parser or panics.
func TestDispatcher_NoCGOLanguageHintsDoNotForceCompiledLanguageRouting(t *testing.T) {
	cases := []struct {
		name string
		hint string
	}{
		{name: "cpp hint without registered parser", hint: "cpp"},
		{name: "rust hint without registered parser", hint: "rust"},
		{name: "go hint without registered parser", hint: "go"},
		{name: "csharp hint without registered parser", hint: "csharp"},
		{name: "c hint without registered parser", hint: "c"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher(&fallbackFakeWriter{})
			if p := d.selectParser("src/a.unknown", []string{tc.hint}); p != nil {
				t.Errorf("selectParser(\"src/a.unknown\", [%q]) = %T (Language=%q); "+
					"want nil under !cgo (hint cannot synthesise an unregistered parser)",
					tc.hint, p, p.Language())
			}
		})
	}
}
