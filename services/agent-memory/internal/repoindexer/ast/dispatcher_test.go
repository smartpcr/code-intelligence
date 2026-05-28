// The dispatcher pinned-scenario suite asserts the full Stage
// 3.2 dispatcher contract (block subdivision, fingerprint
// derivation, extends/implements/static_calls/contains/imports
// edge emission, embedding publish ordering). The full V2
// pipeline now lives in dispatcher.go (iter 6 restored the
// emission code from commit 9a61865 with TouchedNodes +
// receiver multimap + Pass 2d overrides + alias-aware hints +
// embedding publisher hook); these tests are un-gated so they
// run in the default `go test` suite.
package ast

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// fakeNodeEdgeWriter is the test-only writer the dispatcher
// unit tests inject in place of `*graphwriter.Writer`. It
// captures every Node / Edge insert in source order so
// assertions can compare expected vs actual without needing
// a PostgreSQL connection.
//
// It mints synthetic NodeID values (`node-N`) and deterministic
// fingerprints from the input sig string so two calls with the
// same canonical signature return the same NodeID (mirroring
// the idempotent-insert semantics of the real writer's
// (repo_id, fingerprint) unique key).
type fakeNodeEdgeWriter struct {
	mu      sync.Mutex
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
	idBySig map[string]string
	failOn  map[string]error
}

func newFakeWriter() *fakeNodeEdgeWriter {
	return &fakeNodeEdgeWriter{
		idBySig: map[string]string{},
		failOn:  map[string]error{},
	}
}

func (f *fakeNodeEdgeWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn["InsertNode:"+in.Kind]; ok {
		return graphwriter.NodeRecord{}, err
	}
	if id, dup := f.idBySig[in.CanonicalSignature]; dup {
		fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
		return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: false}, nil
	}
	id := "node-" + itoa(len(f.nodes))
	f.idBySig[in.CanonicalSignature] = id
	f.nodes = append(f.nodes, in)
	fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: true}, nil
}

func (f *fakeNodeEdgeWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn["InsertEdge:"+in.Kind]; ok {
		return graphwriter.EdgeRecord{}, err
	}
	f.edges = append(f.edges, in)
	id := "edge-" + itoa(len(f.edges)-1)
	return graphwriter.EdgeRecord{EdgeID: id, Inserted: true}, nil
}

func (f *fakeNodeEdgeWriter) nodesOf(kind string) []graphwriter.NodeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []graphwriter.NodeInput
	for _, n := range f.nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func (f *fakeNodeEdgeWriter) edgesOf(kind string) []graphwriter.EdgeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []graphwriter.EdgeInput
	for _, e := range f.edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// nodeIDBySimpleSig returns the NodeID of the FIRST inserted
// node whose Kind matches `kind` and whose CanonicalSignature's
// segment after the final `#` (stripped of any trailing
// `(...)` parameter signature) equals `simpleSig`. Returns ""
// when no node matches.
//
// Tuple-level edge assertions use this helper to translate
// from human-readable simple names (`"HelloWorld"`,
// `"Base.Identify"`) to the dispatcher-assigned NodeID without
// the test having to predict the global insertion order.
func (f *fakeNodeEdgeWriter) nodeIDBySimpleSig(kind, simpleSig string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.nodes {
		if n.Kind != kind {
			continue
		}
		if lastSegmentAfterHash(n.CanonicalSignature) == simpleSig {
			return f.idBySig[n.CanonicalSignature]
		}
	}
	return ""
}

// stringReadCloser wraps a string as a ReadCloser for test
// EmitFileEvent.Open functions.
type stringReadCloser struct {
	r *strings.Reader
}

func newStringReadCloser(s string) *stringReadCloser {
	return &stringReadCloser{r: strings.NewReader(s)}
}

func (s *stringReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *stringReadCloser) Close() error               { return nil }

// makeEvent constructs an EmitFileEvent backed by an in-memory
// source string.
func makeEvent(relPath, src string) repoindexer.EmitFileEvent {
	return repoindexer.EmitFileEvent{
		RepoID:     fingerprint.MustParseRepoID("11111111-2222-3333-4444-555555555555"),
		RepoURL:    "https://git.example/acme/svc",
		SHA:        "shaABC",
		FileNodeID: "file-node-id",
		RelPath:    relPath,
		Open: func() (repoindexer.ReadCloser, error) {
			return newStringReadCloser(src), nil
		},
	}
}

