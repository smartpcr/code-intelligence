// dispatcher_fallback_test.go pins three Stage 7.3 contracts the
// `Validation — targeted and full service suite` workstream must
// keep green under BOTH `CGO_ENABLED=1` and `CGO_ENABLED=0`:
//
//  1. The TypeScript scanner-based parser (`tsjsParser`) emits
//     `language=typescript` class + method nodes through the
//     dispatcher even when CGO is OFF (Stage 3.2 contract from
//     parser_typescript.go).
//  2. The Python scanner-based parser (`pythonParser`) emits
//     `language=python` class + method nodes through the
//     dispatcher even when CGO is OFF (Stage 3.2 contract from
//     parser_python.go).
//  3. Duplicate parser registration for the same file extension
//     is DETERMINISTIC: the last parser supplied to `WithParsers`
//     wins (Stage 7.1 story requirement #7 + the
//     `parsers_cgo.go` comment block lines 8-13 that pins the
//     C-before-C++ ordering rule).
//
// Build-tag rationale: this file has NO build tags so it runs
// under every `go test ./internal/repoindexer/ast` invocation the
// evaluator runs (CGO=1 AND CGO=0). The legacy
// `parser_typescript_test.go` / `parser_python_test.go` are
// gated behind `canonical_dispatcher` and therefore do not run
// under the mandated `CGO_ENABLED=0 go test ./internal/repoindexer/ast`
// command — this file closes that gap by re-pinning the
// dispatcher-level routing contract for both parsers with
// uniquely-named helpers (`fallbackFakeWriter`, `fallbackStringRC`,
// `newFallbackEvent`) so no symbols collide with the canonical
// `fakeWriter` / `stringReadCloser` / `makeEvent` in
// `dispatcher_test.go` regardless of tag set.

package ast

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// =====================================================================
// Shared test helpers (also consumed by dispatcher_nocgo_skip_test.go)
// =====================================================================

// fallbackFakeWriter is a capturing `NodeEdgeWriter` that records
// every InsertNode / InsertEdge call. The dispatcher-routing
// tests inspect `nodes` / `edges` to assert what the parser
// pipeline produced; the no-parser skip tests assert both slices
// stay empty.
//
// Uniquely named vs the canonical_dispatcher-gated `fakeWriter`
// (dispatcher_test.go), the cgo-tagged `routingFakeWriter`
// (dispatcher_routing_test.go), and the per-language fakes (e.g.
// `cFakeWriter`, `goFakeWriter`, `rustStage53Writer`) so this
// file compiles under every tag combination without producing a
// duplicate-symbol error.
type fallbackFakeWriter struct {
	nodes []graphwriter.NodeInput
	edges []graphwriter.EdgeInput

	failOnNode error
	failOnEdge error
}

func (w *fallbackFakeWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	if w.failOnNode != nil {
		return graphwriter.NodeRecord{}, w.failOnNode
	}
	id := "node-" + fallbackItoa(len(w.nodes))
	w.nodes = append(w.nodes, in)
	fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: true}, nil
}

func (w *fallbackFakeWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	if w.failOnEdge != nil {
		return graphwriter.EdgeRecord{}, w.failOnEdge
	}
	id := "edge-" + fallbackItoa(len(w.edges))
	w.edges = append(w.edges, in)
	return graphwriter.EdgeRecord{EdgeID: id, Inserted: true}, nil
}

// nodesOfKind returns every captured Node whose Kind matches.
func (w *fallbackFakeWriter) nodesOfKind(kind string) []graphwriter.NodeInput {
	var out []graphwriter.NodeInput
	for _, n := range w.nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// fallbackStringRC wraps a string as a `repoindexer.ReadCloser`.
type fallbackStringRC struct{ r *strings.Reader }

func newFallbackStringRC(s string) *fallbackStringRC {
	return &fallbackStringRC{r: strings.NewReader(s)}
}

func (s *fallbackStringRC) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *fallbackStringRC) Close() error               { return nil }

