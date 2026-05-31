//go:build cgo

package sqlite_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TestReaderInterface pins the compile-time graphsink.Reader
// assertion at the external test boundary as well as the
// package-internal one in `reader.go`.
func TestReaderInterface(t *testing.T) {
	var _ graphsink.Reader = (*sqlite.Sink)(nil)
}

// fixtureSink opens a fresh on-disk Sink, registers one repo,
// inserts a small hierarchy (package -> file -> class -> method
// -> block, with a same-file static_calls edge between two
// methods), and returns the sink along with handles the
// reader-side tests pin on.
type fixture struct {
	sink       *sqlite.Sink
	repoID     fingerprint.RepoID
	repoIDStr  string
	pkgID      string
	fileID     string
	classID    string
	caller     string
	callee     string
	callEdge   string
	importEdge string
	commitTime time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	sink, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	const url = "https://example.invalid/repo.git"
	repoID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "sha1",
		RepoID:         repoID,
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	commitTime := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	if _, err := sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         "sha1",
		CommittedAt: commitTime,
	}); err != nil {
		t.Fatalf("EnsureCommit: %v", err)
	}

	mkNode := func(kind, sig, parent string, attrs string) string {
		t.Helper()
		rec, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               kind,
			CanonicalSignature: sig,
			ParentNodeID:       parent,
			FromSHA:            "sha1",
			AttrsJSON:          json.RawMessage(attrs),
		})
		if err != nil {
			t.Fatalf("InsertNode %s/%s: %v", kind, sig, err)
		}
		return rec.NodeID
	}
	pkgID := mkNode("package", "pkg/main", "", `{"lang":"go"}`)
	fileID := mkNode("file", "pkg/main/main.go", pkgID, `{}`)
	classID := mkNode("class", "pkg/main.Greeter", fileID, `{}`)
	caller := mkNode("method", "pkg/main.Greeter.Hello", classID, `{}`)
	callee := mkNode("method", "pkg/main.Greeter.greet", classID, `{}`)
	// A second file under the same package so ListNodes by
	// ParentNodeID has multiple rows to sort.
	otherFile := mkNode("file", "pkg/main/util.go", pkgID, `{}`)
	_ = otherFile

	mkEdge := func(kind, src, dst string) string {
		t.Helper()
		rec, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      kind,
			SrcNodeID: src,
			DstNodeID: dst,
			FromSHA:   "sha1",
		})
		if err != nil {
			t.Fatalf("InsertEdge %s: %v", kind, err)
		}
		return rec.EdgeID
	}
	callEdge := mkEdge("static_calls", caller, callee)
	importEdge := mkEdge("imports", fileID, otherFile)

	return &fixture{
		sink:       sink,
		repoID:     repoID,
		repoIDStr:  repoID.String(),
		pkgID:      pkgID,
		fileID:     fileID,
		classID:    classID,
		caller:     caller,
		callee:     callee,
		callEdge:   callEdge,
		importEdge: importEdge,
		commitTime: commitTime,
	}
}

func TestListReposPopulatesGeneratedAt(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	repos, err := f.sink.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("ListRepos: want 1 row, got %d", len(repos))
	}
	r := repos[0]
	if r.RepoID != f.repoIDStr {
		t.Errorf("RepoID: got %q want %q", r.RepoID, f.repoIDStr)
	}
	if r.URL != "https://example.invalid/repo.git" {
		t.Errorf("URL: got %q", r.URL)
	}
	if r.SHA != "sha1" {
		t.Errorf("SHA: got %q want sha1", r.SHA)
	}
	if !r.GeneratedAt.Equal(f.commitTime) {
		t.Errorf("GeneratedAt: got %s want %s", r.GeneratedAt, f.commitTime)
	}
}

