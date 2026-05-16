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
// Receiver-qualified calls are unambiguous (scoped to the
// enclosing class) so the resolver does not drop on
// collisions the way bare-name calls do.
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