// newFallbackEvent constructs an EmitFileEvent whose Open
// callback increments `*openCalls` (when non-nil) and returns a
// reader over `src`. Tests that assert the dispatcher's
// no-parser skip path passes a non-nil counter so they can
// verify Open was NEVER called.
func newFallbackEvent(relPath, src string, openCalls *int64) repoindexer.EmitFileEvent {
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
			return newFallbackStringRC(src), nil
		},
	}
}

// fallbackAttrString extracts a string-typed attr from a Node's
// AttrsJSON. Fatals when the key is missing or the value is not
// a string — used by assertions that pin
// `attrs_json["language"]` shape.
func fallbackAttrString(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("fallbackAttrString: unmarshal: %v (raw=%s)", err, string(raw))
	}
	v, ok := m[key]
	if !ok {
		t.Fatalf("fallbackAttrString: key %q missing from attrs=%v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("fallbackAttrString: key %q = %T %v; want string", key, v, v)
	}
	return s
}

// fallbackItoa is a no-import int→string helper avoiding any
// dependency on the canonical_dispatcher-gated `itoa` in
// block_test.go (which is not compiled under default tags).
func fallbackItoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// =====================================================================
// Item #1: TS / Python scanner fallback runs under CGO=0
// =====================================================================

// TestDispatcher_TypeScriptFallback_EmitsClassAndMethodNodes pins
// the Stage 3.2 scanner-fallback contract for TypeScript: even
// when CGO is OFF (so the tree-sitter Go bindings are excluded),
// the dispatcher must still route `.ts` files to `tsjsParser`
// and emit class + method nodes with `attrs_json["language"]`
// set to `"typescript"`.
//
// This test runs under both CGO=1 and CGO=0; under CGO=0 it is
// the only assertion in the mandated `go test
// ./internal/repoindexer/ast` run that verifies the TypeScript
// path is exercised end-to-end (parsers_cgo.go intentionally
// omits the TS parser from the CGO-on default set because the
// tree-sitter parsers cover those languages; under CGO-off
// production wiring registers `tsjsParser` explicitly).
func TestDispatcher_TypeScriptFallback_EmitsClassAndMethodNodes(t *testing.T) {
	src := "" +
		"import { helper } from './util';\n" +
		"\n" +
		"export class Greeter {\n" +
		"    greet(name: string): string {\n" +
		"        return helper(name);\n" +
		"    }\n" +
		"}\n" +
		"\n" +
		"export function shout(msg: string): string {\n" +
		"    return msg.toUpperCase();\n" +
		"}\n"

	fw := &fallbackFakeWriter{}
	d := NewDispatcher(fw, WithParsers(NewTypeScriptParser()))

	res, err := d.EmitFile(context.Background(), newFallbackEvent("src/greeter.ts", src, nil))
	if err != nil {
		t.Fatalf("EmitFile returned error; want nil. err=%v", err)
	}
	if len(res.TouchedNodes) == 0 {
		t.Fatalf("EmitFile returned 0 TouchedNodes; want at least the class + method + free fn")
	}

	classes := fw.nodesOfKind("class")
	if len(classes) != 1 {
		t.Fatalf("class nodes = %d; want 1 (Greeter). nodes=%+v", len(classes), classes)
	}
	if got := fallbackAttrString(t, classes[0].AttrsJSON, "language"); got != "typescript" {
		t.Errorf("class[0].attrs.language = %q; want %q", got, "typescript")
	}

	methods := fw.nodesOfKind("method")
	if len(methods) != 2 {
		// 1 method on Greeter + 1 free function `shout`.
		t.Fatalf("method nodes = %d; want 2 (Greeter.greet + shout). nodes=%+v", len(methods), methods)
	}
	for i, m := range methods {
		if got := fallbackAttrString(t, m.AttrsJSON, "language"); got != "typescript" {
			t.Errorf("method[%d].attrs.language = %q; want %q", i, got, "typescript")
		}
	}
}

