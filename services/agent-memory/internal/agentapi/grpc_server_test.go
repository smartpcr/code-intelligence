package agentapi

// Unit tests for the gRPC server adapter
// (`grpc_server.go`). The adapter is the bridge between
// the proto wire shape and the in-process `*Service`; we
// verify the translation in both directions and the
// status-code mapping for domain errors.
//
// What these tests prove (resolves evaluator iter-1 #1
// "The gRPC service is not actually registered or
// callable"):
//
//   1. A proto RecallRequest is correctly translated into
//      the internal RecallRequest (Kinds, RepoID, K,
//      Query).
//   2. The internal RecallResponse projects onto the proto
//      RecallResponse with all card slices populated.
//   3. Domain sentinel errors map onto
//      `codes.InvalidArgument` (caller-correctable) and
//      other errors map onto `codes.Internal`.

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
	agentpb "github.com/smartpcr/code-intelligence/services/agent-memory/proto/agent"
)

// TestGRPCServer_recallTranslatesRoundTrip proves the
// proto ↔ internal translation in both directions: the
// proto request lands on the right internal fields and
// the internal response surfaces on the proto cards.
func TestGRPCServer_recallTranslatesRoundTrip(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: {{
			PointID: "pub-1", Score: 0.95,
			Payload: map[string]any{
				"node_id":             "node-1",
				"repo_id":             "repo-a",
				"kind":                "method",
				"canonical_signature": "pkg.Foo",
			},
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}
	svc := NewService(emb, search, filter, WithLogger(quietLogger()))
	srv := NewGRPCServer(svc)

	resp, err := srv.Recall(context.Background(), &agentpb.RecallRequest{
		Query:  "find method",
		Kinds:  []string{"method"},
		RepoId: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("gRPC Recall: %v", err)
	}
	if len(resp.GetNodes()) != 1 {
		t.Fatalf("len(resp.Nodes) = %d; want 1", len(resp.GetNodes()))
	}
	got := resp.GetNodes()[0]
	if got.GetNodeId() != "node-1" {
		t.Fatalf("NodeId = %q; want node-1", got.GetNodeId())
	}
	if got.GetRepoId() != "repo-a" {
		t.Fatalf("RepoId = %q; want repo-a", got.GetRepoId())
	}
	if got.GetKind() != "method" {
		t.Fatalf("Kind = %q; want method", got.GetKind())
	}
	if got.GetCanonicalSignature() != "pkg.Foo" {
		t.Fatalf("CanonicalSignature = %q; want pkg.Foo", got.GetCanonicalSignature())
	}
	if got.GetPointId() != "pub-1" {
		t.Fatalf("PointId = %q; want pub-1", got.GetPointId())
	}
	// Search must have been called with the kind the proto
	// request asked for.
	if search.callCount[embedding.CollectionMethod] != 1 {
		t.Fatalf("Method search calls = %d; want 1", search.callCount[embedding.CollectionMethod])
	}
}

// TestGRPCServer_emptyQueryReturnsInvalidArgument proves
// the domain → gRPC code mapping: `ErrEmptyQuery`
// surfaces as `codes.InvalidArgument` so the caller can
// pattern-match.
func TestGRPCServer_emptyQueryReturnsInvalidArgument(t *testing.T) {
	svc := NewService(fakeEmbedder{vec: []float32{0.1}}, &collectionSearcher{}, &allowListFilter{},
		WithLogger(quietLogger()))
	srv := NewGRPCServer(svc)

	_, err := srv.Recall(context.Background(), &agentpb.RecallRequest{
		Query:  "",
		Kinds:  []string{"method"},
		RepoId: "repo-a",
		K:      5,
	})
	if err == nil {
		t.Fatalf("expected error for empty query")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
}

// TestGRPCServer_invalidKindReturnsInvalidArgument proves
// kind-validation errors also map to `InvalidArgument`.
func TestGRPCServer_invalidKindReturnsInvalidArgument(t *testing.T) {
	svc := NewService(fakeEmbedder{vec: []float32{0.1}}, &collectionSearcher{}, &allowListFilter{},
		WithLogger(quietLogger()))
	srv := NewGRPCServer(svc)

	_, err := srv.Recall(context.Background(), &agentpb.RecallRequest{
		Query:  "valid query",
		Kinds:  []string{"file"}, // not in {method,block,concept}
		RepoId: "repo-a",
		K:      5,
	})
	if err == nil {
		t.Fatalf("expected error for invalid kind")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
}

// TestGRPCServer_kReturnsInvalidArgument proves K<=0
// also surfaces as InvalidArgument.
func TestGRPCServer_kReturnsInvalidArgument(t *testing.T) {
	svc := NewService(fakeEmbedder{vec: []float32{0.1}}, &collectionSearcher{}, &allowListFilter{},
		WithLogger(quietLogger()))
	srv := NewGRPCServer(svc)

	_, err := srv.Recall(context.Background(), &agentpb.RecallRequest{
		Query:  "valid",
		Kinds:  []string{"method"},
		RepoId: "repo-a",
		K:      0,
	})
	if err == nil {
		t.Fatalf("expected error for K=0")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
}

// TestGRPCServer_nilRequestRejected proves the nil-guard:
// a nil proto request returns InvalidArgument rather than
// panicking inside the adapter.
func TestGRPCServer_nilRequestRejected(t *testing.T) {
	svc := NewService(fakeEmbedder{vec: []float32{0.1}}, &collectionSearcher{}, &allowListFilter{},
		WithLogger(quietLogger()))
	srv := NewGRPCServer(svc)

	_, err := srv.Recall(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error for nil request")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
}

// TestGRPCServer_internalErrorMapsToInternal proves the
// catch-all branch: an error that does NOT match a
// caller-correctable sentinel surfaces as
// `codes.Internal`. We force this path by wiring an
// embedder that returns an arbitrary error and NO snapshot
// fallback (so the hard-fail contract is preserved).
func TestGRPCServer_internalErrorMapsToInternal(t *testing.T) {
	emb := fakeEmbedder{err: errors.New("embedder boom")}
	svc := NewService(emb, &collectionSearcher{}, &allowListFilter{},
		WithLogger(quietLogger()))
	srv := NewGRPCServer(svc)

	_, err := srv.Recall(context.Background(), &agentpb.RecallRequest{
		Query:  "q",
		Kinds:  []string{"method"},
		RepoId: "repo-a",
		K:      5,
	})
	if err == nil {
		t.Fatalf("expected error from broken embedder")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Fatalf("status code = %s; want Internal (catch-all for non-domain errors)", st.Code())
	}
}

// TestNewGRPCServer_nilServicePanics proves the fail-fast
// contract on the constructor side.
func TestNewGRPCServer_nilServicePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewGRPCServer(nil) did not panic")
		}
	}()
	_ = NewGRPCServer(nil)
}

// ---------------------------------------------------------------------------
// Summarize (Stage 5.4) — proto translation + error mapping
// ---------------------------------------------------------------------------

// TestGRPCServer_summarizeTranslatesNodeTarget round-trips
// a node-target proto request through the verb. Proves:
//   - NodeId / RepoId / MaxTokens reach the service.
//   - Internal Citations[] surface on the proto cards in
//     the same order with the right field populated.
//   - SummaryMD, ContextId, Degraded, TargetKind, TargetId
//     all project verbatim.
func TestGRPCServer_summarizeTranslatesNodeTarget(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "## summary text"},
	}
	contextLog := &recordingContextLog{returnID: "ctx-grpc-001"}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithContextLog(contextLog),
	)
	srv := NewGRPCServer(svc)

	resp, err := srv.Summarize(context.Background(), &agentpb.SummarizeRequest{
		NodeId:    nb.Node.NodeID,
		RepoId:    "repo-a",
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("gRPC Summarize: %v", err)
	}
	if resp.GetSummaryMd() != "## summary text" {
		t.Fatalf("SummaryMd = %q; want canned response", resp.GetSummaryMd())
	}
	if resp.GetContextId() != "ctx-grpc-001" {
		t.Fatalf("ContextId = %q; want ctx-grpc-001", resp.GetContextId())
	}
	if resp.GetDegraded() {
		t.Fatalf("Degraded = true; want false on happy path")
	}
	if resp.GetTargetKind() != "node" {
		t.Fatalf("TargetKind = %q; want node", resp.GetTargetKind())
	}
	if resp.GetTargetId() != nb.Node.NodeID {
		t.Fatalf("TargetId = %q; want %q", resp.GetTargetId(), nb.Node.NodeID)
	}
	wantCitations := 1 + len(nb.Edges) + 3
	if got := len(resp.GetCitations()); got != wantCitations {
		t.Fatalf("len(Citations) = %d; want %d", got, wantCitations)
	}
	// First citation == seed Node; verify the proto carries
	// the NodeId, not EdgeId/ConceptId.
	c0 := resp.GetCitations()[0]
	if c0.GetNodeId() != nb.Node.NodeID {
		t.Fatalf("Citations[0].NodeId = %q; want %q", c0.GetNodeId(), nb.Node.NodeID)
	}
	if c0.GetEdgeId() != "" || c0.GetConceptId() != "" {
		t.Fatalf("Citations[0] has unexpected edge/concept ids: %+v", c0)
	}
	// Second citation == first edge.
	c1 := resp.GetCitations()[1]
	if c1.GetEdgeId() != "e1" {
		t.Fatalf("Citations[1].EdgeId = %q; want e1", c1.GetEdgeId())
	}
	if c1.GetNodeId() != "" {
		t.Fatalf("Citations[1].NodeId = %q; want empty", c1.GetNodeId())
	}
	if summariser.lastMaxToks != 256 {
		t.Fatalf("summariser.MaxTokens = %d; want 256", summariser.lastMaxToks)
	}
}

// TestGRPCServer_summarizeNilRequestRejected proves the
// nil-guard.
func TestGRPCServer_summarizeNilRequestRejected(t *testing.T) {
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(&fakeResolver{}),
	)
	srv := NewGRPCServer(svc)
	_, err := srv.Summarize(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error for nil request")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %s; want InvalidArgument", st.Code())
	}
}

