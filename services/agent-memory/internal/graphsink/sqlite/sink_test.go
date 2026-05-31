//go:build cgo

package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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

func TestEnsureRepoDeterministicRepoIDPersisted(t *testing.T) {
	// Item 1 from evaluator feedback: when the caller supplies a
	// deterministic RepoID (the fingerprint.RepoIDFromURL path
	// the codeintel CLI uses), the SQLite Sink must persist that
	// exact UUID as the row's repo_id so node / edge fingerprints
	// match across backends.
	ctx := context.Background()
	sink := openTempSink(t)

	url := "https://example.invalid/deterministic.git"
	wantID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}

	rec, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		RepoID:         wantID,
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if !rec.Inserted {
		t.Fatalf("first EnsureRepo: want Inserted=true")
	}
	if rec.ID != wantID {
		t.Fatalf("repo_id drift: got %s, want %s (deterministic)", rec.ID, wantID)
	}
	if rec.RepoID != wantID.String() {
		t.Fatalf("repo_id text drift: got %q, want %q", rec.RepoID, wantID.String())
	}

	// Second EnsureRepo with the same URL but a different
	// (zero) supplied RepoID must still return the previously
	// persisted deterministic ID; the legacy-collision rule
	// keeps the PK stable on conflict.
	rec2, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "cafebabe",
	})
	if err != nil {
		t.Fatalf("second EnsureRepo: %v", err)
	}
	if rec2.Inserted {
		t.Fatalf("second EnsureRepo: want Inserted=false")
	}
	if rec2.ID != wantID {
		t.Fatalf("repo_id drift on conflict: got %s, want %s", rec2.ID, wantID)
	}
}

func TestNodeFingerprintParityWithDeterministicRepoID(t *testing.T) {
	// Item 2 from evaluator feedback: a node inserted into SQLite
	// with a deterministic RepoID must carry the SAME fingerprint
	// an outside re-computation (the canonical helper) produces,
	// because Postgres uses that same helper. If the SQLite Sink
	// silently substituted a random repo_id, fingerprints would
	// diverge and cross-backend dedupe would break.
	ctx := context.Background()
	sink := openTempSink(t)

	url := "https://example.invalid/parity.git"
	repoID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}

	repo, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: "shaParity",
		RepoID:         repoID,
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if repo.ID != repoID {
		t.Fatalf("EnsureRepo did not persist deterministic RepoID: got %s want %s",
			repo.ID, repoID)
	}

	const (
		kind = "method"
		sig  = "pkg.Mod.Func()"
		sha  = "shaParity"
	)
	want, err := fingerprint.NodeFingerprint(repoID, kind, sig, sha)
	if err != nil {
		t.Fatalf("NodeFingerprint: %v", err)
	}

	rec, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               kind,
		CanonicalSignature: sig,
		FromSHA:            sha,
	})
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}
	if rec.Fingerprint != want {
		t.Fatalf("fingerprint parity broken: got %x, want %x", rec.Fingerprint, want)
	}

	// Also verify the row landed under the deterministic
	// repo_id, not under a synthesised UUID: the unique key
	// (repo_id, fingerprint) is the cross-backend dedupe primitive.
	var storedRepoID string
	var storedFP []byte
	if err := sqlQueryOne(ctx, sink,
		`SELECT repo_id, fingerprint FROM node WHERE node_id = ?`,
		rec.NodeID,
	).Scan(&storedRepoID, &storedFP); err != nil {
		t.Fatalf("verify node: %v", err)
	}
	if storedRepoID != repoID.String() {
		t.Fatalf("node.repo_id drift: got %q, want %q", storedRepoID, repoID.String())
	}
	if len(storedFP) != 32 {
		t.Fatalf("node.fingerprint length: got %d, want 32", len(storedFP))
	}
	for i := range want {
		if storedFP[i] != want[i] {
			t.Fatalf("stored fingerprint mismatch at byte %d", i)
		}
	}
}

func TestEdgeFingerprintParityWithDeterministicRepoID(t *testing.T) {
	// Edge fingerprints also embed the repo_id (via the src/dst
	// node fingerprints, which embed it themselves). The parity
	// check below builds the fingerprint the same way an
	// out-of-process verifier would and asserts SQLite stored it
	// unchanged.
	ctx := context.Background()
	sink := openTempSink(t)

	url := "https://example.invalid/edge-parity.git"
	repoID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: url, DefaultBranch: "main", CurrentHeadSHA: "s", RepoID: repoID,
	}); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	src, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method", CanonicalSignature: "A.a()", FromSHA: "s",
	})
	if err != nil {
		t.Fatalf("InsertNode src: %v", err)
	}
	dst, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method", CanonicalSignature: "A.b()", FromSHA: "s",
	})
	if err != nil {
		t.Fatalf("InsertNode dst: %v", err)
	}

	want, err := fingerprint.EdgeFingerprint(
		repoID, "static_calls", src.Fingerprint, dst.Fingerprint, "s",
	)
	if err != nil {
		t.Fatalf("EdgeFingerprint: %v", err)
	}

	got, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    repoID,
		Kind:      "static_calls",
		SrcNodeID: src.NodeID,
		DstNodeID: dst.NodeID,
		FromSHA:   "s",
	})
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	if got.Fingerprint != want {
		t.Fatalf("edge fingerprint parity broken: got %x, want %x", got.Fingerprint, want)
	}
}

