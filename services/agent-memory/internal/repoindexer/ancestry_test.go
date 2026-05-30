package repoindexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Unit tests for AncestryWriter (REPO-SCANNER Phase 2 / impl-plan
// "AncestryWriter factored from worker"). They drive the
// pre-walk + per-file sequence against a deterministic in-memory
// implementation of RepoCommitNodeEdgeWriter so the assertions
// pin (kind, canonical_signature, parent_node_id, from_sha)
// ordering without needing a live PostgreSQL fixture. The
// integration sibling (worker_integration_test.go) exercises the
// real graphwriter against Postgres; this file pins the
// AncestryWriter contract that integration test relies on once
// the worker is rewired through this type.

// ----- mockRCNEWriter ---------------------------------------------

// mockRCNEWriter is a deterministic in-memory implementation of
// RepoCommitNodeEdgeWriter. It mirrors the dedupe semantics of
// graphwriter.Writer:
//
//   - EnsureRepo is upsert-by-URL. Inserted=true on first call
//     for the URL, false on subsequent calls.
//   - EnsureCommit is INSERT ... ON CONFLICT DO NOTHING by
//     (RepoID, SHA). Inserted reflects the conflict path.
//   - InsertNode dedupes by (RepoID, Kind, CanonicalSignature,
//     FromSHA) — close enough to the real
//     (RepoID, fingerprint) dedupe for this test's purposes and
//     deterministic without invoking the fingerprint domain.
//   - InsertEdge dedupes by (RepoID, Kind, Src, Dst, FromSHA).
//
// All call payloads are recorded in their respective slices so
// tests can assert on call order and arguments.
type mockRCNEWriter struct {
	repoInputs   []graphwriter.RepoInput
	commitInputs []graphwriter.CommitInput
	nodeInputs   []graphwriter.NodeInput
	edgeInputs   []graphwriter.EdgeInput

	repos    map[string]graphwriter.RepoRecord // url -> record
	commits  map[string]struct{}               // repo_id|sha -> seen
	nodes    map[string]graphwriter.NodeRecord // repo_id|kind|sig|sha -> record
	edges    map[string]graphwriter.EdgeRecord // repo_id|kind|src|dst|sha -> record
	nodeSeq  int
	edgeSeq  int
	repoSeq  int

	// Optional injection points so failure-path tests can
	// surface errors at specific steps without rebuilding the
	// whole mock for each scenario.
	errEnsureRepo   error
	errEnsureCommit error
	errInsertNode   error
	errInsertEdge   error
}

func newMockRCNEWriter() *mockRCNEWriter {
	return &mockRCNEWriter{
		repos:   make(map[string]graphwriter.RepoRecord),
		commits: make(map[string]struct{}),
		nodes:   make(map[string]graphwriter.NodeRecord),
		edges:   make(map[string]graphwriter.EdgeRecord),
	}
}

func (m *mockRCNEWriter) EnsureRepo(_ context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	m.repoInputs = append(m.repoInputs, in)
	if m.errEnsureRepo != nil {
		return graphwriter.RepoRecord{}, m.errEnsureRepo
	}
	if rec, ok := m.repos[in.URL]; ok {
		rec.Inserted = false
		m.repos[in.URL] = rec
		return rec, nil
	}
	m.repoSeq++
	// Deterministic per-mock-instance repo UUID. Bytes 0..3
	// encode the per-call sequence so two repos in the same
	// test never collide.
	id := fingerprint.RepoID{}
	id[0] = byte(m.repoSeq)
	id[15] = 0xAB
	rec := graphwriter.RepoRecord{
		RepoID:   id.String(),
		ID:       id,
		Inserted: true,
	}
	m.repos[in.URL] = rec
	return rec, nil
}

func (m *mockRCNEWriter) EnsureCommit(_ context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	m.commitInputs = append(m.commitInputs, in)
	if m.errEnsureCommit != nil {
		return graphwriter.CommitRecord{}, m.errEnsureCommit
	}
	key := in.RepoID.String() + "|" + in.SHA
	if _, ok := m.commits[key]; ok {
		return graphwriter.CommitRecord{
			RepoID: in.RepoID.String(), SHA: in.SHA, Inserted: false,
		}, nil
	}
	m.commits[key] = struct{}{}
	return graphwriter.CommitRecord{
		RepoID: in.RepoID.String(), SHA: in.SHA, Inserted: true,
	}, nil
}

func (m *mockRCNEWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	m.nodeInputs = append(m.nodeInputs, in)
	if m.errInsertNode != nil {
		return graphwriter.NodeRecord{}, m.errInsertNode
	}
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.CanonicalSignature + "|" + in.FromSHA
	if rec, ok := m.nodes[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	m.nodeSeq++
	rec := graphwriter.NodeRecord{
		NodeID:   fmt.Sprintf("node-%04d", m.nodeSeq),
		Inserted: true,
	}
	m.nodes[key] = rec
	return rec, nil
}

func (m *mockRCNEWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	m.edgeInputs = append(m.edgeInputs, in)
	if m.errInsertEdge != nil {
		return graphwriter.EdgeRecord{}, m.errInsertEdge
	}
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.SrcNodeID + "|" + in.DstNodeID + "|" + in.FromSHA
	if rec, ok := m.edges[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	m.edgeSeq++
	rec := graphwriter.EdgeRecord{
		EdgeID:   fmt.Sprintf("edge-%04d", m.edgeSeq),
		Inserted: true,
	}
	m.edges[key] = rec
	return rec, nil
}

// ----- tests ------------------------------------------------------

func TestNewAncestryWriter_panicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil writer")
		}
	}()
	_ = NewAncestryWriter(nil, "https://example.test/x", "abc123")
}

