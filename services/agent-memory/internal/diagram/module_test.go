package diagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// fakeReader is an in-memory `graphsink.Reader` test double. It
// holds explicit slices of Nodes + Edges and filters them on
// each call, so a test can assemble a fixture graph row by row
// without standing up SQLite or the in-process memory sink (and
// without invoking the dispatcher). The fake intentionally
// implements ONLY the methods BuildModuleDiagram touches
// (ListRepos, ListNodes, ListEdgesFrom); the call-chain
// methods are stubbed to return an error so a misuse surfaces
// loudly.
type fakeReader struct {
	repos []graphreader.RepoSummary
	nodes []graphreader.Node
	edges []graphreader.Edge

	// nodeLimitOverride / edgeLimitOverride force the
	// corresponding list call to return exactly the supplied
	// count (>= 0) regardless of the underlying fixture. Used
	// by the truncation test to simulate hitting MaxListLimit
	// without needing to materialise 10 000 fixture rows.
	nodeLimitOverride map[string]int // key: kindFilter joined with `+`
	edgeLimitOverride map[string]int // key: srcNodeID
}

func (f *fakeReader) ListRepos(_ context.Context, _ graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	out := make([]graphreader.RepoSummary, len(f.repos))
	copy(out, f.repos)
	return out, nil
}

func (f *fakeReader) ListNodes(
	_ context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	filter graphreader.ListNodesFilter,
	_ graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	wantKinds := stringSet(kinds)
	var out []graphreader.Node
	for _, n := range f.nodes {
		if n.RepoID != repoID.String() {
			continue
		}
		if len(wantKinds) > 0 && !wantKinds[n.Kind] {
			continue
		}
		if filter.ParentNodeID != "" && n.ParentNodeID != filter.ParentNodeID {
			continue
		}
		if filter.FromSHA != "" && n.FromSHA != filter.FromSHA {
			continue
		}
		if filter.CanonicalSignature != "" && n.CanonicalSignature != filter.CanonicalSignature {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].CanonicalSignature != out[j].CanonicalSignature {
			return out[i].CanonicalSignature < out[j].CanonicalSignature
		}
		return out[i].NodeID < out[j].NodeID
	})
	if f.nodeLimitOverride != nil {
		if cap, ok := f.nodeLimitOverride[strings.Join(kinds, "+")]; ok {
			if cap < len(out) {
				out = out[:cap]
			}
			// Pad with synthetic dummies so the projector sees
			// `len(out) >= MaxListLimit` and trips truncation.
			for len(out) < cap {
				out = append(out, graphreader.Node{
					NodeID: fmt.Sprintf("pad-%d", len(out)),
					RepoID: repoID.String(), Kind: "package",
					CanonicalSignature: fmt.Sprintf("pad::%d", len(out)),
				})
			}
		}
	}
	return out, nil
}

func (f *fakeReader) ListEdgesFrom(
	_ context.Context, srcNodeID string, kinds []string, _ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	wantKinds := stringSet(kinds)
	var out []graphreader.Edge
	for _, e := range f.edges {
		if e.SrcNodeID != srcNodeID {
			continue
		}
		if len(wantKinds) > 0 && !wantKinds[e.Kind] {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].EdgeID < out[j].EdgeID
	})
	if f.edgeLimitOverride != nil {
		if cap, ok := f.edgeLimitOverride[srcNodeID]; ok {
			if cap < len(out) {
				out = out[:cap]
			}
			for len(out) < cap {
				out = append(out, graphreader.Edge{
					EdgeID: fmt.Sprintf("pad-edge-%d", len(out)),
					Kind:   "imports", SrcNodeID: srcNodeID,
					DstNodeID: "pad-dst",
				})
			}
		}
	}
	return out, nil
}

func (f *fakeReader) ListEdgesTo(
	_ context.Context, _ string, _ []string, _ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return nil, errors.New("fakeReader: ListEdgesTo not implemented")
}

func (f *fakeReader) GetNode(
	_ context.Context, _ string, _ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	return graphreader.Node{}, errors.New("fakeReader: GetNode not implemented")
}

func (f *fakeReader) LookupBySignature(
	_ context.Context, _ fingerprint.RepoID, _ string, _ string, _ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	return graphreader.Node{}, errors.New("fakeReader: LookupBySignature not implemented")
}

func stringSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// fixedTime pins nowFunc for deterministic envelope GeneratedAt.
func fixedTime(t *testing.T) (restore func()) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time {
		return time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	}
	return func() { nowFunc = prev }
}

// findEdge locates a single edge by (from, to, kind) and fails
// the test when zero or more-than-one matches exist.
func findEdge(t *testing.T, edges []Edge, from, to, kind string) Edge {
	t.Helper()
	var hits []Edge
	for _, e := range edges {
		if e.From == from && e.To == to && e.Kind == kind {
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 {
		t.Fatalf("expected an edge %s -[%s]-> %s, got none in %d edges",
			from, kind, to, len(edges))
	}
	if len(hits) > 1 {
		t.Fatalf("expected exactly one edge %s -[%s]-> %s, got %d",
			from, kind, to, len(hits))
	}
	return hits[0]
}

// countByKind groups edges by `kind` for shape assertions.
func countByKind(edges []Edge) map[string]int {
	out := make(map[string]int)
	for _, e := range edges {
		out[e.Kind]++
	}
	return out
}

// newFixtureRepo creates a RepoID + matching RepoSummary for use
// in test fixtures. Returns the RepoID for ListNodes calls and
// the URL/sha that the projector should surface on the envelope.
func newFixtureRepo(t *testing.T, url string) (fingerprint.RepoID, graphreader.RepoSummary) {
	t.Helper()
	repoID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("RepoIDFromURL(%q): %v", url, err)
	}
	return repoID, graphreader.RepoSummary{
		RepoID:      repoID.String(),
		URL:         url,
		SHA:         "9d1a2c47b3e95f80a162b08c5d3f4e71",
		GeneratedAt: time.Date(2026, 5, 30, 11, 30, 0, 0, time.UTC),
	}
}

// TestBuildModuleDiagram_TreeShape covers the implementation-plan
// scenario `module-tree-shape`: a fixture with 1 repo, 2 packages,
// 5 files at granularity=package yields 3 Nodes (repo + 2 pkgs)
// and 2 `contains` edges (repo->pkg/a, repo->pkg/b).
func TestBuildModuleDiagram_TreeShape(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/app")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: []graphreader.Node{
			// repo
			{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
				CanonicalSignature: "https://example.com/example/app::repo"},
			// packages
			{NodeID: "node-pkg-a", RepoID: repoIDStr, Kind: "package",
				CanonicalSignature: "example/app/pkg/a",
				ParentNodeID:       repoNodeID,
				AttrsJSON:          json.RawMessage(`{"language":"go"}`)},
			{NodeID: "node-pkg-b", RepoID: repoIDStr, Kind: "package",
				CanonicalSignature: "example/app/pkg/b",
				ParentNodeID:       repoNodeID,
				AttrsJSON:          json.RawMessage(`{"language":"go"}`)},
			// 5 files (3 in a, 2 in b)
			{NodeID: "node-file-a1", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "example/app/pkg/a/one.go", ParentNodeID: "node-pkg-a"},
			{NodeID: "node-file-a2", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "example/app/pkg/a/two.go", ParentNodeID: "node-pkg-a"},
			{NodeID: "node-file-a3", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "example/app/pkg/a/three.go", ParentNodeID: "node-pkg-a"},
			{NodeID: "node-file-b1", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "example/app/pkg/b/one.go", ParentNodeID: "node-pkg-b"},
			{NodeID: "node-file-b2", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "example/app/pkg/b/two.go", ParentNodeID: "node-pkg-b"},
		},
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityPackage)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}

	if d.Diagram != KindModule {
		t.Errorf("Diagram kind = %q, want %q", d.Diagram, KindModule)
	}
	if d.LayoutHint != LayoutHierarchicalTopDown {
		t.Errorf("LayoutHint = %q, want %q", d.LayoutHint, LayoutHierarchicalTopDown)
	}
	if d.Repo.URL != summary.URL || d.Repo.SHA != summary.SHA {
		t.Errorf("Repo metadata = %+v, want url=%q sha=%q", d.Repo, summary.URL, summary.SHA)
	}
	if d.Truncated {
		t.Errorf("Truncated = true on a small fixture, want false")
	}

	// 3 Nodes: repo + 2 packages. No file / class Nodes at
	// granularity=package.
	if got := len(d.Nodes); got != 3 {
		t.Fatalf("len(Nodes) = %d, want 3 (repo + 2 packages): %+v", got, d.Nodes)
	}
	if d.Nodes[0].Kind != "repo" || d.Nodes[0].ID != repoNodeID {
		t.Errorf("Nodes[0] = %+v, want repo Node", d.Nodes[0])
	}
	for _, want := range []string{"pkg:example/app/pkg/a", "pkg:example/app/pkg/b"} {
		found := false
		for _, n := range d.Nodes {
			if n.ID == want {
				found = true
				if n.Kind != "package" {
					t.Errorf("Node %s kind = %q, want package", want, n.Kind)
				}
				if n.Group != repoNodeID {
					t.Errorf("Node %s group = %q, want %q", want, n.Group, repoNodeID)
				}
				if n.Language != "go" {
					t.Errorf("Node %s language = %q, want go", want, n.Language)
				}
			}
		}
		if !found {
			t.Errorf("expected synthetic package id %s in Nodes %+v", want, d.Nodes)
		}
	}

	// Exactly 2 contains edges (repo -> pkg/a, repo -> pkg/b).
	kindCounts := countByKind(d.Edges)
	if kindCounts["contains"] != 2 {
		t.Errorf("contains-edge count = %d, want 2 (got edges %+v)", kindCounts["contains"], d.Edges)
	}
	if kindCounts["imports"] != 0 {
		t.Errorf("imports-edge count = %d, want 0 (no import edges in this fixture)",
			kindCounts["imports"])
	}
	findEdge(t, d.Edges, repoNodeID, "pkg:example/app/pkg/a", "contains")
	findEdge(t, d.Edges, repoNodeID, "pkg:example/app/pkg/b", "contains")

	// Stats reflect the surfaced counts.
	if d.Stats.NodeCount != len(d.Nodes) || d.Stats.EdgeCount != len(d.Edges) {
		t.Errorf("Stats counts mismatch: %+v vs nodes=%d edges=%d",
			d.Stats, len(d.Nodes), len(d.Edges))
	}
	if d.Stats.CappedAt != MaxListLimit {
		t.Errorf("Stats.CappedAt = %d, want %d", d.Stats.CappedAt, MaxListLimit)
	}
}

