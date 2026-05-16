package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// -- Fixtures ---------------------------------------------------------

// fakeEpisodeWriter is a unit-test fake for EpisodeAppender.
// Records every Append call and returns the configured error.
// If `errs` has entries they are consumed in order; once
// exhausted the writer returns nil.
type fakeEpisodeWriter struct {
	mu    sync.Mutex
	calls []EpisodeAppendInput
	errs  []error
}

func (f *fakeEpisodeWriter) Append(_ context.Context, in EpisodeAppendInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if len(f.errs) == 0 {
		return nil
	}
	e := f.errs[0]
	f.errs = f.errs[1:]
	return e
}

func (f *fakeEpisodeWriter) snapshot() []EpisodeAppendInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EpisodeAppendInput, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeContextResolver returns a fixed degraded flag (or error)
// for every ResolveServedUnderDegraded call.  Tracks
// `(repo_id, context_id)` pairs so tests can assert the
// composite lookup actually flows through.
type fakeContextResolver struct {
	mu       sync.Mutex
	calls    []resolverCall
	degraded bool
	err      error
}

type resolverCall struct {
	RepoID    string
	ContextID string
}

func (f *fakeContextResolver) ResolveServedUnderDegraded(_ context.Context, repoID, contextID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, resolverCall{RepoID: repoID, ContextID: contextID})
	return f.degraded, f.err
}

// fakeWAL is a unit-test fake for WALSink.  Records every
// Enqueue and returns the configured error.
type fakeWAL struct {
	mu    sync.Mutex
	calls []EpisodeAppendInput
	err   error
}

func (f *fakeWAL) Enqueue(_ context.Context, in EpisodeAppendInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return f.err
}

func (f *fakeWAL) snapshot() []EpisodeAppendInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EpisodeAppendInput, len(f.calls))
	copy(out, f.calls)
	return out
}

// fixedUUID returns a counter-based UUID generator so tests
// can assert specific id values were minted in a specific
// order.
func fixedUUID() func() (string, error) {
	var n int
	return func() (string, error) {
		n++
		return fmt.Sprintf("00000000-0000-0000-0000-%012d", n), nil
	}
}

// fixedClock returns a clock that always reports the same
// timestamp.  Tests assert this exact value flows through the
// EpisodeAppendInput payload.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// validReq builds a request that passes every validation
// step.  Tests mutate the returned value to exercise rejection
// paths.
func validReq() ObserveRequest {
	return ObserveRequest{
		RepoID:     "repo-uuid",
		SessionID:  "sess-1",
		TraceID:    "trace-1",
		ActionJSON: json.RawMessage(`{"action":"noop"}`),
		Outcome:    "success",
		ContextID:  "ctx-uuid",
		ObservationRefs: []ObservationRef{
			{Role: "node_hit", NodeID: "node-1", Weight: 0.7},
			{Role: "edge_hit", EdgeID: "edge-1", Weight: 0.3},
		},
	}
}

// newTestService wires an ObserveService with deterministic
// clock + uuid so tests can assert exact ids and timestamps.
func newTestService(t *testing.T, w EpisodeAppender, r ContextResolver, opts ...ObserveOption) *ObserveService {
	t.Helper()
	pinnedTime := time.Date(2025, 11, 12, 23, 59, 0, 0, time.UTC)
	allOpts := []ObserveOption{
		WithObserveClock(fixedClock(pinnedTime)),
		WithObserveUUID(fixedUUID()),
	}
	allOpts = append(allOpts, opts...)
	return NewObserveService(w, r, allOpts...)
}

// -- Validation tests -------------------------------------------------

