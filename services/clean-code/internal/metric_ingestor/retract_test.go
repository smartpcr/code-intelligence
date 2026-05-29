package metric_ingestor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

// fixedClock returns the given time on every call. Used so
// tests can assert on `scan_run.started_at` /
// `metric_retraction.created_at` without flake.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", s, err)
	}
	return id
}

func newSeededStore(t *testing.T, sampleID uuid.UUID, repoID uuid.UUID, sha string) *metric_ingestor.InMemoryRetractStore {
	t.Helper()
	s := metric_ingestor.NewInMemoryRetractStore()
	s.SeedSample(sampleID, repoID, sha)
	return s
}

// TestRetractRequest_Validate pins the per-field validation
// surface so callers can front-load validation without
// dispatching.
func TestRetractRequest_Validate(t *testing.T) {
	t.Parallel()

	sampleID := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	good := metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     "vendored file -- not our code",
		AppendedBy: "operator:alice@example.com",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good Validate(): err=%v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*metric_ingestor.RetractRequest)
		want error
	}{
		{"zero-SampleID", func(r *metric_ingestor.RetractRequest) { r.SampleID = uuid.Nil }, metric_ingestor.ErrRetractZeroSampleID},
		{"empty-Reason", func(r *metric_ingestor.RetractRequest) { r.Reason = "" }, metric_ingestor.ErrRetractEmptyReason},
		{"whitespace-Reason", func(r *metric_ingestor.RetractRequest) { r.Reason = "   " }, metric_ingestor.ErrRetractEmptyReason},
		{"empty-AppendedBy", func(r *metric_ingestor.RetractRequest) { r.AppendedBy = "" }, metric_ingestor.ErrRetractEmptyAppendedBy},
		{"whitespace-AppendedBy", func(r *metric_ingestor.RetractRequest) { r.AppendedBy = "  " }, metric_ingestor.ErrRetractEmptyAppendedBy},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := good
			tc.mut(&req)
			if err := req.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate(): err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestRetractDispatcher_RetractAppendsRetractionRow pins the
// happy path: an active sample, dispatch returns the
// retraction row, a fresh scan_run(kind='retract',
// status='succeeded') is recorded, and the metric_retraction
// row has the right shape.
func TestRetractDispatcher_RetractAppendsRetractionRow(t *testing.T) {
	t.Parallel()

	sampleID := mustUUID(t, "22222222-3333-4444-5555-666666666666")
	repoID := mustUUID(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	clockT := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	store := newSeededStore(t, sampleID, repoID, sha)

	d := metric_ingestor.NewRetractDispatcher(store, store, store,
		metric_ingestor.WithRetractDispatcherClock(fixedClock(clockT)))

	res, err := d.Dispatch(context.Background(), metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     "file is vendored",
		AppendedBy: "operator:alice@example.com",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.Inserted {
		t.Errorf("Inserted=false, want true on first retract")
	}
	if res.ScanRunID == uuid.Nil {
		t.Error("ScanRunID is the zero UUID")
	}
	if res.Retraction.RetractionID == uuid.Nil {
		t.Error("Retraction.RetractionID is the zero UUID")
	}
	if res.Retraction.SampleID != sampleID {
		t.Errorf("Retraction.SampleID=%s, want %s", res.Retraction.SampleID, sampleID)
	}
	if res.Retraction.Reason != "file is vendored" {
		t.Errorf("Retraction.Reason=%q, want %q", res.Retraction.Reason, "file is vendored")
	}
	if res.Retraction.AppendedBy != "operator:alice@example.com" {
		t.Errorf("Retraction.AppendedBy=%q, want %q", res.Retraction.AppendedBy, "operator:alice@example.com")
	}
	if !res.Retraction.CreatedAt.Equal(clockT) {
		t.Errorf("Retraction.CreatedAt=%v, want %v", res.Retraction.CreatedAt, clockT)
	}

	// scan_run row: kind='retract', status='succeeded',
	// to_sha=sha.
	rec, ok := store.ScanRunRecord(res.ScanRunID)
	if !ok {
		t.Fatalf("scan_run id=%s not recorded in store", res.ScanRunID)
	}
	if rec.Kind != metric_ingestor.ScanRunKindRetract {
		t.Errorf("scan_run.kind=%q, want %q", rec.Kind, metric_ingestor.ScanRunKindRetract)
	}
	if rec.Status != metric_ingestor.ScanRunStatusSucceeded {
		t.Errorf("scan_run.status=%q, want %q", rec.Status, metric_ingestor.ScanRunStatusSucceeded)
	}
	if rec.SHABinding != metric_ingestor.SHABindingSingle {
		t.Errorf("scan_run.sha_binding=%q, want %q", rec.SHABinding, metric_ingestor.SHABindingSingle)
	}
	if rec.ToSHA != sha {
		t.Errorf("scan_run.to_sha=%q, want %q", rec.ToSHA, sha)
	}
	if rec.RepoID != repoID {
		t.Errorf("scan_run.repo_id=%s, want %s", rec.RepoID, repoID)
	}

	if store.CountRetractions() != 1 {
		t.Errorf("CountRetractions=%d, want 1", store.CountRetractions())
	}
	if store.CountScanRuns() != 1 {
		t.Errorf("CountScanRuns=%d, want 1", store.CountScanRuns())
	}
}

// TestRetractDispatcher_Idempotent pins the impl-plan
// line 331 contract: a second retract for the
// already-retracted sample is a no-op (returns the
// existing row, Inserted=false). The metric_retraction
// table still has exactly ONE row. Stage 3.4 evaluator
// scenario `retract-appends-retraction-row` is satisfied
// by the FIRST dispatch above; this test pins idempotency
// at the dispatcher layer (the integration test will
// re-verify at the HTTP layer).
func TestRetractDispatcher_Idempotent(t *testing.T) {
	t.Parallel()

	sampleID := mustUUID(t, "44444444-5555-6666-7777-888888888888")
	repoID := mustUUID(t, "aaaaaaaa-1111-2222-3333-444444444444")
	sha := "cafebabecafebabecafebabecafebabecafebabe"
	store := newSeededStore(t, sampleID, repoID, sha)

	d := metric_ingestor.NewRetractDispatcher(store, store, store)

	first, err := d.Dispatch(context.Background(), metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     "first retract",
		AppendedBy: "operator:alice",
	})
	if err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	if !first.Inserted {
		t.Errorf("first.Inserted=false, want true")
	}

	// Second dispatch with a different reason / actor.
	// The retraction row stays the ORIGINAL one; the
	// dispatcher returns Inserted=false.
	second, err := d.Dispatch(context.Background(), metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     "second retract (different reason)",
		AppendedBy: "operator:bob",
	})
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if second.Inserted {
		t.Errorf("second.Inserted=true, want false (idempotent no-op)")
	}
	if second.Retraction.RetractionID != first.Retraction.RetractionID {
		t.Errorf("second.RetractionID=%s, want first.RetractionID=%s (idempotent)",
			second.Retraction.RetractionID, first.Retraction.RetractionID)
	}
	if second.Retraction.Reason != "first retract" {
		t.Errorf("second.Reason=%q, want %q (original reason preserved)",
			second.Retraction.Reason, "first retract")
	}
	if second.Retraction.AppendedBy != "operator:alice" {
		t.Errorf("second.AppendedBy=%q, want %q (original actor preserved)",
			second.Retraction.AppendedBy, "operator:alice")
	}

	// The metric_retraction table has exactly ONE row.
	if got := store.CountRetractions(); got != 1 {
		t.Errorf("CountRetractions=%d, want 1 (UNIQUE on sample_id)", got)
	}

	// Iter 2 fix #1: the sequential idempotent no-op MUST
	// NOT open a new scan_run row. Before iter 2 the
	// dispatcher opened a fresh scan_run BEFORE probing
	// the retraction store, contradicting the documented
	// "ScanRunID is uuid.Nil on the idempotent no-op
	// path" contract on [RetractResult]. The test pins
	// the post-fix behaviour at two layers:
	//   - the wire-layer result has ScanRunID=Nil;
	//   - the underlying scan_run table has exactly ONE
	//     row (the FIRST dispatch's row), not two.
	if second.ScanRunID != uuid.Nil {
		t.Errorf("second.ScanRunID=%s, want uuid.Nil (no new scan_run on sequential idempotent path)", second.ScanRunID)
	}
	if got := store.CountScanRuns(); got != 1 {
		t.Errorf("CountScanRuns=%d, want 1 (idempotent no-op MUST NOT open a fresh scan_run row)", got)
	}
}

// TestRetractDispatcher_UnknownSampleReturnsSentinel verifies
// the dispatcher refuses to retract a sample that doesn't
// exist in the Measurement sub-store. The wire layer maps
// this to 404.
func TestRetractDispatcher_UnknownSampleReturnsSentinel(t *testing.T) {
	t.Parallel()

	missing := mustUUID(t, "deadbeef-dead-beef-dead-beefdeadbeef")
	store := metric_ingestor.NewInMemoryRetractStore() // no SeedSample

	d := metric_ingestor.NewRetractDispatcher(store, store, store)

	_, err := d.Dispatch(context.Background(), metric_ingestor.RetractRequest{
		SampleID:   missing,
		Reason:     "doesn't matter",
		AppendedBy: "operator:carol",
	})
	if !errors.Is(err, metric_ingestor.ErrRetractUnknownSample) {
		t.Fatalf("Dispatch: err=%v, want ErrRetractUnknownSample", err)
	}
	// NO scan_run row was opened -- the sample didn't
	// resolve; the dispatcher short-circuits BEFORE
	// OpenRetractScanRun.
	if got := store.CountScanRuns(); got != 0 {
		t.Errorf("CountScanRuns=%d, want 0 (no scan_run opened for missing sample)", got)
	}
	if got := store.CountRetractions(); got != 0 {
		t.Errorf("CountRetractions=%d, want 0 (no metric_retraction for missing sample)", got)
	}
}

// TestRetractDispatcher_RejectsInvalidRequest pins the
// validation order so a wire-layer caller that omits a
// required field gets ErrRetract* back instead of a 500.
func TestRetractDispatcher_RejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	store := metric_ingestor.NewInMemoryRetractStore()
	d := metric_ingestor.NewRetractDispatcher(store, store, store)

	for _, tc := range []struct {
		name string
		req  metric_ingestor.RetractRequest
		want error
	}{
		{
			"zero-SampleID",
			metric_ingestor.RetractRequest{Reason: "x", AppendedBy: "y"},
			metric_ingestor.ErrRetractZeroSampleID,
		},
		{
			"empty-Reason",
			metric_ingestor.RetractRequest{
				SampleID:   mustUUID(t, "11111111-2222-3333-4444-555555555555"),
				AppendedBy: "operator:dave",
			},
			metric_ingestor.ErrRetractEmptyReason,
		},
		{
			"empty-AppendedBy",
			metric_ingestor.RetractRequest{
				SampleID: mustUUID(t, "11111111-2222-3333-4444-555555555555"),
				Reason:   "x",
			},
			metric_ingestor.ErrRetractEmptyAppendedBy,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := d.Dispatch(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Dispatch: err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestRetractDispatcher_PanicsOnNilDeps locks the wiring-bug
// surface: each of the three dependencies is non-optional,
// so a nil composition-root argument MUST surface at
// construction rather than at first dispatch.
func TestRetractDispatcher_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	store := metric_ingestor.NewInMemoryRetractStore()

	expectPanic(t, "nil RetractScanRunStore", func() {
		metric_ingestor.NewRetractDispatcher(nil, store, store)
	})
	expectPanic(t, "nil RetractionStore", func() {
		metric_ingestor.NewRetractDispatcher(store, nil, store)
	})
	expectPanic(t, "nil SampleResolver", func() {
		metric_ingestor.NewRetractDispatcher(store, store, nil)
	})
}

func expectPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: expected panic, got nil", name)
		}
	}()
	f()
}

