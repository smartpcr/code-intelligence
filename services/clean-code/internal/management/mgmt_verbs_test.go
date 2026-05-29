package management

// Stage 3.4 -- HTTP-level tests for `mgmt.retract_sample` and
// `mgmt.rescan`.
//
// The dispatcher / enqueuer themselves are covered by
// `internal/metric_ingestor/retract_test.go` +
// `internal/metric_ingestor/rescan_test.go`. THIS suite pins
// the wire layer:
//
//   * happy paths and the canonical 200 response body shapes,
//   * the `repo_event(kind='retract_intent')` audit row is
//     appended BEFORE the dispatcher runs (and is NOT appended
//     when the sample is missing -- per architecture Sec 6.3
//     "Management only emits repo_event and delegates"),
//   * `mgmt.rescan` MUST NOT emit any repo_event row (no
//     canonical `rescan_intent` kind per architecture
//     Sec 5.1.4),
//   * status-code mapping (400/401/404/405/503),
//   * idempotency at the HTTP boundary (a second
//     `mgmt.retract_sample` returns the existing retraction
//     row with `inserted=false`),
//   * actor attribution sourced from the X-OIDC-Subject header
//     (the body's `sample_id` / `reason` MUST NOT carry an
//     `actor` field -- DisallowUnknownFields rejects it).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

// ---- in-memory dispatcher / enqueuer fakes -------------------------------

// fakeRetractDispatcher is a hand-rolled in-memory
// implementation of the management-side [RetractDispatcher]
// interface. The metric_ingestor package's
// `*RetractDispatcher` is the production wiring; this fake
// keeps the management package's tests self-contained and
// avoids dragging the foundation-dispatch / repo_indexer
// tree into the test binary.
type fakeRetractDispatcher struct {
	// resolver looks up (repo_id, sha) for a sample_id -- the
	// fake dispatcher only checks idempotency by sample_id;
	// the resolver is shared with the MgmtWriter's
	// SampleResolver dep so a missing sample fails at the
	// resolve step (mirrors production sequencing).
	dispatched map[uuid.UUID]RetractResult
	// nextErr, when non-nil, is returned by Dispatch instead
	// of executing the in-memory write. Used by tests that
	// pin the "dispatcher failed" wire mapping.
	nextErr error
}

func newFakeRetractDispatcher() *fakeRetractDispatcher {
	return &fakeRetractDispatcher{dispatched: make(map[uuid.UUID]RetractResult)}
}

func (f *fakeRetractDispatcher) Dispatch(_ context.Context, sampleID uuid.UUID, reason, appendedBy string) (RetractResult, error) {
	if f.nextErr != nil {
		return RetractResult{}, f.nextErr
	}
	if existing, ok := f.dispatched[sampleID]; ok {
		// Idempotent re-dispatch -- mirror the
		// metric_ingestor contract: return the existing row
		// with Inserted=false and a zero ScanRunID (no new
		// scan_run is opened on the idempotent path).
		return RetractResult{
			Retraction: existing.Retraction,
			ScanRunID:  uuid.Nil,
			Inserted:   false,
		}, nil
	}
	retractionID := uuid.Must(uuid.NewV4())
	scanRunID := uuid.Must(uuid.NewV4())
	res := RetractResult{
		Retraction: RetractionRow{
			RetractionID: retractionID,
			SampleID:     sampleID,
			Reason:       reason,
			AppendedBy:   appendedBy,
			CreatedAt:    time.Now().UTC(),
		},
		ScanRunID: scanRunID,
		Inserted:  true,
	}
	f.dispatched[sampleID] = res
	return res, nil
}

// fakeRescanEnqueuer is a hand-rolled in-memory
// implementation of the management-side [RescanEnqueuer]
// interface.
type fakeRescanEnqueuer struct {
	enqueued []RescanResult
	nextErr  error
}

func newFakeRescanEnqueuer() *fakeRescanEnqueuer {
	return &fakeRescanEnqueuer{}
}