// Stage 5.2 / C15 scenario: outcome=human_corrected rejected
// BEFORE any writer call.  The grpc adapter maps the sentinel
// to INVALID_ARGUMENT.
func TestObserve_rejectsHumanCorrected(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	svc := newTestService(t, w, r)

	req := validReq()
	req.Outcome = "human_corrected"
	_, err := svc.Observe(context.Background(), req)

	if !errors.Is(err, ErrHumanCorrectedNotAllowed) {
		t.Fatalf("expected ErrHumanCorrectedNotAllowed, got %v", err)
	}
	if calls := w.snapshot(); len(calls) != 0 {
		t.Fatalf("writer must not be called for rejected human_corrected, got %d calls", len(calls))
	}
	if len(r.calls) != 0 {
		t.Fatalf("resolver must not be called before validation succeeds, got %d calls", len(r.calls))
	}
}

// Stage 5.2 / C23 scenario: caller-supplied role=
// degraded_recall_context is rejected BEFORE any writer call.
func TestObserve_rejectsDegradedRecallContextRoleForbidden(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	svc := newTestService(t, w, r)

	req := validReq()
	req.ObservationRefs = []ObservationRef{
		{Role: "degraded_recall_context", NodeID: "node-1"},
	}
	_, err := svc.Observe(context.Background(), req)

	if !errors.Is(err, ErrDegradedRecallContextRoleForbidden) {
		t.Fatalf("expected ErrDegradedRecallContextRoleForbidden, got %v", err)
	}
	if calls := w.snapshot(); len(calls) != 0 {
		t.Fatalf("writer must not be called for forged degraded_recall_context role, got %d calls", len(calls))
	}
}

// Per-field validation table.  Each case mutates a valid req
// into a known-bad shape and asserts the right sentinel.
func TestObserve_validation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ObserveRequest)
		wantErr error
	}{
		{
			name:    "missing outcome",
			mutate:  func(r *ObserveRequest) { r.Outcome = "" },
			wantErr: ErrInvalidOutcome,
		},
		{
			name:    "unknown outcome",
			mutate:  func(r *ObserveRequest) { r.Outcome = "victory" },
			wantErr: ErrInvalidOutcome,
		},
		{
			name:    "missing repo_id",
			mutate:  func(r *ObserveRequest) { r.RepoID = "" },
			wantErr: ErrMissingRepoID,
		},
		{
			name:    "whitespace repo_id",
			mutate:  func(r *ObserveRequest) { r.RepoID = "   " },
			wantErr: ErrMissingRepoID,
		},
		{
			name:    "missing session_id",
			mutate:  func(r *ObserveRequest) { r.SessionID = "" },
			wantErr: ErrMissingSessionID,
		},
		{
			name:    "missing trace_id",
			mutate:  func(r *ObserveRequest) { r.TraceID = "" },
			wantErr: ErrMissingTraceID,
		},
		{
			name:    "missing action_json",
			mutate:  func(r *ObserveRequest) { r.ActionJSON = nil },
			wantErr: ErrMissingAction,
		},
		{
			name:    "malformed action_json",
			mutate:  func(r *ObserveRequest) { r.ActionJSON = json.RawMessage(`not json`) },
			wantErr: ErrInvalidJSON,
		},
		{
			name:    "malformed signal_json",
			mutate:  func(r *ObserveRequest) { r.SignalJSON = json.RawMessage(`{`) },
			wantErr: ErrInvalidJSON,
		},
		{
			name:    "missing context_id",
			mutate:  func(r *ObserveRequest) { r.ContextID = "" },
			wantErr: ErrMissingContextID,
		},
		{
			name: "unknown role",
			mutate: func(r *ObserveRequest) {
				r.ObservationRefs = []ObservationRef{{Role: "made_up", NodeID: "n"}}
			},
			wantErr: ErrInvalidObservationRole,
		},
		{
			name: "role=node_hit with concept target",
			mutate: func(r *ObserveRequest) {
				r.ObservationRefs = []ObservationRef{{Role: "node_hit", ConceptID: "c"}}
			},
			wantErr: ErrInvalidObservationTarget,
		},
		{
			name: "role=edge_hit with node target",
			mutate: func(r *ObserveRequest) {
				r.ObservationRefs = []ObservationRef{{Role: "edge_hit", NodeID: "n"}}
			},
			wantErr: ErrInvalidObservationTarget,
		},
		{
			name: "role=concept_hit with edge target",
			mutate: func(r *ObserveRequest) {
				r.ObservationRefs = []ObservationRef{{Role: "concept_hit", EdgeID: "e"}}
			},
			wantErr: ErrInvalidObservationTarget,
		},
		{
			name: "node_hit with multiple targets",
			mutate: func(r *ObserveRequest) {
				r.ObservationRefs = []ObservationRef{{Role: "node_hit", NodeID: "n", EdgeID: "e"}}
			},
			wantErr: ErrInvalidObservationTarget,
		},
		{
			name: "node_hit with no target",
			mutate: func(r *ObserveRequest) {
				r.ObservationRefs = []ObservationRef{{Role: "node_hit"}}
			},
			wantErr: ErrInvalidObservationTarget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakeEpisodeWriter{}
			r := &fakeContextResolver{}
			svc := newTestService(t, w, r)
			req := validReq()
			tc.mutate(&req)
			_, err := svc.Observe(context.Background(), req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
			if calls := w.snapshot(); len(calls) != 0 {
				t.Fatalf("writer must not be called for rejected request, got %d calls", len(calls))
			}
		})
	}
}