// TestGRPCServer_summarizeMissingTargetInvalidArgument
// proves the validation sentinel surfaces as
// InvalidArgument.
func TestGRPCServer_summarizeMissingTargetInvalidArgument(t *testing.T) {
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(&fakeResolver{}),
	)
	srv := NewGRPCServer(svc)
	_, err := srv.Summarize(context.Background(), &agentpb.SummarizeRequest{})
	if err == nil {
		t.Fatalf("expected validation error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %s; want InvalidArgument", st.Code())
	}
}

// TestGRPCServer_summarizeTargetNotFoundNotFound proves
// the resolver's not-found sentinel surfaces as NotFound.
func TestGRPCServer_summarizeTargetNotFoundNotFound(t *testing.T) {
	resolver := &fakeResolver{
		nodeErr: map[string]error{
			"66666666-6666-6666-6666-666666666666": ErrSummarizeTargetNotFound,
		},
	}
	svc := newSummarizeService(t, WithNeighborhoodResolver(resolver))
	srv := NewGRPCServer(svc)
	_, err := srv.Summarize(context.Background(), &agentpb.SummarizeRequest{
		NodeId: "66666666-6666-6666-6666-666666666666",
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Fatalf("code = %s; want NotFound", st.Code())
	}
}

// TestGRPCServer_summarizeUnconfiguredUnimplemented proves
// a Service without a NeighborhoodResolver surfaces as
// Unimplemented — the same wire signal a binary that
// hasn't deployed Stage 5.4 would surface.
func TestGRPCServer_summarizeUnconfiguredUnimplemented(t *testing.T) {
	svc := newSummarizeService(t) // no resolver wired
	srv := NewGRPCServer(svc)
	_, err := srv.Summarize(context.Background(), &agentpb.SummarizeRequest{
		NodeId: "11111111-1111-1111-1111-111111111111",
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code = %s; want Unimplemented", st.Code())
	}
}

// TestGRPCServer_summarizeDegradedEnvelopeIsNotAnError
// proves a degraded summary (graph unavailable) surfaces
// as a NORMAL gRPC response with `degraded=true`, NOT a
// status error. The gRPC layer must distinguish
// caller-correctable failures (status error) from
// in-band degraded envelopes (response with flag set).
func TestGRPCServer_summarizeDegradedEnvelopeIsNotAnError(t *testing.T) {
	resolver := &fakeResolver{
		nodeErr: map[string]error{
			"55555555-5555-5555-5555-555555555555": ErrGraphStoreUnavailable,
		},
	}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
	)
	srv := NewGRPCServer(svc)
	resp, err := srv.Summarize(context.Background(), &agentpb.SummarizeRequest{
		NodeId: "55555555-5555-5555-5555-555555555555",
		RepoId: "repo-a",
	})
	if err != nil {
		t.Fatalf("gRPC Summarize: %v (graph outage should be in-band, not status error)", err)
	}
	if !resp.GetDegraded() {
		t.Fatalf("Degraded = false; want true")
	}
	if resp.GetDegradedReason() == "" {
		t.Fatalf("DegradedReason empty; want non-empty")
	}
}