func TestAncestryWriter_EnsureRepoAndCommit_callsThreeStepSequence(t *testing.T) {
	t.Parallel()
	mw := newMockRCNEWriter()
	pinnedNow := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	const repoURL = "https://example.test/repo-A"
	const sha = "deadbeefcafe"
	aw := NewAncestryWriter(mw, repoURL, sha)
	aw.now = func() time.Time { return pinnedNow }

	got, err := aw.EnsureRepoAndCommit(context.Background(), "main", []string{"go", "ts"})
	if err != nil {
		t.Fatalf("EnsureRepoAndCommit: %v", err)
	}

	// 1. Exactly one EnsureRepo with the URL/branch/sha/hints.
	if len(mw.repoInputs) != 1 {
		t.Fatalf("EnsureRepo call count = %d, want 1", len(mw.repoInputs))
	}
	repoIn := mw.repoInputs[0]
	if repoIn.URL != repoURL {
		t.Errorf("RepoInput.URL = %q, want %q", repoIn.URL, repoURL)
	}
	if repoIn.DefaultBranch != "main" {
		t.Errorf("RepoInput.DefaultBranch = %q, want %q", repoIn.DefaultBranch, "main")
	}
	if repoIn.CurrentHeadSHA != sha {
		t.Errorf("RepoInput.CurrentHeadSHA = %q, want %q", repoIn.CurrentHeadSHA, sha)
	}
	if len(repoIn.LanguageHints) != 2 || repoIn.LanguageHints[0] != "go" || repoIn.LanguageHints[1] != "ts" {
		t.Errorf("RepoInput.LanguageHints = %#v, want [go ts]", repoIn.LanguageHints)
	}

	// 2. Exactly one EnsureCommit, using the assigned RepoID,
	//    SHA, no parent, and the pinned timestamp.
	if len(mw.commitInputs) != 1 {
		t.Fatalf("EnsureCommit call count = %d, want 1", len(mw.commitInputs))
	}
	commitIn := mw.commitInputs[0]
	assigned := mw.repos[repoURL].ID
	if commitIn.RepoID != assigned {
		t.Errorf("CommitInput.RepoID = %s, want assigned %s", commitIn.RepoID, assigned)
	}
	if commitIn.SHA != sha {
		t.Errorf("CommitInput.SHA = %q, want %q", commitIn.SHA, sha)
	}
	if commitIn.ParentSHA != "" {
		t.Errorf("CommitInput.ParentSHA = %q, want empty", commitIn.ParentSHA)
	}
	if !commitIn.CommittedAt.Equal(pinnedNow) {
		t.Errorf("CommitInput.CommittedAt = %s, want %s", commitIn.CommittedAt, pinnedNow)
	}

	// 3. Exactly one InsertNode(kind=repo), parent empty,
	//    canonical signature == CanonicalRepoSig(URL).
	if len(mw.nodeInputs) != 1 {
		t.Fatalf("InsertNode call count = %d, want 1", len(mw.nodeInputs))
	}
	nodeIn := mw.nodeInputs[0]
	if nodeIn.Kind != "repo" {
		t.Errorf("InsertNode.Kind = %q, want repo", nodeIn.Kind)
	}
	if nodeIn.CanonicalSignature != CanonicalRepoSig(repoURL) {
		t.Errorf("InsertNode.CanonicalSignature = %q, want %q",
			nodeIn.CanonicalSignature, CanonicalRepoSig(repoURL))
	}
	if nodeIn.ParentNodeID != "" {
		t.Errorf("InsertNode.ParentNodeID = %q, want empty (repo is root)", nodeIn.ParentNodeID)
	}
	if nodeIn.FromSHA != sha {
		t.Errorf("InsertNode.FromSHA = %q, want %q", nodeIn.FromSHA, sha)
	}
	if nodeIn.RepoID != assigned {
		t.Errorf("InsertNode.RepoID = %s, want assigned %s", nodeIn.RepoID, assigned)
	}
	// attrs_json shape: Producer field must be present.
	var attrs map[string]any
	if err := json.Unmarshal(nodeIn.AttrsJSON, &attrs); err != nil {
		t.Errorf("InsertNode.AttrsJSON not valid JSON: %v", err)
	} else if attrs["producer"] != "repoindexer.full" {
		t.Errorf("InsertNode.AttrsJSON.producer = %v, want repoindexer.full", attrs["producer"])
	}

	// 4. RepoAncestry has deterministic RepoID (per arch S3.4)
	//    and matches the assigned RepoUUID + node ID.
	deterministic, _ := fingerprint.RepoIDFromURL(repoURL)
	if got.RepoID != deterministic {
		t.Errorf("RepoAncestry.RepoID = %s, want deterministic %s", got.RepoID, deterministic)
	}
	if got.RepoUUID != assigned.String() {
		t.Errorf("RepoAncestry.RepoUUID = %q, want assigned %q", got.RepoUUID, assigned.String())
	}
	if got.RepoNodeID == "" {
		t.Errorf("RepoAncestry.RepoNodeID is empty")
	}
	if got.CommitID != sha {
		t.Errorf("RepoAncestry.CommitID = %q, want %q", got.CommitID, sha)
	}
	if !got.CommitInserted {
		t.Errorf("RepoAncestry.CommitInserted = false, want true (cold ingest)")
	}

	// 5. Writer.Ready / Writer.Ancestry both reflect success.
	if !aw.Ready() {
		t.Errorf("AncestryWriter.Ready() = false, want true after EnsureRepoAndCommit")
	}
	if aw.Ancestry() != got {
		t.Errorf("AncestryWriter.Ancestry() != returned RepoAncestry")
	}
}