// TestDispatcher_RoutesByExtension verifies the dispatcher
// picks the TS/JS parser for TS/JS extensions, the Python
// parser for `.py`, and falls through (no-op) for unknown
// extensions.
func TestDispatcher_RoutesByExtension(t *testing.T) {
	cases := []struct {
		name      string
		relPath   string
		src       string
		wantLang  string // language attr on the first class/method node
		wantNodes int    // count of class+method nodes expected (rough)
	}{
		{name: "ts class", relPath: "src/a.ts", src: "class Foo { bar() { return 1; } }", wantLang: "typescript", wantNodes: 2},
		{name: "tsx class", relPath: "src/a.tsx", src: "class Foo { bar() { return 1; } }", wantLang: "typescript", wantNodes: 2},
		{name: "js class", relPath: "src/a.js", src: "class Foo { bar() { return 1; } }", wantLang: "typescript", wantNodes: 2},
		{name: "py class", relPath: "src/a.py", src: "class Foo:\n    def bar(self):\n        return 1\n", wantLang: "python", wantNodes: 2},
		{name: "unknown ext", relPath: "src/a.xyz", src: "blah blah", wantLang: "", wantNodes: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFakeWriter()
			d := NewDispatcher(fw)
			if _, err := d.EmitFile(context.Background(), makeEvent(tc.relPath, tc.src)); err != nil {
				t.Fatalf("EmitFile: %v", err)
			}
			nodes := append(fw.nodesOf("class"), fw.nodesOf("method")...)
			if len(nodes) != tc.wantNodes {
				t.Fatalf("class+method nodes = %d; want %d (nodes=%+v)", len(nodes), tc.wantNodes, nodes)
			}
			if tc.wantLang == "" {
				return
			}
			gotLang := attrString(t, nodes[0].AttrsJSON, "language")
			if gotLang != tc.wantLang {
				t.Errorf("attrs.language = %q; want %q", gotLang, tc.wantLang)
			}
		})
	}
}

// TestDispatcher_LanguageHintsFallbackForUnknownExtension
// confirms the v1 hint contract: hints route unknown / unmapped
// extensions ONLY (a known extension always wins).
func TestDispatcher_LanguageHintsFallbackForUnknownExtension(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithLanguageHints([]string{"python"}))
	src := "class Foo:\n    def bar(self):\n        return 1\n"
	// Use an unknown extension to force the hint path.
	if _, err := d.EmitFile(context.Background(), makeEvent("oddball.unknown", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	classes := fw.nodesOf("class")
	if len(classes) != 1 {
		t.Fatalf("expected 1 class node via hint; got %d", len(classes))
	}
	if got := attrString(t, classes[0].AttrsJSON, "language"); got != "python" {
		t.Errorf("language attr = %q; want python", got)
	}
}

// TestDispatcher_UnknownExtensionWithoutHintIsSilent confirms
// the dispatcher returns nil (does NOT fail the ingest) when
// there's no parser for the file. This is the contract
// documented on `repoindexer.NoopASTEmitter.EmitFile`.
func TestDispatcher_UnknownExtensionWithoutHintIsSilent(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	if _, err := d.EmitFile(context.Background(), makeEvent("README.md", "# heading")); err != nil {
		t.Fatalf("expected nil for unknown extension; got %v", err)
	}
	if len(fw.nodes) != 0 {
		t.Fatalf("expected zero node writes for unknown extension; got %d", len(fw.nodes))
	}
	if len(fw.edges) != 0 {
		t.Fatalf("expected zero edge writes for unknown extension; got %d", len(fw.edges))
	}
}

// TestDispatcher_OpenErrorPropagates verifies an IO error
// from ev.Open is surfaced as a non-nil error (the worker
// must mark the ingest failed). Parser errors -- in contrast
// -- are swallowed.
func TestDispatcher_OpenErrorPropagates(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	ev := makeEvent("src/a.ts", "ignored")
	ev.Open = func() (repoindexer.ReadCloser, error) {
		return nil, io.ErrUnexpectedEOF
	}
	_, err := d.EmitFile(context.Background(), ev)
	if err == nil {
		t.Fatalf("expected IO error; got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected wrapped io.ErrUnexpectedEOF; got %v", err)
	}
}

// TestDispatcher_FingerprintIncludesRelPathToPreventCrossFileCollision
// is the rubber-duck-flagged scenario: two files in the same
// repo that each declare `class Foo { bar() {} }` MUST produce
// distinct fingerprints; collapsing them would silently merge
// distinct logical entities and violate G2.
func TestDispatcher_FingerprintIncludesRelPathToPreventCrossFileCollision(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	src := "class Foo { bar() { return 1; } }"
	for _, p := range []string{"src/a.ts", "src/b.ts"} {
		if _, err := d.EmitFile(context.Background(), makeEvent(p, src)); err != nil {
			t.Fatalf("EmitFile %s: %v", p, err)
		}
	}
	classes := fw.nodesOf("class")
	if len(classes) != 2 {
		t.Fatalf("expected 2 class nodes; got %d", len(classes))
	}
	if classes[0].CanonicalSignature == classes[1].CanonicalSignature {
		t.Fatalf("class signatures collided across files: %q", classes[0].CanonicalSignature)
	}
	methods := fw.nodesOf("method")
	if len(methods) != 2 {
		t.Fatalf("expected 2 method nodes; got %d", len(methods))
	}
	if methods[0].CanonicalSignature == methods[1].CanonicalSignature {
		t.Fatalf("method signatures collided across files: %q", methods[0].CanonicalSignature)
	}
}

// TestNewDispatcher_PanicsOnNilWriter verifies the
// constructor's wiring guard.
func TestNewDispatcher_PanicsOnNilWriter(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil writer; got none")
		}
	}()
	NewDispatcher(nil)
}