// TestDispatcher_PythonFallback_EmitsClassAndMethodNodes pins
// the equivalent Stage 3.2 scanner-fallback contract for Python.
// See `TestDispatcher_TypeScriptFallback_EmitsClassAndMethodNodes`
// for build-tag rationale.
func TestDispatcher_PythonFallback_EmitsClassAndMethodNodes(t *testing.T) {
	src := "" +
		"import os\n" +
		"\n" +
		"class Greeter:\n" +
		"    def greet(self, name):\n" +
		"        return helper(name)\n" +
		"\n" +
		"def helper(msg):\n" +
		"    return os.path.join('greetings', msg)\n"

	fw := &fallbackFakeWriter{}
	d := NewDispatcher(fw, WithParsers(NewPythonParser()))

	res, err := d.EmitFile(context.Background(), newFallbackEvent("src/greeter.py", src, nil))
	if err != nil {
		t.Fatalf("EmitFile returned error; want nil. err=%v", err)
	}
	if len(res.TouchedNodes) == 0 {
		t.Fatalf("EmitFile returned 0 TouchedNodes; want at least the class + method + free fn")
	}

	classes := fw.nodesOfKind("class")
	if len(classes) != 1 {
		t.Fatalf("class nodes = %d; want 1 (Greeter). nodes=%+v", len(classes), classes)
	}
	if got := fallbackAttrString(t, classes[0].AttrsJSON, "language"); got != "python" {
		t.Errorf("class[0].attrs.language = %q; want %q", got, "python")
	}

	methods := fw.nodesOfKind("method")
	if len(methods) != 2 {
		// 1 method on Greeter + 1 free function `helper`.
		t.Fatalf("method nodes = %d; want 2 (Greeter.greet + helper). nodes=%+v", len(methods), methods)
	}
	for i, m := range methods {
		if got := fallbackAttrString(t, m.AttrsJSON, "language"); got != "python" {
			t.Errorf("method[%d].attrs.language = %q; want %q", i, got, "python")
		}
	}
}

// TestDispatcher_TypeScriptFallback_RoutesAllTsExtensions pins
// the per-extension routing decision (`.ts`, `.tsx`, `.js`,
// `.jsx`, `.mjs`, `.cjs` → `tsjsParser`) declared in
// `tsjsParser.Extensions()`. Asserts `selectParser` returns a
// non-nil parser with Language()=="typescript" for each
// extension. Routing is tested independently of parse execution
// to match the convention in `dispatcher_routing_test.go`.
func TestDispatcher_TypeScriptFallback_RoutesAllTsExtensions(t *testing.T) {
	d := NewDispatcher(&fallbackFakeWriter{}, WithParsers(NewTypeScriptParser()))
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		t.Run("ext="+ext, func(t *testing.T) {
			p := d.selectParser("src/a"+ext, nil)
			if p == nil {
				t.Fatalf("selectParser(\"src/a%s\", nil) = nil; want tsjsParser", ext)
			}
			if got := p.Language(); got != "typescript" {
				t.Errorf("selectParser(\"src/a%s\").Language() = %q; want %q", ext, got, "typescript")
			}
		})
	}
}

// TestDispatcher_PythonFallback_RoutesPyAndPyiExtensions pins
// the `.py` / `.pyi` routing decision declared in
// `pythonParser.Extensions()`.
func TestDispatcher_PythonFallback_RoutesPyAndPyiExtensions(t *testing.T) {
	d := NewDispatcher(&fallbackFakeWriter{}, WithParsers(NewPythonParser()))
	for _, ext := range []string{".py", ".pyi"} {
		t.Run("ext="+ext, func(t *testing.T) {
			p := d.selectParser("src/a"+ext, nil)
			if p == nil {
				t.Fatalf("selectParser(\"src/a%s\", nil) = nil; want pythonParser", ext)
			}
			if got := p.Language(); got != "python" {
				t.Errorf("selectParser(\"src/a%s\").Language() = %q; want %q", ext, got, "python")
			}
		})
	}
}

// =====================================================================
// Item #5: duplicate parser registration is deterministic
// =====================================================================

// dupRegFakeParser is a no-op `Parser` claiming the configured
// extensions and language label. Used only by the
// `TestDispatcher_DuplicateParserRegistration_LastWins` tests
// to exercise the dispatcher's `extMap` collision policy without
// requiring real parser machinery.
type dupRegFakeParser struct {
	lang string
	exts []string
}