func TestAncestryWriter_EnsureRepoAndCommit_idempotentReplay(t *testing.T) {
	t.Parallel()
	mw := newMockRCNEWriter()
	const repoURL = "https://example.test/replay"
	const sha = "feedface00"
	aw := NewAncestryWriter(mw, repoURL, sha)
	first, err := aw.EnsureRepoAndCommit(context.Background(), "main", nil)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !first.CommitInserted {
		t.Fatalf("first call: CommitInserted = false, want true")
	}

	// A second writer for the same (URL, sha) hitting the same
	// backing mock should observe CommitInserted=false (replay)
	// and the same RepoNodeID (fingerprint dedupe).
	aw2 := NewAncestryWriter(mw, repoURL, sha)
	second, err := aw2.EnsureRepoAndCommit(context.Background(), "main", nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.CommitInserted {
		t.Errorf("second call: CommitInserted = true, want false on idempotent replay")
	}
	if second.RepoNodeID != first.RepoNodeID {
		t.Errorf("second call: RepoNodeID = %s, want %s (idempotent dedupe)",
			second.RepoNodeID, first.RepoNodeID)
	}
	if second.RepoUUID != first.RepoUUID {
		t.Errorf("second call: RepoUUID = %s, want %s (upsert preserves PK)",
			second.RepoUUID, first.RepoUUID)
	}
}

func TestAncestryWriter_EnsureRepoAndCommit_rejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		repoURL string
		sha     string
		wantSub string
	}{
		{"empty url", "", "abc", "empty repo URL"},
		{"empty sha", "https://example.test/x", "", "empty sha"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mw := newMockRCNEWriter()
			aw := NewAncestryWriter(mw, tc.repoURL, tc.sha)
			_, err := aw.EnsureRepoAndCommit(context.Background(), "main", nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
			if aw.Ready() {
				t.Errorf("Ready() = true after failed EnsureRepoAndCommit")
			}
		})
	}
}

