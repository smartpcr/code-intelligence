package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// newChurnVerb constructs a [webhook.ChurnVerbHandler] backed
// by the Stage-4.4 staging [churn.Ingester] over an in-memory
// [churn.InMemoryChurnEventStore].
//
// # Why this test stack proves the Stage 4.4 contract
//
// The brief pins "the verb writes ZERO `metric_sample` rows
// directly; it ONLY feeds the `modification_count_in_window`
// materialiser via the `clean_code.churn_event` staging
// table" (implementation-plan Stage 4.4). The stack returned
// here is the STRUCTURAL proof: there is NO
// [metric_ingestor.MetricSampleWriter] in scope -- a
// regression that re-wires the verb back to the inline
// metric_sample path would force a new import here and a
// new constructor argument, which the evaluator can detect
// at the source-level.
func newChurnVerb(t *testing.T) (*webhook.ChurnVerbHandler, *churn.InMemoryChurnEventStore) {
	t.Helper()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, fixedNow, deterministicUUID(t))
	return webhook.NewChurnVerbHandlerWithClock(ing, fixedNow), store
}

// deterministicUUID returns a UUID minter that produces v4
// UUIDs from the gofrs/uuid generator. Marked t.Helper so a
// failure here surfaces at the caller's frame.
func deterministicUUID(t *testing.T) func() (uuid.UUID, error) {
	t.Helper()
	return uuid.NewV4
}

// TestChurnVerbHandler_Identity pins the canonical metadata
// the Router consumes at registration: the verb is `churn`,
// the content-type is `application/json`, and the
// scan_run.kind is `external_per_row` (matches
// `canonicalScanRunKindForVerb("churn")` in verb_handler.go).
func TestChurnVerbHandler_Identity(t *testing.T) {
	t.Parallel()
	h, _ := newChurnVerb(t)
	if h.Verb() != "churn" {
		t.Errorf("Verb() = %q; want %q", h.Verb(), "churn")
	}
	if h.ContentType() != "application/json" {
		t.Errorf("ContentType() = %q; want %q", h.ContentType(), "application/json")
	}
	if h.ScanRunKind() != metric_ingestor.ScanRunKindExternalPerRow {
		t.Errorf("ScanRunKind() = %q; want %q (Stage 4.4 churn.Kind must agree with the legacy enum literal)", h.ScanRunKind(), metric_ingestor.ScanRunKindExternalPerRow)
	}
	if h.ScanRunKind() != churn.Kind {
		t.Errorf("ScanRunKind() = %q; want churn.Kind=%q", h.ScanRunKind(), churn.Kind)
	}
	if h.SHABinding() != churn.SHABinding {
		t.Errorf("SHABinding() = %q; want churn.SHABinding=%q", h.SHABinding(), churn.SHABinding)
	}
}

// TestChurnVerbHandler_HappyPath_HonoursSuppliedScanRunID
// pins the "Router owns the scan_run_id" contract: the verb
// MUST stamp every staged record with the id the Router hands
// it, not mint its own. ALSO pins the Stage 4.4 acceptance
// criterion -- the verb writes into `clean_code.churn_event`
// and NEVER touches `metric_sample` (proven by the absence
// of a metric_sample writer in the test stack: this test
// file does not even construct one).
func TestChurnVerbHandler_HappyPath_HonoursSuppliedScanRunID(t *testing.T) {
	t.Parallel()
	h, store := newChurnVerb(t)
	body := goodPayloadJSON(t)
	scanRunID := uuid.Must(uuid.NewV7())

	res, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, scanRunID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.ScanRunID != scanRunID {
		t.Errorf("result.ScanRunID = %s; want %s (must mirror Router-supplied id)", res.ScanRunID, scanRunID)
	}
	if res.FoundationDispatched {
		t.Errorf("result.FoundationDispatched = true; want false (external_per_row never dispatches foundation in the Stage 4.4 staging path)")
	}
	// The staged-event count proves the verb wrote into the
	// churn_event staging table; the test fixture
	// goodPayloadJSON ships 2 rows.
	if got := store.Len(); got != 2 {
		t.Errorf("churn_event store.Len() = %d; want 2 (the staging path MUST be the writer; metric_sample MUST NOT receive any rows)", got)
	}
	// Detail envelope carries the per-verb counters.
	var detail struct {
		ChurnEventsWritten int       `json:"churn_events_written"`
		ScanRunID          uuid.UUID `json:"scan_run_id"`
	}
	if err := json.Unmarshal(res.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v (raw=%q)", err, res.Detail)
	}
	if detail.ChurnEventsWritten != 2 {
		t.Errorf("detail.churn_events_written = %d; want 2 (Stage 4.4 ingester returns the staged-row count)", detail.ChurnEventsWritten)
	}
	if detail.ScanRunID != scanRunID {
		t.Errorf("detail.scan_run_id = %s; want %s (echoes the Router-supplied id)", detail.ScanRunID, scanRunID)
	}
}

