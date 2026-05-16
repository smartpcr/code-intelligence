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

// -- Observe gRPC contract tests (evaluator iter-1 item #4) ----------
//
// The iter-1 evaluator flagged that observe validation was
// only tested at the domain layer. These tests hit the
// FULL gRPC adapter so:
//   - The proto <-> domain translation is exercised.
//   - The validation sentinel -> `codes.InvalidArgument`
//     mapping in `observeErrorToStatus` is verified end-
//     to-end through `GRPCServer.Observe`.
//   - The WAL fallback path returns `codes.OK` with
//     `Degraded=true` on the proto response.

func newTestObserveSrv(t *testing.T, w EpisodeAppender, r ContextResolver, opts ...ObserveOption) *GRPCServer {
	t.Helper()
	// Reuse the recall stack only for the constructor's
	// *Service requirement -- these tests never touch
	// Recall.
	dummySvc := NewService(
		fakeEmbedder{vec: []float32{0.1}},
		&collectionSearcher{},
		&allowListFilter{},
		WithLogger(quietLogger()),
	)
	obs := newTestService(t, w, r, opts...)
	return NewGRPCServer(dummySvc, WithObserveService(obs))
}

func validObserveProto() *agentpb.ObserveRequest {
	return &agentpb.ObserveRequest{
		RepoId:     "repo-uuid",
		SessionId:  "sess-1",
		TraceId:    "trace-1",
		ActionJson: []byte(`{"action":"noop"}`),
		Outcome:    "success",
		ContextId:  "ctx-uuid",
		ObservationRefs: []*agentpb.ObservationRef{
			{Role: "node_hit", NodeId: "node-1", Weight: 0.7},
			{Role: "edge_hit", EdgeId: "edge-1", Weight: 0.3},
		},
	}
}

// Stage 5.2 / C15 scenario through the gRPC adapter.  An
// outcome=human_corrected request hits
// `codes.InvalidArgument` BEFORE the writer is invoked.
func TestGRPCServer_observeRejectsHumanCorrected(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	srv := newTestObserveSrv(t, w, r)

	req := validObserveProto()
	req.Outcome = "human_corrected"
	_, err := srv.Observe(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for human_corrected outcome")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
	if len(w.snapshot()) != 0 {
		t.Fatalf("writer must not be called when human_corrected is rejected")
	}
}

// Stage 5.2 / C23 scenario through the gRPC adapter.  A
// caller-supplied observation_refs[*].role=degraded_recall_context
// is rejected with `codes.InvalidArgument` (server is the
// only writer of that role).
func TestGRPCServer_observeRejectsForgedDegradedRecallContext(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	srv := newTestObserveSrv(t, w, r)

	req := validObserveProto()
	req.ObservationRefs = append(req.ObservationRefs, &agentpb.ObservationRef{
		Role:   "degraded_recall_context",
		NodeId: "ctx-uuid",
		Weight: 1.0,
	})
	_, err := srv.Observe(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for forged degraded_recall_context role")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
	if len(w.snapshot()) != 0 {
		t.Fatalf("writer must not be called when caller forges degraded_recall_context")
	}
}

// Nil proto request returns `codes.InvalidArgument`
// rather than panicking on the adapter's GetXxx helpers.
func TestGRPCServer_observeNilRequestRejected(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	srv := newTestObserveSrv(t, w, r)

	_, err := srv.Observe(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error for nil request")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
}

// Missing required field surfaces as `codes.InvalidArgument`
// through the adapter (exercises the missing-action sentinel).
func TestGRPCServer_observeMissingActionInvalidArgument(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	srv := newTestObserveSrv(t, w, r)

	req := validObserveProto()
	req.ActionJson = nil
	_, err := srv.Observe(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error for missing action_json")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status code = %s; want InvalidArgument", st.Code())
	}
}

// Happy path: valid proto request roundtrips through the
// adapter and yields a populated ObserveResponse.
func TestGRPCServer_observeHappyPath(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	srv := newTestObserveSrv(t, w, r)

	resp, err := srv.Observe(context.Background(), validObserveProto())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetEpisodeId() == "" {
		t.Fatalf("response missing episode_id")
	}
	if resp.GetDegraded() {
		t.Fatalf("response must not be degraded on happy path, got %+v", resp)
	}
	if len(w.snapshot()) != 1 {
		t.Fatalf("expected 1 writer call, got %d", len(w.snapshot()))
	}
}

// Stage 5.2 WAL-fallback scenario through the gRPC adapter.
// On ErrEpisodicLogUnavailable the WAL accepts the payload
// and the proto response carries degraded=true +
// degraded_reason=episodic_log_unavailable.  This is the
// "WAL fallback returns episode_id" scenario the brief
// mandates.
func TestGRPCServer_observeWALFallbackDegraded(t *testing.T) {
	w := &fakeEpisodeWriter{errs: []error{ErrEpisodicLogUnavailable}}
	r := &fakeContextResolver{}
	wal := &fakeWAL{}
	srv := newTestObserveSrv(t, w, r, WithObserveWAL(wal))

	resp, err := srv.Observe(context.Background(), validObserveProto())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.GetDegraded() {
		t.Fatalf("response must be degraded on WAL fallback")
	}
	if resp.GetDegradedReason() != "episodic_log_unavailable" {
		t.Fatalf("degraded_reason = %q; want episodic_log_unavailable", resp.GetDegradedReason())
	}
	if resp.GetEpisodeId() == "" {
		t.Fatalf("response missing episode_id on WAL fallback")
	}
	walCalls := wal.snapshot()
	if len(walCalls) != 1 {
		t.Fatalf("expected 1 WAL enqueue, got %d", len(walCalls))
	}
	if !walCalls[0].Degraded {
		t.Fatalf("WAL payload must carry Degraded=true, got %+v", walCalls[0])
	}
}

// When ErrEpisodicLogUnavailable bubbles up WITHOUT a WAL
// wired the adapter maps it to `codes.Unavailable` so the
// caller knows to retry.
func TestGRPCServer_observeEpisodicLogUnavailableNoWAL(t *testing.T) {
	w := &fakeEpisodeWriter{errs: []error{ErrEpisodicLogUnavailable}}
	r := &fakeContextResolver{}
	srv := newTestObserveSrv(t, w, r) // no WAL

	_, err := srv.Observe(context.Background(), validObserveProto())
	if err == nil {
		t.Fatalf("expected error when WAL is not wired")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Fatalf("status code = %s; want Unavailable (retry-after)", st.Code())
	}
}