func TestAncestryWriter_EnsureRepoAndCommit_propagatesStepErrors(t *testing.T) {
	t.Parallel()
	const repoURL = "https://example.test/err"
	const sha = "abc"

	// EnsureRepo failure.
	mwR := newMockRCNEWriter()
	mwR.errEnsureRepo = errors.New("boom-repo")
	awR := NewAncestryWriter(mwR, repoURL, sha)
	if _, err := awR.EnsureRepoAndCommit(context.Background(), "main", nil); err == nil ||
		!contains(err.Error(), "EnsureRepo") || !contains(err.Error(), "boom-repo") {
		t.Errorf("EnsureRepo error: got %v, want wrapped boom-repo", err)
	}
	if awR.Ready() {
		t.Errorf("Ready() = true after EnsureRepo failure")
	}
	if len(mwR.commitInputs) != 0 || len(mwR.nodeInputs) != 0 {
		t.Errorf("downstream steps invoked after EnsureRepo failure: commits=%d nodes=%d",
			len(mwR.commitInputs), len(mwR.nodeInputs))
	}

	// EnsureCommit failure.
	mwC := newMockRCNEWriter()
	mwC.errEnsureCommit = errors.New("boom-commit")
	awC := NewAncestryWriter(mwC, repoURL, sha)
	if _, err := awC.EnsureRepoAndCommit(context.Background(), "main", nil); err == nil ||
		!contains(err.Error(), "EnsureCommit") || !contains(err.Error(), "boom-commit") {
		t.Errorf("EnsureCommit error: got %v, want wrapped boom-commit", err)
	}
	if awC.Ready() {
		t.Errorf("Ready() = true after EnsureCommit failure")
	}
	if len(mwC.nodeInputs) != 0 {
		t.Errorf("InsertNode invoked after EnsureCommit failure: %d", len(mwC.nodeInputs))
	}

	// InsertNode failure.
	mwN := newMockRCNEWriter()
	mwN.errInsertNode = errors.New("boom-node")
	awN := NewAncestryWriter(mwN, repoURL, sha)
	if _, err := awN.EnsureRepoAndCommit(context.Background(), "main", nil); err == nil ||
		!contains(err.Error(), "InsertNode(repo)") || !contains(err.Error(), "boom-node") {
		t.Errorf("InsertNode error: got %v, want wrapped boom-node", err)
	}
	if awN.Ready() {
		t.Errorf("Ready() = true after InsertNode failure")
	}
}