// TestBuildModuleDiagram_ImportsRollupWeight covers the
// implementation-plan scenario `imports-rollup-weight`: 3 files
// in pkg/a each import 2 files in pkg/b yields exactly one
// `imports` edge pkg:a -> pkg:b with weight=6.
func TestBuildModuleDiagram_ImportsRollupWeight(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/rollup")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	nodes := []graphreader.Node{
		{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
			CanonicalSignature: "https://example.com/example/rollup::repo"},
		{NodeID: "node-pkg-a", RepoID: repoIDStr, Kind: "package",
			CanonicalSignature: "rollup/pkg/a", ParentNodeID: repoNodeID},
		{NodeID: "node-pkg-b", RepoID: repoIDStr, Kind: "package",
			CanonicalSignature: "rollup/pkg/b", ParentNodeID: repoNodeID},
	}
	// 3 files in pkg/a, 2 files in pkg/b
	for i := 1; i <= 3; i++ {
		nodes = append(nodes, graphreader.Node{
			NodeID: fmt.Sprintf("node-file-a%d", i),
			RepoID: repoIDStr, Kind: "file",
			CanonicalSignature: fmt.Sprintf("rollup/pkg/a/f%d.go", i),
			ParentNodeID:       "node-pkg-a",
		})
	}
	for i := 1; i <= 2; i++ {
		nodes = append(nodes, graphreader.Node{
			NodeID: fmt.Sprintf("node-file-b%d", i),
			RepoID: repoIDStr, Kind: "file",
			CanonicalSignature: fmt.Sprintf("rollup/pkg/b/f%d.go", i),
			ParentNodeID:       "node-pkg-b",
		})
	}

	// 3 * 2 = 6 imports edges (file a_i -> file b_j).
	var edges []graphreader.Edge
	for i := 1; i <= 3; i++ {
		for j := 1; j <= 2; j++ {
			edges = append(edges, graphreader.Edge{
				EdgeID:    fmt.Sprintf("e-a%d-b%d", i, j),
				RepoID:    repoIDStr,
				Kind:      "imports",
				SrcNodeID: fmt.Sprintf("node-file-a%d", i),
				DstNodeID: fmt.Sprintf("node-file-b%d", j),
			})
		}
	}

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: nodes,
		edges: edges,
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityPackage)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}

	// Exactly ONE imports edge: pkg:a -> pkg:b with weight=6.
	var importsEdges []Edge
	for _, e := range d.Edges {
		if e.Kind == "imports" {
			importsEdges = append(importsEdges, e)
		}
	}
	if len(importsEdges) != 1 {
		t.Fatalf("expected exactly 1 rolled-up imports edge, got %d: %+v",
			len(importsEdges), importsEdges)
	}
	got := importsEdges[0]
	wantFrom := "pkg:rollup/pkg/a"
	wantTo := "pkg:rollup/pkg/b"
	if got.From != wantFrom || got.To != wantTo {
		t.Errorf("imports edge endpoints = %s -> %s, want %s -> %s",
			got.From, got.To, wantFrom, wantTo)
	}
	if got.Weight != 6 {
		t.Errorf("imports edge weight = %d, want 6", got.Weight)
	}
	if got.Label != "imports" {
		t.Errorf("imports edge label = %q, want %q", got.Label, "imports")
	}
}

