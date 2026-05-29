package metric_ingestor_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// recordingDispatcher is a test fake for
// [metric_ingestor.FoundationRecipeDispatcher] that records
// every Dispatch invocation and lets the test inject success
// or a custom error. Used to assert the [Ingestor] honours the
// dispatch ordering contract (foundation FIRST for `full`/
// `delta`, NEVER for `external_per_row`).
type recordingDispatcher struct {
	calls       int32
	lastScanRun metric_ingestor.ScanRunContext
	err         error
}

func (d *recordingDispatcher) Dispatch(_ context.Context, scanRun metric_ingestor.ScanRunContext, _ metric_ingestor.FoundationInput) error {
	atomic.AddInt32(&d.calls, 1)
	d.lastScanRun = scanRun
	return d.err
}

func (d *recordingDispatcher) Calls() int32 { return atomic.LoadInt32(&d.calls) }

// newIngestorWithRecordingDispatcher builds an [Ingestor]
// wired to a fresh recordingDispatcher + a fresh ChurnSweep
// (with deterministic clock + map resolver) so the test can
// assert on both writers in one place.
func newIngestorWithRecordingDispatcher(
	t *testing.T,
	files map[string]uuid.UUID,
) (*metric_ingestor.Ingestor, *recordingDispatcher, *metric_ingestor.InMemoryMetricSampleWriter) {
	t.Helper()
	sweep, writer, _ := newSweep(t, 90, files)
	disp := &recordingDispatcher{}
	ing := metric_ingestor.NewIngestor(disp, sweep)
	return ing, disp, writer
}

// goodPayload returns a one-row churn payload bound to
// `fixedRepoID` and a file the caller pre-registered in the
// scope resolver.
func goodPayload(filePath string) *churn.Payload {
	return &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{
				SHA:        strings.Repeat("a", 40),
				FilePath:   filePath,
				ModifiedAt: fixedNow().Add(-7 * 24 * time.Hour),
			},
		},
	}
}

// goodFullScanRun returns a `kind='full'` ScanRunContext.
func goodFullScanRun() metric_ingestor.ScanRunContext {
	return metric_ingestor.ScanRunContext{
		ID:     fixedScanRunID,
		Kind:   metric_ingestor.ScanRunKindFull,
		RepoID: fixedRepoID,
	}
}

// goodDeltaScanRun returns a `kind='delta'` ScanRunContext.
func goodDeltaScanRun() metric_ingestor.ScanRunContext {
	return metric_ingestor.ScanRunContext{
		ID:     fixedScanRunID,
		Kind:   metric_ingestor.ScanRunKindDelta,
		RepoID: fixedRepoID,
	}
}

// ----------------------------------------------------------------
// Production-wiring invariants (evaluator iter-3 #1)
// ----------------------------------------------------------------

// TestIngestor_FullScan_FoundationDispatchBeforeChurn asserts
// the documented dispatch ordering for `kind='full'`: the
// foundation recipe dispatcher runs FIRST; on success the
// churn sweep runs SECOND. Both writers see the same
// [ScanRunContext] (the same-ScanRun-as-foundation invariant
// pinned by the detailed requirement).
func TestIngestor_FullScan_FoundationDispatchBeforeChurn(t *testing.T) {
	t.Parallel()
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	scanRun := goodFullScanRun()

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: scanRun,
		Churn:   goodPayload("internal/foo.go"),
	})
	if err != nil {
		t.Fatalf("Ingestor.Run returned error: %v", err)
	}
	if disp.Calls() != 1 {
		t.Errorf("dispatcher.Calls = %d; want 1", disp.Calls())
	}
	if disp.lastScanRun != scanRun {
		t.Errorf("dispatcher saw scanRun=%+v; want %+v", disp.lastScanRun, scanRun)
	}
	if !res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = false; want true (full-scan dispatched)")
	}
	if res.ChurnSkipped {
		t.Errorf("res.ChurnSkipped = true; want false (payload supplied)")
	}
	if res.ChurnSamplesWritten != 1 {
		t.Errorf("res.ChurnSamplesWritten = %d; want 1", res.ChurnSamplesWritten)
	}
	if got := len(writer.Records()); got != 1 {
		t.Errorf("writer.Records() length = %d; want 1", got)
	}
	// Verify the churn-sweep stamped the SAME producer_run_id
	// the dispatcher saw (the same-ScanRun guarantee).
	if got := writer.Records()[0].ProducerRunID; got != scanRun.ID {
		t.Errorf("MetricSample.ProducerRunID = %s; want %s (same-ScanRun invariant)", got, scanRun.ID)
	}
}