func (f *fakeRescanEnqueuer) Enqueue(_ context.Context, repoID uuid.UUID, sha, requestedBy string) (RescanResult, error) {
	if f.nextErr != nil {
		return RescanResult{}, f.nextErr
	}
	res := RescanResult{
		ScanRunID:   uuid.Must(uuid.NewV4()),
		RepoID:      repoID,
		SHA:         sha,
		RequestedBy: requestedBy,
		OpenedAt:    time.Now().UTC(),
	}
	f.enqueued = append(f.enqueued, res)
	return res, nil
}

// fakeSampleResolver is an in-memory [SampleResolver].
type fakeSampleResolver struct {
	samples map[uuid.UUID]fakeSampleLocator
	err     error
}

type fakeSampleLocator struct {
	RepoID uuid.UUID
	SHA    string
}

func newFakeSampleResolver() *fakeSampleResolver {
	return &fakeSampleResolver{samples: make(map[uuid.UUID]fakeSampleLocator)}
}

func (f *fakeSampleResolver) seed(sampleID, repoID uuid.UUID, sha string) {
	f.samples[sampleID] = fakeSampleLocator{RepoID: repoID, SHA: sha}
}

func (f *fakeSampleResolver) ResolveSample(_ context.Context, sampleID uuid.UUID) (uuid.UUID, string, bool, error) {
	if f.err != nil {
		return uuid.Nil, "", false, f.err
	}
	loc, ok := f.samples[sampleID]
	if !ok {
		return uuid.Nil, "", false, nil
	}
	return loc.RepoID, loc.SHA, true, nil
}

// ---- shared fixtures ----------------------------------------------------

// fixedSampleID is the canonical sample_id used across the
// happy-path tests. Pinning a literal value keeps assertions
// readable.
var (
	fixedSampleID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))
	fixedRepoID   = uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"))
	fixedSHA      = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
)

// newWiredMgmtWriter returns an MgmtWriter wired with
// seeded in-memory deps and the shared fixedSampleID known to
// the resolver. Tests that need to exercise a missing-sample
// path build their own writer with an empty resolver.
func newWiredMgmtWriter(t *testing.T) (*MgmtWriter, *fakeRetractDispatcher, *fakeRescanEnqueuer, *InMemoryRepoEventAppender, *fakeSampleResolver) {
	t.Helper()
	res := newFakeSampleResolver()
	res.seed(fixedSampleID, fixedRepoID, fixedSHA)
	disp := newFakeRetractDispatcher()
	enq := newFakeRescanEnqueuer()
	app := NewInMemoryRepoEventAppender()
	w := NewMgmtWriter(res, disp, enq, app)
	return w, disp, enq, app, res
}

// retractBody returns a JSON body for the canonical
// `mgmt.retract_sample` happy path.
func retractBody(t *testing.T, sampleID uuid.UUID, reason string) []byte {
	t.Helper()
	body := map[string]any{
		"sample_id": sampleID.String(),
		"reason":    reason,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal retract body: %v", err)
	}
	return buf
}

// rescanBody returns a JSON body for the canonical
// `mgmt.rescan` happy path.
func rescanBody(t *testing.T, repoID uuid.UUID, sha string) []byte {
	t.Helper()
	body := map[string]any{
		"repo_id": repoID.String(),
		"sha":     sha,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal rescan body: %v", err)
	}
	return buf
}

// ---- mgmt.retract_sample happy paths -------------------------------------