func TestAncestryWriter_EnsureFile_requiresEnsureRepoAndCommit(t *testing.T) {
	t.Parallel()
	mw := newMockRCNEWriter()
	aw := NewAncestryWriter(mw, "https://example.test/y", "abc")
	_, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/a/file.go"})
	if !errors.Is(err, ErrAncestryNotReady) {
		t.Errorf("EnsureFile before EnsureRepoAndCommit: err = %v, want ErrAncestryNotReady", err)
	}
	if len(mw.nodeInputs) != 0 || len(mw.edgeInputs) != 0 {
		t.Errorf("EnsureFile wrote to the backing store before ready: nodes=%d edges=%d",
			len(mw.nodeInputs), len(mw.edgeInputs))
	}
}

func TestAncestryWriter_EnsureFile_buildsPackageThenFileAncestry(t *testing.T) {
	t.Parallel()
	mw := newMockRCNEWriter()
	const repoURL = "https://example.test/walk"
	const sha = "1234abcd"
	aw := NewAncestryWriter(mw, repoURL, sha)
	if _, err := aw.EnsureRepoAndCommit(context.Background(), "main", nil); err != nil {
		t.Fatalf("EnsureRepoAndCommit: %v", err)
	}

	// First file in pkg/a — mints package + repo->pkg edge,
	// then file + pkg->file edge.
	preNodes := len(mw.nodeInputs)
	preEdges := len(mw.edgeInputs)
	fa1, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/a/file1.go"})
	if err != nil {
		t.Fatalf("first EnsureFile: %v", err)
	}
	gotNodes := len(mw.nodeInputs) - preNodes
	gotEdges := len(mw.edgeInputs) - preEdges
	if gotNodes != 2 {
		t.Errorf("first file: InsertNode calls = %d, want 2 (package + file)", gotNodes)
	}
	if gotEdges != 2 {
		t.Errorf("first file: InsertEdge calls = %d, want 2 (repo->pkg + pkg->file)", gotEdges)
	}
	if !fa1.PackageNewlyInserted {
		t.Errorf("first file: PackageNewlyInserted = false, want true")
	}
	if !fa1.PackageEdgeInserted {
		t.Errorf("first file: PackageEdgeInserted = false, want true")
	}
	if !fa1.NewlyInserted {
		t.Errorf("first file: NewlyInserted = false, want true")
	}
	if !fa1.FileEdgeInserted {
		t.Errorf("first file: FileEdgeInserted = false, want true")
	}
	if fa1.PackageDir != "pkg/a" {
		t.Errorf("first file: PackageDir = %q, want %q", fa1.PackageDir, "pkg/a")
	}

	// Second file in pkg/a — package is cached; only the
	// file Node + pkg->file edge are inserted.
	preNodes = len(mw.nodeInputs)
	preEdges = len(mw.edgeInputs)
	fa2, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/a/file2.go"})
	if err != nil {
		t.Fatalf("second EnsureFile: %v", err)
	}
	if got := len(mw.nodeInputs) - preNodes; got != 1 {
		t.Errorf("second file (cached pkg): InsertNode calls = %d, want 1", got)
	}
	if got := len(mw.edgeInputs) - preEdges; got != 1 {
		t.Errorf("second file (cached pkg): InsertEdge calls = %d, want 1", got)
	}
	if fa2.PackageNewlyInserted {
		t.Errorf("second file: PackageNewlyInserted = true, want false (cache hit)")
	}
	if fa2.PackageEdgeInserted {
		t.Errorf("second file: PackageEdgeInserted = true, want false (cache hit)")
	}
	if fa2.PackageNodeID != fa1.PackageNodeID {
		t.Errorf("second file: PackageNodeID = %s, want %s (same cached pkg)",
			fa2.PackageNodeID, fa1.PackageNodeID)
	}

	// Third file in pkg/b — different directory, new package.
	preNodes = len(mw.nodeInputs)
	preEdges = len(mw.edgeInputs)
	fa3, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/b/file.go"})
	if err != nil {
		t.Fatalf("third EnsureFile: %v", err)
	}
	if got := len(mw.nodeInputs) - preNodes; got != 2 {
		t.Errorf("third file (new pkg): InsertNode calls = %d, want 2", got)
	}
	if got := len(mw.edgeInputs) - preEdges; got != 2 {
		t.Errorf("third file (new pkg): InsertEdge calls = %d, want 2", got)
	}
	if !fa3.PackageNewlyInserted {
		t.Errorf("third file: PackageNewlyInserted = false, want true (new dir)")
	}
	if fa3.PackageNodeID == fa1.PackageNodeID {
		t.Errorf("third file: PackageNodeID = %s, want different from pkg/a (%s)",
			fa3.PackageNodeID, fa1.PackageNodeID)
	}
	if fa3.PackageDir != "pkg/b" {
		t.Errorf("third file: PackageDir = %q, want %q", fa3.PackageDir, "pkg/b")
	}

	// Fourth file at the repo root — PackageDir == "".
	fa4, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "README.md"})
	if err != nil {
		t.Fatalf("fourth EnsureFile: %v", err)
	}
	if fa4.PackageDir != "" {
		t.Errorf("repo-root file: PackageDir = %q, want \"\"", fa4.PackageDir)
	}

	// Pin the canonical-signature values emitted across the
	// scan so a future drift in canonical.go that breaks
	// AncestryWriter's wire-compat surfaces here too.
	wantSigs := map[string]string{
		"repo":          CanonicalRepoSig(repoURL),
		"pkg:pkg/a":     CanonicalPackageSig(repoURL, "pkg/a"),
		"pkg:pkg/b":     CanonicalPackageSig(repoURL, "pkg/b"),
		"pkg:":          CanonicalPackageSig(repoURL, ""),
		"file:pkg/a/1":  CanonicalFileSig(repoURL, "pkg/a/file1.go"),
		"file:pkg/a/2":  CanonicalFileSig(repoURL, "pkg/a/file2.go"),
		"file:pkg/b":   CanonicalFileSig(repoURL, "pkg/b/file.go"),
		"file:README":  CanonicalFileSig(repoURL, "README.md"),
	}
	seenSigs := map[string]bool{}
	for _, in := range mw.nodeInputs {
		seenSigs[in.CanonicalSignature] = true
	}
	for label, sig := range wantSigs {
		if !seenSigs[sig] {
			t.Errorf("canonical signature %s = %q was not used by any InsertNode", label, sig)
		}
	}
}

