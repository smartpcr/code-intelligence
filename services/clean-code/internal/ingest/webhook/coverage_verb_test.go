package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/materialisers"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

const (
	covTestRepoID    = "11111111-1111-1111-1111-111111111111"
	covTestSHA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	covTestHandlerGo = "internal/svc/handler.go"
	covTestHelperGo  = "internal/util/helper.go"
)

// goodCoverageBody returns a Cobertura XML body whose root
// `<coverage>` carries the operator-pinned repo_id + sha
// attributes the verb handler's [ExtractMetadata] consults.
func goodCoverageBody() []byte {
	return []byte(`<?xml version="1.0" ?>
<coverage repo_id="` + covTestRepoID + `" sha="` + covTestSHA + `" line-rate="0.75" branch-rate="0.5">
  <packages>
    <package name="internal.svc">
      <classes>
        <class name="Foo" filename="` + covTestHandlerGo + `">
          <lines>
            <line number="10" hits="1" branch="false"/>
            <line number="20" hits="0" branch="true" condition-coverage="50% (1/2)"/>
          </lines>
        </class>
      </classes>
    </package>
    <package name="internal.util">
      <classes>
        <class name="Helper" filename="` + covTestHelperGo + `">
          <lines>
            <line number="5" hits="3" branch="false"/>
          </lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>`)
}

// newCoverageVerb wires a webhook.CoverageVerbHandler with
// an in-memory metric_sample writer and a populated scope
// resolver. Returns the handler + the writer for assertions.
func newCoverageVerb(t *testing.T) (*webhook.CoverageVerbHandler, *metric_ingestor.InMemoryMetricSampleWriter) {
	t.Helper()
	repoID, err := uuid.FromString(covTestRepoID)
	if err != nil {
		t.Fatalf("FromString: %v", err)
	}
	resolver := coverage.NewMapScopeResolver()
	for _, p := range []string{covTestHandlerGo, covTestHelperGo} {
		scopeID, err := uuid.NewV4()
		if err != nil {
			t.Fatalf("NewV4: %v", err)
		}
		resolver.Add(repoID, p, scopeID, recipes.ScopeRef{
			Kind:          scope.KindFile,
			Path:          p,
			QualifiedName: p,
		})
	}
	hyd := coverage.NewHydrator(resolver)
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewCoverageSweep(hyd, writer)

	churnSweep := newPipelineChurnSweep(t)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep).
		WithCoverageSweep(sweep)
	return webhook.NewCoverageVerbHandler(ing), writer
}

// newPipelineChurnSweep returns a working ChurnSweep so the
// Ingestor's required dep is satisfied (the coverage verb
// itself does NOT touch the churn pipeline; we wire one
// because the Ingestor constructor refuses a nil sweep).
func newPipelineChurnSweep(t *testing.T) *metric_ingestor.ChurnSweep {
	t.Helper()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	return metric_ingestor.NewChurnSweep(mat, hyd, writer)
}

// TestCoverageVerbHandler_Identity pins the canonical
// metadata the Router consumes at registration: the verb is
// `coverage`, the content-type is `application/xml`, and
// the scan_run.kind is `external_single`.
func TestCoverageVerbHandler_Identity(t *testing.T) {
	t.Parallel()
	h, _ := newCoverageVerb(t)
	if h.Verb() != "coverage" {
		t.Errorf("Verb() = %q; want %q", h.Verb(), "coverage")
	}
	if h.ContentType() != "application/xml" {
		t.Errorf("ContentType() = %q; want %q", h.ContentType(), "application/xml")
	}
	if h.ScanRunKind() != metric_ingestor.ScanRunKindExternalSingle {
		t.Errorf("ScanRunKind() = %q; want %q", h.ScanRunKind(), metric_ingestor.ScanRunKindExternalSingle)
	}
	if h.SHABinding() != metric_ingestor.SHABindingSingle {
		t.Errorf("SHABinding() = %q; want %q", h.SHABinding(), metric_ingestor.SHABindingSingle)
	}
}

// TestCoverageVerbHandler_ExtractMetadata_HappyPath asserts
// the streaming root-attribute reader returns the canonical
// (RepoID, SHA) pair so the Router can open the scan_run row.
func TestCoverageVerbHandler_ExtractMetadata_HappyPath(t *testing.T) {
	t.Parallel()
	h, _ := newCoverageVerb(t)
	md, err := h.ExtractMetadata(context.Background(), http.Header{}, goodCoverageBody())
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	want, err := uuid.FromString(covTestRepoID)
	if err != nil {
		t.Fatalf("FromString: %v", err)
	}
	if md.RepoID != want {
		t.Errorf("RepoID = %v; want %v", md.RepoID, want)
	}
	if md.SHA != covTestSHA {
		t.Errorf("SHA = %q; want %q", md.SHA, covTestSHA)
	}
}