// TestListReposMostRecentCommit verifies the JOIN picks the
// MAX(committed_at) row, not the first or last inserted.
func TestListReposMostRecentCommit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	later := f.commitTime.Add(48 * time.Hour)
	earlier := f.commitTime.Add(-48 * time.Hour)
	if _, err := f.sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      f.repoID,
		SHA:         "sha-earlier",
		CommittedAt: earlier,
	}); err != nil {
		t.Fatalf("EnsureCommit earlier: %v", err)
	}
	if _, err := f.sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      f.repoID,
		SHA:         "sha-later",
		CommittedAt: later,
	}); err != nil {
		t.Fatalf("EnsureCommit later: %v", err)
	}
	repos, err := f.sink.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !repos[0].GeneratedAt.Equal(later) {
		t.Errorf("GeneratedAt: got %s want most-recent %s", repos[0].GeneratedAt, later)
	}
}

// TestListReposFallbackToCreatedAt verifies a repo with no
// commits still gets a non-zero GeneratedAt sourced from
// `repo.created_at`.
func TestListReposFallbackToCreatedAt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	sink, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	const url = "https://example.invalid/orphan.git"
	repoID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	before := time.Now().UTC().Add(-time.Second)
	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: url, DefaultBranch: "main", CurrentHeadSHA: "", RepoID: repoID,
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	repos, err := sink.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(repos))
	}
	if repos[0].GeneratedAt.Before(before) {
		t.Errorf("GeneratedAt should fallback to repo.created_at >= %s, got %s",
			before, repos[0].GeneratedAt)
	}
}

func TestListNodesByParentAndKinds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// All files under the package node: 2 (main.go, util.go).
	nodes, err := f.sink.ListNodes(ctx, f.repoID, []string{"file"},
		graphreader.ListNodesFilter{ParentNodeID: f.pkgID}, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 files under package, got %d", len(nodes))
	}
	// Stable order: kind, canonical_signature, node_id.
	if nodes[0].CanonicalSignature >= nodes[1].CanonicalSignature {
		t.Errorf("nodes not ordered by canonical_signature: %q before %q",
			nodes[0].CanonicalSignature, nodes[1].CanonicalSignature)
	}
}

func TestListNodesByCanonicalSignature(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	nodes, err := f.sink.ListNodes(ctx, f.repoID, nil,
		graphreader.ListNodesFilter{CanonicalSignature: "pkg/main.Greeter.Hello"},
		graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].NodeID != f.caller {
		t.Fatalf("want exactly the caller node, got %+v", nodes)
	}
	if nodes[0].Kind != "method" {
		t.Errorf("kind: got %q want method", nodes[0].Kind)
	}
}

