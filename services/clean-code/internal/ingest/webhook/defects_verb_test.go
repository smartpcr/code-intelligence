package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ingest/defects"
	"forge/services/clean-code/internal/ingest/webhook"
)

// goodDefectsPayloadJSON returns a serialised defects payload
// with two well-formed rows. Used as the happy-path body for
// the verb-handler round-trip tests.
func goodDefectsPayloadJSON(t *testing.T) []byte {
	t.Helper()
	p := defects.Payload{
		RepoID: fixedRepoID,
		Rows: []defects.PayloadRow{
			{SHA: validSHA('a'), FilePath: "internal/foo.go", DefectID: "JIRA-1", Severity: "critical"},
			{SHA: validSHA('b'), FilePath: "internal/bar.go", DefectID: "JIRA-2", Severity: "minor"},
		},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal defects payload: %v", err)
	}
	return body
}

// TestDefectsVerbHandler_Identity pins the canonical metadata
// the Router consumes at registration. The closed-set
// canon-guard in `canonicalScanRunKindForVerb("defects")`
// returns `external_per_row`, and `canonicalSHABindingForKind("external_per_row")`
// returns `per_row` -- this test mirrors those pins so a
// drift breaks loudly here.
func TestDefectsVerbHandler_Identity(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	if h.Verb() != "defects" {
		t.Errorf("Verb() = %q; want %q", h.Verb(), "defects")
	}
	if h.ContentType() != "application/json" {
		t.Errorf("ContentType() = %q; want %q", h.ContentType(), "application/json")
	}
	if h.ScanRunKind() != "external_per_row" {
		t.Errorf("ScanRunKind() = %q; want %q", h.ScanRunKind(), "external_per_row")
	}
	if h.SHABinding() != "per_row" {
		t.Errorf("SHABinding() = %q; want %q", h.SHABinding(), "per_row")
	}
	if h.ScanRunKind() != defects.ScanRunKindExternalPerRow {
		t.Errorf("ScanRunKind() = %q; want defects.ScanRunKindExternalPerRow=%q",
			h.ScanRunKind(), defects.ScanRunKindExternalPerRow)
	}
}

// TestDefectsVerbHandler_ExtractMetadata_HappyPath pins the
// canonical extraction: RepoID is surfaced from the body
// shape; SHA is intentionally empty (per-row binding leaves
// scan_run.to_sha NULL).
func TestDefectsVerbHandler_ExtractMetadata_HappyPath(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	body := goodDefectsPayloadJSON(t)
	md, err := h.ExtractMetadata(context.Background(), http.Header{}, body)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if md.RepoID != fixedRepoID {
		t.Errorf("RepoID = %s; want %s", md.RepoID, fixedRepoID)
	}
	if md.SHA != "" {
		t.Errorf("SHA = %q; want empty (per-row binding)", md.SHA)
	}
}

// TestDefectsVerbHandler_ExtractMetadata_FullValidation pins
// the "full validation here, not just in Handle" contract
// (see DefectsVerbHandler.ExtractMetadata doc). A malformed
// row surfaces at ExtractMetadata time so the Router does
// NOT open a durable scan_run row for it.
func TestDefectsVerbHandler_ExtractMetadata_FullValidation(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	p := defects.Payload{
		RepoID: fixedRepoID,
		Rows: []defects.PayloadRow{
			{SHA: "not-a-real-sha", FilePath: "internal/foo.go", DefectID: "JIRA-1", Severity: "critical"},
		},
	}
	body, _ := json.Marshal(p)
	_, err := h.ExtractMetadata(context.Background(), http.Header{}, body)
	if err == nil {
		t.Fatalf("ExtractMetadata(invalid SHA): want error, got nil")
	}
	if !errors.Is(err, defects.ErrInvalidSHA) {
		t.Errorf("ExtractMetadata error %v; want errors.Is ErrInvalidSHA", err)
	}
}

// TestDefectsVerbHandler_Handle_HappyPath_HonoursSuppliedScanRunID
// pins the "Router owns the scan_run_id" contract: the verb
// MUST mirror the id the Router hands it, not mint its own.
// Also asserts the result envelope is shape-correct (nil
// Detail, FoundationDispatched=false).
func TestDefectsVerbHandler_Handle_HappyPath_HonoursSuppliedScanRunID(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	body := goodDefectsPayloadJSON(t)
	scanRunID := uuid.Must(uuid.NewV7())

	res, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, scanRunID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.ScanRunID != scanRunID {
		t.Errorf("result.ScanRunID = %s; want %s (must mirror Router-supplied id)", res.ScanRunID, scanRunID)
	}
	if res.FoundationDispatched {
		t.Errorf("result.FoundationDispatched = true; want false (external_per_row, no foundation)")
	}
	if res.Detail != nil {
		t.Errorf("result.Detail = %q; want nil (v1 store-only, no per-verb counters)", res.Detail)
	}
}