// TestCoverageVerbHandler_ExtractMetadata_MissingRepoID
// pins the "metadata missing -> 400/EMPTY_REPO_ID" mapping.
func TestCoverageVerbHandler_ExtractMetadata_MissingRepoID(t *testing.T) {
	t.Parallel()
	h, _ := newCoverageVerb(t)
	body := []byte(`<?xml version="1.0"?><coverage sha="` + covTestSHA + `"/>`)
	_, err := h.ExtractMetadata(context.Background(), http.Header{}, body)
	if err == nil {
		t.Fatalf("ExtractMetadata: want error, got nil")
	}
	if !errors.Is(err, coverage.ErrEmptyRepoID) {
		t.Errorf("err = %v; want errors.Is ErrEmptyRepoID", err)
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "EMPTY_REPO_ID" {
		t.Errorf("ClassifyError = (%d, %q); want (400, EMPTY_REPO_ID)", status, code)
	}
}

// TestCoverageVerbHandler_HappyPath_HonoursSuppliedScanRunID
// pins the "Router owns the scan_run_id" contract: the verb
// MUST stamp every persisted record with the id the Router
// hands it, not mint its own.
func TestCoverageVerbHandler_HappyPath_HonoursSuppliedScanRunID(t *testing.T) {
	t.Parallel()
	h, writer := newCoverageVerb(t)
	scanRunID := uuid.Must(uuid.NewV7())
	body := goodCoverageBody()

	res, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, scanRunID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.ScanRunID != scanRunID {
		t.Errorf("result.ScanRunID = %s; want %s (must mirror Router-supplied id)", res.ScanRunID, scanRunID)
	}
	if res.FoundationDispatched {
		t.Errorf("result.FoundationDispatched = true; want false (external_single coverage path never dispatches foundation)")
	}
	records := writer.Records()
	// handler.go emits line+branch (2 rows); helper.go emits line only (1 row) = 3 records.
	if len(records) != 3 {
		t.Fatalf("writer.Records: want 3, got %d (%+v)", len(records), records)
	}
	for i, r := range records {
		if r.ProducerRunID != scanRunID {
			t.Errorf("record[%d].ProducerRunID = %s; want %s (same-ScanRun invariant)", i, r.ProducerRunID, scanRunID)
		}
		if r.SHA != covTestSHA {
			t.Errorf("record[%d].SHA = %q; want %q (single-SHA binding)", i, r.SHA, covTestSHA)
		}
		if r.Pack != recipes.PackIngested {
			t.Errorf("record[%d].Pack = %q; want %q", i, r.Pack, recipes.PackIngested)
		}
		if r.Source != recipes.SourceIngested {
			t.Errorf("record[%d].Source = %q; want %q", i, r.Source, recipes.SourceIngested)
		}
		// Forbidden legacy aliases must NEVER appear.
		if r.MetricKind == "coverage_line" || r.MetricKind == "coverage_branch" {
			t.Errorf("record[%d].MetricKind = %q is a legacy alias (iter-1 evaluator item 4)", i, r.MetricKind)
		}
	}

	// Detail envelope carries the per-verb counters.
	var detail struct {
		CoverageSamplesWritten      int `json:"coverage_samples_written"`
		CoverageRowsHydrated        int `json:"coverage_rows_hydrated"`
		CoverageSkippedUnboundScope int `json:"coverage_skipped_unbound_scope"`
	}
	if err := json.Unmarshal(res.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v (raw=%q)", err, res.Detail)
	}
	if detail.CoverageSamplesWritten != 3 {
		t.Errorf("detail.coverage_samples_written = %d; want 3", detail.CoverageSamplesWritten)
	}
	if detail.CoverageRowsHydrated != 3 {
		t.Errorf("detail.coverage_rows_hydrated = %d; want 3", detail.CoverageRowsHydrated)
	}
	if detail.CoverageSkippedUnboundScope != 0 {
		t.Errorf("detail.coverage_skipped_unbound_scope = %d; want 0 (every file resolved)", detail.CoverageSkippedUnboundScope)
	}
}

// TestCoverageVerbHandler_DetailReportsSkippedUnboundScope
// pins the "unbound files counted, not dropped silently"
// contract from iter-1 evaluator item 4.
func TestCoverageVerbHandler_DetailReportsSkippedUnboundScope(t *testing.T) {
	t.Parallel()
	// Build a handler whose resolver knows ONLY helper.go
	// so handler.go's rows land in the skip bucket.
	repoID, err := uuid.FromString(covTestRepoID)
	if err != nil {
		t.Fatalf("FromString: %v", err)
	}
	resolver := coverage.NewMapScopeResolver()
	scopeID, _ := uuid.NewV4()
	resolver.Add(repoID, covTestHelperGo, scopeID, recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          covTestHelperGo,
		QualifiedName: covTestHelperGo,
	})
	hyd := coverage.NewHydrator(resolver)
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewCoverageSweep(hyd, writer)
	churnSweep := newPipelineChurnSweep(t)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep).
		WithCoverageSweep(sweep)
	h := webhook.NewCoverageVerbHandler(ing)

	res, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, goodCoverageBody(), uuid.Must(uuid.NewV7()))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var detail struct {
		CoverageSamplesWritten      int `json:"coverage_samples_written"`
		CoverageSkippedUnboundScope int `json:"coverage_skipped_unbound_scope"`
	}
	if err := json.Unmarshal(res.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.CoverageSkippedUnboundScope != 1 {
		t.Errorf("detail.coverage_skipped_unbound_scope = %d; want 1 (handler.go unbound)", detail.CoverageSkippedUnboundScope)
	}
	if detail.CoverageSamplesWritten != 1 {
		t.Errorf("detail.coverage_samples_written = %d; want 1 (helper.go line ratio)", detail.CoverageSamplesWritten)
	}
}

