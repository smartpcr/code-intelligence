package postgres

// Pure forwarding tests for the Postgres reader adapter. A
// `fakeBackend` records every call to its `graphsink.Reader`
// surface; each test invokes one method on the adapter and
// asserts the corresponding backend method was hit exactly
// once with identical arguments, and that the backend's return
// value flows back through the adapter unmodified.
//
// Covers impl-plan Stage 3.3 scenarios:
//   - postgres-forwarding (read side)
//   - listrepos-forwards-to-graphreader
//   - lookupbysignature-uses-filter (forwarding half;
//     `*graphreader.Reader.LookupBySignature` semantics
//     -- the ListNodes-with-CanonicalSignature dispatch --
//     are pinned by the integration test in
//     `internal/graphreader/listrepos_test.go` and a unit
//     assertion right here.)

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// --------------------------------------------------------------
// fakeBackend -- records every call and returns canned outputs
// --------------------------------------------------------------

type fakeBackendCall struct {
	method string
	args   []any
}

type fakeBackend struct {
	calls []fakeBackendCall

	// canned responses keyed by method.
	repos    []graphreader.RepoSummary
	nodes    []graphreader.Node
	edges    []graphreader.Edge
	node     graphreader.Node
	listErr  error
	getErr   error
	lookupOK graphreader.Node
}

// Compile-time assertion: fakeBackend satisfies graphsink.Reader.
var _ graphsink.Reader = (*fakeBackend)(nil)

func (f *fakeBackend) record(method string, args ...any) {
	f.calls = append(f.calls, fakeBackendCall{method: method, args: args})
}

func (f *fakeBackend) ListRepos(_ context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	f.record("ListRepos", opts)
	return f.repos, f.listErr
}

func (f *fakeBackend) ListNodes(
	_ context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	filter graphreader.ListNodesFilter,
	opts graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	f.record("ListNodes", repoID, kinds, filter, opts)
	return f.nodes, f.listErr
}

func (f *fakeBackend) ListEdgesFrom(_ context.Context, src string, kinds []string, opts graphreader.ReaderOptions) ([]graphreader.Edge, error) {
	f.record("ListEdgesFrom", src, kinds, opts)
	return f.edges, f.listErr
}

func (f *fakeBackend) ListEdgesTo(_ context.Context, dst string, kinds []string, opts graphreader.ReaderOptions) ([]graphreader.Edge, error) {
	f.record("ListEdgesTo", dst, kinds, opts)
	return f.edges, f.listErr
}

func (f *fakeBackend) GetNode(_ context.Context, id string, opts graphreader.ReaderOptions) (graphreader.Node, error) {
	f.record("GetNode", id, opts)
	return f.node, f.getErr
}

func (f *fakeBackend) LookupBySignature(
	_ context.Context,
	repoID fingerprint.RepoID,
	kind string,
	sig string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	f.record("LookupBySignature", repoID, kind, sig, opts)
	return f.lookupOK, f.getErr
}

// --------------------------------------------------------------
// constructor guard
// --------------------------------------------------------------

func TestNewReader_panicsOnNilBackend(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewReader(nil) did not panic")
		}
	}()
	_ = NewReader(nil)
}

// --------------------------------------------------------------
// forwarding tests -- one per method on graphsink.Reader
// --------------------------------------------------------------

func TestReader_ListRepos_forwardsAndReturnsUnchanged(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	want := []graphreader.RepoSummary{
		{RepoID: "11111111-1111-1111-1111-111111111111", URL: "file:///r/a", SHA: "abc", GeneratedAt: now},
		{RepoID: "22222222-2222-2222-2222-222222222222", URL: "file:///r/b", SHA: "def", GeneratedAt: now.Add(-time.Hour)},
	}
	be := &fakeBackend{repos: want}
	r := newReaderWithBackend(be)
	opts := graphreader.ReaderOptions{Limit: 7}

	got, err := r.ListRepos(context.Background(), opts)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("returned slice mutated by adapter:\n got %+v\nwant %+v", got, want)
	}
	if len(be.calls) != 1 || be.calls[0].method != "ListRepos" {
		t.Fatalf("expected exactly one ListRepos call, got %+v", be.calls)
	}
	if !reflect.DeepEqual(be.calls[0].args[0], opts) {
		t.Errorf("opts mutated by adapter: got %+v want %+v", be.calls[0].args[0], opts)
	}
}

func TestReader_ListRepos_propagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("backend boom")
	r := newReaderWithBackend(&fakeBackend{listErr: sentinel})
	if _, err := r.ListRepos(context.Background(), graphreader.ReaderOptions{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v; want errors.Is sentinel", err)
	}
}