// TestDispatcher_BlockSubdivisionFiresThroughEmitter does the
// end-to-end version of block_test.go's threshold checks:
// drive a 81-line method through the dispatcher and assert two
// block nodes show up under the method as `contains` children.
func TestDispatcher_BlockSubdivisionFiresThroughEmitter(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	body := repeatStatementLines(81)
	src := "class Foo {\n  bar() {\n" + body + "\n  }\n}\n"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/big.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	blocks := fw.nodesOf("block")
	if len(blocks) != 2 {
		t.Fatalf("expected 2 block nodes (entry+exit); got %d", len(blocks))
	}
	methods := fw.nodesOf("method")
	if len(methods) != 1 {
		t.Fatalf("expected 1 method node; got %d", len(methods))
	}
	// Verify every block's parent_node_id points to the method.
	methodNodeID := "node-1" // class is node-0, method is node-1
	for _, b := range blocks {
		if b.ParentNodeID != methodNodeID {
			t.Errorf("block parent_node_id = %q; want %q", b.ParentNodeID, methodNodeID)
		}
	}
	// Verify there are 2 file->class + class->method + 2 method->block contains edges = 4.
	containsEdges := fw.edgesOf("contains")
	if len(containsEdges) != 4 {
		t.Errorf("expected 4 contains edges (1 file->class, 1 class->method, 2 method->block); got %d", len(containsEdges))
	}
}

// TestDispatcher_BlockSubdivisionDoesNotFireBelowThreshold is
// the dispatcher-level companion to the unit-level scenario.
func TestDispatcher_BlockSubdivisionDoesNotFireBelowThreshold(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	body := repeatStatementLines(80)
	src := "class Foo {\n  bar() {\n" + body + "\n  }\n}\n"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/just-under.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	if blocks := fw.nodesOf("block"); len(blocks) != 0 {
		t.Fatalf("expected 0 block nodes at exactly threshold; got %d", len(blocks))
	}
}

// TestDispatcher_PanicInParserIsRecovered verifies the safe-
// parse wrapper -- a panicking parser MUST NOT crash the
// worker goroutine.
func TestDispatcher_PanicInParserIsRecovered(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithParsers(panickingParser{}))
	if _, err := d.EmitFile(context.Background(), makeEvent("src/boom.bang", "anything")); err != nil {
		t.Fatalf("expected nil despite parser panic; got %v", err)
	}
}

type panickingParser struct{}

func (panickingParser) Language() string     { return "boom" }
func (panickingParser) Extensions() []string { return []string{".bang"} }
func (panickingParser) Parse(string, []byte) (ParseResult, error) {
	panic("boom")
}

// attrString reads a string-valued key from a JSON-encoded
// attrs blob. Failing assertion via t.Fatalf rather than
// returning ("", err) so callers stay terse.
func attrString(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("attrs JSON unmarshal: %v (raw=%s)", err, string(raw))
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("attrs[%q] is %T; want string", key, v)
	}
	return s
}

// attrStringSlice extracts a []string from a JSON-encoded
// attrs blob. Returns nil when the key is absent so callers
// can `len(...) == 0` regardless of whether the producer
// chose to omit the key or write an empty array.
func attrStringSlice(t *testing.T, raw json.RawMessage, key string) []string {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("attrs JSON unmarshal: %v (raw=%s)", err, string(raw))
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("attrs[%q] is %T; want []any", key, v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("attrs[%q] entry is %T; want string", key, e)
		}
		out = append(out, s)
	}
	return out
}

// attrInt extracts an int-valued key (deserialised through
// json.Unmarshal as float64) from a JSON-encoded attrs blob.
// Returns 0 when the key is absent so callers can branch on
// `== 0` for "missing" cases (the producer never writes 0
// for any boundary that matters to consumers).
func attrInt(t *testing.T, raw json.RawMessage, key string) int {
	t.Helper()
	if len(raw) == 0 {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("attrs JSON unmarshal: %v (raw=%s)", err, string(raw))
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("attrs[%q] is %T; want number", key, v)
	}
	return int(f)
}

