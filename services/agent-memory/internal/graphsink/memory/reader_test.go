package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TestLookupBySignature_UsesSigIndexFastPath asserts the
// `map[sigKey]nodeID` fast-path the workstream brief calls out:
// InsertNode populates `sigIndex` and LookupBySignature reads
// from it (no linear scan over `nodes`). The shape assertion
// guards against a future refactor silently dropping the index.
func TestLookupBySignature_UsesSigIndexFastPath(t *testing.T) {
	s := New(Options{})
	ensureFixtureRepo(t, s)
	ctx := context.Background()
	repoID := mustRepoID(t)

	// The sigIndex must have the literal shape the brief names.
	if _, ok := any(s.sigIndex).(map[sigKey]string); !ok {
		t.Fatalf("sigIndex should be map[sigKey]string, got %T", s.sigIndex)
	}

	rec, err := s.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/lookup.go",
		FromSHA:            "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}

	// Index must contain the inserted node.
	got, ok := s.sigIndex[sigKey{Kind: "file", Sig: "file://example/lookup.go"}]
	if !ok || got != rec.NodeID {
		t.Fatalf("sigIndex miss after insert: got %q ok=%v want %q",
			got, ok, rec.NodeID)
	}

	node, err := s.LookupBySignature(ctx, repoID, "file",
		"file://example/lookup.go", graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("LookupBySignature: %v", err)
	}
	if node.NodeID != rec.NodeID {
		t.Errorf("LookupBySignature NodeID = %q, want %q",
			node.NodeID, rec.NodeID)
	}

	// A miss returns graphreader.ErrNotFound (errors.Is portable).
	_, err = s.LookupBySignature(ctx, repoID, "file",
		"file://no/such/path.go", graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Errorf("LookupBySignature miss: got %v, want ErrNotFound", err)
	}

	// A mismatched RepoID also returns ErrNotFound (single-repo
	// architecture S3.2.4: the index is implicitly scoped).
	other, err := fingerprint.RepoIDFromURL("https://github.com/example/other")
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	_, err = s.LookupBySignature(ctx, other, "file",
		"file://example/lookup.go", graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Errorf("LookupBySignature with wrong repo: got %v, want ErrNotFound", err)
	}
}

// Idempotent re-emit must not double-populate the sigIndex.
// (It's a map so duplicate writes are harmless, but the entry
// must continue to point at the original NodeID.)
func TestLookupBySignature_IdempotentReemitKeepsIndex(t *testing.T) {
	s := New(Options{})
	ensureFixtureRepo(t, s)
	ctx := context.Background()
	repoID := mustRepoID(t)

	in := graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "method",
		CanonicalSignature: "func://example.Foo",
		FromSHA:            "deadbeef",
	}
	first, err := s.InsertNode(ctx, in)
	if err != nil {
		t.Fatalf("InsertNode 1: %v", err)
	}
	second, err := s.InsertNode(ctx, in)
	if err != nil {
		t.Fatalf("InsertNode 2: %v", err)
	}
	if first.NodeID != second.NodeID {
		t.Fatalf("idempotent insert minted new NodeID: %q vs %q",
			first.NodeID, second.NodeID)
	}
	if got := s.sigIndex[sigKey{Kind: "method", Sig: "func://example.Foo"}]; got != first.NodeID {
		t.Errorf("sigIndex moved off original NodeID: got %q want %q",
			got, first.NodeID)
	}
}

// LoadExport must re-populate the sigIndex so a rehydrated
// reader's LookupBySignature fast-path works without re-running
// the writer.
func TestLoadExport_RebuildsSigIndex(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sig.json"
	writeRoundtripExport(t, path)

	loaded, err := loadExportToSink(path)
	if err != nil {
		t.Fatalf("loadExportToSink: %v", err)
	}
	want, ok := loaded.sigIndex[sigKey{Kind: "package", Sig: "pkg://example"}]
	if !ok || want == "" {
		t.Fatalf("sigIndex not rebuilt after load: keys=%v", loaded.sigIndex)
	}
	repoID := mustRepoID(t)
	node, err := loaded.LookupBySignature(context.Background(), repoID,
		"package", "pkg://example", graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("LookupBySignature after load: %v", err)
	}
	if node.NodeID != want {
		t.Errorf("LookupBySignature after load: NodeID %q != indexed %q",
			node.NodeID, want)
	}
}