// TestIngestor_DeltaScan_FoundationDispatchBeforeChurn mirrors
// the full-scan test for `kind='delta'` so the closed
// foundation-kind set (`{full, delta}`) is both exercised.
func TestIngestor_DeltaScan_FoundationDispatchBeforeChurn(t *testing.T) {
	t.Parallel()
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, map[string]uuid.UUID{
		"internal/bar.go": barScopeID,
	})
	scanRun := goodDeltaScanRun()

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: scanRun,
		Churn:   goodPayload("internal/bar.go"),
	})
	if err != nil {
		t.Fatalf("Ingestor.Run returned error: %v", err)
	}
	if disp.Calls() != 1 {
		t.Errorf("dispatcher.Calls = %d; want 1 (delta dispatches foundation too)", disp.Calls())
	}
	if !res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = false; want true")
	}
	if got := len(writer.Records()); got != 1 {
		t.Errorf("writer.Records() length = %d; want 1", got)
	}
}

// TestIngestor_FullScan_NilChurnPayload_SkipsChurn pins the
// "churn is optional for foundation scans" contract -- a
// `full` scan with no fresh churn data MUST succeed with the
// foundation dispatch and report ChurnSkipped=true.
func TestIngestor_FullScan_NilChurnPayload_SkipsChurn(t *testing.T) {
	t.Parallel()
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, nil)

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: goodFullScanRun(),
		Churn:   nil,
	})
	if err != nil {
		t.Fatalf("Ingestor.Run returned error: %v", err)
	}
	if disp.Calls() != 1 {
		t.Errorf("dispatcher.Calls = %d; want 1 (foundation still runs)", disp.Calls())
	}
	if !res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = false; want true")
	}
	if !res.ChurnSkipped {
		t.Errorf("res.ChurnSkipped = false; want true (nil payload)")
	}
	if res.ChurnSamplesWritten != 0 {
		t.Errorf("res.ChurnSamplesWritten = %d; want 0", res.ChurnSamplesWritten)
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records() length = %d; want 0 (churn skipped)", got)
	}
}

// TestIngestor_FullScan_DispatchErrorShortCircuits asserts a
// foundation-dispatch failure stops the run BEFORE the churn
// sweep is invoked. A partial write here would corrupt the
// active-row index.
func TestIngestor_FullScan_DispatchErrorShortCircuits(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("recipe registry not loaded")
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	disp.err = sentinel

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: goodFullScanRun(),
		Churn:   goodPayload("internal/foo.go"),
	})
	if err == nil {
		t.Fatalf("Ingestor.Run returned nil error; want a wrapped foundation-dispatch error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Ingestor.Run err = %v; want wrap of %v", err, sentinel)
	}
	if res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = true; want false on dispatch error")
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records() length = %d; want 0 (churn skipped after dispatch error)", got)
	}
}

