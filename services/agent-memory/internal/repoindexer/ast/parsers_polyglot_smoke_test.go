//go:build cgo

package ast

// Stage 1.2 polyglot parse smoke test.
//
// This file lives behind `//go:build cgo` because the v1
// support matrix for the additional language set (C, C++, C#,
// Go, Rust) is CGO-only by design (per tech-spec C7 and §4.3):
// each parser binds to a tree-sitter grammar via the smacker
// Go bindings, which require a working C toolchain at build
// time. The TypeScript / Python scanners and the PowerShell
// subprocess parser would also satisfy the test, but gating on
// `cgo` keeps the polyglot smoke run anchored to the canonical
// production build flavour so a regression that breaks the
// dispatcher → tree-sitter contract for one of the additional
// languages surfaces here, not just in a per-parser test.
//
// The PowerShell row depends on `pwsh` being on PATH at test
// run time (the parser shells out to it). When `pwsh` is
// missing the row is `t.Skip`-ped with a diagnostic instead
// of failing the table, so a host that has the C toolchain
// but no PowerShell SDK still gets full coverage of the
// other seven languages. The skip is recorded by Go's
// standard `-v` output for the dispatching matrix at the
// bottom of `.claude/context/tests.md`.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TestPolyglotParseSmoke is the table-driven smoke test that
// pins the workstream's stated minimum-coverage contract:
// for every supported language, the dispatcher's `EmitFile`
// path against the canonical polyglot fixture under
// `testdata/polyglot/hello.<ext>` MUST emit at least:
//
//   - 1 class-kind Node (the class/struct/interface/trait/
//     enum / type-alias each fixture declares),
//   - 1 method-kind Node (the free function or class member
//     the fixture declares), and
//   - 1 `static_calls` Edge (the same-file caller→callee
//     wired by Pass 2b's bare-name OR receiver-qualified
//     resolver, depending on language idiom).
//
// The test is deliberately threshold-based (`>=1`) rather
// than count-pinned: per-language counts are already pinned
// by the dedicated fixture tests in this package (e.g.
// `TestGoTreeSitterFixture_EmitsExpectedNodeAndEdgeSet`,
// `TestCFixture_EmitFile_EmitsExpectedNodesAndEdges`). This
// smoke is the coverage matrix gate -- it proves that ALL
// supported extensions reach the dispatcher under one build
// invocation, so a regression that drops a language from
// `defaultParsers()` is caught here even if the per-language
// test still passes in isolation.
//
// Fixtures live under `testdata/polyglot/`: Go's testing
// tooling never compiles or vets files under `testdata`, so
// the `.go` fixture (`hello.go`) cannot accidentally
// participate in `go build ./...` or `go vet ./...` and
// trip the package's own toolchain even though it carries
// a `package hello` declaration.
func TestPolyglotParseSmoke(t *testing.T) {
	cases := []struct {
		// language label used in t.Run subtest names; matches
		// the LanguageParser.Language() slug the dispatcher
		// records on each emitted node's
		// attrs_json["language"] field.
		language string
		// repo-relative path the dispatcher routes by; the
		// extension is the only routing input the dispatcher
		// uses (per dispatcher.go::EmitFile).
		relPath string
		// requirePwsh is set on the PowerShell row to make
		// the test gracefully skip on hosts without `pwsh`
		// instead of failing -- the PowerShell parser shells
		// out to it at parse time.
		requirePwsh bool
	}{
		{language: "typescript", relPath: "src/hello.ts"},
		{language: "python", relPath: "src/hello.py"},
		{language: "c", relPath: "src/hello.c"},
		{language: "cpp", relPath: "src/hello.cpp"},
		{language: "csharp", relPath: "src/hello.cs"},
		{language: "go", relPath: "src/hello.go"},
		{language: "rust", relPath: "src/hello.rs"},
		{language: "powershell", relPath: "scripts/hello.ps1", requirePwsh: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.language, func(t *testing.T) {
			if tc.requirePwsh {
				if _, err := exec.LookPath("pwsh"); err != nil {
					t.Skipf("pwsh not on PATH: %v (PowerShell parser shells out to pwsh; coverage row recorded as 'skip')", err)
				}
			}

			ext := strings.ToLower(filepath.Ext(tc.relPath))
			fixturePath := filepath.Join("testdata", "polyglot", "hello"+ext)
			src, err := os.ReadFile(fixturePath)
			if err != nil {
				t.Fatalf("read fixture %s: %v", fixturePath, err)
			}

			fw := newPgFakeWriter()
			d := NewDispatcher(fw, WithParsers(polyglotParserSet()...))
			ev := pgMakeEvent(tc.relPath, string(src))
			if _, err := d.EmitFile(context.Background(), ev); err != nil {
				t.Fatalf("EmitFile(%s): %v", tc.relPath, err)
			}

			classes := fw.nodesOf("class")
			if len(classes) < 1 {
				t.Errorf("class/type Nodes = %d; want >=1 (every polyglot fixture declares a class/struct/interface/trait/enum/type-alias)", len(classes))
			}

			methods := fw.nodesOf("method")
			if len(methods) < 1 {
				t.Errorf("method Nodes = %d; want >=1 (every polyglot fixture declares a free function or class method)", len(methods))
			}

			calls := fw.edgesOf("static_calls")
			if len(calls) < 1 {
				t.Errorf("static_calls Edges = %d; want >=1 (every polyglot fixture exercises a same-file caller→callee; Pass 2b should resolve at least one)", len(calls))
			}

			// Sanity: when nodes are emitted, the dispatcher
			// stamped the parser's Language() slug on
			// attrs_json["language"]. A drift here (e.g. the
			// dispatcher routes `.ts` to the Python parser)
			// would let a later attrs-keyed downstream
			// consumer mis-route silently. A non-`cgo`
			// regression that strands one of the additional
			// languages on the no-CGO parser set would also
			// show up here as a wrong language slug rather
			// than a missing Node.
			if len(classes)+len(methods) > 0 {
				var probe graphwriter.NodeInput
				switch {
				case len(classes) > 0:
					probe = classes[0]
				default:
					probe = methods[0]
				}
				gotLang := pgAttrString(t, probe.AttrsJSON, "language")
				if gotLang != tc.language {
					t.Errorf("first emitted node attrs.language = %q; want %q (dispatcher routed %s to the wrong parser)",
						gotLang, tc.language, tc.relPath)
				}
			}
		})
	}
}