// TestRetractDispatcher_ScanRunRecordsRetractKind pins the
// canonical `scan_run.kind='retract'` literal so any future
// refactor that renames the kind (or accidentally writes
// 'full' / 'delta' on the retract path) trips a test.
func TestRetractDispatcher_ScanRunRecordsRetractKind(t *testing.T) {
	t.Parallel()

	sampleID := mustUUID(t, "12121212-3434-5656-7878-909090909090")
	repoID := mustUUID(t, "bcbcbcbc-dada-fafa-1212-343434343434")
	store := newSeededStore(t, sampleID, repoID, "f00dbabef00dbabef00dbabef00dbabef00dbabe")

	d := metric_ingestor.NewRetractDispatcher(store, store, store)
	res, err := d.Dispatch(context.Background(), metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     "defective recipe emission",
		AppendedBy: "operator:erin",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	rec, ok := store.ScanRunRecord(res.ScanRunID)
	if !ok {
		t.Fatalf("scan_run not recorded")
	}
	if rec.Kind != "retract" {
		t.Errorf("scan_run.kind=%q, want literal %q", rec.Kind, "retract")
	}
	if !strings.EqualFold(string(rec.Status), "succeeded") {
		t.Errorf("scan_run.status=%q, want literal %q", rec.Status, "succeeded")
	}
}

// raceLoserStore wraps an [InMemoryRetractStore] so a test
// can force the rare race-loser path where the
// dispatcher's [RetractionStore.Lookup] returns "no row"
// but a concurrent writer lands a retraction before this
// dispatcher's [RetractionStore.Append] fires. The first
// call to Lookup returns (zero, false, nil) -- mimicking
// the sequential view; on the SECOND call (which Append
// internally does to learn about the conflict) it returns
// the row that the racing writer landed.
//
// The wrapper drives the dispatcher down the post-Open
// race-loser branch so the test can pin the iter 2 contract:
// on a race-loser path the dispatcher returns the
// freshly-minted scan_run_id (honest audit trail) and
// Inserted=false (caller can dedupe). This is the rare
// path; the sequential-idempotency test above covers the
// common path.
type raceLoserStore struct {
	inner       *metric_ingestor.InMemoryRetractStore
	lookupCalls int
	racingRow   metric_ingestor.RetractionRow
}

func (s *raceLoserStore) SampleExists(ctx context.Context, sampleID uuid.UUID) (bool, error) {
	return s.inner.SampleExists(ctx, sampleID)
}

func (s *raceLoserStore) Lookup(ctx context.Context, sampleID uuid.UUID) (metric_ingestor.RetractionRow, bool, error) {
	s.lookupCalls++
	if s.lookupCalls == 1 {
		// First Lookup -- pretend there is no
		// retraction yet. The dispatcher proceeds to
		// open a scan_run.
		return metric_ingestor.RetractionRow{}, false, nil
	}
	// Subsequent Lookup -- not used by Dispatch in the
	// fix path, but kept for completeness.
	return s.inner.Lookup(ctx, sampleID)
}

func (s *raceLoserStore) Append(ctx context.Context, row metric_ingestor.RetractionRow) (metric_ingestor.RetractionRow, bool, error) {
	// Simulate the concurrent winner landing a row
	// before our Append. Pre-seed the inner store so the
	// inner Append returns (existing, inserted=false, nil).
	if _, ok, _ := s.inner.Lookup(ctx, row.SampleID); !ok {
		if _, _, err := s.inner.Append(ctx, s.racingRow); err != nil {
			return metric_ingestor.RetractionRow{}, false, err
		}
	}
	return s.inner.Append(ctx, row)
}

func (s *raceLoserStore) ResolveSample(ctx context.Context, sampleID uuid.UUID) (uuid.UUID, string, bool, error) {
	return s.inner.ResolveSample(ctx, sampleID)
}

func (s *raceLoserStore) OpenRetractScanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error) {
	return s.inner.OpenRetractScanRun(ctx, repoID, sha, openedAt)
}

