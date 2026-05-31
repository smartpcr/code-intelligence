//go:build cgo

package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// openTempSink opens a fresh SQLite Sink backed by an on-disk
// file under t.TempDir(). On-disk (not :memory:) so the FOREIGN
// KEYS pragma can be exercised against a real file.
func openTempSink(t *testing.T) *sqlite.Sink {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.db")
	sink, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sink.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return sink
}

// TestSinkInterface is a compile-time assertion that *Sink
// satisfies graphsink.Sink. The package's source already pins
// this with a `var _ graphsink.Sink = (*Sink)(nil)` declaration;
// asserting it from the test package as well catches any future
// vendored-package-rename hazard.
func TestSinkInterface(t *testing.T) {
	var _ graphsink.Sink = (*sqlite.Sink)(nil)
}

func TestEnsureRepoIdempotent(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)

	in := graphwriter.RepoInput{
		URL:            "https://example.invalid/repo.git",
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go", "python"},
	}
	first, err := sink.EnsureRepo(ctx, in)
	if err != nil {
		t.Fatalf("first EnsureRepo: %v", err)
	}
	if !first.Inserted {
		t.Fatalf("first EnsureRepo: want Inserted=true")
	}
	if first.RepoID == "" {
		t.Fatalf("first EnsureRepo: empty repo_id")
	}
	if first.ID.IsZero() {
		t.Fatalf("first EnsureRepo: zero RepoID")
	}

	// Same URL, mutated mutable fields: must hit conflict path
	// and return the same PK with Inserted=false.
	in.CurrentHeadSHA = "cafebabe"
	in.LanguageHints = []string{"rust"}
	second, err := sink.EnsureRepo(ctx, in)
	if err != nil {
		t.Fatalf("second EnsureRepo: %v", err)
	}
	if second.Inserted {
		t.Fatalf("second EnsureRepo: want Inserted=false")
	}
	if second.RepoID != first.RepoID {
		t.Fatalf("second EnsureRepo: repo_id drift: %q vs %q", second.RepoID, first.RepoID)
	}
}

func TestInsertNodeFingerprintIdentity(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)

	repo, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            "https://example.invalid/repo.git",
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	want, err := fingerprint.NodeFingerprint(
		repo.ID, "file", "src/main.go", "deadbeef",
	)
	if err != nil {
		t.Fatalf("NodeFingerprint: %v", err)
	}

	first, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repo.ID,
		Kind:               "file",
		CanonicalSignature: "src/main.go",
		FromSHA:            "deadbeef",
		AttrsJSON:          json.RawMessage(`{"language":"go"}`),
	})
	if err != nil {
		t.Fatalf("first InsertNode: %v", err)
	}
	if !first.Inserted {
		t.Fatalf("first InsertNode: want Inserted=true")
	}
	if first.Fingerprint != want {
		t.Fatalf("first InsertNode: fingerprint drift")
	}

	// Idempotency: same input must collide on (repo_id, fingerprint)
	// and recover the same node_id with Inserted=false.
	second, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repo.ID,
		Kind:               "file",
		CanonicalSignature: "src/main.go",
		FromSHA:            "deadbeef",
		AttrsJSON:          json.RawMessage(`{"language":"go"}`),
	})
	if err != nil {
		t.Fatalf("second InsertNode: %v", err)
	}
	if second.Inserted {
		t.Fatalf("second InsertNode: want Inserted=false")
	}
	if second.NodeID != first.NodeID {
		t.Fatalf("second InsertNode: node_id drift: %q vs %q", second.NodeID, first.NodeID)
	}
}