func (p dupRegFakeParser) Language() string     { return p.lang }
func (p dupRegFakeParser) Extensions() []string { return p.exts }
func (p dupRegFakeParser) Parse(_ string, _ []byte) (ParseResult, error) {
	return ParseResult{}, nil
}

// TestDispatcher_DuplicateParserRegistration_LastWins pins the
// dispatcher's duplicate-extension policy: when two parsers
// supplied to `WithParsers` both claim the same file extension,
// the LAST parser in the slice wins (it overwrites the prior
// entry during the `extMap[ext] = p` loop in `NewDispatcher`,
// dispatcher.go:163-167).
//
// This is the deterministic contract the `parsers_cgo.go`
// comment block lines 8-13 explicitly relies on:
//
//	"the LAST one wins via the dispatcher's extMap overwrite
//	 during NewDispatcher. The C parser MUST be listed BEFORE
//	 the C++ parser so the C++ parser's `.h` claim (if it
//	 declares one) does NOT overwrite the C parser's `.h`."
//
// The test addresses Stage 7.1 story requirement #7:
// "Verify duplicate parser registration fails or is
// deterministic, depending on current dispatcher contract."
// Current contract is last-wins (not failure), so this test
// pins that behaviour.
func TestDispatcher_DuplicateParserRegistration_LastWins(t *testing.T) {
	first := dupRegFakeParser{lang: "first-lang", exts: []string{".dup"}}
	second := dupRegFakeParser{lang: "second-lang", exts: []string{".dup"}}

	d := NewDispatcher(&fallbackFakeWriter{}, WithParsers(first, second))
	got := d.dispatcherParsersForTest()
	registered, ok := got[".dup"]
	if !ok {
		t.Fatalf("extMap missing entry for \".dup\"; got=%v", got)
	}
	if registered.Language() != "second-lang" {
		t.Errorf("extMap[\".dup\"].Language() = %q; want %q (last parser supplied to WithParsers must win)",
			registered.Language(), "second-lang")
	}

	// And in the reverse order: first now wins.
	dRev := NewDispatcher(&fallbackFakeWriter{}, WithParsers(second, first))
	gotRev := dRev.dispatcherParsersForTest()
	registeredRev, ok := gotRev[".dup"]
	if !ok {
		t.Fatalf("reverse-order extMap missing entry for \".dup\"; got=%v", gotRev)
	}
	if registeredRev.Language() != "first-lang" {
		t.Errorf("reverse-order extMap[\".dup\"].Language() = %q; want %q (reversing input order must flip the winner deterministically)",
			registeredRev.Language(), "first-lang")
	}
}

// TestDispatcher_DuplicateParserRegistration_IsStableAcrossConstructions
// asserts that the determinism is repeatable: ten constructions
// of the same dispatcher input always pick the same winner.
// Catches a regression where a future change introduces
// nondeterministic iteration over `d.parsers` (e.g. via map
// ranging) into `extMap` building.
func TestDispatcher_DuplicateParserRegistration_IsStableAcrossConstructions(t *testing.T) {
	first := dupRegFakeParser{lang: "first-lang", exts: []string{".dup"}}
	second := dupRegFakeParser{lang: "second-lang", exts: []string{".dup"}}
	for i := 0; i < 10; i++ {
		d := NewDispatcher(&fallbackFakeWriter{}, WithParsers(first, second))
		if got := d.dispatcherParsersForTest()[".dup"].Language(); got != "second-lang" {
			t.Fatalf("construction %d: extMap[\".dup\"].Language() = %q; want %q (must be stable)",
				i, got, "second-lang")
		}
	}
}

// TestDispatcher_DuplicateParserRegistration_DistinctExtensionsCoexist
// pins the negative case: when two parsers claim DISJOINT
// extensions, both end up in `extMap`. Catches a regression
// where extMap building accidentally drops entries.
func TestDispatcher_DuplicateParserRegistration_DistinctExtensionsCoexist(t *testing.T) {
	a := dupRegFakeParser{lang: "lang-a", exts: []string{".aaa"}}
	b := dupRegFakeParser{lang: "lang-b", exts: []string{".bbb"}}

	d := NewDispatcher(&fallbackFakeWriter{}, WithParsers(a, b))
	got := d.dispatcherParsersForTest()
	if entry, ok := got[".aaa"]; !ok || entry.Language() != "lang-a" {
		t.Errorf("extMap[\".aaa\"] missing or wrong language: ok=%v, lang=%q",
			ok, langOrEmpty(entry))
	}
	if entry, ok := got[".bbb"]; !ok || entry.Language() != "lang-b" {
		t.Errorf("extMap[\".bbb\"] missing or wrong language: ok=%v, lang=%q",
			ok, langOrEmpty(entry))
	}
}