// TestBuildModuleDiagram_GranularityFile covers the
// implementation-plan scenario `granularity-file`: same fixture,
// granularity="file" surfaces file Nodes AND preserves per-file
// imports edges (NOT rolled up).
func TestBuildModuleDiagram_GranularityFile(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/rollup")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	nodes := []graphreader.Node{
		{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
			CanonicalSignature: "https://example.com/example/rollup::repo"},
		{NodeID: "node-pkg-a", RepoID: repoIDStr, Kind: "package",
			CanonicalSignature: "rollup/pkg/a", ParentNodeID: repoNodeID},
		{NodeID: "node-pkg-b", RepoID: repoIDStr, Kind: "package",
			CanonicalSignature: "rollup/pkg/b", ParentNodeID: repoNodeID},
	}
	for i := 1; i <= 3; i++ {
		nodes = append(nodes, graphreader.Node{
			NodeID: fmt.Sprintf("node-file-a%d", i),
			RepoID: repoIDStr, Kind: "file",
			CanonicalSignature: fmt.Sprintf("rollup/pkg/a/f%d.go", i),
			ParentNodeID:       "node-pkg-a",
		})
	}
	for i := 1; i <= 2; i++ {
		nodes = append(nodes, graphreader.Node{
			NodeID: fmt.Sprintf("node-file-b%d", i),
			RepoID: repoIDStr, Kind: "file",
			CanonicalSignature: fmt.Sprintf("rollup/pkg/b/f%d.go", i),
			ParentNodeID:       "node-pkg-b",
		})
	}
	var edges []graphreader.Edge
	for i := 1; i <= 3; i++ {
		for j := 1; j <= 2; j++ {
			edges = append(edges, graphreader.Edge{
				EdgeID:    fmt.Sprintf("e-a%d-b%d", i, j),
				RepoID:    repoIDStr,
				Kind:      "imports",
				SrcNodeID: fmt.Sprintf("node-file-a%d", i),
				DstNodeID: fmt.Sprintf("node-file-b%d", j),
			})
		}
	}

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: nodes,
		edges: edges,
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityFile)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}

	// Layout hint stays hierarchical-top-down regardless of
	// granularity (architecture S4.4 + implementation-plan).
	if d.LayoutHint != LayoutHierarchicalTopDown {
		t.Errorf("LayoutHint = %q, want %q", d.LayoutHint, LayoutHierarchicalTopDown)
	}

	// File Nodes MUST appear in the diagram.
	var fileNodeCount int
	for _, n := range d.Nodes {
		if n.Kind == "file" {
			fileNodeCount++
		}
	}
	if fileNodeCount != 5 {
		t.Errorf("file-Node count = %d, want 5 (3 in pkg/a + 2 in pkg/b)", fileNodeCount)
	}

	// Imports edges MUST be preserved per-file (file -> file), NOT
	// rolled up to pkg -> pkg. Expect exactly 6 imports edges,
	// each weight 1, and each referencing its original file
	// endpoints.
	importsEdges := 0
	for _, e := range d.Edges {
		if e.Kind != "imports" {
			continue
		}
		importsEdges++
		if e.Weight != 1 {
			t.Errorf("per-file imports edge weight = %d, want 1: %+v", e.Weight, e)
		}
		if strings.HasPrefix(e.From, "pkg:") || strings.HasPrefix(e.To, "pkg:") {
			t.Errorf("file-granularity edge wired to a pkg-synthetic id (roll-up leaked): %+v", e)
		}
	}
	if importsEdges != 6 {
		t.Errorf("imports-edge count at granularity=file = %d, want 6", importsEdges)
	}

	// Contains edges: 2 (repo -> pkg) + 5 (pkg -> file) = 7.
	if got := countByKind(d.Edges)["contains"]; got != 7 {
		t.Errorf("contains-edge count = %d, want 7 (repo->pkg + pkg->file)", got)
	}
}