func TestMgmtWriter_RetractSample_HappyPath(t *testing.T) {
	t.Parallel()
	w, _, _, app, _ := newWiredMgmtWriter(t)

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "file is vendored")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()

	w.RetractSample(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json prefix", ct)
	}
	var resp retractWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rr.Body.String())
	}
	if _, err := uuid.FromString(resp.RetractionID); err != nil {
		t.Errorf("RetractionID=%q is not a uuid: %v", resp.RetractionID, err)
	}
	if resp.SampleID != fixedSampleID.String() {
		t.Errorf("SampleID=%q, want %q", resp.SampleID, fixedSampleID)
	}
	if resp.Reason != "file is vendored" {
		t.Errorf("Reason=%q, want %q", resp.Reason, "file is vendored")
	}
	if resp.AppendedBy != "operator:alice@example.com" {
		t.Errorf("AppendedBy=%q, want operator:alice@example.com (X-OIDC-Subject stamped)", resp.AppendedBy)
	}
	if !resp.Inserted {
		t.Errorf("Inserted=false, want true on first retract")
	}
	if _, err := uuid.FromString(resp.ScanRunID); err != nil {
		t.Errorf("ScanRunID=%q is not a uuid: %v", resp.ScanRunID, err)
	}

	// repo_event row appended.
	events := app.Events()
	if len(events) != 1 {
		t.Fatalf("len(repo_event)=%d, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != RepoEventKindRetractIntent {
		t.Errorf("repo_event.kind=%q, want %q", ev.Kind, RepoEventKindRetractIntent)
	}
	if ev.RepoID != fixedRepoID {
		t.Errorf("repo_event.repo_id=%s, want %s", ev.RepoID, fixedRepoID)
	}
	if got, want := ev.Payload["sample_id"], fixedSampleID.String(); got != want {
		t.Errorf("repo_event.payload.sample_id=%v, want %q", got, want)
	}
	if got, want := ev.Payload["reason"], "file is vendored"; got != want {
		t.Errorf("repo_event.payload.reason=%v, want %q", got, want)
	}
}

// TestMgmtWriter_RetractSample_Idempotent pins the
// dispatcher-layer idempotency contract at the HTTP boundary:
// a second `mgmt.retract_sample` for the same sample returns
// 200 with the SAME `retraction_id`, `inserted=false`, and a
// zero `scan_run_id` (no new scan_run is opened).
//
// NB the architecture-level intent log (`repo_event`) is
// APPEND-ONLY -- a retry creates a second `retract_intent` row
// even though only one `metric_retraction` exists. The wire
// surface accepts this duplication because the audit trail at
// the repo_event layer is the operator-intent log, not the
// applied-state log.
func TestMgmtWriter_RetractSample_Idempotent(t *testing.T) {
	t.Parallel()
	w, _, _, app, _ := newWiredMgmtWriter(t)

	doRetract := func() retractWireResponse {
		r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "first reason")))
		r.Header.Set(OIDCSubjectHeader, "alice@example.com")
		rr := httptest.NewRecorder()
		w.RetractSample(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var resp retractWireResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v; body=%s", err, rr.Body.String())
		}
		return resp
	}

	first := doRetract()
	if !first.Inserted {
		t.Errorf("first.Inserted=false, want true")
	}

	second := doRetract()
	if second.Inserted {
		t.Errorf("second.Inserted=true, want false (idempotent no-op)")
	}
	if second.RetractionID != first.RetractionID {
		t.Errorf("second.RetractionID=%q, want %q (idempotent)", second.RetractionID, first.RetractionID)
	}
	if second.ScanRunID != uuid.Nil.String() {
		t.Errorf("second.ScanRunID=%q, want zero UUID (no new scan_run on idempotent no-op)", second.ScanRunID)
	}

	// Two repo_event rows were appended -- the intent log is
	// append-only and accepts retry duplicates.
	if got := app.Count(); got != 2 {
		t.Errorf("repo_event count=%d, want 2 (intent log accepts retries)", got)
	}
}

