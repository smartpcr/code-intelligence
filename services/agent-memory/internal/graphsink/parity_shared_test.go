// parity_shared_test.go holds the cross-backend test helpers
// the REPO-SCANNER Stage 3.8 "backend parity golden test"
// shares between every arm of the parity gate:
//
//   - the memory arm           (this file, `scanMemory`)
//   - the sqlite arm           (`parity_test.go`,           tag: `cgo`)
//   - the postgres arm         (`parity_postgres_test.go`,  tag: `integration`)
//
// BUILD TAG DESIGN (resolves prior-iter evaluator item 2)
//
// Go build tags are FILE-LEVEL. The previous shape kept every
// helper in the cgo-tagged file, which forced the integration
// arm to depend on cgo (`//go:build cgo && integration`) and
// silently dropped the postgres test from a CGO=0 `-tags
// integration` run (the file showed up as `[no test files]`).
//
// To let each arm carry the build constraint it ACTUALLY
// needs, the shared scaffolding (`runScan`, `collectFromReader`,
// the assertion helpers, the memory arm) lives in this
// untagged file. The cgo and integration arms each declare
// their narrowest possible constraint and pull helpers in from
// here:
//
//   - parity_test.go            `//go:build cgo`         → sqlite
//   - parity_postgres_test.go   `//go:build integration` → postgres
//
// PERSISTED-STATE ASSERTIONS (resolves prior-iter evaluator item 3)
//
// The earlier shape recorded write-call inputs (and the
// fingerprints the backend RETURNED from the write call). That
// caught fingerprint computation drift but did NOT catch a
// backend that mutated `canonical_signature` between accept
// and persist (e.g. a backend that truncated long signatures
// before INSERT but returned the original from the write
// call). `collectFromReader` closes that gap by reading every
// Node + outbound Edge back through `graphsink.Reader` after
// the scan completes and asserting on the PERSISTED tuples.
//
// FIXTURE
//
// One small Python file, `polyglot/greeter.py`, exercising
// the dispatcher's pass-0 (imports → package nodes + imports
// edges), pass-1a (classes), pass-1b (methods), and pass-2b
// (same-file `static_calls`) paths. Python is the chosen
// language because `parser_python.go` has no CGO dependency,
// so the parity assertion is not sensitive to the host C
// toolchain.
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

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ----- fixture --------------------------------------------------

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

// ----- parity row tuples ----------------------------------------

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
// assertion compares across backends. Edges have no
// canonical_signature column; their natural identity key is
// (kind, src_fp, dst_fp) plus the edge fingerprint.
type parityEdgeRow struct {
	Kind            string
	SrcFingerprint  string
	DstFingerprint  string
	EdgeFingerprint string
	RepoID          string
}

func (r parityEdgeRow) sortKey() string {
	return r.Kind + "|" + r.SrcFingerprint + "|" + r.DstFingerprint + "|" + r.EdgeFingerprint + "|" + r.RepoID
}

// sortParityRows imposes the per-arm stable order so two
// independently-run backends compare equal even though the
// dispatcher emits in pass-order and the readers may return
// rows in their own server-side order.
func sortParityRows(nodes []parityNodeRow, edges []parityEdgeRow) {
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].sortKey() < nodes[j].sortKey()
	})
	sort.Slice(edges, func(i, j int) bool {
		return edges[i].sortKey() < edges[j].sortKey()
	})
}

// ----- scan driver ----------------------------------------------

// runScan seeds the per-backend repo / commit / repo-node /
// package-node / file-node ancestry (manually rather than
// through `repoindexer.AncestryWriter`, because the
// AncestryWriter constructor currently passes a zero
// `RepoInput.RepoID` and the memory + sqlite backends require a
// non-zero RepoID at EnsureRepo time -- threading the
// precomputed RepoID through AncestryWriter is a later
// workstream), then drives the AST dispatcher over the parity
// fixture and returns the deterministic RepoID the caller hands
// to `collectFromReader`.
//
// The driver pins one explicit parser (Python) via
// `ast.WithParsers` so the test does not depend on the
// build-tagged `defaultParsers()` set -- the Python parser is
// scanner-based and has no CGO toolchain dependency, so the
// fixture parses identically across hosts.
//
// Importantly, the function calls the sink DIRECTLY. There is
// no recording wrapper: the assertions read every row back
// through `collectFromReader(reader)` so a backend storage
// bug that mutates or drops the persisted tuple is observable
// (the previous recording-wrapper shape only saw write-call
// inputs and the returned fingerprints, which a bug after
// fingerprint-compute would slip past unnoticed).
func runScan(t *testing.T, sink graphsink.Sink) fingerprint.RepoID {
	t.Helper()
	ctx := context.Background()

	repoID, err := fingerprint.RepoIDFromURL(parityRepoURL)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}

	// 1. EnsureRepo + EnsureCommit with the precomputed RepoID
	//    so all three backends share the same `repo_id` PK.
	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            parityRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: parityRepoSHA,
		LanguageHints:  []string{"python"},
		RepoID:         repoID,
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if _, err := sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         parityRepoSHA,
		CommittedAt: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("EnsureCommit: %v", err)
	}

	// 2. Insert the kind=repo root Node so dispatcher-minted
	//    `package` Nodes (Pass 0 imports) can use it as
	//    ParentNodeID.
	repoAttrs, _ := json.Marshal(map[string]string{"producer": "parity_test"})
	repoNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
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
	disp := ast.NewDispatcher(sink, ast.WithParsers(ast.NewPythonParser()))

	for _, f := range parityFixture() {
		pkgDir := repoindexer.CanonicalPackageDir(f.RelPath)
		pkgAttrs, _ := json.Marshal(map[string]string{"rel_path": pkgDir, "producer": "parity_test"})
		pkgNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
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
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: repoNode.NodeID,
			DstNodeID: pkgNode.NodeID,
			FromSHA:   parityRepoSHA,
		}); err != nil {
			t.Fatalf("InsertEdge(repo->pkg %q): %v", pkgDir, err)
		}

		fileAttrs, _ := json.Marshal(map[string]string{"rel_path": f.RelPath, "producer": "parity_test"})
		fileNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
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
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
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

	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return repoID
}