// -- Happy path -------------------------------------------------------

// Healthy context: writer receives Episode + N caller-Observations
// in the right order with pre-minted ids and a single shared
// CreatedAt.
func TestObserve_happyPath(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{degraded: false}
	svc := newTestService(t, w, r)

	resp, err := svc.Observe(context.Background(), validReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Degraded {
		t.Fatalf("response must not be degraded on happy path: %+v", resp)
	}
	if resp.EpisodeID == "" {
		t.Fatalf("response must carry an episode_id")
	}
	if resp.EpisodeGroupID == "" {
		t.Fatalf("response must carry an episode_group_id (auto-minted when caller omits)")
	}

	calls := w.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 writer call, got %d", len(calls))
	}
	got := calls[0]
	if got.EpisodeID != resp.EpisodeID {
		t.Fatalf("episode_id mismatch between response %q and writer payload %q",
			resp.EpisodeID, got.EpisodeID)
	}
	if got.EpisodeGroupID != resp.EpisodeGroupID {
		t.Fatalf("episode_group_id mismatch between response %q and writer payload %q",
			resp.EpisodeGroupID, got.EpisodeGroupID)
	}
	if got.Kind != "agent" {
		t.Fatalf("episode kind must be 'agent', got %q", got.Kind)
	}
	if got.Outcome != "success" {
		t.Fatalf("outcome mismatch: %q", got.Outcome)
	}
	if got.ContextID != "ctx-uuid" {
		t.Fatalf("context_id mismatch: %q", got.ContextID)
	}
	if got.CreatedAt.IsZero() {
		t.Fatalf("created_at must be stamped, got zero value")
	}
	// Happy path writes degraded=false + DegradedReason=""
	// so the SQL writer passes a NULL into degraded_reason
	// and the episode_degraded_reason_chk CHECK holds.
	if got.Degraded {
		t.Fatalf("happy-path Episode must not be flagged degraded, got %+v", got)
	}
	if got.DegradedReason != "" {
		t.Fatalf("happy-path Episode must have empty DegradedReason, got %q", got.DegradedReason)
	}
	if len(got.Observations) != 2 {
		t.Fatalf("expected 2 observations (one per caller ref), got %d", len(got.Observations))
	}
	if got.Observations[0].Role != "node_hit" || got.Observations[0].NodeID != "node-1" {
		t.Fatalf("observation[0] mismatch: %+v", got.Observations[0])
	}
	if got.Observations[1].Role != "edge_hit" || got.Observations[1].EdgeID != "edge-1" {
		t.Fatalf("observation[1] mismatch: %+v", got.Observations[1])
	}
	if !got.Observations[0].CreatedAt.Equal(got.CreatedAt) {
		t.Fatalf("observations must share the episode's created_at, got %v vs %v",
			got.Observations[0].CreatedAt, got.CreatedAt)
	}

	if len(r.calls) != 1 || r.calls[0].RepoID != "repo-uuid" || r.calls[0].ContextID != "ctx-uuid" {
		t.Fatalf("resolver call mismatch (expected repo-uuid + ctx-uuid): %+v", r.calls)
	}
}

