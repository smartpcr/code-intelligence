package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers"
)

// newChurnVerb constructs a [webhook.ChurnVerbHandler] backed
// by the standard in-memory pipeline.
func newChurnVerb(t *testing.T) (*webhook.ChurnVerbHandler, *metric_ingestor.InMemoryMetricSampleWriter) {
	t.Helper()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	return webhook.NewChurnVerbHandler(ing), writer
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
		t.Errorf("ScanRunKind() = %q; want %q", h.ScanRunKind(), metric_ingestor.ScanRunKindExternalPerRow)
	}
}

// TestChurnVerbHandler_HappyPath_HonoursSuppliedScanRunID
// pins the "Router owns the scan_run_id" contract: the verb
// MUST stamp every persisted record with the id the Router
// hands it, not mint its own.
func TestChurnVerbHandler_HappyPath_HonoursSuppliedScanRunID(t *testing.T) {
	t.Parallel()
	h, writer := newChurnVerb(t)
	body := goodPayloadJSON(t)
	scanRunID := uuid.Must(uuid.NewV7())

	res, err := h.Handle(context.Background(), body, scanRunID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.ScanRunID != scanRunID {
		t.Errorf("result.ScanRunID = %s; want %s (must mirror Router-supplied id)", res.ScanRunID, scanRunID)
	}
	if res.FoundationDispatched {
		t.Errorf("result.FoundationDispatched = true; want false (external_per_row)")
	}
	records := writer.Records()
	if len(records) == 0 {
		t.Fatalf("writer.Records: want >=1, got 0")
	}
	for _, r := range records {
		if r.ProducerRunID != scanRunID {
			t.Errorf("record.ProducerRunID = %s; want %s (same-ScanRun invariant)", r.ProducerRunID, scanRunID)
		}
	}
	// Detail envelope carries the per-verb counters.
	var detail struct {
		ChurnSamplesWritten int `json:"churn_samples_written"`
		ChurnRowsHydrated   int `json:"churn_rows_hydrated"`
	}
	if err := json.Unmarshal(res.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v (raw=%q)", err, res.Detail)
	}
	if detail.ChurnSamplesWritten != 2 || detail.ChurnRowsHydrated != 2 {
		t.Errorf("detail counters = %+v; want 2/2", detail)
	}
}

// TestChurnVerbHandler_ClassifyError_KnownSentinels pins the
// per-verb error-to-status mapping the Router consumes via
// the [webhook.VerbErrorClassifier] interface.
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
		{"RepoIDMismatch", metric_ingestor.ErrRepoIDMismatch, http.StatusBadRequest, "REPO_ID_MISMATCH"},
		{"ZeroRepoID", metric_ingestor.ErrZeroRepoID, http.StatusBadRequest, "EMPTY_REPO_ID"},
		{"WriterFailure", metric_ingestor.ErrWriterFailure, http.StatusInternalServerError, "WRITER_FAILURE"},
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
// BAD_REQUEST.
func TestChurnVerbHandler_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	h, writer := newChurnVerb(t)
	scanRunID := uuid.Must(uuid.NewV7())
	_, err := h.Handle(context.Background(), []byte("{not json"), scanRunID)
	if err == nil {
		t.Fatalf("Handle bad JSON: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError(bad JSON) = (%d, %q); want (400, %q)", status, code, "BAD_REQUEST")
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records: want 0 (decode failed), got %d", got)
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
	_, err := h.Handle(context.Background(), body, uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle with unknown field: want error, got nil")
	}
	status, _ := h.ClassifyError(err)
	if status != http.StatusBadRequest {
		t.Errorf("ClassifyError(unknown field): status = %d; want 400", status)
	}
}

// TestChurnVerbHandler_PanicsOnNilIngestor pins the wiring
// guard.
func TestNewChurnVerbHandler_PanicsOnNilIngestor(t *testing.T) {
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

// Touch unused imports for the few tests that need them.
var _ = bytes.NewReader
var _ = time.Hour
