//go:build cgo

// parity_test.go drives the AST dispatcher against a single
// fixture file three times -- once per graphsink backend
// (memory, sqlite, postgres) -- and asserts the
// `(repo_id, fingerprint, kind, canonical_signature)` tuples
// for Nodes and the `(kind, src_fingerprint, dst_fingerprint,
// fingerprint)` tuples for Edges agree across every backend.
//
// This file pins the cross-backend identity invariant the
// architecture calls out as REPO-SCANNER S3.4 / R5: a repo
// scanned to one backend and then to another MUST land on the
// IDENTICAL `(repo_id, fingerprint)` natural keys, because all
// three backends compute their fingerprints through the same
// `pkg/fingerprint` helpers and the same canonical-signature
// helpers in `internal/repoindexer`.
//
// BACKENDS COVERED IN THIS FILE
//
//   - memory   (graphsink/memory)         -- always exercised
//   - sqlite   (graphsink/sqlite)         -- always exercised
//
// The third backend, postgres (graphsink/postgres), is exercised
// from the sibling file `parity_postgres_test.go`, which is
// guarded by `//go:build cgo && integration`. The shared
// helpers (`parityFixture`, `runScan`, `recordingSink`, the
// `sortParityRows` comparator, etc.) live in this file because
// they have no Postgres dependency and the integration arm
// reuses them verbatim.
//
// BUILD TAGS
//
//   - `//go:build cgo` because the sqlite backend wraps
//     `mattn/go-sqlite3` and is itself `//go:build cgo`. The
//     test file inherits the same constraint so a CGO=0 build
//     of `go test ./internal/graphsink/...` compiles cleanly
//     (this file vanishes) instead of failing to resolve
//     `graphsink/sqlite`.
//
// FIXTURE
//
// One small Python file, `polyglot/greeter.py`, exercising the
// dispatcher's pass-0 (imports -> package nodes + imports
// edges), pass-1a (classes), pass-1b (methods), and pass-2b
// (same-file `static_calls`) paths. Python is the chosen
// language because `parser_python.go` has no CGO dependency,
// so the parity assertion is not sensitive to the host C
// toolchain (the host's CGO toolchain is already required by
// the sqlite backend, but a future move of this test to a
// pure-Go SQLite driver would not need to swap fixtures).
package graphsink_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	sqlitesink "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ----- fixture ----------------------------------------------------

// parityRepoURL is the synthetic URL every backend resolves to
// the same deterministic `RepoID` via
// `fingerprint.RepoIDFromURL`. Pinning a single URL lets the
// per-backend `runScan` invocations agree on the cross-backend
// identity anchor without round-tripping through git.
const parityRepoURL = "https://example.test/graphsink/parity"

// parityRepoSHA is the synthetic SHA every backend stamps onto
// the `FromSHA` field of every Node/Edge. Pinning it makes the
// fingerprints fully deterministic so the per-arm tuples can be
// compared by sorted equality.
const parityRepoSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// parityFile is the single fixture file scanned per backend.
// Kept inline (rather than under `testdata/`) so the fixture is
// readable from the test source and so the assertion logic and
// the input are in lockstep.
type parityFile struct {
	RelPath string
	Body    string
}

func parityFixture() []parityFile {
	return []parityFile{
		{
			RelPath: "polyglot/greeter.py",
			Body: "" +
				"import os\n" +
				"from typing import Optional\n" +
				"\n" +
				"class Greeter:\n" +
				"    def greet(self, name):\n" +
				"        return f\"Hello, {name}\"\n" +
				"\n" +
				"def main():\n" +
				"    g = Greeter()\n" +
				"    print(g.greet(\"world\"))\n",
		},
	}
}

// ----- recording sink --------------------------------------------

// parityNodeRow is the (repo_id, fingerprint, kind,
// canonical_signature) tuple the brief's parity assertion
// compares across backends.
type parityNodeRow struct {
	RepoID             string
	FingerprintHex     string
	Kind               string
	CanonicalSignature string
}

func (r parityNodeRow) sortKey() string {
	return r.Kind + "|" + r.CanonicalSignature + "|" + r.FingerprintHex + "|" + r.RepoID
}

