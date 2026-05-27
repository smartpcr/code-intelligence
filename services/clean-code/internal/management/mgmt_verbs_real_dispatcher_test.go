package management

// Stage 3.4 iter 2 -- the "real-dispatcher" integration
// test that proves the management HTTP layer composes
// cleanly with the genuine
// [metric_ingestor.RetractDispatcher] (and its
// [metric_ingestor.InMemoryRetractStore]) AS WIRED IN
// PRODUCTION via [AdaptMetricIngestorRetractDispatcher].
//
// # Why this exists
//
// Iter 1's mgmt_verbs_test.go uses a hand-rolled
// `fakeRetractDispatcher` whose duplicate-path branch
// always returned a zero scan_run_id (it was the canonical
// expected value). The evaluator (iter 1, item #5)
// flagged this as a TEST that masks a behavioral bug in
// the real dispatcher: the dispatcher used to open a
// fresh scan_run BEFORE consulting the retraction store,
// so a second retract for the SAME sample_id surfaced a
// NON-zero scan_run_id even though no new retraction was
// appended -- contradicting the brief's idempotency
// claim.
//
// Iter 2 fixed the dispatcher (retract.go: now performs a
// Lookup-first probe). This test pins that fix THROUGH
// the HTTP wire: two POSTs against /v1/mgmt/retract_sample
// with the same `sample_id`. The second response MUST
// carry `scan_run_id == uuid.Nil` AND `inserted == false`,
// AND the InMemoryRetractStore must record only ONE
// scan_run row.
//
// These tests use the SAME concrete types main.go wires
// (NewRetractDispatcher + InMemoryRetractStore), so a
// regression in either layer fails here.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// realDispatcherFixture stitches together the production
// wiring: the in-memory store doubles as
// RetractScanRunStore + RetractionStore + SampleResolver
// (it already exposes all three contracts). The
// dispatcher itself is the real
// metric_ingestor.RetractDispatcher, adapted into the
// management-side interface via
// AdaptMetricIngestorRetractDispatcher.
type realDispatcherFixture struct {
	store    *metric_ingestor.InMemoryRetractStore
	writer   *MgmtWriter
	appender *InMemoryRepoEventAppender
	sampleID uuid.UUID
	repoID   uuid.UUID
	sha      string
}

func newRealDispatcherFixture(t *testing.T) *realDispatcherFixture {
	t.Helper()
	store := metric_ingestor.NewInMemoryRetractStore()
	sampleID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	sha := "deadbeef00000000000000000000000000000000"
	store.SeedSample(sampleID, repoID, sha)

	dispatcher := metric_ingestor.NewRetractDispatcher(store, store, store)
	appender := NewInMemoryRepoEventAppender()
	writer := NewMgmtWriter(
		store, // SampleResolver -- same store
		AdaptMetricIngestorRetractDispatcher(dispatcher),
		nil, // rescan enqueuer not exercised here
		appender,
	)
	return &realDispatcherFixture{
		store:    store,
		writer:   writer,
		appender: appender,
		sampleID: sampleID,
		repoID:   repoID,
		sha:      sha,
	}
}

// retractRequestBody returns the canonical wire body for
// a `POST /v1/mgmt/retract_sample` call.
func retractRequestBody(sampleID uuid.UUID, reason string) []byte {
	body, _ := json.Marshal(map[string]string{
		"sample_id": sampleID.String(),
		"reason":    reason,
	})
	return body
}

// doRetract POSTs against the management writer with the
// X-OIDC-Subject header set, returning the response.
func doRetract(t *testing.T, w *MgmtWriter, sampleID uuid.UUID, reason, actor string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractRequestBody(sampleID, reason)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OIDC-Subject", actor)
	req = req.WithContext(context.Background())
	rr := httptest.NewRecorder()
	w.RetractSample(rr, req)
	return rr
}