func langOrEmpty(p Parser) string {
	if p == nil {
		return ""
	}
	return p.Language()
}

// =====================================================================
// Bonus: methodAttrs ReceiverCalls union contract (item #4 proof)
// =====================================================================

// TestMethodAttrs_CallsRawIncludesReceiverCalls pins the
// implementation-plan.md Stage 1.3 lines 67, 78-79 contract that
// `methodAttrs` writes a DEDUPED UNION of `m.Calls` and
// `m.ReceiverCalls` into `attrs_json["calls_raw"]`. Without this
// pin (and without the matching dispatcher.go:1167 fix in this
// commit) the dispatcher silently drops receiver-qualified call
// names from `calls_raw`, defeating the future cross-file
// resolver's ability to retry unresolved `this.foo()` /
// `self.foo()` call sites without re-parsing the file.
//
// Three subcases cover the three slice-shape combinations the
// merge helper must handle without losing data:
//   - Calls only           → unchanged behaviour.
//   - ReceiverCalls only   → MUST still emit calls_raw (regression
//                            guard against `len(m.Calls)>0` gate).
//   - Both with overlap    → deduped, source-order preserved.
func TestMethodAttrs_CallsRawIncludesReceiverCalls(t *testing.T) {
	cases := []struct {
		name           string
		calls          []string
		receiverCalls  []string
		wantCallsRaw   []string
		wantKeyPresent bool
	}{
		{
			name:           "calls only",
			calls:          []string{"helper", "log"},
			wantCallsRaw:   []string{"helper", "log"},
			wantKeyPresent: true,
		},
		{
			name:           "receiver calls only",
			receiverCalls:  []string{"resolve", "render"},
			wantCallsRaw:   []string{"resolve", "render"},
			wantKeyPresent: true,
		},
		{
			name:           "union with overlap is deduped in first-occurrence order",
			calls:          []string{"helper", "shared"},
			receiverCalls:  []string{"shared", "render"},
			wantCallsRaw:   []string{"helper", "shared", "render"},
			wantKeyPresent: true,
		},
		{
			name:           "both empty omits calls_raw",
			wantKeyPresent: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := MethodDecl{
				QualifiedName:  "Foo.bar",
				ParamSignature: "()",
				StartLine:      10,
				EndLine:        20,
				Calls:          tc.calls,
				ReceiverCalls:  tc.receiverCalls,
			}
			raw := methodAttrs("typescript", m)
			var attrs map[string]any
			if err := json.Unmarshal(raw, &attrs); err != nil {
				t.Fatalf("unmarshal methodAttrs: %v", err)
			}
			gotRaw, present := attrs["calls_raw"]
			if present != tc.wantKeyPresent {
				t.Fatalf("calls_raw presence = %v; want %v (attrs=%v)", present, tc.wantKeyPresent, attrs)
			}
			if !tc.wantKeyPresent {
				return
			}
			gotSlice, ok := gotRaw.([]any)
			if !ok {
				t.Fatalf("calls_raw = %T; want []any (attrs=%v)", gotRaw, attrs)
			}
			gotStrs := make([]string, len(gotSlice))
			for i, v := range gotSlice {
				s, ok := v.(string)
				if !ok {
					t.Fatalf("calls_raw[%d] = %T %v; want string", i, v, v)
				}
				gotStrs[i] = s
			}
			if !stringSlicesEqual(gotStrs, tc.wantCallsRaw) {
				t.Errorf("calls_raw = %v; want %v (deduped union of Calls + ReceiverCalls, source order)",
					gotStrs, tc.wantCallsRaw)
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