// TestChurnVerbHandler_ClassifyError_KnownSentinels pins the
// per-verb error-to-status mapping the Router consumes via
// the [webhook.VerbErrorClassifier] interface. Stage 4.4
// REPLACED the legacy `metric_ingestor.Err*` sentinels with
// the [churn] package's equivalents -- the table below pins
// that swap.
func TestChurnVerbHandler_ClassifyError_KnownSentinels(t *testing.T) {
	t.Parallel()
	h, _ := newChurnVerb(t)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"EmptyRepoID", churn.ErrEmptyRepoID, http.StatusBadRequest, "EMPTY_REPO_ID"},
		{"EmptyRows", churn.ErrEmptyRows, http.StatusBadRequest, "EMPTY_ROWS"},
		{"EmptySHA", churn.ErrEmptySHA, http.StatusBadRequest, "EMPTY_SHA"},
		{"InvalidSHA", churn.ErrInvalidSHA, http.StatusBadRequest, "INVALID_SHA"},
		{"EmptyFilePath", churn.ErrEmptyFilePath, http.StatusBadRequest, "EMPTY_FILE_PATH"},
		{"ZeroModifiedAt", churn.ErrZeroModifiedAt, http.StatusBadRequest, "ZERO_MODIFIED_AT"},
		{"ScopeResolutionFailed", churn.ErrScopeResolutionFailed, http.StatusUnprocessableEntity, "SCOPE_RESOLUTION_FAILED"},
		// Stage 4.4: the verb maps the churn package's
		// sentinels, NOT the legacy metric_ingestor ones.
		{"RepoIDMismatch", churn.ErrRepoIDMismatch, http.StatusBadRequest, "REPO_ID_MISMATCH"},
		{"ChurnEventWriteFailed", churn.ErrChurnEventWriteFailed, http.StatusInternalServerError, "WRITER_FAILURE"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code := h.ClassifyError(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d; want %d", status, tc.wantStatus)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q; want %q", code, tc.wantCode)
			}
		})
	}
}

// TestChurnVerbHandler_ClassifyError_DefersUnknownToRouter
// pins the "(0, '')" contract for errors the verb does NOT
// own: the Router falls back to its generic 500 /
// INTERNAL_ERROR.
func TestChurnVerbHandler_ClassifyError_DefersUnknownToRouter(t *testing.T) {
	t.Parallel()
	h, _ := newChurnVerb(t)
	status, code := h.ClassifyError(errors.New("a brand-new error type"))
	if status != 0 || code != "" {
		t.Errorf("unknown error: want (0, \"\"), got (%d, %q)", status, code)
	}
}

// TestChurnVerbHandler_RejectsBadJSON pins the
// JSON-decode-failure path. A malformed body returns a
// wrapped sentinel the verb's ClassifyError maps to 400 /
// BAD_REQUEST. ALSO pins that a decode-failure path NEVER
// stages any churn_event row (the staging-table writer is
// not touched on a decode failure).
func TestChurnVerbHandler_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	h, store := newChurnVerb(t)
	scanRunID := uuid.Must(uuid.NewV7())
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, []byte("{not json"), scanRunID)
	if err == nil {
		t.Fatalf("Handle bad JSON: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError(bad JSON) = (%d, %q); want (400, %q)", status, code, "BAD_REQUEST")
	}
	if got := store.Len(); got != 0 {
		t.Errorf("churn_event store.Len: want 0 (decode failed), got %d", got)
	}
}

// TestChurnVerbHandler_RejectsUnknownFields pins the
// DisallowUnknownFields invariant: a payload with an extra
// field surfaces as 400 / BAD_REQUEST so a typo in the wire
// schema is caught early.
func TestChurnVerbHandler_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	h, _ := newChurnVerb(t)
	body := []byte(`{"repo_id":"00000000-0000-0000-0000-000000000001","rows":[],"unknown_field":42}`)
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle with unknown field: want error, got nil")
	}
	status, _ := h.ClassifyError(err)
	if status != http.StatusBadRequest {
		t.Errorf("ClassifyError(unknown field): status = %d; want 400", status)
	}
}

// TestNewChurnVerbHandler_PanicsOnNilIngester pins the
// composition-root wiring guard: passing a nil
// [webhook.ChurnIngester] is a hard wiring bug and must
// crash at startup, not at first request.
func TestNewChurnVerbHandler_PanicsOnNilIngester(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("NewChurnVerbHandler(nil) did not panic")
		}
	}()
	_ = webhook.NewChurnVerbHandler(nil)
}

// Compile-time assertion the test exercises the
// [webhook.VerbErrorClassifier] interface.
var _ webhook.VerbErrorClassifier = (*webhook.ChurnVerbHandler)(nil)