// TestDispatcher_EmitsImportsEdgesForExternalModules pins
// evaluator finding #2: external imports MUST materialise
// as `imports` edges from the File Node to a synthetic
// external-package Node. Relative imports (`./utils`) MUST
// NOT produce edges -- they belong to the future cross-file
// resolver story.
func TestDispatcher_EmitsImportsEdgesForExternalModules(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	src := "import { format } from \"lodash\";\n" +
		"import os from \"node:os\";\n" +
		"import { helper } from \"./utils\";\n" +
		"class Foo { bar() {} }\n"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/main.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	importEdges := fw.edgesOf("imports")
	if len(importEdges) != 2 {
		t.Fatalf("expected 2 imports edges (lodash, node:os); got %d (%+v)",
			len(importEdges), importEdges)
	}
	pkgNodes := fw.nodesOf("package")
	if len(pkgNodes) != 2 {
		t.Fatalf("expected 2 external package nodes; got %d", len(pkgNodes))
	}
	// Every package node must carry "source"="external" so
	// the worker-emitted first-party packages stay
	// distinguishable.
	for _, p := range pkgNodes {
		if got := attrString(t, p.AttrsJSON, "source"); got != "external" {
			t.Errorf("package %s attrs.source = %q; want external",
				p.CanonicalSignature, got)
		}
	}
	// Edge src must be the File Node id.
	for _, e := range importEdges {
		if e.SrcNodeID != "file-node-id" {
			t.Errorf("imports edge src = %q; want file-node-id", e.SrcNodeID)
		}
	}
}

// TestDispatcher_EmitsReadsAndWritesEdgesToEnclosingClass
// pins evaluator finding #2 for `reads` / `writes`: a method
// that touches `this.X` / `self.X` MUST produce a class-
// scoped edge with the touched member names recorded in the
// edge's `attrs_json["members"]`. The class-scope target
// reflects v1's lack of a field-Node kind (per migration
// 0001's closed `node_kind` enum).
func TestDispatcher_EmitsReadsAndWritesEdgesToEnclosingClass(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	src := "class Greeter {\n" +
		"  constructor(prefix) { this.prefix = prefix; this.count = 0; }\n" +
		"  greet(name) { this.count = this.count + 1; return this.prefix + name; }\n" +
		"}\n"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/g.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	writes := fw.edgesOf("writes")
	if len(writes) == 0 {
		t.Fatalf("expected at least one writes edge; got 0")
	}
	reads := fw.edgesOf("reads")
	if len(reads) == 0 {
		t.Fatalf("expected at least one reads edge; got 0")
	}
	// Constructor writes prefix + count.
	foundCtorMembers := false
	for _, e := range writes {
		ms := attrStringSlice(t, e.AttrsJSON, "members")
		if hasAll(ms, "prefix", "count") {
			foundCtorMembers = true
			break
		}
	}
	if !foundCtorMembers {
		t.Errorf("no writes edge listed both prefix and count members; edges=%+v", writes)
	}
	// Class is node-0, ctor is node-1, greet is node-2.
	// `reads`/`writes` edges target the class node.
	for _, e := range append(reads, writes...) {
		if e.DstNodeID != "node-0" {
			t.Errorf("reads/writes edge dst = %q; want class node-0", e.DstNodeID)
		}
	}
}

// TestDispatcher_ResolvesReceiverQualifiedStaticCalls pins
// evaluator finding #5: `this.helper()` / `self.helper()`
// MUST produce `static_calls` edges resolved against
// `<EnclosingClass>.helper` in the local symbol table.
//
// Resolution runs through the Pass 2b receiver-index
// multimap (`dispatcher.go::emit` lines 612-690). The
// multimap is keyed by `<EnclosingClass>.<simpleName>` and
// can hold multiple node ids per key when the Go parser
// emits a value-receiver + pointer-receiver pair whose
// simple name collides (the same `Bar` exposed as both
// `Foo.Bar` and `*Foo.Bar`); the resolver then DROPS the
// callee per the A5 rule (`len(ids) != 1` -> skip). This
// TS fixture exercises only the unambiguous size==1 path
// where `Foo.helper` has a single producer; the sister
// tests `TestDispatcher_GoMultimapDropsOnReceiverCollision`
// and `TestDispatcher_GoMultimapResolvesPointerReceiverAlone`
// cover the collision-drop and pointer-only-alias paths.
func TestDispatcher_ResolvesReceiverQualifiedStaticCalls(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	// `Foo.bar` calls `this.helper()`; `Foo.helper` exists,
	// so the dispatcher must emit a Foo.bar -> Foo.helper
	// static_calls edge.
	src := "class Foo {\n" +
		"  helper() { return 42; }\n" +
		"  bar() { return this.helper() + 1; }\n" +
		"}\n"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/r.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	calls := fw.edgesOf("static_calls")
	if len(calls) == 0 {
		t.Fatalf("expected at least one static_calls edge for this.helper(); got 0")
	}
	// helper is node-1, bar is node-2 (class is node-0).
	foundReceiverEdge := false
	for _, e := range calls {
		if e.SrcNodeID == "node-2" && e.DstNodeID == "node-1" {
			foundReceiverEdge = true
			break
		}
	}
	if !foundReceiverEdge {
		t.Errorf("no Foo.bar -> Foo.helper static_calls edge; edges=%+v", calls)
	}
}