// decodeRetractResponse decodes the wire body of a
// successful retract response.
func decodeRetractResponse(t *testing.T, body []byte) struct {
	RetractionID string `json:"retraction_id"`
	SampleID     string `json:"sample_id"`
	ScanRunID    string `json:"scan_run_id"`
	Inserted     bool   `json:"inserted"`
} {
	t.Helper()
	var resp struct {
		RetractionID string `json:"retraction_id"`
		SampleID     string `json:"sample_id"`
		ScanRunID    string `json:"scan_run_id"`
		Inserted     bool   `json:"inserted"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	return resp
}

// TestMgmtWriter_RetractSample_WithRealDispatcher_FirstRetractInserts
// pins the production happy path: the FIRST retract for
// a sample writes a metric_retraction row, opens a single
// scan_run(kind='retract'), and surfaces both on the wire
// with `inserted=true`.
func TestMgmtWriter_RetractSample_WithRealDispatcher_FirstRetractInserts(t *testing.T) {
	t.Parallel()
	f := newRealDispatcherFixture(t)
	rr := doRetract(t, f.writer, f.sampleID, "vendored file", "alice@contoso.com")
	if rr.Code != http.StatusOK {
		t.Fatalf("first retract: status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeRetractResponse(t, rr.Body.Bytes())
	if !resp.Inserted {
		t.Errorf("inserted: got false; want true on first retract")
	}
	if resp.ScanRunID == uuid.Nil.String() {
		t.Errorf("scan_run_id: got zero UUID; want non-zero on first retract")
	}
	if got, want := f.store.CountScanRuns(), 1; got != want {
		t.Errorf("CountScanRuns: got %d; want %d", got, want)
	}
	if got, want := f.store.CountRetractions(), 1; got != want {
		t.Errorf("CountRetractions: got %d; want %d", got, want)
	}
}

// TestMgmtWriter_RetractSample_WithRealDispatcher_SecondRetractIsNoOp
// pins the iter 2 idempotency fix: the SECOND retract
// for the SAME sample_id MUST surface
// `scan_run_id == uuid.Nil` AND `inserted == false`, AND
// the underlying store MUST NOT record a second
// scan_run row (the dispatcher's Lookup-first guard
// short-circuits before opening one).
//
// This is the failure mode the iter 1 fake masked: the
// fake's `else` branch returned a zero scan_run_id
// regardless of dispatcher behaviour. Driving the REAL
// dispatcher proves the bug is fixed at the layer that
// actually ships.
func TestMgmtWriter_RetractSample_WithRealDispatcher_SecondRetractIsNoOp(t *testing.T) {
	t.Parallel()
	f := newRealDispatcherFixture(t)

	// First retract -- captures the canonical state.
	first := doRetract(t, f.writer, f.sampleID, "vendored file", "alice@contoso.com")
	if first.Code != http.StatusOK {
		t.Fatalf("first retract: status=%d; body=%s", first.Code, first.Body.String())
	}
	firstResp := decodeRetractResponse(t, first.Body.Bytes())
	if firstResp.ScanRunID == uuid.Nil.String() {
		t.Fatalf("first retract scan_run_id: got zero; want non-zero (precondition)")
	}

	// Second retract -- the idempotent path.
	second := doRetract(t, f.writer, f.sampleID, "vendored file", "alice@contoso.com")
	if second.Code != http.StatusOK {
		t.Fatalf("second retract: status=%d; body=%s", second.Code, second.Body.String())
	}
	secondResp := decodeRetractResponse(t, second.Body.Bytes())

	if secondResp.Inserted {
		t.Errorf("second retract inserted: got true; want false")
	}
	if secondResp.ScanRunID != uuid.Nil.String() {
		t.Errorf("second retract scan_run_id: got %s; want %s (uuid.Nil) -- the dispatcher MUST NOT open a fresh scan_run on the idempotent path",
			secondResp.ScanRunID, uuid.Nil.String())
	}
	if secondResp.RetractionID != firstResp.RetractionID {
		t.Errorf("second retract retraction_id: got %s; want %s (must surface the EXISTING row)",
			secondResp.RetractionID, firstResp.RetractionID)
	}

	// Underlying store state: exactly one scan_run, one
	// retraction. If the iter 1 bug regressed, the
	// second retract would have opened a fresh scan_run
	// (CountScanRuns would be 2).
	if got, want := f.store.CountScanRuns(), 1; got != want {
		t.Errorf("CountScanRuns after second retract: got %d; want %d (the dispatcher opened a duplicate scan_run -- iter 1 bug has regressed)", got, want)
	}
	if got, want := f.store.CountRetractions(), 1; got != want {
		t.Errorf("CountRetractions after second retract: got %d; want %d", got, want)
	}

	// Both calls must emit ONE repo_event(kind='retract_intent')
	// each, mirroring the architecture's audit-trail
	// invariant: the intent is always recorded; the
	// dispatcher's idempotency is INTERNAL to the
	// metric_ingestor layer.
	if got, want := f.appender.Count(), 2; got != want {
		t.Errorf("repo_event count: got %d; want %d (intent rows MUST be written on every call, idempotency is downstream)", got, want)
	}
}

// TestMgmtWriter_RetractSample_WithRealDispatcher_DifferentSamplesGetDistinctScanRuns
// pins that the iter 2 idempotency fix is keyed on
// sample_id, not a global flag: retracting a SECOND
// distinct sample still opens its own scan_run.
func TestMgmtWriter_RetractSample_WithRealDispatcher_DifferentSamplesGetDistinctScanRuns(t *testing.T) {
	t.Parallel()
	f := newRealDispatcherFixture(t)
	sampleB := uuid.Must(uuid.NewV4())
	f.store.SeedSample(sampleB, f.repoID, f.sha)

	rrA := doRetract(t, f.writer, f.sampleID, "reason A", "alice@contoso.com")
	rrB := doRetract(t, f.writer, sampleB, "reason B", "alice@contoso.com")
	if rrA.Code != http.StatusOK || rrB.Code != http.StatusOK {
		t.Fatalf("statuses: A=%d B=%d", rrA.Code, rrB.Code)
	}
	respA := decodeRetractResponse(t, rrA.Body.Bytes())
	respB := decodeRetractResponse(t, rrB.Body.Bytes())
	if respA.ScanRunID == respB.ScanRunID {
		t.Errorf("scan_run_ids: A=%s == B=%s; want distinct", respA.ScanRunID, respB.ScanRunID)
	}
	if !respA.Inserted || !respB.Inserted {
		t.Errorf("inserted flags: A=%v B=%v; both should be true (fresh samples)", respA.Inserted, respB.Inserted)
	}
	if got, want := f.store.CountScanRuns(), 2; got != want {
		t.Errorf("CountScanRuns: got %d; want %d", got, want)
	}
}