// parityEdgeRow is the per-Edge tuple the brief's edge-parity
// assertion compares across backends. The CanonicalSignature
// field is included for parity-test symmetry with
// `parityNodeRow` even though edges have no
// `canonical_signature` column -- the kind + src/dst
// fingerprint tuple is the natural identity key.
type parityEdgeRow struct {
	Kind             string
	SrcFingerprint   string
	DstFingerprint   string
	EdgeFingerprint  string
	RepoID           string
}

func (r parityEdgeRow) sortKey() string {
	return r.Kind + "|" + r.SrcFingerprint + "|" + r.DstFingerprint + "|" + r.EdgeFingerprint + "|" + r.RepoID
}

// recordingSink wraps a graphsink.Sink and captures the
// post-insert `(repo_id, fingerprint, kind, canonical_signature)`
// tuple for every Node + Edge the backend writes. The recorder
// forwards every call to the wrapped sink unchanged; it does not
// re-compute fingerprints itself so the assertion proves the
// backend agrees with `pkg/fingerprint`, not just with the
// recorder.
type recordingSink struct {
	inner graphsink.Sink
	nodes []parityNodeRow
	edges []parityEdgeRow
}

func newRecordingSink(inner graphsink.Sink) *recordingSink {
	return &recordingSink{inner: inner}
}

func (r *recordingSink) EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	return r.inner.EnsureRepo(ctx, in)
}

func (r *recordingSink) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	return r.inner.EnsureCommit(ctx, in)
}

func (r *recordingSink) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	rec, err := r.inner.InsertNode(ctx, in)
	if err != nil {
		return rec, err
	}
	r.nodes = append(r.nodes, parityNodeRow{
		RepoID:             in.RepoID.String(),
		FingerprintHex:     rec.Fingerprint.Hex(),
		Kind:               in.Kind,
		CanonicalSignature: in.CanonicalSignature,
	})
	return rec, nil
}

func (r *recordingSink) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	rec, err := r.inner.InsertEdge(ctx, in)
	if err != nil {
		return rec, err
	}
	r.edges = append(r.edges, parityEdgeRow{
		Kind:            in.Kind,
		SrcFingerprint:  rec.SrcFP.Hex(),
		DstFingerprint:  rec.DstFP.Hex(),
		EdgeFingerprint: rec.Fingerprint.Hex(),
		RepoID:          in.RepoID.String(),
	})
	return rec, nil
}

func (r *recordingSink) Flush(ctx context.Context) error { return r.inner.Flush(ctx) }
func (r *recordingSink) Close() error                    { return r.inner.Close() }

// Compile-time assertion the recording wrapper still satisfies
// graphsink.Sink. If a future graphsink.Sink edit drifts the
// shape this fails at build time before the parity tests run.
var _ graphsink.Sink = (*recordingSink)(nil)

// ----- scan driver -----------------------------------------------