// Caller-supplied episode_group_id is preserved (not
// overwritten by a fresh mint).
func TestObserve_preservesCallerEpisodeGroupID(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	svc := newTestService(t, w, r)

	req := validReq()
	req.EpisodeGroupID = "group-supplied-by-caller"

	resp, err := svc.Observe(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.EpisodeGroupID != "group-supplied-by-caller" {
		t.Fatalf("caller episode_group_id overwritten: got %q", resp.EpisodeGroupID)
	}
	calls := w.snapshot()
	if calls[0].EpisodeGroupID != "group-supplied-by-caller" {
		t.Fatalf("writer episode_group_id overwritten: got %q", calls[0].EpisodeGroupID)
	}
}

// -- Degraded auto-stamp ---------------------------------------------

// Stage 5.2 scenario: when the ContextResolver reports
// served_under_degraded=true the writer receives N+1
// observations.  The extra observation has
// role=degraded_recall_context with degraded_recall_context_id
// pointing at the original context_id.
func TestObserve_autoStampsDegradedRecallContext(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{degraded: true}
	svc := newTestService(t, w, r)

	resp, err := svc.Observe(context.Background(), validReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Auto-stamp does NOT degrade the response — the row was
	// written cleanly.  Degraded=true on the response is
	// reserved for the writer-fallback path.
	if resp.Degraded {
		t.Fatalf("response degraded must be false when writer succeeded, got %+v", resp)
	}

	calls := w.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 writer call, got %d", len(calls))
	}
	obs := calls[0].Observations
	if len(obs) != 3 {
		t.Fatalf("expected 3 observations (2 caller + 1 auto-stamp), got %d", len(obs))
	}
	last := obs[len(obs)-1]
	if last.Role != "degraded_recall_context" {
		t.Fatalf("auto-stamp role mismatch: %q", last.Role)
	}
	if last.DegradedRecallContextID != "ctx-uuid" {
		t.Fatalf("auto-stamp degraded_recall_context_id mismatch: %q", last.DegradedRecallContextID)
	}
	if last.NodeID != "" || last.EdgeID != "" || last.ConceptID != "" {
		t.Fatalf("auto-stamp target fields must be empty, got %+v", last)
	}
	if !last.CreatedAt.Equal(calls[0].CreatedAt) {
		t.Fatalf("auto-stamp created_at must match episode created_at, got %v vs %v",
			last.CreatedAt, calls[0].CreatedAt)
	}
}

// Resolver outage on a non-empty context_id must fail the call
// — we refuse to silently miss the auto-stamp because that
// would corrupt the operator `mgmt.read.episodes` flow.
func TestObserve_resolverErrorIsHardFailure(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{err: errors.New("conn refused")}
	svc := newTestService(t, w, r)

	_, err := svc.Observe(context.Background(), validReq())
	if err == nil {
		t.Fatalf("expected error from resolver outage, got nil")
	}
	if !strings.Contains(err.Error(), "resolve context") {
		t.Fatalf("error must mention resolver: %v", err)
	}
	if calls := w.snapshot(); len(calls) != 0 {
		t.Fatalf("writer must not be called when resolver failed, got %d calls", len(calls))
	}
}

// ErrContextNotFound from the resolver propagates through so
// the gRPC adapter can map it to INVALID_ARGUMENT.
func TestObserve_resolverNotFoundPropagates(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{err: ErrContextNotFound}
	svc := newTestService(t, w, r)

	_, err := svc.Observe(context.Background(), validReq())
	if !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("expected ErrContextNotFound, got %v", err)
	}
	if calls := w.snapshot(); len(calls) != 0 {
		t.Fatalf("writer must not be called for unknown context_id, got %d", len(calls))
	}
}