func (s *raceLoserStore) FinalizeRetractScanRun(ctx context.Context, scanRunID uuid.UUID, status metric_ingestor.ScanRunStatus, endedAt time.Time) error {
	return s.inner.FinalizeRetractScanRun(ctx, scanRunID, status, endedAt)
}

// TestRetractDispatcher_RaceLoserReturnsActualScanRunID
// pins the iter 2 fix #1 honesty contract for the rare
// race-loser path. When the dispatcher's [Lookup] sees no
// row but [Append] discovers a concurrent winner landed
// the retraction (UNIQUE collision on sample_id), the
// dispatcher MUST:
//
//  1. Finalise the scan_run it already opened as
//     `succeeded` (the row is real and durable -- hiding
//     it would create an orphan).
//  2. Return [RetractResult.ScanRunID]=that scan_run's id
//     (NOT uuid.Nil -- the wire response is dishonest
//     otherwise, and downstream correlation tooling
//     cannot find the orphan run).
//  3. Return [RetractResult.Inserted]=false so the caller
//     can dedupe.
func TestRetractDispatcher_RaceLoserReturnsActualScanRunID(t *testing.T) {
	t.Parallel()

	sampleID := mustUUID(t, "abcdabcd-1234-5678-9abc-def012345678")
	repoID := mustUUID(t, "12345678-abcd-1234-abcd-123456789abc")
	sha := "1234567890abcdef1234567890abcdef12345678"

	inner := newSeededStore(t, sampleID, repoID, sha)

	racingID := mustUUID(t, "99999999-1111-2222-3333-444444444444")
	wrapper := &raceLoserStore{
		inner: inner,
		racingRow: metric_ingestor.RetractionRow{
			RetractionID: racingID,
			SampleID:     sampleID,
			Reason:       "raced by concurrent writer",
			AppendedBy:   "operator:alice",
			CreatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	d := metric_ingestor.NewRetractDispatcher(wrapper, wrapper, wrapper)

	res, err := d.Dispatch(context.Background(), metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     "this writer lost the race",
		AppendedBy: "operator:bob",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if res.Inserted {
		t.Errorf("Inserted=true, want false (race-loser path)")
	}
	if res.ScanRunID == uuid.Nil {
		t.Errorf("ScanRunID=uuid.Nil, want non-zero (honest audit trail: the scan_run row IS opened on the race-loser path)")
	}
	if res.Retraction.RetractionID != racingID {
		t.Errorf("RetractionID=%s, want %s (the racing writer's row)", res.Retraction.RetractionID, racingID)
	}
	// The scan_run was opened AND finalised; the inner
	// store has exactly one scan_run row.
	if got := inner.CountScanRuns(); got != 1 {
		t.Errorf("CountScanRuns=%d, want 1 (race-loser opens exactly one scan_run)", got)
	}
	rec, ok := inner.ScanRunRecord(res.ScanRunID)
	if !ok {
		t.Fatalf("scan_run %s not in store (race-loser must NOT hide the row)", res.ScanRunID)
	}
	if !strings.EqualFold(string(rec.Status), "succeeded") {
		t.Errorf("race-loser scan_run.status=%q, want %q", rec.Status, "succeeded")
	}
}
