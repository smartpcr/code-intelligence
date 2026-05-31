//go:build cgo

package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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

func TestListNodesLimitClamp(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// Negative and zero both clamp to MaxListLimit (=10_000); a
	// value > MaxListLimit also clamps. Verify the query
	// produces a usable result without error in all three
	// shapes.
	for _, lim := range []int{0, -5, graphreader.MaxListLimit + 100} {
		got, err := f.sink.ListNodes(ctx, f.repoID, nil,
			graphreader.ListNodesFilter{Limit: lim}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes limit=%d: %v", lim, err)
		}
		if len(got) == 0 {
			t.Errorf("ListNodes limit=%d: got 0 rows, want >0", lim)
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

// TestListEdgesOrderByKindDstNodeID asserts the workstream
// brief's order contract: rows sort by (kind ASC, dst_node_id
// ASC).
func TestListEdgesOrderByKindDstNodeID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Add a second outbound edge from caller (a `reads` edge to
	// the callee node so we can verify cross-kind ordering).
	if _, err := f.sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    f.repoID,
		Kind:      "reads",
		SrcNodeID: f.caller,
		DstNodeID: f.callee,
		FromSHA:   "sha1",
	}); err != nil {
		t.Fatalf("InsertEdge reads: %v", err)
	}
	edges, err := f.sink.ListEdgesFrom(ctx, f.caller, nil, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	if len(edges) < 2 {
		t.Fatalf("want >=2 edges, got %d", len(edges))
	}
	for i := 1; i < len(edges); i++ {
		if edges[i-1].Kind > edges[i].Kind {
			t.Errorf("edges not ordered by kind ASC: %q before %q",
				edges[i-1].Kind, edges[i].Kind)
		}
		if edges[i-1].Kind == edges[i].Kind &&
			edges[i-1].DstNodeID > edges[i].DstNodeID {
			t.Errorf("within kind, edges not ordered by dst_node_id ASC")
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