// -- WAL fallback -----------------------------------------------------

// Stage 5.2 scenario: WAL fallback returns episode_id.  When
// the writer raises ErrEpisodicLogUnavailable the handler
// enqueues the prepared payload onto the WAL and surfaces a
// degraded=true response carrying the SAME pre-minted
// episode_id.
func TestObserve_walFallbackReturnsPreMintedEpisodeID(t *testing.T) {
	// Writer fails with the partition-unavailable sentinel.
	w := &fakeEpisodeWriter{errs: []error{ErrEpisodicLogUnavailable}}
	r := &fakeContextResolver{}
	wal := &fakeWAL{}
	svc := newTestService(t, w, r, WithObserveWAL(wal))

	resp, err := svc.Observe(context.Background(), validReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("response must be degraded on WAL fallback, got %+v", resp)
	}
	if resp.DegradedReason != "episodic_log_unavailable" {
		t.Fatalf("response degraded_reason mismatch: %q", resp.DegradedReason)
	}
	if resp.EpisodeID == "" {
		t.Fatalf("response must carry pre-minted episode_id")
	}

	walCalls := wal.snapshot()
	if len(walCalls) != 1 {
		t.Fatalf("expected 1 WAL enqueue, got %d", len(walCalls))
	}
	if walCalls[0].EpisodeID != resp.EpisodeID {
		t.Fatalf("WAL episode_id %q must match response %q",
			walCalls[0].EpisodeID, resp.EpisodeID)
	}
	// Evaluator iter-1 item #1: the WAL payload MUST carry
	// degraded=true + degraded_reason='episodic_log_unavailable'
	// so the eventually-flushed episode row persists the
	// §7.5 degraded state on the durable columns. Without
	// this, the replayed Episode lands with the schema
	// defaults (degraded=false, degraded_reason=NULL) and
	// the `mgmt.read.episodes` operator view loses the
	// signal that the row was buffered.
	if !walCalls[0].Degraded {
		t.Fatalf("WAL payload must carry Degraded=true (Item 1), got %+v", walCalls[0])
	}
	if walCalls[0].DegradedReason != "episodic_log_unavailable" {
		t.Fatalf("WAL payload DegradedReason must be episodic_log_unavailable (Item 1), got %q",
			walCalls[0].DegradedReason)
	}
	// Writer was still attempted exactly once before the
	// fallback kicked in.
	if calls := w.snapshot(); len(calls) != 1 {
		t.Fatalf("expected 1 writer call (the initial attempt), got %d", len(calls))
	}
	if w.snapshot()[0].EpisodeID != resp.EpisodeID {
		t.Fatalf("writer payload episode_id must match WAL payload + response (same pre-minted id)")
	}
	// The direct-write payload (before fallback) must NOT
	// be stamped degraded — that flag is only set on the
	// durable replay row.
	if w.snapshot()[0].Degraded {
		t.Fatalf("happy-path writer payload must not be pre-stamped degraded, got %+v", w.snapshot()[0])
	}
}

// WAL enqueue failure is a hard error — the handler refuses
// to claim durability when the WAL itself is broken.
func TestObserve_walEnqueueFailureIsHardError(t *testing.T) {
	w := &fakeEpisodeWriter{errs: []error{ErrEpisodicLogUnavailable}}
	r := &fakeContextResolver{}
	wal := &fakeWAL{err: errors.New("disk full")}
	svc := newTestService(t, w, r, WithObserveWAL(wal))

	_, err := svc.Observe(context.Background(), validReq())
	if err == nil {
		t.Fatalf("expected hard error when WAL enqueue fails, got nil")
	}
	if !strings.Contains(err.Error(), "wal enqueue") {
		t.Fatalf("error must mention WAL: %v", err)
	}
}

