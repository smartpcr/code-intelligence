package metric_ingestor_test

// Stage 3.4 -- RescanEnqueuer unit tests.
//
// Pins the per-call shape required by the e2e scenario
// "rescan-enqueues-scan-run":
//
//   "Given a `mgmt.rescan(repo_id, sha)` call, When the verb
//    completes, Then a service-internal rescan request is
//    logged and a `scan_run(kind='full', status='running')`
//    is observable (no `rescan_intent` RepoEvent kind is
//    emitted -- the canonical RepoEvent enum at architecture
//    Sec 5.1.4 has no `rescan_intent` value)."
//
// Tests cover:
//
//   * Validate() rejects every required-field failure mode.
//   * Enqueue() opens exactly one scan_run with the
//     canonical (kind='full', sha_binding='single',
//     status='running', to_sha=<sha>) shape.
//   * Repeat Enqueue() calls open a FRESH scan_run each
//     time (the rescan verb is intentionally NOT idempotent
//     -- an operator who clicks "rescan" twice expects two
//     scan_runs because they want the recipe loop to run
//     twice).
//   * NewRescanEnqueuer panics on nil store.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

func TestRescanRequest_Validate(t *testing.T) {
	t.Parallel()

	good := metric_ingestor.RescanRequest{
		RepoID:      mustUUID(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		SHA:         "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		RequestedBy: "operator:alice@example.com",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good Validate(): err=%v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*metric_ingestor.RescanRequest)
		want error
	}{
		{"zero-RepoID", func(r *metric_ingestor.RescanRequest) { r.RepoID = uuid.Nil }, metric_ingestor.ErrRescanZeroRepoID},
		{"empty-SHA", func(r *metric_ingestor.RescanRequest) { r.SHA = "" }, metric_ingestor.ErrRescanEmptySHA},
		{"whitespace-SHA", func(r *metric_ingestor.RescanRequest) { r.SHA = "   " }, metric_ingestor.ErrRescanEmptySHA},
		{"empty-RequestedBy", func(r *metric_ingestor.RescanRequest) { r.RequestedBy = "" }, metric_ingestor.ErrRescanEmptyRequestedBy},
		{"whitespace-RequestedBy", func(r *metric_ingestor.RescanRequest) { r.RequestedBy = "   " }, metric_ingestor.ErrRescanEmptyRequestedBy},
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

// TestRescanEnqueuer_OpensScanRunWithCanonicalShape pins the
// happy path. The verb opens a scan_run row with the
// canonical (kind='full', sha_binding='single',
// status='running', to_sha=<sha>) shape.
func TestRescanEnqueuer_OpensScanRunWithCanonicalShape(t *testing.T) {
	t.Parallel()

	repoID := mustUUID(t, "11112222-3333-4444-5555-666677778888")
	sha := "0123456789abcdef0123456789abcdef01234567"
	clockT := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)

	store := metric_ingestor.NewInMemoryRescanStore()
	e := metric_ingestor.NewRescanEnqueuer(store,
		metric_ingestor.WithRescanEnqueuerClock(fixedClock(clockT)))

	res, err := e.Enqueue(context.Background(), metric_ingestor.RescanRequest{
		RepoID:      repoID,
		SHA:         sha,
		RequestedBy: "operator:alice@example.com",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res.ScanRunID == uuid.Nil {
		t.Error("ScanRunID is the zero UUID")
	}
	if res.RepoID != repoID {
		t.Errorf("RepoID=%s, want %s", res.RepoID, repoID)
	}
	if res.SHA != sha {
		t.Errorf("SHA=%q, want %q", res.SHA, sha)
	}
	if res.RequestedBy != "operator:alice@example.com" {
		t.Errorf("RequestedBy=%q, want %q", res.RequestedBy, "operator:alice@example.com")
	}
	if !res.OpenedAt.Equal(clockT) {
		t.Errorf("OpenedAt=%v, want %v", res.OpenedAt, clockT)
	}

	rec, ok := store.ScanRunRecord(res.ScanRunID)
	if !ok {
		t.Fatalf("scan_run id=%s not recorded in store", res.ScanRunID)
	}
	if rec.Kind != metric_ingestor.ScanRunKindFull {
		t.Errorf("scan_run.kind=%q, want %q", rec.Kind, metric_ingestor.ScanRunKindFull)
	}
	if rec.SHABinding != metric_ingestor.SHABindingSingle {
		t.Errorf("scan_run.sha_binding=%q, want %q", rec.SHABinding, metric_ingestor.SHABindingSingle)
	}
	if rec.Status != metric_ingestor.ScanRunStatusRunning {
		t.Errorf("scan_run.status=%q, want %q", rec.Status, metric_ingestor.ScanRunStatusRunning)
	}
	if rec.ToSHA != sha {
		t.Errorf("scan_run.to_sha=%q, want %q", rec.ToSHA, sha)
	}
	if rec.RepoID != repoID {
		t.Errorf("scan_run.repo_id=%s, want %s", rec.RepoID, repoID)
	}
	if !rec.StartedAt.Equal(clockT) {
		t.Errorf("scan_run.started_at=%v, want %v", rec.StartedAt, clockT)
	}
	if store.CountRuns() != 1 {
		t.Errorf("CountRuns=%d, want 1", store.CountRuns())
	}
}

// TestRescanEnqueuer_StringLiteralsArePinned guards against
// a future refactor that renames the canonical scan_run
// kind / status / sha_binding constants. The e2e contract
// pins exact wire literals -- a constant rename that
// silently drifts these labels would invalidate every
// downstream consumer (PG enum, evaluator agent, audit
// reader).
func TestRescanEnqueuer_StringLiteralsArePinned(t *testing.T) {
	t.Parallel()

	repoID := mustUUID(t, "22221111-3333-4444-5555-666677778888")
	store := metric_ingestor.NewInMemoryRescanStore()
	e := metric_ingestor.NewRescanEnqueuer(store)
	res, err := e.Enqueue(context.Background(), metric_ingestor.RescanRequest{
		RepoID:      repoID,
		SHA:         "abcdef0123456789abcdef0123456789abcdef01",
		RequestedBy: "operator:bob",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rec, _ := store.ScanRunRecord(res.ScanRunID)

	if rec.Kind != "full" {
		t.Errorf("scan_run.kind=%q, want literal %q", rec.Kind, "full")
	}
	if rec.SHABinding != "single" {
		t.Errorf("scan_run.sha_binding=%q, want literal %q", rec.SHABinding, "single")
	}
	if !strings.EqualFold(string(rec.Status), "running") {
		t.Errorf("scan_run.status=%q, want literal %q", rec.Status, "running")
	}
}

// TestRescanEnqueuer_RepeatedCallsOpenFreshRuns pins the
// intentional NON-idempotency of the rescan verb. An
// operator who calls rescan twice has explicitly asked for
// two scan_runs (e.g. the first run was cancelled
// externally and the operator wants a second pass). The
// dispatcher MUST open a fresh scan_run per call.
//
// (Compare with retract, which IS idempotent on
// `metric_retraction.sample_id` because the UNIQUE
// constraint forbids two retractions of the same sample.
// The scan_run table has no analogous UNIQUE.)
func TestRescanEnqueuer_RepeatedCallsOpenFreshRuns(t *testing.T) {
	t.Parallel()

	repoID := mustUUID(t, "33334444-5555-6666-7777-888899990000")
	sha := "f00dbabef00dbabef00dbabef00dbabef00dbabe"
	store := metric_ingestor.NewInMemoryRescanStore()
	e := metric_ingestor.NewRescanEnqueuer(store)

	first, err := e.Enqueue(context.Background(), metric_ingestor.RescanRequest{
		RepoID:      repoID,
		SHA:         sha,
		RequestedBy: "operator:alice",
	})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	second, err := e.Enqueue(context.Background(), metric_ingestor.RescanRequest{
		RepoID:      repoID,
		SHA:         sha,
		RequestedBy: "operator:alice",
	})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if first.ScanRunID == second.ScanRunID {
		t.Errorf("first.ScanRunID==second.ScanRunID=%s; want two distinct runs", first.ScanRunID)
	}
	if store.CountRuns() != 2 {
		t.Errorf("CountRuns=%d, want 2 (rescan is NOT idempotent at the scan_run layer)", store.CountRuns())
	}
}

// TestRescanEnqueuer_RejectsInvalidRequest pins the
// per-field validation surface.
func TestRescanEnqueuer_RejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	store := metric_ingestor.NewInMemoryRescanStore()
	e := metric_ingestor.NewRescanEnqueuer(store)
	goodSHA := "abcdef0123456789abcdef0123456789abcdef01"
	goodRepo := mustUUID(t, "44445555-6666-7777-8888-999900001111")

	for _, tc := range []struct {
		name string
		req  metric_ingestor.RescanRequest
		want error
	}{
		{
			"zero-RepoID",
			metric_ingestor.RescanRequest{SHA: goodSHA, RequestedBy: "operator:y"},
			metric_ingestor.ErrRescanZeroRepoID,
		},
		{
			"empty-SHA",
			metric_ingestor.RescanRequest{RepoID: goodRepo, RequestedBy: "operator:y"},
			metric_ingestor.ErrRescanEmptySHA,
		},
		{
			"empty-RequestedBy",
			metric_ingestor.RescanRequest{RepoID: goodRepo, SHA: goodSHA},
			metric_ingestor.ErrRescanEmptyRequestedBy,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.Enqueue(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Enqueue: err=%v, want %v", err, tc.want)
			}
		})
	}
	// No scan_run was opened on any failure.
	if store.CountRuns() != 0 {
		t.Errorf("CountRuns=%d, want 0 (no scan_run opened on validation failure)", store.CountRuns())
	}
}

// TestRescanEnqueuer_PanicsOnNilStore locks the wiring-bug
// surface: a nil composition-root argument MUST surface at
// construction rather than at first Enqueue.
func TestRescanEnqueuer_PanicsOnNilStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewRescanEnqueuer(nil): expected panic, got nil")
		}
	}()
	_ = metric_ingestor.NewRescanEnqueuer(nil)
}

// TestRescanEnqueuer_SHATrimmedBeforePersist verifies the
// enqueuer strips leading/trailing whitespace from the SHA
// before stamping it into the scan_run row. A caller that
// sends `"  abcdef...  "` should land an `abcdef...` row
// (the CHECK constraint and downstream string-match logic
// expects the trimmed form).
func TestRescanEnqueuer_SHATrimmedBeforePersist(t *testing.T) {
	t.Parallel()

	repoID := mustUUID(t, "55556666-7777-8888-9999-000011112222")
	store := metric_ingestor.NewInMemoryRescanStore()
	e := metric_ingestor.NewRescanEnqueuer(store)

	rawSHA := "  cafebabecafebabecafebabecafebabecafebabe  "
	want := strings.TrimSpace(rawSHA)
	res, err := e.Enqueue(context.Background(), metric_ingestor.RescanRequest{
		RepoID:      repoID,
		SHA:         rawSHA,
		RequestedBy: "operator:carol",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res.SHA != want {
		t.Errorf("RescanResult.SHA=%q, want trimmed %q", res.SHA, want)
	}
	rec, _ := store.ScanRunRecord(res.ScanRunID)
	if rec.ToSHA != want {
		t.Errorf("scan_run.to_sha=%q, want trimmed %q", rec.ToSHA, want)
	}
}

// TestRescanEnqueuer_ContextCanceledShortCircuits ensures a
// cancelled caller context aborts the open BEFORE the store
// is touched. Important so a flapping caller doesn't leak
// scan_run rows.
func TestRescanEnqueuer_ContextCanceledShortCircuits(t *testing.T) {
	t.Parallel()

	store := metric_ingestor.NewInMemoryRescanStore()
	e := metric_ingestor.NewRescanEnqueuer(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := e.Enqueue(ctx, metric_ingestor.RescanRequest{
		RepoID:      mustUUID(t, "66667777-8888-9999-aaaa-bbbbccccdddd"),
		SHA:         "1234567890123456789012345678901234567890",
		RequestedBy: "operator:dave",
	})
	if err == nil {
		t.Fatal("Enqueue: nil error on cancelled context, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Enqueue: err=%v, want errors.Is(err, context.Canceled)", err)
	}
	if store.CountRuns() != 0 {
		t.Errorf("CountRuns=%d, want 0 (cancelled context must not open scan_run)", store.CountRuns())
	}
}
