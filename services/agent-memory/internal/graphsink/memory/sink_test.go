package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// repoURL / repoID is the synthetic URL all test fixtures use.
const repoURL = "https://github.com/example/scan-target"

func mustRepoID(t *testing.T) fingerprint.RepoID {
	t.Helper()
	id, err := fingerprint.RepoIDFromURL(repoURL)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	return id
}

func ensureFixtureRepo(t *testing.T, s *Sink) graphwriter.RepoRecord {
	t.Helper()
	ctx := context.Background()
	rec, err := s.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            repoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go", "ts"},
		RepoID:         mustRepoID(t),
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	return rec
}

// Scenario: memory-idempotent.
// Given the same NodeInput inserted twice, the second call must
// return the cached id and the underlying slice length stays 1.
func TestInsertNode_IdempotentReemit(t *testing.T) {
	s := New(Options{})
	ensureFixtureRepo(t, s)
	ctx := context.Background()
	repoID := mustRepoID(t)

	in := graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/foo.go",
		FromSHA:            "deadbeef",
		AttrsJSON:          json.RawMessage(`{"lang":"go"}`),
	}
	first, err := s.InsertNode(ctx, in)
	if err != nil {
		t.Fatalf("first InsertNode: %v", err)
	}
	if !first.Inserted {
		t.Fatalf("first InsertNode should set Inserted=true")
	}
	second, err := s.InsertNode(ctx, in)
	if err != nil {
		t.Fatalf("second InsertNode: %v", err)
	}
	if second.Inserted {
		t.Fatalf("second InsertNode should set Inserted=false (cached re-emit)")
	}
	if second.NodeID != first.NodeID {
		t.Fatalf("re-emit returned different NodeID: first=%s second=%s",
			first.NodeID, second.NodeID)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("re-emit returned different fingerprint")
	}
	if got := len(s.nodes); got != 1 {
		t.Fatalf("nodes slice grew on re-emit: len=%d (want 1)", got)
	}
}

// TestInsertEdge_IdempotentReemit covers the edge fingerprint
// idempotent path: the same EdgeInput hashes identically and
// must NOT append a second row.
func TestInsertEdge_IdempotentReemit(t *testing.T) {
	s := New(Options{})
	ensureFixtureRepo(t, s)
	ctx := context.Background()
	repoID := mustRepoID(t)

	src, err := s.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method", CanonicalSignature: "go://m1",
		FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("src InsertNode: %v", err)
	}
	dst, err := s.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method", CanonicalSignature: "go://m2",
		FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("dst InsertNode: %v", err)
	}
	in := graphwriter.EdgeInput{
		RepoID: repoID, Kind: "static_calls",
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID,
		FromSHA: "deadbeef",
	}
	first, err := s.InsertEdge(ctx, in)
	if err != nil {
		t.Fatalf("first InsertEdge: %v", err)
	}
	if !first.Inserted {
		t.Fatalf("first InsertEdge should set Inserted=true")
	}
	second, err := s.InsertEdge(ctx, in)
	if err != nil {
		t.Fatalf("second InsertEdge: %v", err)
	}
	if second.Inserted {
		t.Fatalf("second InsertEdge should set Inserted=false")
	}
	if second.EdgeID != first.EdgeID {
		t.Fatalf("re-emit returned different EdgeID: first=%s second=%s",
			first.EdgeID, second.EdgeID)
	}
	if got := len(s.edges); got != 1 {
		t.Fatalf("edges slice grew on re-emit: len=%d (want 1)", got)
	}
}

