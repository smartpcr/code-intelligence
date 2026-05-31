package diagram

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// fakeReader is a hand-rolled graphsink.Reader satisfying the
// surface BuildCallChain consumes. It is deliberately
// independent of the SQLite / memory / Postgres backends so the
// BFS-shape tests pin the projector's behaviour without coupling
// to a backend's idempotency / fingerprinting details.
type fakeReader struct {
	repos    []graphreader.RepoSummary
	nodes    map[string]graphreader.Node
	bySig    map[sigKey]string // (repoID, kind, sig) -> nodeID
	outEdges map[string][]graphreader.Edge
	inEdges  map[string][]graphreader.Edge
}

type sigKey struct {
	repoID string
	kind   string
	sig    string
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		nodes:    map[string]graphreader.Node{},
		bySig:    map[sigKey]string{},
		outEdges: map[string][]graphreader.Edge{},
		inEdges:  map[string][]graphreader.Edge{},
	}
}

func (f *fakeReader) addRepo(s graphreader.RepoSummary) {
	f.repos = append(f.repos, s)
}

func (f *fakeReader) addNode(n graphreader.Node) {
	f.nodes[n.NodeID] = n
	f.bySig[sigKey{repoID: n.RepoID, kind: n.Kind, sig: n.CanonicalSignature}] = n.NodeID
}

func (f *fakeReader) addEdge(e graphreader.Edge) {
	f.outEdges[e.SrcNodeID] = append(f.outEdges[e.SrcNodeID], e)
	f.inEdges[e.DstNodeID] = append(f.inEdges[e.DstNodeID], e)
}

func (f *fakeReader) ListRepos(
	_ context.Context, _ graphreader.ReaderOptions,
) ([]graphreader.RepoSummary, error) {
	out := make([]graphreader.RepoSummary, len(f.repos))
	copy(out, f.repos)
	return out, nil
}