// TestListNodesLimitClampAtMax inserts more than MaxListLimit
// real rows and asserts the reader caps at exactly MaxListLimit.
// Uses the test-only DBForTest helper to bulk-insert via a single
// transaction with raw SQL -- going through InsertNode would
// take 30+ seconds for 10k+ rows because each call commits its
// own tx. The reader cannot tell the difference: it queries the
// `node` table directly.
func TestListNodesLimitClampAtMax(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Existing fixture has 6 nodes (package/file/file/class/
	// method/method). Insert (MaxListLimit + 50 - 6) extra
	// `block` rows so the total exceeds the cap.
	const overshoot = 50
	want := graphreader.MaxListLimit
	extra := graphreader.MaxListLimit + overshoot - 6

	db := sqlite.DBForTest(f.sink)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO node (node_id, fingerprint, repo_id, kind,
		    canonical_signature, parent_node_id, from_sha, attrs_json)
		VALUES (?, ?, ?, 'block', ?, ?, ?, '{}')`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < extra; i++ {
		nodeID := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		// 32-byte fingerprint with i encoded so each row is
		// unique against the (repo_id, fingerprint) UNIQUE index.
		fp := make([]byte, 32)
		binary.BigEndian.PutUint64(fp[24:], uint64(i)+1)
		sig := fmt.Sprintf("bulk/block/%d", i)
		if _, err := stmt.ExecContext(ctx, nodeID, fp, f.repoIDStr, sig, f.callee, "sha1"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("Exec %d: %v", i, err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Caller passes 0 -> clamped to MaxListLimit; an oversize
	// request must also clamp to MaxListLimit.
	for _, lim := range []int{0, graphreader.MaxListLimit + 1, graphreader.MaxListLimit * 2} {
		got, err := f.sink.ListNodes(ctx, f.repoID, nil,
			graphreader.ListNodesFilter{Limit: lim}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes limit=%d: %v", lim, err)
		}
		if len(got) != want {
			t.Errorf("ListNodes limit=%d: got %d rows, want exactly %d (MaxListLimit)",
				lim, len(got), want)
		}
	}

	// An explicit small limit (<MaxListLimit) must be honoured
	// as-is so callers can paginate manually.
	const small = 7
	got, err := f.sink.ListNodes(ctx, f.repoID, nil,
		graphreader.ListNodesFilter{Limit: small}, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListNodes small: %v", err)
	}
	if len(got) != small {
		t.Errorf("ListNodes small=%d: got %d rows, want %d", small, len(got), small)
	}
}

// TestListEdgesLimitClampAtMax does the same MaxListLimit clamp
// check for ListEdgesFrom / ListEdgesTo. Both endpoints route
// through `listEdges` so testing one path proves both, but we
// also assert ListEdgesTo's tie-breaker stays deterministic
// when every row shares (kind, dst_node_id).
func TestListEdgesLimitClampAtMax(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Add (MaxListLimit + 50) extra `static_calls` edges from
	// caller -> callee. They all collide on (kind, dst_node_id)
	// so ordering reduces entirely to the edge_id tie-breaker.
	const overshoot = 50
	want := graphreader.MaxListLimit
	extra := graphreader.MaxListLimit + overshoot

	db := sqlite.DBForTest(f.sink)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO edge (edge_id, fingerprint, repo_id, kind,
		    src_node_id, dst_node_id, from_sha, attrs_json)
		VALUES (?, ?, ?, 'reads', ?, ?, ?, '{}')`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < extra; i++ {
		edgeID := fmt.Sprintf("11111111-1111-1111-1111-%012d", i)
		fp := make([]byte, 32)
		binary.BigEndian.PutUint64(fp[24:], uint64(i)+1)
		if _, err := stmt.ExecContext(ctx, edgeID, fp, f.repoIDStr,
			f.caller, f.callee, "sha1"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("Exec %d: %v", i, err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := f.sink.ListEdgesTo(ctx, f.callee, []string{"reads"},
		graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesTo: %v", err)
	}
	if len(got) != want {
		t.Errorf("ListEdgesTo: got %d rows, want exactly %d (MaxListLimit)",
			len(got), want)
	}
	// Confirm edge_id tie-breaker ordered the rows ascending.
	for i := 1; i < len(got); i++ {
		if got[i-1].EdgeID >= got[i].EdgeID {
			t.Errorf("edge_id tie-breaker broken at %d: %q >= %q",
				i, got[i-1].EdgeID, got[i].EdgeID)
			break
		}
	}
}

func TestListNodesInvalidKind(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	_, err := f.sink.ListNodes(ctx, f.repoID, []string{"bogus"},
		graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
	if err == nil {
		t.Fatalf("ListNodes invalid kind: want error, got nil")
	}
}

func TestListNodesZeroRepoID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	_, err := f.sink.ListNodes(ctx, fingerprint.RepoID{}, nil,
		graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
	if err == nil {
		t.Fatalf("ListNodes zero repoID: want error, got nil")
	}
}

func TestListEdgesFromKindFilter(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	edges, err := f.sink.ListEdgesFrom(ctx, f.caller, []string{"static_calls"},
		graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	if len(edges) != 1 || edges[0].EdgeID != f.callEdge {
		t.Fatalf("want the static_calls edge, got %+v", edges)
	}
	if edges[0].SrcNodeID != f.caller || edges[0].DstNodeID != f.callee {
		t.Errorf("edge endpoints wrong: %+v", edges[0])
	}
}

func TestListEdgesFromAllKinds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	edges, err := f.sink.ListEdgesFrom(ctx, f.fileID, nil, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	// One imports edge from main.go.
	if len(edges) != 1 || edges[0].EdgeID != f.importEdge {
		t.Fatalf("want imports edge, got %+v", edges)
	}
}

func TestListEdgesToKindFilter(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	edges, err := f.sink.ListEdgesTo(ctx, f.callee, []string{"static_calls"},
		graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesTo: %v", err)
	}
	if len(edges) != 1 || edges[0].EdgeID != f.callEdge {
		t.Fatalf("want the static_calls inbound edge, got %+v", edges)
	}
}

// TestListEdgesOrderByKindEdgeID asserts the cross-backend
// ordering contract: rows sort by (kind ASC, edge_id ASC),
// mirroring the Postgres reader at graphreader/query.go
// :244,254,282,292 so the diagram projector sees the same
// row order regardless of backend. The test inserts MULTIPLE
// same-kind edges to DIFFERENT destinations so the edge_id
// secondary sort is actually exercised (not satisfied by an
// already-sorted-by-kind-only result).
func TestListEdgesOrderByKindEdgeID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Build a set of extra destination method nodes so the
	// caller fans out to several same-kind static_calls edges
	// with distinct edge_ids.
	mkDest := func(sig string) string {
		t.Helper()
		rec, err := f.sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             f.repoID,
			Kind:               "method",
			CanonicalSignature: sig,
			ParentNodeID:       f.classID,
			FromSHA:            "sha1",
		})
		if err != nil {
			t.Fatalf("InsertNode dest %q: %v", sig, err)
		}
		return rec.NodeID
	}
	destA := mkDest("pkg/main.Greeter.fnA")
	destB := mkDest("pkg/main.Greeter.fnB")
	destC := mkDest("pkg/main.Greeter.fnC")

	mkEdge := func(kind, dst string) {
		t.Helper()
		if _, err := f.sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    f.repoID,
			Kind:      kind,
			SrcNodeID: f.caller,
			DstNodeID: dst,
			FromSHA:   "sha1",
		}); err != nil {
			t.Fatalf("InsertEdge %s->%s: %v", kind, dst, err)
		}
	}
	// static_calls fan-out to destA/destB/destC AND the
	// fixture's existing static_calls edge to f.callee. Also
	// add a `reads` edge to assert the kind primary sort.
	mkEdge("static_calls", destA)
	mkEdge("static_calls", destB)
	mkEdge("static_calls", destC)
	mkEdge("reads", f.callee)

	edges, err := f.sink.ListEdgesFrom(ctx, f.caller, nil, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	// Verify primary sort by kind is ascending.
	kinds := make([]string, len(edges))
	for i, e := range edges {
		kinds[i] = e.Kind
	}
	if !sort.StringsAreSorted(kinds) {
		t.Errorf("edges not ordered by kind ASC: %v", kinds)
	}

	// Collect same-kind blocks and assert edge_id ASC inside
	// each (the Postgres secondary sort). We expect a
	// static_calls block with 4 rows and a reads block with 1.
	type block struct {
		kind    string
		edgeIDs []string
	}
	var blocks []block
	var cur block
	for _, e := range edges {
		if e.Kind != cur.kind {
			if cur.kind != "" {
				blocks = append(blocks, cur)
			}
			cur = block{kind: e.Kind}
		}
		cur.edgeIDs = append(cur.edgeIDs, e.EdgeID)
	}
	if cur.kind != "" {
		blocks = append(blocks, cur)
	}
	foundStaticCallsWith4 := false
	for _, b := range blocks {
		if !sort.StringsAreSorted(b.edgeIDs) {
			t.Errorf("kind=%s: edge_id not ASC: %v", b.kind, b.edgeIDs)
		}
		if b.kind == "static_calls" && len(b.edgeIDs) == 4 {
			foundStaticCallsWith4 = true
		}
	}
	if !foundStaticCallsWith4 {
		t.Errorf("expected 4 same-kind static_calls rows to exercise edge_id secondary sort; got blocks=%v", blocks)
	}
}

// TestListEdgesToEdgeIdTieBreaker asserts ListEdgesTo orders
// deterministically by `edge_id` when every row shares the
// same (kind, dst_node_id). Without the tie-breaker SQLite
// would return rows in rowid order, which is not portable
// across re-scans.
func TestListEdgesToEdgeIdTieBreaker(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// Add several inbound `static_calls` edges to the callee
	// with distinct sources. All rows share (kind, dst_node_id);
	// only edge_id can distinguish them.
	mkSrc := func(sig string) string {
		t.Helper()
		rec, err := f.sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             f.repoID,
			Kind:               "method",
			CanonicalSignature: sig,
			ParentNodeID:       f.classID,
			FromSHA:            "sha1",
		})
		if err != nil {
			t.Fatalf("InsertNode %q: %v", sig, err)
		}
		return rec.NodeID
	}
	for _, sig := range []string{"src/A", "src/B", "src/C", "src/D"} {
		src := mkSrc(sig)
		if _, err := f.sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    f.repoID,
			Kind:      "static_calls",
			SrcNodeID: src,
			DstNodeID: f.callee,
			FromSHA:   "sha1",
		}); err != nil {
			t.Fatalf("InsertEdge: %v", err)
		}
	}

	edges, err := f.sink.ListEdgesTo(ctx, f.callee, []string{"static_calls"},
		graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesTo: %v", err)
	}
	if len(edges) < 4 {
		t.Fatalf("want >=4 inbound edges, got %d", len(edges))
	}
	ids := make([]string, len(edges))
	for i, e := range edges {
		ids[i] = e.EdgeID
		if e.DstNodeID != f.callee {
			t.Errorf("unexpected dst %q", e.DstNodeID)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("ListEdgesTo: edge_id tie-breaker broken: %v", ids)
	}
	// Two back-to-back calls must return the same order.
	again, err := f.sink.ListEdgesTo(ctx, f.callee, []string{"static_calls"},
		graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesTo 2nd: %v", err)
	}
	for i := range edges {
		if edges[i].EdgeID != again[i].EdgeID {
			t.Errorf("non-deterministic ListEdgesTo at %d: %q vs %q",
				i, edges[i].EdgeID, again[i].EdgeID)
		}
	}
}

func TestGetNodeRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	n, err := f.sink.GetNode(ctx, f.caller, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if n.NodeID != f.caller || n.Kind != "method" {
		t.Errorf("GetNode: wrong node %+v", n)
	}
	if n.ParentNodeID != f.classID {
		t.Errorf("ParentNodeID: got %q want %q", n.ParentNodeID, f.classID)
	}
}

func TestGetNodeNotFound(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	_, err := f.sink.GetNode(ctx, "00000000-0000-0000-0000-000000000000",
		graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("GetNode unknown id: want ErrNotFound, got %v", err)
	}
}

func TestLookupBySignature(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	n, err := f.sink.LookupBySignature(ctx, f.repoID, "method",
		"pkg/main.Greeter.greet", graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("LookupBySignature: %v", err)
	}
	if n.NodeID != f.callee {
		t.Errorf("LookupBySignature: got %q want %q", n.NodeID, f.callee)
	}
}

func TestLookupBySignatureNotFound(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	_, err := f.sink.LookupBySignature(ctx, f.repoID, "method",
		"pkg/main.Missing", graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("LookupBySignature missing: want ErrNotFound, got %v", err)
	}
}

func TestReaderAfterCloseReturnsError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.sink.ListRepos(ctx, graphreader.ReaderOptions{}); err == nil {
		t.Errorf("ListRepos after close: want error, got nil")
	}
	if _, err := f.sink.ListNodes(ctx, f.repoID, nil,
		graphreader.ListNodesFilter{}, graphreader.ReaderOptions{}); err == nil {
		t.Errorf("ListNodes after close: want error, got nil")
	}
	if _, err := f.sink.ListEdgesFrom(ctx, f.caller, nil, graphreader.ReaderOptions{}); err == nil {
		t.Errorf("ListEdgesFrom after close: want error, got nil")
	}
	if _, err := f.sink.GetNode(ctx, f.caller, graphreader.ReaderOptions{}); err == nil {
		t.Errorf("GetNode after close: want error, got nil")
	}
	if _, err := f.sink.LookupBySignature(ctx, f.repoID, "method",
		"pkg/main.Greeter.Hello", graphreader.ReaderOptions{}); err == nil {
		t.Errorf("LookupBySignature after close: want error, got nil")
	}
}