// TestMgmtWriter_RetractSample_RoutesAndPathMounted verifies
// the canonical mux mounts the Stage 3.4 verbs at the pinned
// paths -- a misnamed path would silently 404 in production.
func TestMgmtWriter_RetractSample_RoutesAndPathMounted(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	mux := w.Routes()

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "via mux")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("via mux: status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// ---- mgmt.retract_sample error paths -------------------------------------

func TestMgmtWriter_RetractSample_RejectsGET(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodGet, VerbMgmtRetractSamplePath, nil)
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_RejectsMissingOIDCSubject(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	// no header set
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), OIDCSubjectHeader) {
		t.Errorf("body=%q does not name the header for operators reading the log", rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_RejectsBlankOIDCSubject(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	for _, blank := range []string{"", "   ", "\t\n"} {
		r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
		r.Header.Set(OIDCSubjectHeader, blank)
		rr := httptest.NewRecorder()
		w.RetractSample(rr, r)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("blank=%q: status=%d, want 401", blank, rr.Code)
		}
	}
}

func TestMgmtWriter_RetractSample_RejectsZeroSampleID(t *testing.T) {
	t.Parallel()
	w, _, _, app, _ := newWiredMgmtWriter(t)
	body := []byte(`{"sample_id":"00000000-0000-0000-0000-000000000000","reason":"x"}`)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(body))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sample_id") {
		t.Errorf("body=%q does not name the offending field 'sample_id'", rr.Body.String())
	}
	if app.Count() != 0 {
		t.Errorf("repo_event count=%d, want 0 (no audit row on validation failure)", app.Count())
	}
}

func TestMgmtWriter_RetractSample_RejectsBadSampleID(t *testing.T) {
	t.Parallel()
	w, _, _, app, _ := newWiredMgmtWriter(t)
	body := []byte(`{"sample_id":"not-a-uuid","reason":"x"}`)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(body))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if app.Count() != 0 {
		t.Errorf("repo_event count=%d, want 0 (no audit row on malformed sample_id)", app.Count())
	}
}

func TestMgmtWriter_RetractSample_RejectsEmptyReason(t *testing.T) {
	t.Parallel()
	w, _, _, app, _ := newWiredMgmtWriter(t)
	for _, blank := range []string{"", "   ", "\t\n"} {
		body := []byte(fmt.Sprintf(`{"sample_id":%q,"reason":%q}`, fixedSampleID.String(), blank))
		r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(body))
		r.Header.Set(OIDCSubjectHeader, "alice@example.com")
		rr := httptest.NewRecorder()
		w.RetractSample(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("reason=%q: status=%d, want 400", blank, rr.Code)
		}
	}
	if app.Count() != 0 {
		t.Errorf("repo_event count=%d, want 0 (no audit row on validation failure)", app.Count())
	}
}

func TestMgmtWriter_RetractSample_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	// `actor` in the body is rejected -- the actor MUST come
	// from the X-OIDC-Subject header so a caller cannot spoof
	// attribution.
	body := []byte(`{"sample_id":"` + fixedSampleID.String() + `","reason":"x","actor":"mallory@evil"}`)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(body))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, strings.NewReader("not-json"))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_UnknownSampleReturns404(t *testing.T) {
	t.Parallel()
	// Build a writer whose resolver knows ZERO samples.
	res := newFakeSampleResolver()
	disp := newFakeRetractDispatcher()
	enq := newFakeRescanEnqueuer()
	app := NewInMemoryRepoEventAppender()
	w := NewMgmtWriter(res, disp, enq, app)

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	// No repo_event row -- per architecture, an intent log
	// row for a non-existent sample would be misleading.
	if app.Count() != 0 {
		t.Errorf("repo_event count=%d, want 0 (no intent log for missing sample)", app.Count())
	}
	if got := len(disp.dispatched); got != 0 {
		t.Errorf("dispatched count=%d, want 0 (no dispatch on missing sample)", got)
	}
}