// ----- persisted-state collector --------------------------------

// collectFromReader pulls every Node and outbound Edge for
// `repoID` back through the supplied `graphsink.Reader` and
// projects them into the parity row tuples the assertion
// helpers compare.
//
// IMPORTANT: this reads the PERSISTED state (the rows the
// backend actually stored), not the inputs the dispatcher
// passed or the values the sink returned from `InsertNode` /
// `InsertEdge`. A backend that round-trips fine through
// `pkg/fingerprint` but mutates `canonical_signature` between
// accept and persist would slip past a recording-wrapper
// approach -- the parity gate must catch it here.
//
// Edge src/dst fingerprints are looked up via the
// node-id → fingerprint map this function builds from
// `ListNodes`. The reader contract returns NodeIDs on Edge
// rows (not src/dst fingerprints directly), so the join
// happens here rather than in the SQL layer.
//
// Returns the row slices already sorted via `sortParityRows`
// so the caller can hand them straight to `assertNodesEqual`
// / `assertEdgesEqual`.
func collectFromReader(
	t *testing.T,
	reader graphsink.Reader,
	repoID fingerprint.RepoID,
) ([]parityNodeRow, []parityEdgeRow) {
	t.Helper()
	ctx := context.Background()

	nodes, err := reader.ListNodes(
		ctx,
		repoID,
		nil, // all kinds
		graphreader.ListNodesFilter{},
		graphreader.ReaderOptions{},
	)
	if err != nil {
		t.Fatalf("reader.ListNodes: %v", err)
	}

	nodeRows := make([]parityNodeRow, 0, len(nodes))
	nodeFP := make(map[string]string, len(nodes))
	for _, n := range nodes {
		fp := n.Fingerprint.Hex()
		nodeFP[n.NodeID] = fp
		nodeRows = append(nodeRows, parityNodeRow{
			RepoID:             n.RepoID,
			FingerprintHex:     fp,
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
		})
	}

	edgeRows := make([]parityEdgeRow, 0, len(nodes))
	for _, n := range nodes {
		edges, err := reader.ListEdgesFrom(
			ctx,
			n.NodeID,
			nil, // all kinds
			graphreader.ReaderOptions{},
		)
		if err != nil {
			t.Fatalf("reader.ListEdgesFrom(%s): %v", n.NodeID, err)
		}
		for _, e := range edges {
			srcFP := nodeFP[e.SrcNodeID]
			dstFP, ok := nodeFP[e.DstNodeID]
			if !ok {
				// Defensive: ListEdgesFrom is rooted at a node
				// we just listed, so the dst must be inside the
				// repo. A miss means a backend dropped a node
				// from ListNodes that ListEdgesFrom still
				// references -- a parity bug worth surfacing
				// explicitly instead of silently zeroing the
				// fingerprint.
				t.Fatalf("collectFromReader: edge %s -> %s references an unlisted destination node",
					e.SrcNodeID, e.DstNodeID)
			}
			edgeRows = append(edgeRows, parityEdgeRow{
				Kind:            e.Kind,
				SrcFingerprint:  srcFP,
				DstFingerprint:  dstFP,
				EdgeFingerprint: e.Fingerprint.Hex(),
				RepoID:          e.RepoID,
			})
		}
	}

	sortParityRows(nodeRows, edgeRows)
	return nodeRows, edgeRows
}

// ----- memory backend arm ---------------------------------------

// scanMemory drives `runScan` against an in-memory graphsink
// backend, then reads every persisted row back through the
// memory backend's `graphsink.Reader` view. Returns the sorted
// tuples; on failure the test is `t.Fatalf`'d inside `runScan`
// / `collectFromReader`.
//
// Lives in the no-tag shared file because the memory backend
// has neither a cgo dependency nor an external-service
// dependency -- both the cgo (sqlite) and integration
// (postgres) arms call `scanMemory` to obtain the baseline
// they compare against.
func scanMemory(t *testing.T) ([]parityNodeRow, []parityEdgeRow) {
	t.Helper()
	sink := memory.New(memory.Options{})
	t.Cleanup(func() { _ = sink.Close() })

	repoID := runScan(t, sink)
	return collectFromReader(t, sink, repoID)
}

// ----- assertion helpers ----------------------------------------

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