func TestReader_ListNodes_forwardsAllArgs(t *testing.T) {
	t.Parallel()

	repoID := fingerprint.MustParseRepoID("33333333-3333-3333-3333-333333333333")
	kinds := []string{"method", "class"}
	filter := graphreader.ListNodesFilter{
		ParentNodeID:       "parent-1",
		FromSHA:            "abc",
		CanonicalSignature: "sig://x",
		Limit:              42,
	}
	opts := graphreader.ReaderOptions{IncludeRetired: true, Limit: 5}
	want := []graphreader.Node{{NodeID: "n1", RepoID: repoID.String(), Kind: "method", AttrsJSON: json.RawMessage(`{}`)}}

	be := &fakeBackend{nodes: want}
	r := newReaderWithBackend(be)

	got, err := r.ListNodes(context.Background(), repoID, kinds, filter, opts)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("returned slice mutated:\n got %+v\nwant %+v", got, want)
	}
	if len(be.calls) != 1 || be.calls[0].method != "ListNodes" {
		t.Fatalf("expected one ListNodes call; got %+v", be.calls)
	}
	gotArgs := be.calls[0].args
	if !reflect.DeepEqual(gotArgs[0], repoID) ||
		!reflect.DeepEqual(gotArgs[1], kinds) ||
		!reflect.DeepEqual(gotArgs[2], filter) ||
		!reflect.DeepEqual(gotArgs[3], opts) {
		t.Errorf("forwarded args mutated:\n got %+v\nwant repoID=%v kinds=%v filter=%+v opts=%+v",
			gotArgs, repoID, kinds, filter, opts)
	}
}

func TestReader_ListEdgesFrom_forwardsAllArgs(t *testing.T) {
	t.Parallel()
	be := &fakeBackend{edges: []graphreader.Edge{{EdgeID: "e1"}}}
	r := newReaderWithBackend(be)
	src, kinds, opts := "node-src", []string{"static_calls"}, graphreader.ReaderOptions{Limit: 9}

	if _, err := r.ListEdgesFrom(context.Background(), src, kinds, opts); err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	if len(be.calls) != 1 || be.calls[0].method != "ListEdgesFrom" {
		t.Fatalf("expected one ListEdgesFrom call; got %+v", be.calls)
	}
	if be.calls[0].args[0] != src ||
		!reflect.DeepEqual(be.calls[0].args[1], kinds) ||
		!reflect.DeepEqual(be.calls[0].args[2], opts) {
		t.Errorf("args mismatch: got %+v", be.calls[0].args)
	}
}

func TestReader_ListEdgesTo_forwardsAllArgs(t *testing.T) {
	t.Parallel()
	be := &fakeBackend{edges: []graphreader.Edge{{EdgeID: "e2"}}}
	r := newReaderWithBackend(be)
	dst, kinds, opts := "node-dst", []string{"observed_calls"}, graphreader.ReaderOptions{}

	if _, err := r.ListEdgesTo(context.Background(), dst, kinds, opts); err != nil {
		t.Fatalf("ListEdgesTo: %v", err)
	}
	if len(be.calls) != 1 || be.calls[0].method != "ListEdgesTo" {
		t.Fatalf("expected one ListEdgesTo call; got %+v", be.calls)
	}
	if be.calls[0].args[0] != dst ||
		!reflect.DeepEqual(be.calls[0].args[1], kinds) {
		t.Errorf("args mismatch: got %+v", be.calls[0].args)
	}
}

func TestReader_GetNode_forwardsAndPropagatesNotFound(t *testing.T) {
	t.Parallel()
	be := &fakeBackend{getErr: graphreader.ErrNotFound}
	r := newReaderWithBackend(be)
	_, err := r.GetNode(context.Background(), "nope", graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if len(be.calls) != 1 || be.calls[0].method != "GetNode" {
		t.Fatalf("expected one GetNode call; got %+v", be.calls)
	}
}

func TestReader_LookupBySignature_forwardsAllArgs(t *testing.T) {
	t.Parallel()
	repoID := fingerprint.MustParseRepoID("44444444-4444-4444-4444-444444444444")
	want := graphreader.Node{NodeID: "n7", Kind: "method", CanonicalSignature: "sig://foo"}
	be := &fakeBackend{lookupOK: want}
	r := newReaderWithBackend(be)

	got, err := r.LookupBySignature(context.Background(), repoID, "method", "sig://foo", graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("LookupBySignature: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("returned Node mutated: got %+v want %+v", got, want)
	}
	if len(be.calls) != 1 || be.calls[0].method != "LookupBySignature" {
		t.Fatalf("expected one LookupBySignature call; got %+v", be.calls)
	}
	if be.calls[0].args[0] != repoID ||
		be.calls[0].args[1] != "method" ||
		be.calls[0].args[2] != "sig://foo" {
		t.Errorf("args mismatch: got %+v", be.calls[0].args)
	}
}

// TestReader_compileTime_graphreaderReaderSatisfiesBackend pins
// the production-wiring invariant: the concrete
// `*graphreader.Reader` value built at process startup must
// satisfy `graphsink.Reader` so the call site
// `postgres.NewReader(graphreader.New(pool, log))` compiles
// without a wrapper. This is a structural assertion, not a
// behavioural one, but the workstream brief depends on it.
func TestReader_compileTime_graphreaderReaderSatisfiesBackend(t *testing.T) {
	t.Parallel()
	var _ graphsink.Reader = (*graphreader.Reader)(nil)
}