// Scenario: json-export-key-order.
// The top-level keys of the export MUST be `repo`, `nodes`,
// `edges` in that order. We verify by streaming-decode of the
// raw bytes via json.Decoder.Token (the only way to assert key
// order without parsing into a map -- maps lose ordering).
func TestClose_JSONExportKeyOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.json")

	s := New(Options{ExportPath: path, Now: func() time.Time {
		return time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	}})
	ensureFixtureRepo(t, s)
	ctx := context.Background()
	repoID := mustRepoID(t)
	n, err := s.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "repo",
		CanonicalSignature: repoURL, FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode repo: %v", err)
	}
	_, err = s.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/foo.go",
		ParentNodeID:       n.NodeID,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode file: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	keys := topLevelObjectKeys(t, data)
	want := []string{"repo", "nodes", "edges"}
	if len(keys) != len(want) {
		t.Fatalf("got %d top-level keys (%v), want %d (%v)",
			len(keys), keys, len(want), want)
	}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("top-level key[%d] = %q, want %q (full: %v)", i, keys[i], k, keys)
		}
	}

	// Sanity-check repo block keys: id, url, sha must lead.
	var probe struct {
		Repo json.RawMessage `json:"repo"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("decode repo block: %v", err)
	}
	repoKeys := topLevelObjectKeys(t, probe.Repo)
	wantRepo := []string{"id", "url", "sha"}
	for i, k := range wantRepo {
		if i >= len(repoKeys) || repoKeys[i] != k {
			t.Errorf("repo key[%d] = %v (full: %v), want %q",
				i, safeIdx(repoKeys, i), repoKeys, k)
		}
	}
}

// Close is idempotent: a second Close must return nil and must
// NOT re-write the file.
func TestClose_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.json")
	s := New(Options{ExportPath: path})
	ensureFixtureRepo(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile second: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("Close re-wrote file on second call")
	}
}

// Close without ExportPath must NOT touch disk and must NOT
// require EnsureRepo. This is the path the CLI takes when the
// scan ran in --store=memory mode without --export.
func TestClose_NoExportPath_NoIO(t *testing.T) {
	s := New(Options{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close empty sink: %v", err)
	}
}

// Scenario: roundtrip-via-loadexport.
// A scan written via Close + reloaded via LoadExport must
// produce a Reader whose Node and Edge counts match the
// original sink.
func TestLoadExport_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.json")

	src := New(Options{ExportPath: path})
	ensureFixtureRepo(t, src)
	ctx := context.Background()
	repoID := mustRepoID(t)

	repoNode, err := src.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "repo",
		CanonicalSignature: repoURL, FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode repo: %v", err)
	}
	pkgNode, err := src.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "package",
		CanonicalSignature: "pkg://example",
		ParentNodeID:       repoNode.NodeID,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode pkg: %v", err)
	}
	fileNode, err := src.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/foo.go",
		ParentNodeID:       pkgNode.NodeID,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertNode file: %v", err)
	}
	_, err = src.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repoID, Kind: "contains",
		SrcNodeID: repoNode.NodeID, DstNodeID: pkgNode.NodeID,
		FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertEdge contains repo->pkg: %v", err)
	}
	_, err = src.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repoID, Kind: "contains",
		SrcNodeID: pkgNode.NodeID, DstNodeID: fileNode.NodeID,
		FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("InsertEdge contains pkg->file: %v", err)
	}
	wantNodes := len(src.nodes)
	wantEdges := len(src.edges)
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	loaded, err := LoadExport(path)
	if err != nil {
		t.Fatalf("LoadExport: %v", err)
	}
	if got := len(loaded.nodes); got != wantNodes {
		t.Errorf("loaded nodes len=%d, want %d", got, wantNodes)
	}
	if got := len(loaded.edges); got != wantEdges {
		t.Errorf("loaded edges len=%d, want %d", got, wantEdges)
	}

	// Reader surface: ListNodes returns the same count.
	gotNodes, err := loaded.ListNodes(ctx, repoID, nil, graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("loaded ListNodes: %v", err)
	}
	if len(gotNodes) != wantNodes {
		t.Errorf("ListNodes returned %d, want %d", len(gotNodes), wantNodes)
	}
	// ListEdgesFrom on the repo node should return the
	// contains-edge to the package node.
	outbound, err := loaded.ListEdgesFrom(ctx, repoNode.NodeID, nil, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("loaded ListEdgesFrom: %v", err)
	}
	if len(outbound) != 1 {
		t.Errorf("repo outbound edges = %d, want 1", len(outbound))
	}
	// ListRepos surfaces the SHA from the original export.
	repos, err := loaded.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("loaded ListRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].URL != repoURL || repos[0].SHA != "deadbeef" {
		t.Errorf("ListRepos = %+v, want one entry with url=%s sha=deadbeef", repos, repoURL)
	}

	// Fingerprint parity: a node fetched from the loaded sink
	// matches the writer's fingerprint computed independently.
	wantFP, err := fingerprint.NodeFingerprint(repoID, "file", "file://example/foo.go", "deadbeef")
	if err != nil {
		t.Fatalf("NodeFingerprint: %v", err)
	}
	got, err := loaded.LookupBySignature(ctx, repoID, "file", "file://example/foo.go", graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("LookupBySignature: %v", err)
	}
	if got.Fingerprint != wantFP {
		t.Errorf("loaded fingerprint mismatch: got %s want %s",
			got.Fingerprint.Hex(), wantFP.Hex())
	}
}

// EnsureRepo rejects a zero RepoID per the workstream brief.
func TestEnsureRepo_RejectsZeroRepoID(t *testing.T) {
	s := New(Options{})
	_, err := s.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL: repoURL,
	})
	if err == nil {
		t.Fatalf("EnsureRepo with zero RepoID should error")
	}
	if !strings.Contains(err.Error(), "zero RepoID") {
		t.Errorf("error %q does not mention zero RepoID", err)
	}
}

// EnsureRepo rejects a second call with a different RepoID --
// single-repo invariant.
func TestEnsureRepo_RejectsRepoMismatch(t *testing.T) {
	s := New(Options{})
	ensureFixtureRepo(t, s)
	other, err := fingerprint.RepoIDFromURL("https://github.com/other/repo")
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	_, err = s.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL: "https://github.com/other/repo", RepoID: other,
	})
	if err == nil {
		t.Fatalf("EnsureRepo with mismatched RepoID should error")
	}
}

// After Close the writer surface returns ErrClosed.
func TestClose_ReturnsErrClosedAfterClose(t *testing.T) {
	s := New(Options{})
	ensureFixtureRepo(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := s.InsertNode(context.Background(), graphwriter.NodeInput{
		RepoID:             mustRepoID(t),
		Kind:               "file",
		CanonicalSignature: "file://x",
		FromSHA:            "deadbeef",
	})
	if err == nil {
		t.Fatalf("InsertNode after Close should error")
	}
}

// ---- helpers --------------------------------------------------

// topLevelObjectKeys streams the first object in `data` via
// json.Decoder.Token and returns the ordered list of its keys.
// Using a map[string]json.RawMessage would lose ordering, so
// this is the load-bearing assertion mechanism for the
// architecture-pinned `repo, nodes, edges` ordering test.
func topLevelObjectKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("Token open: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("expected '{', got %v", tok)
	}
	var keys []string
	for dec.More() {
		// First token is the key string.
		ktok, err := dec.Token()
		if err != nil {
			t.Fatalf("Token key: %v", err)
		}
		k, ok := ktok.(string)
		if !ok {
			t.Fatalf("expected key string, got %v", ktok)
		}
		keys = append(keys, k)
		// Consume the value, including any nested structure.
		consumeValue(t, dec)
	}
	return keys
}

// consumeValue reads exactly one JSON value from dec, recursing
// into nested objects / arrays so the outer dec.More() advances
// across the parent object correctly.
func consumeValue(t *testing.T, dec *json.Decoder) {
	t.Helper()
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("Token value: %v", err)
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			for dec.More() {
				if _, err := dec.Token(); err != nil { // key
					t.Fatalf("Token nested key: %v", err)
				}
				consumeValue(t, dec)
			}
			if _, err := dec.Token(); err != nil { // closing }
				t.Fatalf("Token closing }: %v", err)
			}
		case '[':
			for dec.More() {
				consumeValue(t, dec)
			}
			if _, err := dec.Token(); err != nil { // closing ]
				t.Fatalf("Token closing ]: %v", err)
			}
		}
	}
}

func safeIdx(s []string, i int) any {
	if i < 0 || i >= len(s) {
		return "<missing>"
	}
	return s[i]
}