func TestMergeDefaultPragmasPreservesCallerQuery(t *testing.T) {
	// Item 3 from iter-1 feedback (soft-default behaviour after
	// iter-3 split): MergeDefaultPragmasForTest covers ONLY the
	// soft defaults (`_journal_mode`, `_busy_timeout`). The
	// mandatory `_foreign_keys=on` enforcement is verified by
	// TestBuildDSNForcesForeignKeysOn / TestForeignKeysEnforcedRuntime.
	got := sqlite.MergeDefaultPragmasForTest("graph.db?cache=shared")
	wantSubstrings := []string{
		"cache=shared",
		"_journal_mode=WAL",
		"_busy_timeout=5000",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(got, sub) {
			t.Fatalf("merged DSN %q missing %q", got, sub)
		}
	}

	// Caller-supplied SOFT-default pragmas must override.
	got2 := sqlite.MergeDefaultPragmasForTest("graph.db?_journal_mode=DELETE")
	if strings.Contains(got2, "_journal_mode=WAL") {
		t.Fatalf("caller override lost: %q still contains _journal_mode=WAL", got2)
	}
	if !strings.Contains(got2, "_journal_mode=DELETE") {
		t.Fatalf("caller override stripped: %q", got2)
	}
}

func TestBuildDSNForcesForeignKeysOn(t *testing.T) {
	// Item 2 from iter-2 feedback: the package documents that
	// foreign-key enforcement is always on, so `_foreign_keys=off`
	// in a caller-supplied DSN must be stripped, not honoured.
	cases := []struct {
		name        string
		in          string
		mustContain []string
		mustNotHave []string
	}{
		{
			name: "bare-path-no-query",
			in:   "graph.db",
			mustContain: []string{
				"_foreign_keys=on", "_journal_mode=WAL", "_busy_timeout=5000",
			},
		},
		{
			name: "caller-cache-shared",
			in:   "graph.db?cache=shared",
			mustContain: []string{
				"cache=shared",
				"_foreign_keys=on", "_journal_mode=WAL", "_busy_timeout=5000",
			},
		},
		{
			name: "caller-tries-to-disable-fk",
			in:   "graph.db?_foreign_keys=off",
			mustContain: []string{"_foreign_keys=on"},
			mustNotHave: []string{"_foreign_keys=off"},
		},
		{
			name: "caller-tries-to-disable-fk-with-other-keys",
			in:   "graph.db?cache=shared&_foreign_keys=off&_journal_mode=DELETE",
			mustContain: []string{
				"cache=shared",
				"_journal_mode=DELETE",
				"_foreign_keys=on",
			},
			mustNotHave: []string{"_foreign_keys=off"},
		},
		{
			name: "caller-fk-on-passes-through",
			in:   "graph.db?_foreign_keys=on",
			mustContain: []string{"_foreign_keys=on"},
			mustNotHave: []string{"_foreign_keys=on&_foreign_keys=on"}, // no duplication
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqlite.BuildDSNForTest(tc.in)
			for _, s := range tc.mustContain {
				if !strings.Contains(got, s) {
					t.Fatalf("DSN %q missing %q (got %q)", tc.in, s, got)
				}
			}
			for _, s := range tc.mustNotHave {
				if strings.Contains(got, s) {
					t.Fatalf("DSN %q must not contain %q (got %q)", tc.in, s, got)
				}
			}
		})
	}
}

func TestForeignKeysEnforcedRuntime(t *testing.T) {
	// End-to-end: open a Sink with a DSN that ATTEMPTS to
	// disable foreign keys, then query `PRAGMA foreign_keys`
	// through the live handle. The pragma MUST report 1 (on)
	// regardless of the caller's input.
	ctx := context.Background()
	path := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "fk.db")) + "?_foreign_keys=off"
	sink, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	var fk int
	if err := sqlite.DBForTest(sink).
		QueryRowContext(ctx, `PRAGMA foreign_keys`).
		Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 (Open must force FK on regardless of DSN)", fk)
	}

	// And an actual FK-violating INSERT must be rejected.
	// repo_commit references repo(repo_id) ON DELETE RESTRICT;
	// inserting a commit row with a non-existent repo_id must
	// fail with a constraint error if FK enforcement is live.
	_, err = sqlite.DBForTest(sink).ExecContext(ctx,
		`INSERT INTO repo_commit (repo_id, sha, committed_at) VALUES (?, ?, ?)`,
		"00000000-0000-0000-0000-000000000000", "abc", time.Now().UnixMilli(),
	)
	if err == nil {
		t.Fatalf("INSERT with bogus repo_id succeeded; FK enforcement is NOT active")
	}
}

// sqlQueryOne is a tiny test helper that runs a single-row
// query against the Sink's underlying *sql.DB through the
// package-private accessor declared in `export_test.go`.
func sqlQueryOne(ctx context.Context, s *sqlite.Sink, q string, args ...any) *sql.Row {
	return sqlite.DBForTest(s).QueryRowContext(ctx, q, args...)
}