// TestDefectsVerbHandler_Handle_BadJSON_Rejected pins the
// decode-failure path. A malformed body returns a wrapped
// sentinel the verb's ClassifyError maps to 400 / BAD_REQUEST.
func TestDefectsVerbHandler_Handle_BadJSON_Rejected(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	scanRunID := uuid.Must(uuid.NewV7())
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, []byte("{not json"), scanRunID)
	if err == nil {
		t.Fatalf("Handle bad JSON: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError(bad JSON) = (%d, %q); want (400, %q)", status, code, "BAD_REQUEST")
	}
}

// TestDefectsVerbHandler_Handle_UnknownFields_Rejected pins
// the DisallowUnknownFields invariant: a payload with an
// extra field surfaces as 400 / BAD_REQUEST so a typo in the
// wire schema is caught early. Important for the v2 forward-
// compat path -- when the schema lifts these rows into the
// catalogue, every field must be load-bearing, so we want
// publishers strict from day one.
func TestDefectsVerbHandler_Handle_UnknownFields_Rejected(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	body := []byte(`{"repo_id":"11111111-2222-3333-4444-555555555555","rows":[],"unknown_field":42}`)
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle with unknown field: want error, got nil")
	}
	status, _ := h.ClassifyError(err)
	if status != http.StatusBadRequest {
		t.Errorf("ClassifyError(unknown field): status = %d; want 400", status)
	}
}

// TestDefectsVerbHandler_Handle_TrailingData_Rejected (iter 2
// evaluator item #2) pins the EOF-after-top-level-value
// guard inside [webhook.DefectsVerbHandler.decode]. Without
// it, a body like
//
//	{"repo_id":"...","rows":[...]} GARBAGE
//
// would be silently accepted because [json.Decoder.Decode]
// only consumes the FIRST top-level value and leaves trailing
// tokens unread. The verb MUST reject this so the scan_run
// is NEVER finalised `succeeded` against a body the publisher
// did not actually send in canonical form.
//
// The test covers two flavours of trailing data:
//
//  1. A second valid JSON value after the payload (the
//     "publisher accidentally sent newline-delimited JSON"
//     mode).
//  2. Free-form non-JSON garbage after the payload (the
//     "transport accidentally appended trailer bytes" mode).
//
// Both MUST return an error from Handle AND classify as
// 400 / BAD_REQUEST so the publisher sees one canonical
// "your body is malformed" code rather than a probe-able
// difference between "extra-value" and "extra-garbage".
func TestDefectsVerbHandler_Handle_TrailingData_Rejected(t *testing.T) {
	t.Parallel()
	good := string(goodDefectsPayloadJSON(t))
	cases := []struct {
		name string
		body []byte
	}{
		// Two top-level JSON values in one body. The first
		// decodes cleanly; the second decode MUST be rejected
		// by the EOF guard.
		{"extra-json-value", []byte(good + `{"another":"object"}`)},
		// Free-form garbage after the payload. The second
		// decode returns a json.SyntaxError; the guard wraps
		// it in errDefectsTrailingData.
		{"trailing-garbage", []byte(good + ` GARBAGE_BYTES`)},
		// A second array. Same shape as #1 but distinct top-
		// level type so the test exercises the "any trailing
		// value" -- not just object -- path.
		{"extra-array", []byte(good + `[1,2,3]`)},
		// Pure whitespace trailing is OK -- json.Decoder
		// permits it as part of the "one top-level value"
		// idiom. Listed here for the inverse assertion below.
	}
	h := webhook.NewDefectsVerbHandler()
	scanRunID := uuid.Must(uuid.NewV7())
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, tc.body, scanRunID)
			if err == nil {
				t.Fatalf("Handle(%s): want error, got nil", tc.name)
			}
			status, code := h.ClassifyError(err)
			if status != http.StatusBadRequest || code != "BAD_REQUEST" {
				t.Errorf("ClassifyError(%s) = (%d, %q); want (400, %q)",
					tc.name, status, code, "BAD_REQUEST")
			}
		})
	}

	// Inverse: pure trailing whitespace MUST still be
	// accepted. json.Decoder consumes whitespace between
	// top-level values as part of the EOF probe and returns
	// io.EOF, so the guard does NOT trip. This keeps the
	// rule "exactly one JSON value, possibly with trailing
	// whitespace" without forcing publishers to byte-trim.
	t.Run("trailing-whitespace-ok", func(t *testing.T) {
		t.Parallel()
		body := []byte(good + "\n\t  \r\n")
		res, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, scanRunID)
		if err != nil {
			t.Fatalf("Handle(trailing-whitespace): want nil error, got %v", err)
		}
		if res.ScanRunID != scanRunID {
			t.Errorf("Handle(trailing-whitespace): result.ScanRunID = %s; want %s",
				res.ScanRunID, scanRunID)
		}
	})
}