func TestAncestryWriter_EnsureFile_idempotentReHits(t *testing.T) {
	t.Parallel()
	mw := newMockRCNEWriter()
	const repoURL = "https://example.test/idem"
	const sha = "feed01"
	aw := NewAncestryWriter(mw, repoURL, sha)
	if _, err := aw.EnsureRepoAndCommit(context.Background(), "main", nil); err != nil {
		t.Fatalf("EnsureRepoAndCommit: %v", err)
	}
	first, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/a/x.go"})
	if err != nil {
		t.Fatalf("first EnsureFile: %v", err)
	}

	// Repeat call from a FRESH writer (cache reset) against
	// the same backing mock: the underlying store dedupes the
	// Nodes / Edges, so NewlyInserted / *EdgeInserted flip to
	// false even though the writer's local cache misses.
	aw2 := NewAncestryWriter(mw, repoURL, sha)
	if _, err := aw2.EnsureRepoAndCommit(context.Background(), "main", nil); err != nil {
		t.Fatalf("EnsureRepoAndCommit (2): %v", err)
	}
	second, err := aw2.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/a/x.go"})
	if err != nil {
		t.Fatalf("second EnsureFile: %v", err)
	}
	if second.FileNodeID != first.FileNodeID {
		t.Errorf("idempotent re-hit: FileNodeID = %s, want %s", second.FileNodeID, first.FileNodeID)
	}
	if second.PackageNodeID != first.PackageNodeID {
		t.Errorf("idempotent re-hit: PackageNodeID = %s, want %s", second.PackageNodeID, first.PackageNodeID)
	}
	if second.NewlyInserted {
		t.Errorf("idempotent re-hit: NewlyInserted = true, want false")
	}
	if second.PackageNewlyInserted {
		t.Errorf("idempotent re-hit: PackageNewlyInserted = true, want false")
	}
	if second.PackageEdgeInserted {
		t.Errorf("idempotent re-hit: PackageEdgeInserted = true, want false")
	}
	if second.FileEdgeInserted {
		t.Errorf("idempotent re-hit: FileEdgeInserted = true, want false")
	}
}