func TestMgmtWriter_RetractSample_ResolverErrorReturns500(t *testing.T) {
	t.Parallel()
	res := newFakeSampleResolver()
	res.err = errors.New("PG connection refused")
	disp := newFakeRetractDispatcher()
	enq := newFakeRescanEnqueuer()
	app := NewInMemoryRepoEventAppender()
	w := NewMgmtWriter(res, disp, enq, app)

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	// Body MUST NOT leak the underlying error message (an
	// unauthenticated client should not see driver / stack
	// details).
	if strings.Contains(rr.Body.String(), "PG connection refused") {
		t.Errorf("body=%q leaks the underlying error", rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_DispatcherErrorReturns500(t *testing.T) {
	t.Parallel()
	w, disp, _, app, _ := newWiredMgmtWriter(t)
	disp.nextErr = errors.New("metric_retraction insert failed")

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	// The repo_event row IS appended before dispatch -- the
	// operator's intent is durable even when the Measurement-
	// sub-store write fails.
	if app.Count() != 1 {
		t.Errorf("repo_event count=%d, want 1 (intent log lands before dispatch)", app.Count())
	}
}

func TestMgmtWriter_RetractSample_UnknownSampleSentinelFromDispatcherReturns404(t *testing.T) {
	t.Parallel()
	// The resolver KNOWS the sample (so we get past the
	// management-layer 404 short-circuit) but the dispatcher
	// returns a wrapped "sample_id not found" -- this can
	// happen in production if the Measurement sub-store's
	// view of the sample was deleted between the
	// management-side resolve and the dispatch call.
	//
	// Stage 7.3 iter 3 -- the dispatcher's error MUST wrap
	// [metric_ingestor.ErrRetractUnknownSample] with `%w` so
	// the wire-layer's `errors.Is(...)` check at
	// `mgmt_verbs.go:611` walks the chain and maps to 404.
	// An earlier version of this test built a plain
	// `fmt.Errorf("metric_ingestor: sample_id not found in
	// metric_sample: id=%s", ...)` whose message LOOKED like
	// the sentinel but was not actually wrapped, so the
	// sentinel check fell through to the 500 fallback. The
	// test's name (`...SentinelFromDispatcher...`) makes the
	// intent unambiguous: it pins the SENTINEL mapping, not
	// substring matching.
	w, disp, _, _, _ := newWiredMgmtWriter(t)
	disp.nextErr = fmt.Errorf("%w: id=%s", metric_ingestor.ErrRetractUnknownSample, fixedSampleID)

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_503WhenResolverNotWired(t *testing.T) {
	t.Parallel()
	w := NewMgmtWriter(nil, newFakeRetractDispatcher(), newFakeRescanEnqueuer(), NewInMemoryRepoEventAppender())
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_503WhenDispatcherNotWired(t *testing.T) {
	t.Parallel()
	res := newFakeSampleResolver()
	res.seed(fixedSampleID, fixedRepoID, fixedSHA)
	w := NewMgmtWriter(res, nil, newFakeRescanEnqueuer(), NewInMemoryRepoEventAppender())
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RetractSample_503WhenAppenderNotWired(t *testing.T) {
	t.Parallel()
	res := newFakeSampleResolver()
	res.seed(fixedSampleID, fixedRepoID, fixedSHA)
	w := NewMgmtWriter(res, newFakeRetractDispatcher(), newFakeRescanEnqueuer(), nil)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRetractSamplePath, bytes.NewReader(retractBody(t, fixedSampleID, "x")))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.RetractSample(rr, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

// ---- mgmt.rescan happy paths --------------------------------------------

func TestMgmtWriter_Rescan_HappyPath(t *testing.T) {
	t.Parallel()
	w, _, enq, app, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json prefix", ct)
	}
	var resp rescanWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rr.Body.String())
	}
	if _, err := uuid.FromString(resp.ScanRunID); err != nil {
		t.Errorf("ScanRunID=%q is not a uuid: %v", resp.ScanRunID, err)
	}
	if resp.RepoID != fixedRepoID.String() {
		t.Errorf("RepoID=%q, want %q", resp.RepoID, fixedRepoID)
	}
	if resp.SHA != fixedSHA {
		t.Errorf("SHA=%q, want %q", resp.SHA, fixedSHA)
	}
	if resp.RequestedBy != "operator:alice@example.com" {
		t.Errorf("RequestedBy=%q, want operator:alice@example.com", resp.RequestedBy)
	}
	if len(enq.enqueued) != 1 {
		t.Errorf("enqueued=%d, want 1", len(enq.enqueued))
	}

	// CRITICAL: NO repo_event row is appended for rescan
	// (canonical RepoEvent.kind enum has no `rescan_intent`
	// value per architecture Sec 5.1.4).
	if got := app.Count(); got != 0 {
		t.Errorf("repo_event count=%d, want 0 (no canonical rescan_intent kind exists)", got)
	}
}

// TestMgmtWriter_Rescan_EmitsNoRepoEvent is a focused
// regression guard for the architecture invariant that the
// rescan verb does NOT append any `repo_event` row -- even on
// repeated calls. A future contributor must not add an
// `intent` audit row here.
func TestMgmtWriter_Rescan_EmitsNoRepoEvent(t *testing.T) {
	t.Parallel()
	w, _, _, app, _ := newWiredMgmtWriter(t)
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
		r.Header.Set(OIDCSubjectHeader, "alice@example.com")
		rr := httptest.NewRecorder()
		w.Rescan(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("iter %d: status=%d, want 200", i, rr.Code)
		}
	}
	if got := app.Count(); got != 0 {
		t.Errorf("after 3 rescans, repo_event count=%d, want 0", got)
	}
}

// TestMgmtWriter_Rescan_NotIdempotent pins the e2e-scenario
// invariant: an operator who clicks rescan twice expects TWO
// scan_runs (so the recipe loop runs twice). The verb is
// deliberately NOT idempotent at the enqueuer layer.
func TestMgmtWriter_Rescan_NotIdempotent(t *testing.T) {
	t.Parallel()
	w, _, enq, _, _ := newWiredMgmtWriter(t)
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
		r.Header.Set(OIDCSubjectHeader, "alice@example.com")
		rr := httptest.NewRecorder()
		w.Rescan(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("iter %d: status=%d, want 200; body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if got := len(enq.enqueued); got != 3 {
		t.Errorf("after 3 rescans, enqueued=%d, want 3 (rescan is NOT idempotent)", got)
	}
	// Each scan_run_id is distinct.
	seen := make(map[uuid.UUID]struct{}, 3)
	for _, r := range enq.enqueued {
		if _, dup := seen[r.ScanRunID]; dup {
			t.Errorf("duplicate ScanRunID=%s observed across enqueues", r.ScanRunID)
		}
		seen[r.ScanRunID] = struct{}{}
	}
}

func TestMgmtWriter_Rescan_RoutesAndPathMounted(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	mux := w.Routes()

	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("via mux: status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// ---- mgmt.rescan error paths --------------------------------------------

func TestMgmtWriter_Rescan_RejectsGET(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodGet, VerbMgmtRescanPath, nil)
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_RejectsMissingOIDCSubject(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
	// no header
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	body := []byte(`{"repo_id":"00000000-0000-0000-0000-000000000000","sha":"deadbeef"}`)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(body))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_RejectsBadRepoID(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	body := []byte(`{"repo_id":"not-a-uuid","sha":"deadbeef"}`)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(body))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_RejectsEmptySHA(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	for _, blank := range []string{"", "   ", "\t\n"} {
		body := []byte(fmt.Sprintf(`{"repo_id":%q,"sha":%q}`, fixedRepoID.String(), blank))
		r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(body))
		r.Header.Set(OIDCSubjectHeader, "alice@example.com")
		rr := httptest.NewRecorder()
		w.Rescan(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("sha=%q: status=%d, want 400", blank, rr.Code)
		}
	}
}

func TestMgmtWriter_Rescan_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	body := []byte(`{"repo_id":"` + fixedRepoID.String() + `","sha":"deadbeef","actor":"mallory@evil"}`)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(body))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	w, _, _, _, _ := newWiredMgmtWriter(t)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, strings.NewReader("not-json"))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_503WhenEnqueuerNotWired(t *testing.T) {
	t.Parallel()
	res := newFakeSampleResolver()
	w := NewMgmtWriter(res, newFakeRetractDispatcher(), nil, NewInMemoryRepoEventAppender())
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_Rescan_EnqueuerErrorReturns500(t *testing.T) {
	t.Parallel()
	w, _, enq, _, _ := newWiredMgmtWriter(t)
	enq.nextErr = errors.New("PG insert failed")
	r := httptest.NewRequest(http.MethodPost, VerbMgmtRescanPath, bytes.NewReader(rescanBody(t, fixedRepoID, fixedSHA)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	w.Rescan(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "PG insert failed") {
		t.Errorf("body=%q leaks the underlying error", rr.Body.String())
	}
}

// ---- regression: no canonical rescan_intent kind anywhere ---------------

// TestRepoEventKind_NoRescanIntent pins the architecture
// Sec 5.1.4 invariant at the package level: the only
// canonical RepoEvent.kind value emitted by THIS package
// during retract / rescan flow is `retract_intent` -- there
// is no `rescan_intent` constant by design.
func TestRepoEventKind_NoRescanIntent(t *testing.T) {
	t.Parallel()
	if RepoEventKindRetractIntent != "retract_intent" {
		t.Fatalf("RepoEventKindRetractIntent=%q, want %q", RepoEventKindRetractIntent, "retract_intent")
	}
	// No `RepoEventKindRescanIntent` constant exists in this
	// package -- a future contributor adding one would break
	// the canonical RepoEvent.kind enum at architecture
	// Sec 5.1.4. The test references the only legitimate
	// constant so a regex search for "RepoEventKind" surfaces
	// just the one symbol.
}

// TestInMemoryRepoEventAppender_DefensiveCopiesPayload pins
// the defensive-copy contract on the in-memory appender: a
// caller that mutates the payload map after the call MUST
// NOT see the change reflected on the persisted row. The
// production PG appender will be naturally immutable (the
// payload is serialised to JSONB before INSERT); the
// in-memory implementation mirrors the contract so tests
// don't accidentally couple to mutable-payload semantics.
func TestInMemoryRepoEventAppender_DefensiveCopiesPayload(t *testing.T) {
	t.Parallel()
	app := NewInMemoryRepoEventAppender()
	payload := map[string]any{"sample_id": "x", "reason": "y"}
	if err := app.AppendRepoEvent(context.Background(), fixedRepoID, RepoEventKindRetractIntent, payload); err != nil {
		t.Fatalf("AppendRepoEvent: %v", err)
	}
	payload["reason"] = "MUTATED"
	stored := app.Events()[0]
	if stored.Payload["reason"] != "y" {
		t.Errorf("stored.Payload[\"reason\"]=%v, want %q (caller mutation must not bleed through)", stored.Payload["reason"], "y")
	}
}

// TestInMemoryRepoEventAppender_RejectsBadInputs pins the
// validation surface: a zero RepoID or empty kind must
// surface as an error rather than a stored row.
func TestInMemoryRepoEventAppender_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	app := NewInMemoryRepoEventAppender()
	// zero repoID
	if err := app.AppendRepoEvent(context.Background(), uuid.Nil, RepoEventKindRetractIntent, nil); err == nil {
		t.Error("zero repoID: err=nil, want non-nil")
	}
	// empty kind
	if err := app.AppendRepoEvent(context.Background(), fixedRepoID, "  ", nil); err == nil {
		t.Error("blank kind: err=nil, want non-nil")
	}
	if got := app.Count(); got != 0 {
		t.Errorf("Count=%d, want 0 (no rows stored on validation failure)", got)
	}
}

// TestInMemoryRepoEventAppender_EventsForRepoFiltersByRepoID
// pins the EventsForRepo helper used by handler tests that
// want to assert per-repo invariants.
func TestInMemoryRepoEventAppender_EventsForRepoFiltersByRepoID(t *testing.T) {
	t.Parallel()
	app := NewInMemoryRepoEventAppender()
	repoA := uuid.Must(uuid.NewV4())
	repoB := uuid.Must(uuid.NewV4())
	for i := 0; i < 2; i++ {
		if err := app.AppendRepoEvent(context.Background(), repoA, "registered", nil); err != nil {
			t.Fatalf("append A: %v", err)
		}
	}
	if err := app.AppendRepoEvent(context.Background(), repoB, "registered", nil); err != nil {
		t.Fatalf("append B: %v", err)
	}
	if got, want := len(app.EventsForRepo(repoA)), 2; got != want {
		t.Errorf("EventsForRepo(A)=%d, want %d", got, want)
	}
	if got, want := len(app.EventsForRepo(repoB)), 1; got != want {
		t.Errorf("EventsForRepo(B)=%d, want %d", got, want)
	}
}