// TestDefectsVerbHandler_ExtractMetadata_TrailingData_Rejected
// pins that the trailing-data guard runs in [ExtractMetadata]
// too, NOT just in [Handle]. This matters because the Router
// opens the durable scan_run row BETWEEN ExtractMetadata and
// Handle -- if the trailing-data check were Handle-only, a
// malformed body would burn a permanent scan_run slot before
// being rejected (sticky-failed payload_hash). ExtractMetadata
// runs the same decode helper, so the guard catches it BEFORE
// the durable claim.
func TestDefectsVerbHandler_ExtractMetadata_TrailingData_Rejected(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	body := []byte(string(goodDefectsPayloadJSON(t)) + `{"extra":"value"}`)
	_, err := h.ExtractMetadata(context.Background(), http.Header{}, body)
	if err == nil {
		t.Fatalf("ExtractMetadata(trailing-data): want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError on trailing-data = (%d, %q); want (400, %q)",
			status, code, "BAD_REQUEST")
	}
}

// TestDefectsVerbHandler_Handle_ValidationFailure_Surfaces pins
// the validation-error surface from Handle. The Router
// classifies via VerbErrorClassifier and the publisher gets a
// structured 400 with the canonical code.
func TestDefectsVerbHandler_Handle_ValidationFailure_Surfaces(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	// Valid JSON shape but empty Rows -> ErrEmptyRows.
	body := []byte(`{"repo_id":"11111111-2222-3333-4444-555555555555","rows":[]}`)
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle empty-rows: want error, got nil")
	}
	if !errors.Is(err, defects.ErrEmptyRows) {
		t.Errorf("Handle empty-rows error = %v; want errors.Is ErrEmptyRows", err)
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "EMPTY_ROWS" {
		t.Errorf("ClassifyError(ErrEmptyRows) = (%d, %q); want (400, %q)", status, code, "EMPTY_ROWS")
	}
}

// TestDefectsVerbHandler_ClassifyError_KnownSentinels pins
// the per-verb error-to-status mapping the Router consumes
// via the [webhook.VerbErrorClassifier] interface. Every
// defects-package sentinel MUST map to a documented (status,
// code) tuple.
func TestDefectsVerbHandler_ClassifyError_KnownSentinels(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"EmptyRepoID", defects.ErrEmptyRepoID, http.StatusBadRequest, "EMPTY_REPO_ID"},
		{"EmptyRows", defects.ErrEmptyRows, http.StatusBadRequest, "EMPTY_ROWS"},
		{"EmptySHA", defects.ErrEmptySHA, http.StatusBadRequest, "EMPTY_SHA"},
		{"InvalidSHA", defects.ErrInvalidSHA, http.StatusBadRequest, "INVALID_SHA"},
		{"EmptyFilePath", defects.ErrEmptyFilePath, http.StatusBadRequest, "EMPTY_FILE_PATH"},
		{"EmptyDefectID", defects.ErrEmptyDefectID, http.StatusBadRequest, "EMPTY_DEFECT_ID"},
		{"EmptySeverity", defects.ErrEmptySeverity, http.StatusBadRequest, "EMPTY_SEVERITY"},
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

// TestDefectsVerbHandler_ClassifyError_DefersUnknownToRouter
// pins the "(0, '')" contract for errors the verb does NOT
// own: the Router falls back to its generic 500 /
// INTERNAL_ERROR.
func TestDefectsVerbHandler_ClassifyError_DefersUnknownToRouter(t *testing.T) {
	t.Parallel()
	h := webhook.NewDefectsVerbHandler()
	status, code := h.ClassifyError(errors.New("a brand-new error type"))
	if status != 0 || code != "" {
		t.Errorf("unknown error: want (0, \"\"), got (%d, %q)", status, code)
	}
}

// TestNewRouter_AcceptsDefectsVerbHandler pins that the
// closed-set canon-guard in NewRouter accepts the defects
// registration -- a regression in the verb name, kind, or
// SHA binding would panic at composition time.
func TestNewRouter_AcceptsDefectsVerbHandler(t *testing.T) {
	t.Parallel()
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		"kv-test-2026-01": []byte("test-hmac-secret-bytes-suffice"),
	})
	store := webhook.NewInMemoryIdempotencyStore(0)
	scanRunRepo := webhook.NewInMemoryScanRunRepository()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewRouter panicked on defects registration: %v", r)
		}
	}()
	_ = webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: scanRunRepo,
		Verbs:       []webhook.VerbHandler{webhook.NewDefectsVerbHandler()},
	})
}

// Compile-time assertion the test exercises the
// [webhook.VerbErrorClassifier] interface.
var _ webhook.VerbErrorClassifier = (*webhook.DefectsVerbHandler)(nil)

// Touch unused import for the few tests that need it.
var _ = strings.Repeat