func TestAncestryWriter_EnsureFile_propagatesStepErrors(t *testing.T) {
	t.Parallel()
	const repoURL = "https://example.test/efail"
	const sha = "abc"

	type setup struct {
		name string
		// errKind: "node-pkg" injects InsertNode failure on
		// first call (package); "edge-pkg" injects InsertEdge
		// failure on first call (repo->pkg); "node-file"
		// injects InsertNode failure after the package call;
		// "edge-file" injects after pkg->file.
		errKind string
	}
	for _, sc := range []setup{
		{"package node failure", "node-pkg"},
		{"package edge failure", "edge-pkg"},
		{"file node failure", "node-file"},
		{"file edge failure", "edge-file"},
	} {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			mw := newMockRCNEWriter()
			aw := NewAncestryWriter(mw, repoURL, sha)
			if _, err := aw.EnsureRepoAndCommit(context.Background(), "main", nil); err != nil {
				t.Fatalf("EnsureRepoAndCommit: %v", err)
			}
			// We schedule the error in a wrapped writer so the
			// step before runs cleanly and only the targeted
			// step fails.
			injectedErr := errors.New("boom")
			swap := &stepInjector{base: mw, kind: sc.errKind, err: injectedErr}
			aw.w = swap

			_, err := aw.EnsureFile(context.Background(), WalkFile{RelPath: "pkg/a/x.go"})
			if err == nil {
				t.Fatalf("EnsureFile: nil error, want wrapped %q", injectedErr)
			}
			if !errors.Is(err, injectedErr) {
				t.Errorf("EnsureFile error: got %v, want wrapping %v", err, injectedErr)
			}
		})
	}
}

// stepInjector wraps a mockRCNEWriter and injects err at the
// nth-targeted call. The first call of each kind is the
// package step, the second is the file step (matching the
// order EnsureFile drives them).
type stepInjector struct {
	base       *mockRCNEWriter
	kind       string
	err        error
	nodeSeen   int
	edgeSeen   int
}

func (s *stepInjector) EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	return s.base.EnsureRepo(ctx, in)
}
func (s *stepInjector) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	return s.base.EnsureCommit(ctx, in)
}
func (s *stepInjector) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	s.nodeSeen++
	if s.kind == "node-pkg" && in.Kind == "package" {
		return graphwriter.NodeRecord{}, s.err
	}
	if s.kind == "node-file" && in.Kind == "file" {
		return graphwriter.NodeRecord{}, s.err
	}
	return s.base.InsertNode(ctx, in)
}
func (s *stepInjector) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	s.edgeSeen++
	// First edge is repo->pkg (kind=contains, src=repoNode).
	// Second is pkg->file. The injector keys on call order
	// rather than fields because the mock's IDs are opaque.
	if s.kind == "edge-pkg" && s.edgeSeen == 1 {
		return graphwriter.EdgeRecord{}, s.err
	}
	if s.kind == "edge-file" && s.edgeSeen == 2 {
		return graphwriter.EdgeRecord{}, s.err
	}
	return s.base.InsertEdge(ctx, in)
}

// ----- helpers ----------------------------------------------------

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