// TestBuildModuleDiagram_IntraPackageImportsSuppressed verifies
// the function doc's "intra-package imports are suppressed at
// granularity=package" rule. A self-loop on the rolled-up pkg
// would be noise on a component diagram and is explicitly
// dropped.
func TestBuildModuleDiagram_IntraPackageImportsSuppressed(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/intra")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: []graphreader.Node{
			{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
				CanonicalSignature: "intra::repo"},
			{NodeID: "node-pkg-x", RepoID: repoIDStr, Kind: "package",
				CanonicalSignature: "intra/pkg/x", ParentNodeID: repoNodeID},
			{NodeID: "node-file-x1", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "intra/pkg/x/one.go", ParentNodeID: "node-pkg-x"},
			{NodeID: "node-file-x2", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "intra/pkg/x/two.go", ParentNodeID: "node-pkg-x"},
		},
		edges: []graphreader.Edge{
			{EdgeID: "e-x1-x2", RepoID: repoIDStr, Kind: "imports",
				SrcNodeID: "node-file-x1", DstNodeID: "node-file-x2"},
		},
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityPackage)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}
	if c := countByKind(d.Edges)["imports"]; c != 0 {
		t.Errorf("intra-package imports edge leaked into module diagram: count=%d edges=%+v",
			c, d.Edges)
	}
}

// TestBuildModuleDiagram_UnresolvedImportSkipped verifies the
// projector drops imports edges whose destination is not a file
// Node under any surfaced package (e.g. external library or
// unresolved import target). Dangling edges would point at
// non-existent diagram Nodes and break the UI.
func TestBuildModuleDiagram_UnresolvedImportSkipped(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/dangling")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: []graphreader.Node{
			{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
				CanonicalSignature: "dangling::repo"},
			{NodeID: "node-pkg-a", RepoID: repoIDStr, Kind: "package",
				CanonicalSignature: "dangling/pkg/a", ParentNodeID: repoNodeID},
			{NodeID: "node-file-a1", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "dangling/pkg/a/one.go", ParentNodeID: "node-pkg-a"},
		},
		edges: []graphreader.Edge{
			// Imports an external/unsurfaced node id; should
			// be dropped by the roll-up.
			{EdgeID: "e-dangling", RepoID: repoIDStr, Kind: "imports",
				SrcNodeID: "node-file-a1", DstNodeID: "node-external"},
		},
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityPackage)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}
	if c := countByKind(d.Edges)["imports"]; c != 0 {
		t.Errorf("dangling import was surfaced: count=%d edges=%+v", c, d.Edges)
	}
}

// TestBuildModuleDiagram_InvalidGranularity exercises the
// validation guard for unknown `--granularity` flag values.
func TestBuildModuleDiagram_InvalidGranularity(t *testing.T) {
	repoID, summary := newFixtureRepo(t, "https://example.com/example/bad")
	f := &fakeReader{repos: []graphreader.RepoSummary{summary}}
	_, err := BuildModuleDiagram(context.Background(), f, repoID, "module")
	if err == nil {
		t.Fatalf("expected error on invalid granularity, got nil")
	}
}