// TestCoverageVerbHandler_RejectsBadXML pins the
// XML-decode-failure path. A malformed body returns an
// error the verb's ClassifyError maps to 400 / BAD_REQUEST.
func TestCoverageVerbHandler_RejectsBadXML(t *testing.T) {
	t.Parallel()
	h, writer := newCoverageVerb(t)
	scanRunID := uuid.Must(uuid.NewV7())
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, []byte("<not xml"), scanRunID)
	if err == nil {
		t.Fatalf("Handle bad XML: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError(bad XML) = (%d, %q); want (400, BAD_REQUEST)", status, code)
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records: want 0 (decode failed), got %d", got)
	}
}

// TestCoverageVerbHandler_RejectsTrailingContent pins the
// EOF-after-root invariant (iter-1 evaluator item 6) at the
// verb-handler layer: a body with extra content after the
// closing `</coverage>` returns ErrTrailingContent which
// the classifier maps to 400 / BAD_REQUEST.
func TestCoverageVerbHandler_RejectsTrailingContent(t *testing.T) {
	t.Parallel()
	h, _ := newCoverageVerb(t)
	body := append([]byte{}, goodCoverageBody()...)
	body = append(body, []byte("<extra/>")...)
	_, err := h.Handle(context.Background(), webhook.VerbPayloadMetadata{}, body, uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle trailing content: want error, got nil")
	}
	if !errors.Is(err, coverage.ErrTrailingContent) {
		t.Errorf("err = %v; want errors.Is ErrTrailingContent", err)
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError = (%d, %q); want (400, BAD_REQUEST)", status, code)
	}
}