// runScan seeds the per-backend repo / commit / repo-node /
// file-node ancestry (manually rather than through
// `repoindexer.AncestryWriter`, because the AncestryWriter
// constructor currently passes a zero `RepoInput.RepoID` and
// the memory + sqlite backends require a non-zero RepoID at
// EnsureRepo time -- the wire-through is a later workstream),
// then drives the AST dispatcher over the parity fixture and
// returns the recorded node + edge rows.
//
// The driver pins one explicit parser (Python) via
// `ast.WithParsers` so the test does not depend on the
// build-tagged `defaultParsers()` set -- the Python parser is
// scanner-based and has no CGO toolchain dependency, so the
// fixture parses identically across hosts.
func runScan(t *testing.T, rec *recordingSink) (nodes []parityNodeRow, edges []parityEdgeRow) {
	t.Helper()
	ctx := context.Background()

	repoID, err := fingerprint.RepoIDFromURL(parityRepoURL)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}

	// 1. EnsureRepo + EnsureCommit with the precomputed RepoID
	//    so all three backends share the same `repo_id` PK.
	repoRec, err := rec.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            parityRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: parityRepoSHA,
		LanguageHints:  []string{"python"},
		RepoID:         repoID,
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if _, err := rec.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         parityRepoSHA,
		CommittedAt: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("EnsureCommit: %v", err)
	}
	_ = repoRec

	// 2. Insert the kind=repo root Node so dispatcher-minted
	//    `package` Nodes (Pass 0 imports) can use it as
	//    ParentNodeID.
	repoAttrs, _ := json.Marshal(map[string]string{"producer": "parity_test"})
	repoNode, err := rec.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "repo",
		CanonicalSignature: repoindexer.CanonicalRepoSig(parityRepoURL),
		FromSHA:            parityRepoSHA,
		AttrsJSON:          repoAttrs,
	})
	if err != nil {
		t.Fatalf("InsertNode(repo): %v", err)
	}

	// 3. Walk the fixture: per file, ensure a package Node +
	//    file Node (so the dispatcher's class/method Nodes can
	//    parent through `FileNodeID`), then call
	//    `Dispatcher.EmitFile`.
	disp := ast.NewDispatcher(rec, ast.WithParsers(ast.NewPythonParser()))

	for _, f := range parityFixture() {
		pkgDir := repoindexer.CanonicalPackageDir(f.RelPath)
		pkgAttrs, _ := json.Marshal(map[string]string{"rel_path": pkgDir, "producer": "parity_test"})
		pkgNode, err := rec.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "package",
			CanonicalSignature: repoindexer.CanonicalPackageSig(parityRepoURL, pkgDir),
			ParentNodeID:       repoNode.NodeID,
			FromSHA:            parityRepoSHA,
			AttrsJSON:          pkgAttrs,
		})
		if err != nil {
			t.Fatalf("InsertNode(package %q): %v", pkgDir, err)
		}
		if _, err := rec.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: repoNode.NodeID,
			DstNodeID: pkgNode.NodeID,
			FromSHA:   parityRepoSHA,
		}); err != nil {
			t.Fatalf("InsertEdge(repo->pkg %q): %v", pkgDir, err)
		}

		fileAttrs, _ := json.Marshal(map[string]string{"rel_path": f.RelPath, "producer": "parity_test"})
		fileNode, err := rec.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "file",
			CanonicalSignature: repoindexer.CanonicalFileSig(parityRepoURL, f.RelPath),
			ParentNodeID:       pkgNode.NodeID,
			FromSHA:            parityRepoSHA,
			AttrsJSON:          fileAttrs,
		})
		if err != nil {
			t.Fatalf("InsertNode(file %q): %v", f.RelPath, err)
		}
		if _, err := rec.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: pkgNode.NodeID,
			DstNodeID: fileNode.NodeID,
			FromSHA:   parityRepoSHA,
		}); err != nil {
			t.Fatalf("InsertEdge(pkg->file %q): %v", f.RelPath, err)
		}

		body := f.Body
		ev := repoindexer.EmitFileEvent{
			RepoID:     repoID,
			RepoURL:    parityRepoURL,
			SHA:        parityRepoSHA,
			RepoNodeID: repoNode.NodeID,
			FileNodeID: fileNode.NodeID,
			RelPath:    f.RelPath,
			AbsPath:    filepath.FromSlash(f.RelPath),
			Open: func() (repoindexer.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(body)), nil
			},
		}
		if _, err := disp.EmitFile(ctx, ev); err != nil {
			t.Fatalf("dispatcher.EmitFile(%q): %v", f.RelPath, err)
		}
	}

	if err := rec.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	nodes = append(nodes, rec.nodes...)
	edges = append(edges, rec.edges...)
	sortParityRows(nodes, edges)
	return nodes, edges
}

// sortParityRows sorts the captured rows by a stable key so two
// independently-run backends compare equal even though the
// dispatcher emits in pass-order.
func sortParityRows(nodes []parityNodeRow, edges []parityEdgeRow) {
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].sortKey() < nodes[j].sortKey()
	})
	sort.Slice(edges, func(i, j int) bool {
		return edges[i].sortKey() < edges[j].sortKey()
	})
}

// ----- backend constructors --------------------------------------

// scanMemory drives `runScan` against an in-memory graphsink
// backend. Returns the captured tuples; on failure the test
// is `t.Fatalf`'d inside runScan.
func scanMemory(t *testing.T) (nodes []parityNodeRow, edges []parityEdgeRow) {
	t.Helper()
	sink := memory.New(memory.Options{})
	t.Cleanup(func() {
		_ = sink.Close()
	})
	rec := newRecordingSink(sink)
	return runScan(t, rec)
}