// ----------------------------------------------------------
// pg-prefixed local helpers. The dispatcher's canonical fake
// writer (`fakeNodeEdgeWriter` in dispatcher_test.go) lives
// behind `//go:build canonical_dispatcher`, so it is not
// available in this `//go:build cgo`-only file. The
// equivalents below mirror the canonical semantics
// (idempotent insert by canonical_signature, slice
// recording, NodeID synthesis) so the smoke assertions stay
// portable to any host that satisfies the `cgo` gate.
// Names are `pg`-prefixed to coexist with the
// canonical_dispatcher helpers AND the `c`-prefixed
// helpers in `parser_treesitter_c_dispatcher_test.go` when
// every applicable build tag is set in the same `go test`
// invocation.
// ----------------------------------------------------------

// pgFakeWriter records every InsertNode / InsertEdge call so
// the smoke test can assert on Node / Edge counts without a
// PostgreSQL connection. Idempotent on canonical_signature
// to mirror the real writer's (repo_id, fingerprint) unique
// key behaviour: a second insert of the same signature
// returns the previously-minted NodeID and does NOT append a
// second entry to `nodes`.
type pgFakeWriter struct {
	mu      sync.Mutex
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
	idBySig map[string]string
}

// polyglotParserSet returns the full per-language parser
// roster the smoke test drives. The CGO `defaultParsers()`
// set (C, C++, C#, Go, Rust, PowerShell) is REPLACED here
// rather than augmented so the smoke is self-contained and
// independent of any future re-ordering inside
// `parsers_cgo.go`; the TypeScript and Python tree-sitter
// parsers (`NewTreeSitterTypeScriptParser`,
// `NewTreeSitterPythonParser`) are appended explicitly so
// the .ts / .py extensions route end-to-end through the
// dispatcher even though `defaultParsers()` does not include
// them in the v1 roster (the production wiring registers
// them separately via the global registry; this smoke wires
// them onto the dispatcher directly to exercise the same
// emission contract under one invocation).
//
// Ordering mirrors `parsers_cgo.go::defaultParsers` for the
// overlapping extensions (C before C++ so .h stays routed to
// C++ via the documented overwrite, etc.); the appended TS /
// Python parsers do not overlap any other extension so their
// position is irrelevant.
func polyglotParserSet() []Parser {
	set := append([]Parser(nil), defaultParsers()...)
	set = append(set,
		NewTreeSitterTypeScriptParser(),
		NewTreeSitterPythonParser(),
	)
	return set
}

func newPgFakeWriter() *pgFakeWriter {
	return &pgFakeWriter{idBySig: map[string]string{}}
}

func (f *pgFakeWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, dup := f.idBySig[in.CanonicalSignature]; dup {
		fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
		return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: false}, nil
	}
	id := "node-" + strconv.Itoa(len(f.nodes))
	f.idBySig[in.CanonicalSignature] = id
	f.nodes = append(f.nodes, in)
	fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: true}, nil
}

func (f *pgFakeWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, in)
	id := "edge-" + strconv.Itoa(len(f.edges)-1)
	return graphwriter.EdgeRecord{EdgeID: id, Inserted: true}, nil
}

func (f *pgFakeWriter) nodesOf(kind string) []graphwriter.NodeInput {
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

func (f *pgFakeWriter) edgesOf(kind string) []graphwriter.EdgeInput {
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

// pgStringReadCloser wraps a string as a
// repoindexer.ReadCloser for EmitFileEvent.Open. Mirrors the
// canonical_dispatcher-gated stringReadCloser so this file
// does not depend on the gated helper.
type pgStringReadCloser struct {
	r *strings.Reader
}

func (s *pgStringReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *pgStringReadCloser) Close() error               { return nil }

// pgMakeEvent constructs an EmitFileEvent backed by an
// in-memory source string. The canonical-signature inputs
// (RepoURL, FileNodeID, SHA, RepoID) are stable across
// invocations so re-reading the fixture would mint identical
// signatures, which is what the dispatcher's idempotency
// contract requires.
func pgMakeEvent(relPath, src string) repoindexer.EmitFileEvent {
	return repoindexer.EmitFileEvent{
		RepoID:     fingerprint.MustParseRepoID("11111111-2222-3333-4444-555555555555"),
		RepoURL:    "https://git.example/acme/svc",
		SHA:        "shaPOLYGLOT",
		FileNodeID: "file-node-id",
		RelPath:    relPath,
		Open: func() (repoindexer.ReadCloser, error) {
			return &pgStringReadCloser{r: strings.NewReader(src)}, nil
		},
	}
}

// pgAttrString reads a string-valued key from a JSON-encoded
// attrs blob via stdlib encoding/json. Returns "" when raw is
// empty or the key is absent; an unmarshal failure or a
// non-string value at the key is escalated via t.Fatalf so
// callers stay terse. Mirrors the canonical_dispatcher-gated
// `attrString` semantics so a future refactor that promotes
// this helper does not change behaviour.
func pgAttrString(t *testing.T, raw json.RawMessage, key string) string {
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