func (f *fakeReader) ListNodes(
	_ context.Context, _ fingerprint.RepoID, _ []string,
	_ graphreader.ListNodesFilter, _ graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	out := make([]graphreader.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (f *fakeReader) ListEdgesFrom(
	_ context.Context, srcNodeID string, kinds []string,
	_ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return filterEdges(f.outEdges[srcNodeID], kinds), nil
}

func (f *fakeReader) ListEdgesTo(
	_ context.Context, dstNodeID string, kinds []string,
	_ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return filterEdges(f.inEdges[dstNodeID], kinds), nil
}

func (f *fakeReader) GetNode(
	_ context.Context, nodeID string, _ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	n, ok := f.nodes[nodeID]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return n, nil
}

func (f *fakeReader) LookupBySignature(
	_ context.Context, repoID fingerprint.RepoID, kind, sig string,
	_ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	id, ok := f.bySig[sigKey{repoID: repoID.String(), kind: kind, sig: sig}]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return f.nodes[id], nil
}

func filterEdges(in []graphreader.Edge, kinds []string) []graphreader.Edge {
	if len(kinds) == 0 {
		out := make([]graphreader.Edge, len(in))
		copy(out, in)
		return out
	}
	allowed := map[string]struct{}{}
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	out := make([]graphreader.Edge, 0, len(in))
	for _, e := range in {
		if _, ok := allowed[e.Kind]; ok {
			out = append(out, e)
		}
	}
	return out
}

// --- Fixture helpers ---

const fixtureRepoURL = "https://github.com/example/scan-target"

func fixtureRepoID(t *testing.T) fingerprint.RepoID {
	t.Helper()
	rid, err := fingerprint.RepoIDFromURL(fixtureRepoURL)
	if err != nil {
		t.Fatalf("RepoIDFromURL: %v", err)
	}
	return rid
}

// methodNode is a tiny helper that builds a graphreader.Node
// representing a method with the supplied canonical signature.
func methodNode(repoID fingerprint.RepoID, nodeID, sig string) graphreader.Node {
	return graphreader.Node{
		NodeID:             nodeID,
		RepoID:             repoID.String(),
		Kind:               "method",
		CanonicalSignature: sig,
		ParentNodeID:       "file-1",
		FromSHA:            "deadbeef",
		AttrsJSON:          json.RawMessage(`{"language":"go"}`),
	}
}

// staticCall returns a static_calls edge from src to dst with a
// stable synthetic id keyed on the (src, dst) pair.
func staticCall(repoID fingerprint.RepoID, src, dst string) graphreader.Edge {
	return graphreader.Edge{
		EdgeID:    "e-" + src + "-" + dst,
		RepoID:    repoID.String(),
		Kind:      "static_calls",
		SrcNodeID: src,
		DstNodeID: dst,
		FromSHA:   "deadbeef",
	}
}

// observedCall mirrors staticCall but with the runtime kind so
// the dedupe/visit logic is exercised on mixed-kind frontiers.
func observedCall(repoID fingerprint.RepoID, src, dst string) graphreader.Edge {
	return graphreader.Edge{
		EdgeID:    "o-" + src + "-" + dst,
		RepoID:    repoID.String(),
		Kind:      "observed_calls",
		SrcNodeID: src,
		DstNodeID: dst,
		FromSHA:   "deadbeef",
	}
}

// fixtureMWithCallees builds the canonical "M has 2 callees and
// 3 callers" graph the impl-plan Stage 6.3 scenarios pin.
func fixtureMWithCallees(t *testing.T) (*fakeReader, fingerprint.RepoID) {
	t.Helper()
	rid := fixtureRepoID(t)
	r := newFakeReader()
	r.addRepo(graphreader.RepoSummary{
		RepoID: rid.String(),
		URL:    fixtureRepoURL,
		SHA:    "deadbeef",
	})
	r.addNode(methodNode(rid, "M", "pkg.M#m()"))
	for _, c := range []string{"C1", "C2"} {
		r.addNode(methodNode(rid, c, "pkg.M#"+c+"()"))
		r.addEdge(staticCall(rid, "M", c))
	}
	for _, c := range []string{"P1", "P2", "P3"} {
		r.addNode(methodNode(rid, c, "pkg.M#"+c+"()"))
		r.addEdge(staticCall(rid, c, "M"))
	}
	return r, rid
}

// fixtureChainABCD builds the chain `A -> B -> C -> D` the
// depth-bounded BFS scenario pins.
func fixtureChainABCD(t *testing.T) (*fakeReader, fingerprint.RepoID) {
	t.Helper()
	rid := fixtureRepoID(t)
	r := newFakeReader()
	r.addRepo(graphreader.RepoSummary{
		RepoID: rid.String(),
		URL:    fixtureRepoURL,
		SHA:    "deadbeef",
	})
	for _, n := range []string{"A", "B", "C", "D"} {
		r.addNode(methodNode(rid, n, "pkg.chain#"+n+"()"))
	}
	r.addEdge(staticCall(rid, "A", "B"))
	r.addEdge(staticCall(rid, "B", "C"))
	r.addEdge(staticCall(rid, "C", "D"))
	return r, rid
}

// --- Tests ---

// TestBuildCallChain_calleesOnly pins impl-plan Stage 6.3 scenario
// `bfs-callees-only`: M with 2 callees + 3 callers at depth=1 with
// direction=callees yields 3 nodes and 2 edges.
func TestBuildCallChain_calleesOnly(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	d, err := BuildCallChain(context.Background(), r, "M", 1, DirectionCallees)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if d.Diagram != KindCallChain {
		t.Errorf("diagram kind = %q, want %q", d.Diagram, KindCallChain)
	}
	if d.LayoutHint != LayoutHierarchicalLeftRight {
		t.Errorf("layoutHint = %q, want %q",
			d.LayoutHint, LayoutHierarchicalLeftRight)
	}
	if got, want := len(d.Nodes), 3; got != want {
		t.Errorf("nodes count = %d, want %d (M + 2 callees)", got, want)
	}
	if got, want := len(d.Edges), 2; got != want {
		t.Errorf("edges count = %d, want %d", got, want)
	}
	for _, e := range d.Edges {
		if e.Kind != "static_calls" {
			t.Errorf("edge %s kind = %q, want static_calls", e.ID, e.Kind)
		}
		if e.From != "M" {
			t.Errorf("edge %s from = %q, want M", e.ID, e.From)
		}
	}
	if d.Stats.NodeCount != 3 || d.Stats.EdgeCount != 2 {
		t.Errorf("stats = %+v, want NodeCount=3 EdgeCount=2", d.Stats)
	}
	if d.Stats.CappedAt != MaxListLimit {
		t.Errorf("stats.cappedAt = %d, want %d", d.Stats.CappedAt, MaxListLimit)
	}
}

// TestBuildCallChain_bothDirections pins impl-plan Stage 6.3 scenario
// `bfs-both-directions`: 6 nodes (M + 2 callees + 3 callers) and 5
// edges with direction=both.
func TestBuildCallChain_bothDirections(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	d, err := BuildCallChain(context.Background(), r, "M", 1, DirectionBoth)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if got, want := len(d.Nodes), 6; got != want {
		t.Errorf("nodes count = %d, want %d", got, want)
	}
	if got, want := len(d.Edges), 5; got != want {
		t.Errorf("edges count = %d, want %d", got, want)
	}
}

// TestBuildCallChain_callersOnly walks only the inbound direction;
// expect M + 3 callers and 3 edges all incoming to M.
func TestBuildCallChain_callersOnly(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	d, err := BuildCallChain(context.Background(), r, "M", 1, DirectionCallers)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if got, want := len(d.Nodes), 4; got != want {
		t.Errorf("nodes count = %d, want %d (M + 3 callers)", got, want)
	}
	if got, want := len(d.Edges), 3; got != want {
		t.Errorf("edges count = %d, want %d", got, want)
	}
	for _, e := range d.Edges {
		if e.To != "M" {
			t.Errorf("edge %s to = %q, want M", e.ID, e.To)
		}
	}
}

// TestBuildCallChain_depthBounded pins impl-plan Stage 6.3 scenario
// `bfs-depth-two`: chain A->B->C->D, seed=A, depth=2, callees: nodes
// A, B, C appear; D does NOT.
func TestBuildCallChain_depthBounded(t *testing.T) {
	r, _ := fixtureChainABCD(t)
	d, err := BuildCallChain(context.Background(), r, "A", 2, DirectionCallees)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	got := map[string]bool{}
	for _, n := range d.Nodes {
		got[n.ID] = true
	}
	for _, want := range []string{"A", "B", "C"} {
		if !got[want] {
			t.Errorf("expected node %q in envelope, got %v", want, got)
		}
	}
	if got["D"] {
		t.Errorf("node D must NOT appear at depth=2, got nodes=%v", got)
	}
	if got, want := len(d.Edges), 2; got != want {
		t.Errorf("edges count = %d, want %d (A->B, B->C)", got, want)
	}
}

// TestBuildCallChain_depthZero is the identity case: seed only,
// no edges walked.
func TestBuildCallChain_depthZero(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	d, err := BuildCallChain(context.Background(), r, "M", 0, DirectionBoth)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if got, want := len(d.Nodes), 1; got != want {
		t.Errorf("nodes count = %d, want %d (seed only)", got, want)
	}
	if got, want := len(d.Edges), 0; got != want {
		t.Errorf("edges count = %d, want %d", got, want)
	}
}

// TestBuildCallChain_seedNotFound pins impl-plan Stage 6.3 scenario
// `seed-unresolved-error`: unknown seed returns ErrSeedNotFound and
// a zero-value diagram envelope.
func TestBuildCallChain_seedNotFound(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	d, err := BuildCallChain(
		context.Background(), r, "does-not-exist", 1, DirectionBoth,
	)
	if !errors.Is(err, ErrSeedNotFound) {
		t.Fatalf("expected ErrSeedNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), SeedNotFoundCode) {
		t.Errorf("error %q must contain code %q", err.Error(), SeedNotFoundCode)
	}
	// e2e-scenarios.md "diagram envelope is the zero value":
	// every field on the returned Diagram must equal its zero
	// value. Slices/maps are compared by len because Go forbids
	// `==` on structs containing slices.
	if d.Diagram != "" || d.LayoutHint != "" {
		t.Errorf("expected zero envelope, got diagram=%q layout=%q",
			d.Diagram, d.LayoutHint)
	}
	if !d.GeneratedAt.IsZero() {
		t.Errorf("expected zero GeneratedAt, got %v", d.GeneratedAt)
	}
	if (d.Repo != Repo{}) {
		t.Errorf("expected zero Repo, got %+v", d.Repo)
	}
	if len(d.Nodes) != 0 || len(d.Edges) != 0 {
		t.Errorf("expected empty nodes/edges, got %d/%d",
			len(d.Nodes), len(d.Edges))
	}
	if d.Truncated {
		t.Errorf("expected Truncated=false on zero envelope")
	}
	if d.Stats.NodeCount != 0 || d.Stats.EdgeCount != 0 ||
		d.Stats.CappedAt != 0 || len(d.Stats.Skipped) != 0 {
		t.Errorf("expected zero Stats, got %+v", d.Stats)
	}
}

// TestBuildCallChain_seedEncodedSignature exercises the
// `<repoID>|<kind>|<sig>` form so the LookupBySignature path is
// covered.
func TestBuildCallChain_seedEncodedSignature(t *testing.T) {
	r, rid := fixtureMWithCallees(t)
	seed := rid.String() + "|method|pkg.M#m()"
	d, err := BuildCallChain(
		context.Background(), r, seed, 1, DirectionCallees,
	)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if got, want := len(d.Nodes), 3; got != want {
		t.Errorf("nodes count = %d, want %d", got, want)
	}
}

// TestBuildCallChain_observedAndStaticMixed asserts that both
// edge kinds are walked and tagged correctly so the UI styling
// rule (architecture S4.4.1) sees them as distinct.
func TestBuildCallChain_observedAndStaticMixed(t *testing.T) {
	rid := fixtureRepoID(t)
	r := newFakeReader()
	r.addRepo(graphreader.RepoSummary{RepoID: rid.String(), URL: fixtureRepoURL})
	r.addNode(methodNode(rid, "M", "pkg.M#m()"))
	r.addNode(methodNode(rid, "S", "pkg.M#s()"))
	r.addNode(methodNode(rid, "O", "pkg.M#o()"))
	r.addEdge(staticCall(rid, "M", "S"))
	r.addEdge(observedCall(rid, "M", "O"))

	d, err := BuildCallChain(context.Background(), r, "M", 1, DirectionCallees)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	kinds := map[string]int{}
	for _, e := range d.Edges {
		kinds[e.Kind]++
	}
	if kinds["static_calls"] != 1 || kinds["observed_calls"] != 1 {
		t.Errorf("expected one of each kind, got %v", kinds)
	}
}

// TestBuildCallChain_cycleTerminates: A -> B -> A must not loop.
func TestBuildCallChain_cycleTerminates(t *testing.T) {
	rid := fixtureRepoID(t)
	r := newFakeReader()
	r.addRepo(graphreader.RepoSummary{RepoID: rid.String(), URL: fixtureRepoURL})
	r.addNode(methodNode(rid, "A", "pkg.cycle#A()"))
	r.addNode(methodNode(rid, "B", "pkg.cycle#B()"))
	r.addEdge(staticCall(rid, "A", "B"))
	r.addEdge(staticCall(rid, "B", "A"))

	d, err := BuildCallChain(context.Background(), r, "A", 5, DirectionCallees)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if got, want := len(d.Nodes), 2; got != want {
		t.Errorf("nodes = %d, want %d", got, want)
	}
	if got, want := len(d.Edges), 2; got != want {
		t.Errorf("edges = %d, want %d", got, want)
	}
}

// TestBuildCallChain_invalidDirection rejects out-of-set values.
func TestBuildCallChain_invalidDirection(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	_, err := BuildCallChain(context.Background(), r, "M", 1, "sideways")
	if !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("expected ErrInvalidDirection, got %v", err)
	}
}

// TestBuildCallChain_negativeDepth rejects depth<0.
func TestBuildCallChain_negativeDepth(t *testing.T) {
	r, _ := fixtureMWithCallees(t)
	_, err := BuildCallChain(context.Background(), r, "M", -1, DirectionBoth)
	if !errors.Is(err, ErrNegativeDepth) {
		t.Fatalf("expected ErrNegativeDepth, got %v", err)
	}
}

// TestBuildCallChain_repoMetadataPopulated confirms the envelope's
// Repo block is filled from ListRepos when a matching summary
// exists.
func TestBuildCallChain_repoMetadataPopulated(t *testing.T) {
	r, rid := fixtureMWithCallees(t)
	d, err := BuildCallChain(context.Background(), r, "M", 0, DirectionBoth)
	if err != nil {
		t.Fatalf("BuildCallChain: %v", err)
	}
	if d.Repo.ID != rid.String() {
		t.Errorf("repo.id = %q, want %q", d.Repo.ID, rid.String())
	}
	if d.Repo.URL != fixtureRepoURL {
		t.Errorf("repo.url = %q, want %q", d.Repo.URL, fixtureRepoURL)
	}
}

// TestDeriveLabel covers the canonical-signature label heuristics.
func TestDeriveLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"pkg.Foo#bar(int,int)", "bar"},
		{"file://example/foo.go", "go"}, // last token after `.`
		{"GreeterImpl.greet", "greet"},
		{"std::vector::push_back", "push_back"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, c := range cases {
		if got := deriveLabel(c.in); got != c.want {
			t.Errorf("deriveLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExtractLanguage covers the attrs language extraction.
func TestExtractLanguage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"language":"go"}`, "go"},
		{`{"lang":"python"}`, "python"},
		{`{"language":"rust","other":1}`, "rust"},
		{`{}`, ""},
		{`not-json`, ""},
		{``, ""},
	}
	for _, c := range cases {
		got := extractLanguage(json.RawMessage(c.in))
		if got != c.want {
			t.Errorf("extractLanguage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