// scanSQLite drives `runScan` against a SQLite-backed graphsink
// rooted in a per-test temp file.
func scanSQLite(t *testing.T) (nodes []parityNodeRow, edges []parityEdgeRow) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parity.db")
	sink, err := sqlitesink.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = sink.Close()
	})
	rec := newRecordingSink(sink)
	return runScan(t, rec)
}

// ----- assertion helpers -----------------------------------------

// assertNodesEqual fails the test with a diff-style message when
// two node row sets disagree.
func assertNodesEqual(t *testing.T, lhsName, rhsName string, lhs, rhs []parityNodeRow) {
	t.Helper()
	if len(lhs) != len(rhs) {
		t.Fatalf("node-count mismatch: %s=%d, %s=%d\nlhs=%s\nrhs=%s",
			lhsName, len(lhs), rhsName, len(rhs),
			formatNodes(lhs), formatNodes(rhs))
	}
	for i := range lhs {
		if lhs[i] != rhs[i] {
			t.Fatalf("node tuple mismatch at index %d:\n  %s = %#v\n  %s = %#v",
				i, lhsName, lhs[i], rhsName, rhs[i])
		}
	}
}

// assertEdgesEqual fails the test with a diff-style message when
// two edge row sets disagree.
func assertEdgesEqual(t *testing.T, lhsName, rhsName string, lhs, rhs []parityEdgeRow) {
	t.Helper()
	if len(lhs) != len(rhs) {
		t.Fatalf("edge-count mismatch: %s=%d, %s=%d\nlhs=%s\nrhs=%s",
			lhsName, len(lhs), rhsName, len(rhs),
			formatEdges(lhs), formatEdges(rhs))
	}
	for i := range lhs {
		if lhs[i] != rhs[i] {
			t.Fatalf("edge tuple mismatch at index %d:\n  %s = %#v\n  %s = %#v",
				i, lhsName, lhs[i], rhsName, rhs[i])
		}
	}
}

func formatNodes(rows []parityNodeRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  %s | %s | %s | %s",
			r.Kind, r.CanonicalSignature, r.FingerprintHex, r.RepoID)
	}
	return b.String()
}

func formatEdges(rows []parityEdgeRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  %s | %s -> %s | %s",
			r.Kind, r.SrcFingerprint, r.DstFingerprint, r.EdgeFingerprint)
	}
	return b.String()
}

// ----- tests -----------------------------------------------------

// TestBackendParity_MemoryAndSQLite is the unit-tier parity
// gate: the two backends that need NO external services
// (memory + sqlite-via-temp-file) MUST emit byte-identical
// `(repo_id, fingerprint, kind, canonical_signature)` Node
// tuples and `(kind, src_fingerprint, dst_fingerprint,
// fingerprint)` Edge tuples when driven over the same fixture.
//
// Failure modes this catches:
//   - A backend mutating `canonical_signature` before hashing
//     (e.g. accidentally lower-casing, trimming, or
//     normalising paths).
//   - A backend computing the fingerprint with a different
//     input order than `pkg/fingerprint.NodeFingerprint` /
//     `EdgeFingerprint`.
//   - A backend dropping or extra-emitting Nodes/Edges (e.g.
//     a missing `contains` edge or a duplicate `imports` Node).
//
// Postgres parity is asserted from `parity_postgres_test.go`,
// which is gated behind `//go:build integration`.
func TestBackendParity_MemoryAndSQLite(t *testing.T) {
	memNodes, memEdges := scanMemory(t)
	sqlNodes, sqlEdges := scanSQLite(t)

	if len(memNodes) == 0 {
		t.Fatalf("memory backend captured 0 node rows; expected the parity fixture to emit at least one Node")
	}
	if len(memEdges) == 0 {
		t.Fatalf("memory backend captured 0 edge rows; expected the parity fixture to emit at least one Edge")
	}

	assertNodesEqual(t, "memory", "sqlite", memNodes, sqlNodes)
	assertEdgesEqual(t, "memory", "sqlite", memEdges, sqlEdges)
}
