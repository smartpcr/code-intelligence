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

// (additional regression tests appended below)

// seedFixtureGraph populates a sink with a small but filter-
// exercising graph: one repo, two file nodes (one with a
// ParentNodeID, two distinct FromSHAs, two distinct
// canonical_signatures), one method, and two edges of distinct
// kinds. Returned values: the sink, the inserted NodeRecords by
// label, and the inserted EdgeRecords by label. Used by the
// ListNodes / ListEdges parity tests below.
func seedFixtureGraph(t *testing.T) (
	*Sink,
	map[string]graphwriter.NodeRecord,
	map[string]graphwriter.EdgeRecord,
) {
	t.Helper()
	s := New(Options{})
	ensureFixtureRepo(t, s)
	ctx := context.Background()
	repoID := mustRepoID(t)

	nodes := map[string]graphwriter.NodeRecord{}
	edges := map[string]graphwriter.EdgeRecord{}

	mk := func(label, kind, sig, parent, sha string) {
		rec, err := s.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               kind,
			CanonicalSignature: sig,
			ParentNodeID:       parent,
			FromSHA:            sha,
		})
		if err != nil {
			t.Fatalf("InsertNode %s: %v", label, err)
		}
		nodes[label] = rec
	}
	mkEdge := func(label, kind, src, dst, sha string) {
		rec, err := s.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID: repoID, Kind: kind,
			SrcNodeID: src, DstNodeID: dst,
			FromSHA: sha,
		})
		if err != nil {
			t.Fatalf("InsertEdge %s: %v", label, err)
		}
		edges[label] = rec
	}

	mk("repo", "repo", repoURL, "", "deadbeef")
	mk("fileA", "file", "file://example/a.go", nodes["repo"].NodeID, "deadbeef")
	mk("fileB", "file", "file://example/b.go", nodes["repo"].NodeID, "cafef00d")
	mk("methodA", "method", "func://example.A", nodes["fileA"].NodeID, "deadbeef")
	mk("methodB", "method", "func://example.B", nodes["fileB"].NodeID, "cafef00d")

	mkEdge("contains-A", "contains",
		nodes["repo"].NodeID, nodes["fileA"].NodeID, "deadbeef")
	mkEdge("contains-B", "contains",
		nodes["repo"].NodeID, nodes["fileB"].NodeID, "cafef00d")
	mkEdge("calls-A-B", "static_calls",
		nodes["methodA"].NodeID, nodes["methodB"].NodeID, "deadbeef")
	mkEdge("imports-A-B", "imports",
		nodes["fileA"].NodeID, nodes["fileB"].NodeID, "deadbeef")

	return s, nodes, edges
}

// Direct ListNodes filter-parity coverage (evaluator iter-1
// item 1). Exercises kind, ParentNodeID, FromSHA,
// CanonicalSignature filters, the limit clamp, and the stable
// `kind, canonical_signature, node_id` ordering.
func TestListNodes_FilterParity(t *testing.T) {
	s, nodes, _ := seedFixtureGraph(t)
	ctx := context.Background()
	repoID := mustRepoID(t)

	t.Run("kind filter", func(t *testing.T) {
		got, err := s.ListNodes(ctx, repoID, []string{"file"},
			graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("kind=file: got %d rows, want 2", len(got))
		}
		for _, n := range got {
			if n.Kind != "file" {
				t.Errorf("got non-file row: %+v", n)
			}
		}
	})

	t.Run("ParentNodeID filter", func(t *testing.T) {
		got, err := s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{ParentNodeID: nodes["fileA"].NodeID},
			graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		if len(got) != 1 || got[0].NodeID != nodes["methodA"].NodeID {
			t.Errorf("ParentNodeID filter: got %+v, want methodA", got)
		}
	})

	t.Run("FromSHA filter", func(t *testing.T) {
		got, err := s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{FromSHA: "cafef00d"},
			graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		// fileB + methodB share cafef00d.
		if len(got) != 2 {
			t.Errorf("FromSHA filter: got %d rows, want 2; rows=%+v", len(got), got)
		}
		for _, n := range got {
			if n.FromSHA != "cafef00d" {
				t.Errorf("FromSHA filter let through %s", n.FromSHA)
			}
		}
	})

	t.Run("CanonicalSignature filter", func(t *testing.T) {
		got, err := s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{CanonicalSignature: "file://example/a.go"},
			graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		if len(got) != 1 || got[0].NodeID != nodes["fileA"].NodeID {
			t.Errorf("CanonicalSignature filter: got %+v, want fileA", got)
		}
	})

	t.Run("Limit clamps", func(t *testing.T) {
		got, err := s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{Limit: 2},
			graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("Limit=2: got %d rows, want 2", len(got))
		}
		// Limit > MaxListLimit collapses to MaxListLimit (no
		// panic, no extra rows). We only have 5 rows in the
		// fixture so we just confirm the call succeeds and
		// returns <= MaxListLimit rows.
		got, err = s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{Limit: graphreader.MaxListLimit + 100},
			graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes oversized limit: %v", err)
		}
		if len(got) > graphreader.MaxListLimit {
			t.Errorf("Limit > MaxListLimit returned %d > %d",
				len(got), graphreader.MaxListLimit)
		}
	})

	t.Run("stable ordering", func(t *testing.T) {
		got, err := s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		// Expected order: kind, canonical_signature, node_id.
		// Our fixture has kinds {file, method, repo}; alphabetic
		// kind ordering puts file rows first, then method, then
		// repo. Within a kind, signatures are alphabetic.
		wantOrder := []string{
			nodes["fileA"].NodeID,
			nodes["fileB"].NodeID,
			nodes["methodA"].NodeID,
			nodes["methodB"].NodeID,
			nodes["repo"].NodeID,
		}
		if len(got) != len(wantOrder) {
			t.Fatalf("got %d rows, want %d", len(got), len(wantOrder))
		}
		for i, want := range wantOrder {
			if got[i].NodeID != want {
				t.Errorf("row %d: got %q (%s/%s), want %q",
					i, got[i].NodeID, got[i].Kind, got[i].CanonicalSignature, want)
			}
		}
		// Calling ListNodes again must yield the byte-identical
		// order -- the stability is per-call (Postgres uses
		// ORDER BY; we use sort.SliceStable).
		again, _ := s.ListNodes(ctx, repoID, nil,
			graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
		for i := range got {
			if got[i].NodeID != again[i].NodeID {
				t.Errorf("ordering not stable across calls at %d: %q vs %q",
					i, got[i].NodeID, again[i].NodeID)
			}
		}
	})

	t.Run("wrong repo returns empty", func(t *testing.T) {
		other, err := fingerprint.RepoIDFromURL("https://github.com/example/other")
		if err != nil {
			t.Fatalf("RepoIDFromURL: %v", err)
		}
		got, err := s.ListNodes(ctx, other, nil,
			graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListNodes wrong repo: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("wrong repo returned %d rows, want 0", len(got))
		}
	})
}