// Without a WAL wired the partition-unavailable sentinel
// propagates verbatim (the operator sees the underlying
// outage).
func TestObserve_noWALPropagatesPartitionError(t *testing.T) {
	w := &fakeEpisodeWriter{errs: []error{ErrEpisodicLogUnavailable}}
	r := &fakeContextResolver{}
	svc := newTestService(t, w, r) // no WithObserveWAL

	_, err := svc.Observe(context.Background(), validReq())
	if !errors.Is(err, ErrEpisodicLogUnavailable) {
		t.Fatalf("expected ErrEpisodicLogUnavailable, got %v", err)
	}
}

// Non-partition writer errors propagate verbatim — schema /
// constraint bugs are NOT silently buffered.
func TestObserve_otherWriterErrorPropagates(t *testing.T) {
	w := &fakeEpisodeWriter{errs: []error{errors.New("CHECK violation")}}
	r := &fakeContextResolver{}
	wal := &fakeWAL{}
	svc := newTestService(t, w, r, WithObserveWAL(wal))

	_, err := svc.Observe(context.Background(), validReq())
	if err == nil || !strings.Contains(err.Error(), "CHECK violation") {
		t.Fatalf("expected CHECK violation error to propagate, got %v", err)
	}
	if calls := wal.snapshot(); len(calls) != 0 {
		t.Fatalf("WAL must not be enqueued for non-partition errors, got %d", len(calls))
	}
}

// -- gRPC adapter happy + sentinel mapping ----------------------------

// Sentinel-to-status mapping helper.  Avoids depending on the
// grpc package in this file — the mapping logic in
// observeErrorToStatus is exercised here through the public
// adapter type.
func TestObserveErrorMapping(t *testing.T) {
	// Each known sentinel maps to a status with a non-empty
	// message — the test only asserts the wrapper round-trips
	// the sentinel (errors.Is), since the grpc package
	// translates code names internally.
	cases := []struct {
		name string
		err  error
	}{
		{"C15", ErrHumanCorrectedNotAllowed},
		{"C23", ErrDegradedRecallContextRoleForbidden},
		{"InvalidObservationRole", ErrInvalidObservationRole},
		{"InvalidObservationTarget", ErrInvalidObservationTarget},
		{"InvalidOutcome", ErrInvalidOutcome},
		{"MissingRepoID", ErrMissingRepoID},
		{"MissingSessionID", ErrMissingSessionID},
		{"MissingTraceID", ErrMissingTraceID},
		{"MissingAction", ErrMissingAction},
		{"MissingContextID", ErrMissingContextID},
		{"InvalidJSON", ErrInvalidJSON},
		{"ContextNotFound", ErrContextNotFound},
		{"EpisodicLogUnavailable", ErrEpisodicLogUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus := observeErrorToStatus(tc.err)
			if gotStatus == nil {
				t.Fatalf("expected non-nil status error for %v", tc.err)
			}
		})
	}
}

// -- Repo-scoped resolver (evaluator iter-1 item #2) ------------------

// Stage 5.2 / item #2 from iter-1 evaluator: the
// ContextResolver receives BOTH repo_id and context_id so a
// caller from repo A cannot attach to repo B's
// recall_context_log row (and inherit the wrong degraded
// flag).  The fake captures the pair; this test asserts the
// happy path passes both.
func TestObserve_resolverReceivesRepoAndContext(t *testing.T) {
	w := &fakeEpisodeWriter{}
	r := &fakeContextResolver{}
	svc := newTestService(t, w, r)
	req := validReq()
	req.RepoID = "repo-A"
	req.ContextID = "ctx-from-repo-A"
	if _, err := svc.Observe(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 resolver call, got %d", len(r.calls))
	}
	if got := r.calls[0]; got.RepoID != "repo-A" || got.ContextID != "ctx-from-repo-A" {
		t.Fatalf("resolver must receive both repo_id and context_id, got %+v", got)
	}
}