// TestDispatcher_RecordsFileRelativeBlockBoundariesInAttrs
// pins evaluator finding #6: Block nodes' attrs MUST carry
// `start_line`, `end_line`, `start_byte`, `end_byte` in
// FILE-relative coordinates. The span ingestor uses these
// to map runtime stack frames back to Block nodes; without
// them spans would have to re-walk the method body offsets.
func TestDispatcher_RecordsFileRelativeBlockBoundariesInAttrs(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw)
	body := repeatStatementLines(81)
	src := "class Foo {\n  bar() {\n" + body + "\n  }\n}\n"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/coords.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	blocks := fw.nodesOf("block")
	if len(blocks) != 2 {
		t.Fatalf("expected 2 block nodes; got %d", len(blocks))
	}
	for i, b := range blocks {
		if attrInt(t, b.AttrsJSON, "start_line") == 0 {
			t.Errorf("block[%d] missing start_line attr", i)
		}
		if attrInt(t, b.AttrsJSON, "end_line") == 0 {
			t.Errorf("block[%d] missing end_line attr", i)
		}
	}
	// The entry block must NOT start at line 1 -- the method
	// body opens on line 2 (line 1 = "class Foo {", line 2
	// = "  bar() {", where the body's opening brace lives).
	// If the dispatcher passed body-relative coordinates
	// (1..N within the body) the entry start_line would be
	// 1; the scanner sets BodyStartLine to the line of the
	// `{` and the tree-sitter parser sets it to the
	// statement_block's start point -- both are file-relative
	// and both should yield >= 2 here.
	if got := attrInt(t, blocks[0].AttrsJSON, "start_line"); got < 2 {
		t.Errorf("entry block start_line = %d; want >= 2 (file-relative)", got)
	}
	if blocks[0].AttrsJSON == nil || len(blocks[0].AttrsJSON) == 0 {
		t.Fatalf("blocks[0].AttrsJSON is empty")
	}
}

// TestDispatcher_PerEventLanguageHintsOverrideGlobal pins
// evaluator finding #4: per-event LanguageHints (from
// EmitFileEvent.LanguageHints, set by the worker from
// `repo.language_hints[]`) MUST take precedence over the
// dispatcher-default hints passed to NewDispatcher. Without
// per-event hints two repos in the same worker would have
// to share a single language profile.
func TestDispatcher_PerEventLanguageHintsOverrideGlobal(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithLanguageHints([]string{"typescript"}))
	src := "class Foo:\n    def bar(self):\n        return 1\n"
	ev := makeEvent("noext.unknown", src)
	ev.LanguageHints = []string{"python"}
	if _, err := d.EmitFile(context.Background(), ev); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	classes := fw.nodesOf("class")
	if len(classes) != 1 {
		t.Fatalf("expected 1 class via per-event hint override; got %d", len(classes))
	}
	if got := attrString(t, classes[0].AttrsJSON, "language"); got != "python" {
		t.Errorf("language attr = %q; want python (per-event hint must win)", got)
	}
}