// Direct ListEdgesFrom / ListEdgesTo coverage (evaluator iter-1
// item 2). Exercises outbound + inbound traversal, kind
// filtering, the limit clamp, and the stable `kind, edge_id`
// ordering.
func TestListEdges_FilterParity(t *testing.T) {
	s, nodes, edges := seedFixtureGraph(t)
	ctx := context.Background()

	t.Run("ListEdgesFrom outbound", func(t *testing.T) {
		// repo node has two outbound `contains` edges.
		got, err := s.ListEdgesFrom(ctx, nodes["repo"].NodeID,
			nil, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListEdgesFrom: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("repo outbound: got %d, want 2", len(got))
		}
		for _, e := range got {
			if e.SrcNodeID != nodes["repo"].NodeID {
				t.Errorf("outbound edge with wrong src: %+v", e)
			}
		}
	})

	t.Run("ListEdgesTo inbound", func(t *testing.T) {
		// methodB has one inbound `static_calls` edge.
		got, err := s.ListEdgesTo(ctx, nodes["methodB"].NodeID,
			nil, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListEdgesTo: %v", err)
		}
		if len(got) != 1 || got[0].EdgeID != edges["calls-A-B"].EdgeID {
			t.Errorf("methodB inbound: got %+v, want calls-A-B", got)
		}
	})

	t.Run("kind filter outbound", func(t *testing.T) {
		got, err := s.ListEdgesFrom(ctx, nodes["fileA"].NodeID,
			[]string{"imports"}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListEdgesFrom imports: %v", err)
		}
		if len(got) != 1 || got[0].Kind != "imports" {
			t.Errorf("kind filter outbound: got %+v, want one imports edge", got)
		}
	})

	t.Run("kind filter inbound", func(t *testing.T) {
		got, err := s.ListEdgesTo(ctx, nodes["fileB"].NodeID,
			[]string{"imports"}, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListEdgesTo imports: %v", err)
		}
		if len(got) != 1 || got[0].EdgeID != edges["imports-A-B"].EdgeID {
			t.Errorf("kind filter inbound: got %+v, want imports-A-B", got)
		}
	})

	t.Run("Limit clamps", func(t *testing.T) {
		got, err := s.ListEdgesFrom(ctx, nodes["repo"].NodeID, nil,
			graphreader.ReaderOptions{Limit: 1})
		if err != nil {
			t.Fatalf("ListEdgesFrom Limit=1: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("Limit=1: got %d rows, want 1", len(got))
		}
		got, err = s.ListEdgesFrom(ctx, nodes["repo"].NodeID, nil,
			graphreader.ReaderOptions{Limit: graphreader.MaxListLimit + 100})
		if err != nil {
			t.Fatalf("ListEdgesFrom oversized: %v", err)
		}
		if len(got) > graphreader.MaxListLimit {
			t.Errorf("oversized limit returned %d > %d",
				len(got), graphreader.MaxListLimit)
		}
	})

	t.Run("stable ordering", func(t *testing.T) {
		// repo's two outbound edges are both `contains`, so
		// ordering is by EdgeID alphabetically. The synthetic
		// EdgeIDs are `e-0000000001` … so contains-A (inserted
		// first) sorts before contains-B.
		got, err := s.ListEdgesFrom(ctx, nodes["repo"].NodeID,
			nil, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListEdgesFrom: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 rows")
		}
		if got[0].EdgeID >= got[1].EdgeID {
			t.Errorf("EdgeIDs not in ascending order: %q then %q",
				got[0].EdgeID, got[1].EdgeID)
		}
		// fileA has one outbound `imports` and one... no, just
		// imports. Confirm a multi-kind src orders kind first
		// then edge_id: use repo (all `contains`) then check
		// no-op holds.
		_ = got
	})

	t.Run("kind first, then edge_id", func(t *testing.T) {
		// Synthetic graph: methodA has one outbound static_calls.
		// To exercise kind-first ordering we need a node with
		// edges of multiple kinds. fileA has one outbound
		// `imports`; the repo has two `contains`. Compose a
		// fresh tiny scenario where one src has both an
		// `imports` and a `static_calls` outbound.
		repoID := mustRepoID(t)
		s2 := New(Options{})
		ensureFixtureRepo(t, s2)
		src, _ := s2.InsertNode(context.Background(), graphwriter.NodeInput{
			RepoID: repoID, Kind: "method",
			CanonicalSignature: "go://src", FromSHA: "deadbeef",
		})
		dstM, _ := s2.InsertNode(context.Background(), graphwriter.NodeInput{
			RepoID: repoID, Kind: "method",
			CanonicalSignature: "go://dstM", FromSHA: "deadbeef",
		})
		dstF, _ := s2.InsertNode(context.Background(), graphwriter.NodeInput{
			RepoID: repoID, Kind: "file",
			CanonicalSignature: "file://dstF", FromSHA: "deadbeef",
		})
		// Insert static_calls FIRST (later EdgeID) then imports
		// SECOND (later EdgeID) to make insertion order disagree
		// with the desired final order (imports < static_calls).
		if _, err := s2.InsertEdge(context.Background(), graphwriter.EdgeInput{
			RepoID: repoID, Kind: "static_calls",
			SrcNodeID: src.NodeID, DstNodeID: dstM.NodeID, FromSHA: "deadbeef",
		}); err != nil {
			t.Fatalf("InsertEdge static_calls: %v", err)
		}
		if _, err := s2.InsertEdge(context.Background(), graphwriter.EdgeInput{
			RepoID: repoID, Kind: "imports",
			SrcNodeID: src.NodeID, DstNodeID: dstF.NodeID, FromSHA: "deadbeef",
		}); err != nil {
			t.Fatalf("InsertEdge imports: %v", err)
		}
		got, err := s2.ListEdgesFrom(context.Background(), src.NodeID,
			nil, graphreader.ReaderOptions{})
		if err != nil {
			t.Fatalf("ListEdgesFrom: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 rows, got %d", len(got))
		}
		if got[0].Kind != "imports" || got[1].Kind != "static_calls" {
			t.Errorf("kind ordering wrong: got [%s, %s], want [imports, static_calls]",
				got[0].Kind, got[1].Kind)
		}
	})
}

// ListRepos must derive SHA from EnsureCommit when EnsureRepo
// was called without a CurrentHeadSHA (evaluator iter-1 item 3).
func TestListRepos_FallsBackToEnsureCommitSHA(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	repoID := mustRepoID(t)

	if _, err := s.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: repoURL, RepoID: repoID, // CurrentHeadSHA intentionally empty
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	// First call: no commits yet -> SHA empty.
	repos, err := s.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos pre-commit: %v", err)
	}
	if len(repos) != 1 || repos[0].SHA != "" {
		t.Fatalf("pre-commit: want one entry with empty SHA, got %+v", repos)
	}

	// Record two commits in order; ListRepos must surface the
	// most-recently-recorded one.
	for _, sha := range []string{"aaaa1111", "bbbb2222"} {
		if _, err := s.EnsureCommit(ctx, graphwriter.CommitInput{
			RepoID: repoID, SHA: sha,
		}); err != nil {
			t.Fatalf("EnsureCommit %s: %v", sha, err)
		}
	}

	repos, err = s.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos post-commit: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(repos))
	}
	if repos[0].SHA != "bbbb2222" {
		t.Errorf("SHA = %q, want %q (most recent EnsureCommit)",
			repos[0].SHA, "bbbb2222")
	}
	if repos[0].URL != repoURL || repos[0].RepoID != repoID.String() {
		t.Errorf("RepoSummary identity drifted: %+v", repos[0])
	}
}

// And the converse: when EnsureRepo carried a non-empty
// CurrentHeadSHA, ListRepos must prefer it over the commit
// slice (otherwise a re-scan that records a different SHA could
// silently override the repo's declared head).
func TestListRepos_PrefersRepoInputSHAOverCommit(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	repoID := mustRepoID(t)

	if _, err := s.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: repoURL, RepoID: repoID, CurrentHeadSHA: "feedface",
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if _, err := s.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID: repoID, SHA: "deadbeef",
	}); err != nil {
		t.Fatalf("EnsureCommit: %v", err)
	}
	repos, err := s.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].SHA != "feedface" {
		t.Errorf("ListRepos should prefer RepoInput SHA: %+v", repos)
	}
}