// TestBuildModuleDiagram_ZeroRepoIDRejected confirms the input
// validation guard against an uninitialised RepoID.
func TestBuildModuleDiagram_ZeroRepoIDRejected(t *testing.T) {
	f := &fakeReader{}
	_, err := BuildModuleDiagram(context.Background(), f, fingerprint.RepoID{}, GranularityPackage)
	if err == nil {
		t.Fatalf("expected error on zero RepoID, got nil")
	}
}

// TestBuildModuleDiagram_RepoNodeMissing exercises the path where
// the backend has registered the repo (so ListRepos returns it)
// but no `repo`-kind Node exists yet. Without an anchor the
// containment tree has nowhere to attach -- the projector MUST
// surface a hard error instead of silently dropping packages.
func TestBuildModuleDiagram_RepoNodeMissing(t *testing.T) {
	repoID, summary := newFixtureRepo(t, "https://example.com/example/nonode")
	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: nil, // no repo Node materialised yet
	}
	_, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityPackage)
	if err == nil {
		t.Fatalf("expected error when repo Node missing, got nil")
	}
}

// TestBuildModuleDiagram_TruncationFlag verifies the
// architecture-S6 invariant: when a list-style read returns at
// `graphreader.MaxListLimit` the envelope's `truncated` flag is
// set so the UI can render the "results clipped at 10 000"
// badge. We simulate the cap on the `package` ListNodes call.
func TestBuildModuleDiagram_TruncationFlag(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/trunc")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: []graphreader.Node{
			{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
				CanonicalSignature: "trunc::repo"},
		},
		nodeLimitOverride: map[string]int{
			"package": MaxListLimit,
		},
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityPackage)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}
	if !d.Truncated {
		t.Fatalf("Truncated = false, want true when a list call returns at the cap")
	}
	if d.Stats.CappedAt != MaxListLimit {
		t.Errorf("Stats.CappedAt = %d, want %d", d.Stats.CappedAt, MaxListLimit)
	}
}

// TestBuildModuleDiagram_GranularityClass surfaces classes under
// files and adds the corresponding contains edges. Imports are
// still preserved file -> file (not rolled up).
func TestBuildModuleDiagram_GranularityClass(t *testing.T) {
	restore := fixedTime(t)
	defer restore()

	repoID, summary := newFixtureRepo(t, "https://example.com/example/cls")
	repoIDStr := repoID.String()
	const repoNodeID = "node-repo"

	f := &fakeReader{
		repos: []graphreader.RepoSummary{summary},
		nodes: []graphreader.Node{
			{NodeID: repoNodeID, RepoID: repoIDStr, Kind: "repo",
				CanonicalSignature: "cls::repo"},
			{NodeID: "node-pkg-a", RepoID: repoIDStr, Kind: "package",
				CanonicalSignature: "cls/pkg/a", ParentNodeID: repoNodeID},
			{NodeID: "node-file-a1", RepoID: repoIDStr, Kind: "file",
				CanonicalSignature: "cls/pkg/a/one.go", ParentNodeID: "node-pkg-a"},
			{NodeID: "node-cls-1", RepoID: repoIDStr, Kind: "class",
				CanonicalSignature: "cls/pkg/a/one.go::Foo", ParentNodeID: "node-file-a1"},
			{NodeID: "node-cls-2", RepoID: repoIDStr, Kind: "class",
				CanonicalSignature: "cls/pkg/a/one.go::Bar", ParentNodeID: "node-file-a1"},
		},
	}

	d, err := BuildModuleDiagram(context.Background(), f, repoID, GranularityClass)
	if err != nil {
		t.Fatalf("BuildModuleDiagram: %v", err)
	}

	classCount := 0
	for _, n := range d.Nodes {
		if n.Kind == "class" {
			classCount++
			if n.Group != "node-file-a1" {
				t.Errorf("class Node %s group = %q, want %q", n.ID, n.Group, "node-file-a1")
			}
		}
	}
	if classCount != 2 {
		t.Errorf("class-Node count = %d, want 2", classCount)
	}
	// repo -> pkg, pkg -> file, file -> 2 classes = 4 contains
	if got := countByKind(d.Edges)["contains"]; got != 4 {
		t.Errorf("contains-edge count = %d, want 4 (repo->pkg + pkg->file + 2 file->class)", got)
	}
}