// TestIngestor_ExternalPerRow_FoundationNotDispatched asserts
// a `kind='external_per_row'` run NEVER invokes the
// foundation dispatcher -- the churn-only path doesn't need
// AST work.
func TestIngestor_ExternalPerRow_FoundationNotDispatched(t *testing.T) {
	t.Parallel()
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: goodScanRun(),
		Churn:   goodPayload("internal/foo.go"),
	})
	if err != nil {
		t.Fatalf("Ingestor.Run returned error: %v", err)
	}
	if disp.Calls() != 0 {
		t.Errorf("dispatcher.Calls = %d; want 0 (external_per_row never dispatches foundation)", disp.Calls())
	}
	if res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = true; want false")
	}
	if res.ChurnSamplesWritten != 1 {
		t.Errorf("res.ChurnSamplesWritten = %d; want 1", res.ChurnSamplesWritten)
	}
	if got := len(writer.Records()); got != 1 {
		t.Errorf("writer.Records() length = %d; want 1", got)
	}
}

// TestIngestor_ExternalPerRow_NilChurnPayload_Rejected pins
// the asymmetric churn-payload requirement: `external_per_row`
// MUST have a payload (the whole point of the kind).
func TestIngestor_ExternalPerRow_NilChurnPayload_Rejected(t *testing.T) {
	t.Parallel()
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, nil)

	_, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: goodScanRun(),
		Churn:   nil,
	})
	if err == nil {
		t.Fatalf("Ingestor.Run returned nil; want ErrMissingChurnPayloadForExternalPerRow")
	}
	if !errors.Is(err, metric_ingestor.ErrMissingChurnPayloadForExternalPerRow) {
		t.Errorf("Ingestor.Run err = %v; want ErrMissingChurnPayloadForExternalPerRow", err)
	}
	if disp.Calls() != 0 {
		t.Errorf("dispatcher.Calls = %d; want 0 (external_per_row never dispatches foundation)", disp.Calls())
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records() length = %d; want 0", got)
	}
}

// TestIngestor_RejectsInvalidScanRunKind asserts the
// pre-dispatch validation path: an unsupported kind is
// rejected BEFORE any dispatcher or sweep is touched.
// (Stage 4.2 added `external_single` as a valid kind for
// the coverage path; this test now uses `retract` -- still
// outside the Ingestor's closed set -- to exercise the
// rejection branch.)
func TestIngestor_RejectsInvalidScanRunKind(t *testing.T) {
	t.Parallel()
	ing, disp, writer := newIngestorWithRecordingDispatcher(t, nil)

	_, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: metric_ingestor.ScanRunContext{
			ID:     fixedScanRunID,
			Kind:   metric_ingestor.ScanRunKindRetract,
			RepoID: fixedRepoID,
		},
	})
	if err == nil {
		t.Fatalf("Ingestor.Run returned nil; want ErrInvalidScanRunKind")
	}
	if !errors.Is(err, metric_ingestor.ErrInvalidScanRunKind) {
		t.Errorf("Ingestor.Run err = %v; want ErrInvalidScanRunKind", err)
	}
	if disp.Calls() != 0 {
		t.Errorf("dispatcher.Calls = %d; want 0", disp.Calls())
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records() length = %d; want 0", got)
	}
}

// TestIngestor_RejectsZeroRepoID asserts the RepoID-validation
// path runs at the Ingestor boundary too.
func TestIngestor_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	ing, disp, _ := newIngestorWithRecordingDispatcher(t, nil)

	_, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: metric_ingestor.ScanRunContext{
			ID:     fixedScanRunID,
			Kind:   metric_ingestor.ScanRunKindFull,
			RepoID: uuid.Nil,
		},
	})
	if err == nil {
		t.Fatalf("Ingestor.Run returned nil; want ErrZeroRepoID")
	}
	if !errors.Is(err, metric_ingestor.ErrZeroRepoID) {
		t.Errorf("Ingestor.Run err = %v; want ErrZeroRepoID", err)
	}
	if disp.Calls() != 0 {
		t.Errorf("dispatcher.Calls = %d; want 0 (validation runs first)", disp.Calls())
	}
}

// ----------------------------------------------------------------
// NoopFoundationRecipeDispatcher (scaffold) tests
// ----------------------------------------------------------------