func TestInsertNodeParentSameRepoGuard(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)

	repoA, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            "https://example.invalid/a.git",
		DefaultBranch:  "main",
		CurrentHeadSHA: "shaA",
	})
	if err != nil {
		t.Fatalf("EnsureRepo A: %v", err)
	}
	repoB, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            "https://example.invalid/b.git",
		DefaultBranch:  "main",
		CurrentHeadSHA: "shaB",
	})
	if err != nil {
		t.Fatalf("EnsureRepo B: %v", err)
	}

	parent, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoA.ID,
		Kind:               "package",
		CanonicalSignature: "pkg/a",
		FromSHA:            "shaA",
	})
	if err != nil {
		t.Fatalf("InsertNode parent: %v", err)
	}

	// Child in repoB with a parent_node_id from repoA must fail.
	_, err = sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoB.ID,
		Kind:               "file",
		CanonicalSignature: "src/main.go",
		ParentNodeID:       parent.NodeID,
		FromSHA:            "shaB",
	})
	if err == nil {
		t.Fatalf("InsertNode cross-repo parent: want error, got nil")
	}
}

func TestInsertEdgeIdempotent(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)

	repo, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            "https://example.invalid/repo.git",
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	src, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repo.ID, Kind: "method", CanonicalSignature: "Foo.bar()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode src: %v", err)
	}
	dst, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repo.ID, Kind: "method", CanonicalSignature: "Foo.baz()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode dst: %v", err)
	}

	first, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    repo.ID,
		Kind:      "static_calls",
		SrcNodeID: src.NodeID,
		DstNodeID: dst.NodeID,
		FromSHA:   "deadbeef",
	})
	if err != nil {
		t.Fatalf("first InsertEdge: %v", err)
	}
	if !first.Inserted {
		t.Fatalf("first InsertEdge: want Inserted=true")
	}
	if first.SrcFP != src.Fingerprint || first.DstFP != dst.Fingerprint {
		t.Fatalf("first InsertEdge: endpoint fingerprint drift")
	}

	second, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    repo.ID,
		Kind:      "static_calls",
		SrcNodeID: src.NodeID,
		DstNodeID: dst.NodeID,
		FromSHA:   "deadbeef",
	})
	if err != nil {
		t.Fatalf("second InsertEdge: %v", err)
	}
	if second.Inserted {
		t.Fatalf("second InsertEdge: want Inserted=false")
	}
	if second.EdgeID != first.EdgeID {
		t.Fatalf("edge_id drift: %q vs %q", second.EdgeID, first.EdgeID)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("edge fingerprint drift")
	}
}

func TestInsertEdgeRejectsUnknownKind(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)

	repo, _ := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: "u", DefaultBranch: "main", CurrentHeadSHA: "s",
	})
	src, _ := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repo.ID, Kind: "method", CanonicalSignature: "a", FromSHA: "s",
	})
	dst, _ := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repo.ID, Kind: "method", CanonicalSignature: "b", FromSHA: "s",
	})

	_, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repo.ID, Kind: "not_a_kind",
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID, FromSHA: "s",
	})
	if err == nil {
		t.Fatalf("want CHECK violation, got nil")
	}
}

func TestEnsureCommit(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)

	repo, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: "u", DefaultBranch: "main", CurrentHeadSHA: "s",
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	in := graphwriter.CommitInput{
		RepoID:      repo.ID,
		SHA:         "abc123",
		ParentSHA:   "",
		CommittedAt: time.Now().UTC(),
	}
	first, err := sink.EnsureCommit(ctx, in)
	if err != nil {
		t.Fatalf("first EnsureCommit: %v", err)
	}
	if !first.Inserted {
		t.Fatalf("first EnsureCommit: want Inserted=true")
	}

	second, err := sink.EnsureCommit(ctx, in)
	if err != nil {
		t.Fatalf("second EnsureCommit: %v", err)
	}
	if second.Inserted {
		t.Fatalf("second EnsureCommit: want Inserted=false")
	}
}

func TestCloseIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	sink, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close (must be nil per Sink contract): %v", err)
	}
	if err := sink.Flush(ctx); !errors.Is(err, sqlite.ErrSinkClosed) {
		t.Fatalf("Flush after Close: want ErrSinkClosed, got %v", err)
	}
}

func TestFlushNoBuffering(t *testing.T) {
	ctx := context.Background()
	sink := openTempSink(t)
	// Repeated Flush is harmless and returns nil; no buffered
	// state to drain since each Sink method commits inline.
	for i := 0; i < 3; i++ {
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("Flush #%d: %v", i, err)
		}
	}
}