// TestNormalizeHints_AliasExpansion pins the v1 alias rows
// described in the AST-PARSER-FOR-ADDIT architecture
// (Section 3) and tech spec (Section 4.2). Each row asserts:
//   - case-folding (`TS` -> `typescript`);
//   - whitespace trimming (`  cpp  ` -> `cpp`);
//   - alias -> canonical (e.g. `golang` -> `go`, `c#` -> `csharp`);
//   - canonical pass-through (e.g. `python` -> `python`).
//
// Regression rows for `ts`, `tsx`, `js`, `jsx`, `mjs`, `cjs`,
// `py`, and `pyi` guard against the existing TypeScript /
// Python contract drifting when new rows are added.
func TestNormalizeHints_AliasExpansion(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		// --- regression: existing TS / Python rows must not drift ---
		{name: "ts -> typescript", in: []string{"ts"}, want: []string{"typescript"}},
		{name: "tsx -> typescript", in: []string{"tsx"}, want: []string{"typescript"}},
		{name: "js -> typescript", in: []string{"js"}, want: []string{"typescript"}},
		{name: "jsx -> typescript", in: []string{"jsx"}, want: []string{"typescript"}},
		{name: "mjs -> typescript", in: []string{"mjs"}, want: []string{"typescript"}},
		{name: "cjs -> typescript", in: []string{"cjs"}, want: []string{"typescript"}},
		{name: "py -> python", in: []string{"py"}, want: []string{"python"}},
		{name: "pyi -> python", in: []string{"pyi"}, want: []string{"python"}},
		// --- new: C ---
		{name: "c -> c", in: []string{"c"}, want: []string{"c"}},
		{name: "h -> c", in: []string{"h"}, want: []string{"c"}},
		// --- new: C++ ---
		{name: "cc -> cpp", in: []string{"cc"}, want: []string{"cpp"}},
		{name: "cxx -> cpp", in: []string{"cxx"}, want: []string{"cpp"}},
		{name: "cpp -> cpp", in: []string{"cpp"}, want: []string{"cpp"}},
		{name: "c++ -> cpp", in: []string{"c++"}, want: []string{"cpp"}},
		{name: "hpp -> cpp", in: []string{"hpp"}, want: []string{"cpp"}},
		// --- new: C# ---
		{name: "cs -> csharp", in: []string{"cs"}, want: []string{"csharp"}},
		{name: "csharp -> csharp", in: []string{"csharp"}, want: []string{"csharp"}},
		{name: "c# -> csharp", in: []string{"c#"}, want: []string{"csharp"}},
		// --- new: Go ---
		{name: "go -> go", in: []string{"go"}, want: []string{"go"}},
		{name: "golang -> go", in: []string{"golang"}, want: []string{"go"}},
		// --- new: Rust ---
		{name: "rs -> rust", in: []string{"rs"}, want: []string{"rust"}},
		{name: "rust -> rust", in: []string{"rust"}, want: []string{"rust"}},
		// --- new: PowerShell ---
		{name: "ps -> powershell", in: []string{"ps"}, want: []string{"powershell"}},
		{name: "ps1 -> powershell", in: []string{"ps1"}, want: []string{"powershell"}},
		{name: "psm1 -> powershell", in: []string{"psm1"}, want: []string{"powershell"}},
		{name: "psd1 -> powershell", in: []string{"psd1"}, want: []string{"powershell"}},
		{name: "powershell -> powershell", in: []string{"powershell"}, want: []string{"powershell"}},
		// --- canonical TS / Python pass-through (unchanged) ---
		{name: "typescript -> typescript", in: []string{"typescript"}, want: []string{"typescript"}},
		{name: "python -> python", in: []string{"python"}, want: []string{"python"}},
		// --- normalization: case + whitespace ---
		{name: "upper-case TS folded", in: []string{"TS"}, want: []string{"typescript"}},
		{name: "mixed-case CSharp folded", in: []string{"CSharp"}, want: []string{"csharp"}},
		{name: "padded cpp trimmed", in: []string{"  cpp  "}, want: []string{"cpp"}},
		{name: "padded mixed-case Go folded+trimmed", in: []string{"  Golang "}, want: []string{"go"}},
		// --- empty / blank entries skipped ---
		{name: "empty entry skipped", in: []string{""}, want: nil},
		{name: "whitespace-only entry skipped", in: []string{"   "}, want: nil},
		// --- unknown hint passes through (lowercased, trimmed) ---
		{name: "unknown java passes through", in: []string{"Java"}, want: []string{"java"}},
		// --- multi-entry: order preserved, mixed mappings ---
		{
			name: "multi: ts + golang + c# preserves order",
			in:   []string{"ts", "Golang", "C#"},
			want: []string{"typescript", "go", "csharp"},
		},
		// --- multi-entry with blanks interleaved ---
		{
			name: "multi: blanks dropped, others kept in order",
			in:   []string{"", "ps1", "  ", "rust"},
			want: []string{"powershell", "rust"},
		},
		// --- nil / empty input ---
		{name: "nil input -> nil", in: nil, want: nil},
		{name: "empty input -> nil", in: []string{}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeHints(tc.in)
			if !equalStringSlice(got, tc.want) {
				t.Errorf("normalizeHints(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeHints_PreservesEntryOrder pins ordering as a
// separable guarantee: selectParser iterates the normalised
// slice in order and returns the first matching parser, so a
// silent re-ordering would silently change routing for repos
// whose `language_hints[]` lists multiple languages.
func TestNormalizeHints_PreservesEntryOrder(t *testing.T) {
	in := []string{"rust", "go", "powershell", "c", "cpp"}
	got := normalizeHints(in)
	want := []string{"rust", "go", "powershell", "c", "cpp"}
	if !equalStringSlice(got, want) {
		t.Fatalf("normalizeHints order drifted: got %q; want %q", got, want)
	}
}

// TestSelectParser_ExtensionWinsOverHints pins the resolution
// precedence documented at dispatcher.go:219-248: a known
// file extension MUST route to its parser even when the
// per-event LanguageHints (or the dispatcher-default hints
// from `WithLanguageHints`) name a different language.
//
// Without this guard a repo whose `language_hints[]` lists
// `python` could silently swallow a `.ts` file with the
// Python parser and emit zero nodes (regression observed
// during the Stage 1.5 alias-expansion design review).
func TestSelectParser_ExtensionWinsOverHints(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithLanguageHints([]string{"python"}))
	src := "class Foo { bar() { return 1; } }"
	ev := makeEvent("src/a.ts", src)
	ev.LanguageHints = []string{"python"}
	if _, err := d.EmitFile(context.Background(), ev); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}
	classes := fw.nodesOf("class")
	if len(classes) != 1 {
		t.Fatalf("expected 1 class node from extension routing; got %d", len(classes))
	}
	if got := attrString(t, classes[0].AttrsJSON, "language"); got != "typescript" {
		t.Errorf("language attr = %q; want typescript (extension must win over hints)", got)
	}
}

// equalStringSlice is a tiny helper because the test table
// includes nil-vs-empty distinctions that `reflect.DeepEqual`
// already supports but we want a single-line call site.
func equalStringSlice(a, b []string) bool {
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

// hasAll returns true when every want entry is present in
// haystack. Linear scan since the slices are tiny.
func hasAll(haystack []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range haystack {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestMergeLangMeta covers the helper in isolation, separately
// from the classAttrs/methodAttrs wiring. The first-class-key-
// wins rule (architecture invariant C11 / Section 4.4.2) is
// the load-bearing contract for the AST-PARSER-FOR-ADDIT
// dispatcher: a parser that emits a LangMeta key colliding
// with a dispatcher first-class key MUST be silently ignored
// rather than overwrite the dispatcher's value.
func TestMergeLangMeta(t *testing.T) {
	t.Run("nil in is a no-op", func(t *testing.T) {
		out := map[string]any{"language": "go"}
		mergeLangMeta(out, nil)
		if len(out) != 1 || out["language"] != "go" {
			t.Fatalf("nil merge mutated out: %#v", out)
		}
	})

	t.Run("empty non-nil in is a no-op", func(t *testing.T) {
		out := map[string]any{"language": "go"}
		mergeLangMeta(out, map[string]any{})
		if len(out) != 1 || out["language"] != "go" {
			t.Fatalf("empty merge mutated out: %#v", out)
		}
	})

	t.Run("new keys are added", func(t *testing.T) {
		out := map[string]any{"language": "go"}
		mergeLangMeta(out, map[string]any{
			"receiver":     "*Foo",
			"receiver_ptr": true,
		})
		if out["receiver"] != "*Foo" {
			t.Errorf("receiver = %v; want *Foo", out["receiver"])
		}
		if out["receiver_ptr"] != true {
			t.Errorf("receiver_ptr = %v; want true", out["receiver_ptr"])
		}
		if out["language"] != "go" {
			t.Errorf("language clobbered: %v", out["language"])
		}
	})

	t.Run("first-class key wins on collision", func(t *testing.T) {
		out := map[string]any{
			"language":   "go",
			"start_line": 10,
		}
		mergeLangMeta(out, map[string]any{
			"language":   "typescript", // collides; must be dropped
			"start_line": 999,          // collides; must be dropped
			"namespace":  "foo.bar",    // non-colliding; must land
		})
		if out["language"] != "go" {
			t.Errorf("language = %v; want go (first-class must win)", out["language"])
		}
		if out["start_line"] != 10 {
			t.Errorf("start_line = %v; want 10 (first-class must win)", out["start_line"])
		}
		if out["namespace"] != "foo.bar" {
			t.Errorf("namespace = %v; want foo.bar (non-colliding key must merge)", out["namespace"])
		}
	})

	t.Run("first-class key with nil value still wins", func(t *testing.T) {
		// `_, exists := out[k]` distinguishes "absent" from
		// "present with nil value". A first-class attr that
		// the dispatcher intentionally set to nil MUST NOT be
		// overwritten by LangMeta.
		out := map[string]any{"extends_raw": nil}
		mergeLangMeta(out, map[string]any{"extends_raw": []string{"BadBase"}})
		if out["extends_raw"] != nil {
			t.Errorf("extends_raw = %v; want nil (presence, not value, wins)", out["extends_raw"])
		}
	})

	t.Run("nested slice and map values pass through by reference", func(t *testing.T) {
		// Documents the shallow-copy contract: parsers that
		// mutate a slice/map they put into LangMeta AFTER the
		// merge will see the change leak into the dispatcher's
		// attrs map. This is acceptable because mustJSON runs
		// immediately and the dispatcher discards the merged
		// map afterwards.
		nested := []string{"a", "b"}
		out := map[string]any{}
		mergeLangMeta(out, map[string]any{"trait": nested})
		got, ok := out["trait"].([]string)
		if !ok {
			t.Fatalf("trait = %T; want []string", out["trait"])
		}
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("trait = %v; want [a b]", got)
		}
	})
}

// TestClassAttrs_MergesLangMetaAfterFirstClassKeys pins the
// wiring: the helper MUST run AFTER all optional first-class
// keys (extends_raw, implements_raw) are populated, so a
// LangMeta key that collides with an optional first-class key
// is dropped rather than silently shadowed by the dispatcher's
// later write. The brief is explicit: "immediately before
// mustJSON" -- no first-class write may happen afterwards.
func TestClassAttrs_MergesLangMetaAfterFirstClassKeys(t *testing.T) {
	c := ClassDecl{
		QualifiedName: "Foo",
		Kind:          "class",
		Extends:       []string{"RealBase"},
		Implements:    []string{"Iface"},
		StartLine:     10,
		EndLine:       20,
		LangMeta: map[string]any{
			// Collides with mandatory first-class key.
			"language": "wrong",
			// Collides with optional first-class key populated
			// from c.Extends. The merge runs AFTER extends_raw
			// is written, so the LangMeta value MUST lose.
			"extends_raw": []string{"FakeBase"},
			// Non-colliding per-language keys.
			"namespace": "foo.bar",
			"partial":   true,
		},
	}
	raw := classAttrs("csharp", c)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal classAttrs: %v", err)
	}
	if got["language"] != "csharp" {
		t.Errorf("language = %v; want csharp (first-class must win)", got["language"])
	}
	if got["namespace"] != "foo.bar" {
		t.Errorf("namespace = %v; want foo.bar (LangMeta must merge non-colliding keys)", got["namespace"])
	}
	if got["partial"] != true {
		t.Errorf("partial = %v; want true (LangMeta bool must round-trip)", got["partial"])
	}
	extends, ok := got["extends_raw"].([]any)
	if !ok {
		t.Fatalf("extends_raw = %T; want []any", got["extends_raw"])
	}
	if len(extends) != 1 || extends[0] != "RealBase" {
		t.Errorf("extends_raw = %v; want [RealBase] (first-class optional key must win over LangMeta)", extends)
	}
}

// TestClassAttrs_NilLangMetaIsByteIdenticalToPreHelperWorld
// pins the "TS/JS/Python baseline pays nothing" guarantee
// from the field doc: a parser that leaves LangMeta nil MUST
// produce exactly the same attrs JSON keys as before the
// helper landed.
func TestClassAttrs_NilLangMetaIsByteIdenticalToPreHelperWorld(t *testing.T) {
	c := ClassDecl{
		QualifiedName: "Foo",
		Kind:          "class",
		StartLine:     1,
		EndLine:       5,
		// LangMeta intentionally left nil.
	}
	raw := classAttrs("typescript", c)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal classAttrs: %v", err)
	}
	// Exactly the four mandatory keys, nothing else.
	want := map[string]any{
		"language":   "typescript",
		"decl_kind":  "class",
		"start_line": float64(1),
		"end_line":   float64(5),
	}
	if len(got) != len(want) {
		t.Fatalf("classAttrs keys = %v; want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("classAttrs[%q] = %v; want %v", k, got[k], v)
		}
	}
}

// TestMethodAttrs_MergesLangMetaAfterFirstClassKeys mirrors
// the class test for methods. Includes a calls_raw collision
// because that's the method-side optional first-class key
// most likely to collide with parser-emitted language data.
func TestMethodAttrs_MergesLangMetaAfterFirstClassKeys(t *testing.T) {
	m := MethodDecl{
		QualifiedName:  "Foo.bar",
		ParamSignature: "(x int)",
		EnclosingClass: "Foo",
		Modifiers:      []string{"static"},
		Calls:          []string{"helper"},
		StartLine:      10,
		EndLine:        20,
		LangMeta: map[string]any{
			// Collides with mandatory first-class key.
			"language": "wrong",
			// Collides with optional first-class key populated
			// from m.Calls. The merge runs AFTER calls_raw is
			// written, so the LangMeta value MUST lose.
			"calls_raw": []string{"fake_call"},
			// Non-colliding per-language keys.
			"receiver":     "*Foo",
			"receiver_ptr": true,
		},
	}
	raw := methodAttrs("go", m)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal methodAttrs: %v", err)
	}
	if got["language"] != "go" {
		t.Errorf("language = %v; want go (first-class must win)", got["language"])
	}
	if got["receiver"] != "*Foo" {
		t.Errorf("receiver = %v; want *Foo (LangMeta must merge non-colliding keys)", got["receiver"])
	}
	if got["receiver_ptr"] != true {
		t.Errorf("receiver_ptr = %v; want true (LangMeta bool must round-trip)", got["receiver_ptr"])
	}
	calls, ok := got["calls_raw"].([]any)
	if !ok {
		t.Fatalf("calls_raw = %T; want []any", got["calls_raw"])
	}
	if len(calls) != 1 || calls[0] != "helper" {
		t.Errorf("calls_raw = %v; want [helper] (first-class optional key must win over LangMeta)", calls)
	}
}

// TestMethodAttrs_NilLangMetaIsByteIdenticalToPreHelperWorld
// mirrors the class baseline test for methods.
func TestMethodAttrs_NilLangMetaIsByteIdenticalToPreHelperWorld(t *testing.T) {
	m := MethodDecl{
		QualifiedName:  "bar",
		ParamSignature: "()",
		StartLine:      1,
		EndLine:        3,
		// LangMeta intentionally left nil; no EnclosingClass,
		// Modifiers, or Calls so the optional-key paths are
		// skipped too.
	}
	raw := methodAttrs("typescript", m)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal methodAttrs: %v", err)
	}
	want := map[string]any{
		"language":   "typescript",
		"start_line": float64(1),
		"end_line":   float64(3),
		"params_raw": "()",
	}
	if len(got) != len(want) {
		t.Fatalf("methodAttrs keys = %v; want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("methodAttrs[%q] = %v; want %v", k, got[k], v)
		}
	}
}