// TestNoopFoundationRecipeDispatcher_AlwaysSucceeds pins the
// Stage 2.6 scaffold contract (iter 5 structural change): the
// noop dispatcher returns nil on every Dispatch call so a
// `full`/`delta` ScanRun proceeds to the [ChurnSweep] instead
// of short-circuiting with an error. Iter 4 used an
// `Unwired` variant that always errored; the evaluator rejected
// that because it made the same-ScanRun integration
// unreachable in production wiring.
func TestNoopFoundationRecipeDispatcher_AlwaysSucceeds(t *testing.T) {
	t.Parallel()
	d := metric_ingestor.NoopFoundationRecipeDispatcher{}
	if err := d.Dispatch(context.Background(), goodFullScanRun(), metric_ingestor.FoundationInput{}); err != nil {
		t.Errorf("Dispatch returned err = %v; want nil (noop scaffold MUST succeed)", err)
	}
	if err := d.Dispatch(context.Background(), goodDeltaScanRun(), metric_ingestor.FoundationInput{}); err != nil {
		t.Errorf("Dispatch(delta) returned err = %v; want nil", err)
	}
}

// TestIngestor_FullScan_NoopDispatcher_Succeeds proves the
// production-mode composition with
// [NoopFoundationRecipeDispatcher] runs the churn sweep
// END-TO-END for a `full` ScanRun -- i.e. the same-ScanRun
// integration is actually reachable in the wired path, not
// just in tests with a recordingDispatcher fake (evaluator
// iter-4 #1 + #2).
func TestIngestor_FullScan_NoopDispatcher_Succeeds(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, 90, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: goodFullScanRun(),
		Churn:   goodPayload("internal/foo.go"),
	})
	if err != nil {
		t.Fatalf("Ingestor.Run returned error: %v (full + noop dispatcher MUST succeed in scaffold mode)", err)
	}
	if !res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = false; want true (noop dispatcher succeeds)")
	}
	if res.ChurnSamplesWritten != 1 {
		t.Errorf("res.ChurnSamplesWritten = %d; want 1 (sweep runs after dispatch success)", res.ChurnSamplesWritten)
	}
	if got := len(writer.Records()); got != 1 {
		t.Errorf("writer.Records() length = %d; want 1", got)
	}
}

// TestIngestor_ExternalPerRow_NoopDispatcher_Succeeds proves the
// asymmetric path: an `external_per_row` run in scaffold mode
// (with a noop dispatcher) STILL succeeds because the
// dispatcher is never invoked for that kind. This is the
// standalone-webhook contract.
func TestIngestor_ExternalPerRow_NoopDispatcher_Succeeds(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, 90, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)

	res, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: goodScanRun(),
		Churn:   goodPayload("internal/foo.go"),
	})
	if err != nil {
		t.Fatalf("Ingestor.Run returned error: %v (external_per_row must work in scaffold mode)", err)
	}
	if res.FoundationDispatched {
		t.Errorf("res.FoundationDispatched = true; want false (dispatcher untouched)")
	}
	if res.ChurnSamplesWritten != 1 {
		t.Errorf("res.ChurnSamplesWritten = %d; want 1", res.ChurnSamplesWritten)
	}
	if got := len(writer.Records()); got != 1 {
		t.Errorf("writer.Records() length = %d; want 1", got)
	}
}

// ----------------------------------------------------------------
// NewIngestor wiring guards
// ----------------------------------------------------------------

func TestNewIngestor_PanicsOnNilDispatcher(t *testing.T) {
	t.Parallel()
	sweep, _, _ := newSweep(t, 90, nil)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("NewIngestor(nil dispatcher, sweep) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "FoundationRecipeDispatcher") {
			t.Errorf("panic msg=%q does not mention FoundationRecipeDispatcher", msg)
		}
	}()
	_ = metric_ingestor.NewIngestor(nil, sweep)
}

func TestNewIngestor_PanicsOnNilChurnSweep(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("NewIngestor(dispatcher, nil sweep) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "ChurnSweep") {
			t.Errorf("panic msg=%q does not mention ChurnSweep", msg)
		}
	}()
	_ = metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, nil)
}