// TestCoverageVerbHandler_ClassifyError_KnownSentinels pins
// the per-verb error-to-status mapping the Router consumes
// via the [webhook.VerbErrorClassifier] interface.
func TestCoverageVerbHandler_ClassifyError_KnownSentinels(t *testing.T) {
	t.Parallel()
	h, _ := newCoverageVerb(t)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"EmptyRepoID", coverage.ErrEmptyRepoID, http.StatusBadRequest, "EMPTY_REPO_ID"},
		{"InvalidRepoID", coverage.ErrInvalidRepoID, http.StatusBadRequest, "INVALID_REPO_ID"},
		{"EmptySHA", coverage.ErrEmptySHA, http.StatusBadRequest, "EMPTY_SHA"},
		{"InvalidSHA", coverage.ErrInvalidSHA, http.StatusBadRequest, "INVALID_SHA"},
		{"EmptyFiles", coverage.ErrEmptyFiles, http.StatusBadRequest, "EMPTY_FILES"},
		{"EmptyFilePath", coverage.ErrEmptyFilePath, http.StatusBadRequest, "EMPTY_FILE_PATH"},
		{"UnsafeFilePath", coverage.ErrUnsafeFilePath, http.StatusBadRequest, "UNSAFE_FILE_PATH"},
		{"InvalidLineCount", coverage.ErrInvalidLineCount, http.StatusBadRequest, "INVALID_LINE_COUNTS"},
		{"InvalidBranchCount", coverage.ErrInvalidBranchCount, http.StatusBadRequest, "INVALID_BRANCH_COUNTS"},
		{"MalformedConditionCoverage", coverage.ErrMalformedConditionCoverage, http.StatusBadRequest, "MALFORMED_CONDITION_COVERAGE"},
		{"MalformedXML", coverage.ErrMalformedXML, http.StatusBadRequest, "BAD_REQUEST"},
		{"TrailingContent", coverage.ErrTrailingContent, http.StatusBadRequest, "BAD_REQUEST"},
		{"ScopeResolutionFailed", coverage.ErrScopeResolutionFailed, http.StatusUnprocessableEntity, "SCOPE_RESOLUTION_FAILED"},
		{"RepoIDMismatch", metric_ingestor.ErrRepoIDMismatch, http.StatusBadRequest, "REPO_ID_MISMATCH"},
		{"CoverageSHAMismatch", metric_ingestor.ErrCoverageSHAMismatch, http.StatusBadRequest, "SHA_MISMATCH"},
		{"ZeroRepoID", metric_ingestor.ErrZeroRepoID, http.StatusBadRequest, "EMPTY_REPO_ID"},
		{"MissingPayload", metric_ingestor.ErrMissingCoveragePayloadForExternalSingle, http.StatusBadRequest, "EMPTY_PAYLOAD"},
		{"CoverageSweepUnwired", metric_ingestor.ErrCoverageSweepUnwired, http.StatusInternalServerError, "INTERNAL_ERROR"},
		{"ZeroScanRunID", metric_ingestor.ErrZeroScanRunID, http.StatusInternalServerError, "INTERNAL_ERROR"},
		{"InvalidScanRunKind", metric_ingestor.ErrInvalidScanRunKind, http.StatusInternalServerError, "INTERNAL_ERROR"},
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

// TestCoverageVerbHandler_ClassifyError_DefersUnknownToRouter
// pins the (0, "") contract for errors the verb does NOT
// own: the Router falls back to its generic 500 /
// INTERNAL_ERROR.
func TestCoverageVerbHandler_ClassifyError_DefersUnknownToRouter(t *testing.T) {
	t.Parallel()
	h, _ := newCoverageVerb(t)
	status, code := h.ClassifyError(errors.New("a brand-new error type"))
	if status != 0 || code != "" {
		t.Errorf("unknown error: want (0, \"\"), got (%d, %q)", status, code)
	}
}

// TestNewCoverageVerbHandler_PanicsOnNilIngestor pins the
// wiring guard.
func TestNewCoverageVerbHandler_PanicsOnNilIngestor(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("NewCoverageVerbHandler(nil) did not panic")
		}
	}()
	_ = webhook.NewCoverageVerbHandler(nil)
}

// TestNewCoverageVerbHandler_PanicsOnUnwiredSweep asserts
// the construction-time fail-fast (iter-3 evaluator item
// 5): a CoverageVerbHandler mounted onto an Ingestor that
// the composition root forgot to wire with a CoverageSweep
// MUST panic during composition, NOT defer the failure to
// request time. This replaces the prior iter's runtime-500
// behaviour because a verb registration without its writer
// is unambiguously a wiring bug -- delaying the failure to
// the first request hides the root-cause behind a 500 log
// line.
func TestNewCoverageVerbHandler_PanicsOnUnwiredSweep(t *testing.T) {
	t.Parallel()
	churnSweep := newPipelineChurnSweep(t)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep)
	// Deliberately do NOT call WithCoverageSweep.

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("NewCoverageVerbHandler did not panic on unwired sweep")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "CoverageSweep") {
			t.Errorf("panic msg=%q does not mention CoverageSweep", msg)
		}
		if !strings.Contains(msg, "WithCoverageSweep") {
			t.Errorf("panic msg=%q does not point at WithCoverageSweep", msg)
		}
	}()
	_ = webhook.NewCoverageVerbHandler(ing)
}

// Compile-time assertion the test exercises the
// [webhook.VerbErrorClassifier] interface.
var _ webhook.VerbErrorClassifier = (*webhook.CoverageVerbHandler)(nil)

// Touch the strings import for fixture generators.
var _ = strings.Repeat
