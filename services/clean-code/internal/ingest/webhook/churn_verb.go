package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// ChurnVerbHandler is the [VerbHandler] implementation for
// the `ingest.churn` verb (architecture Sec 6.4 / tech-spec
// Sec 8.5 row 3). It wraps a [metric_ingestor.Ingestor] and
// translates the inbound JSON body into a
// [metric_ingestor.RunRequest] with
// `kind='external_per_row'` per the verb-to-kind matrix in
// e2e-scenarios.md line 687.
//
// # Why a separate type from ChurnIngestHandler
//
// [ChurnIngestHandler] is the legacy direct mount at the
// fixed path `/v1/ingest/churn` (iter 5 structural fix --
// see handler.go doc-comment). The Router introduced by
// Stage 4.1 sits at `/v1/ingest/{verb}` and does the HMAC /
// idempotency work above the per-verb handler. This type
// is the seam through which the Router drives the SAME
// underlying Ingestor without duplicating the HTTP
// concerns (content-type parsing, error mapping, response
// shaping) the Router already owns.
type ChurnVerbHandler struct {
	ingestor *metric_ingestor.Ingestor
}

// errChurnJSONDecode is the sentinel the verb wraps every
// JSON-decode failure in so [ClassifyError] can pattern-
// match without parsing the inner json.* error's free-form
// text. NOT exported: callers consume the classified status
// code, not this sentinel directly.
var errChurnJSONDecode = errors.New("webhook/churn: JSON decode failed")

// NewChurnVerbHandler constructs a [ChurnVerbHandler] bound
// to `ingestor`. PANICS on a nil ingestor -- a verb handler
// without a writer cannot service any request and the
// composition-root misconfig should fail loudly at startup.
func NewChurnVerbHandler(ingestor *metric_ingestor.Ingestor) *ChurnVerbHandler {
	if ingestor == nil {
		panic("webhook: NewChurnVerbHandler received nil Ingestor")
	}
	return &ChurnVerbHandler{ingestor: ingestor}
}

// Verb implements [VerbHandler].
func (h *ChurnVerbHandler) Verb() string { return "churn" }

// ContentType implements [VerbHandler]. `ingest.churn` is
// pinned to `application/json` per tech-spec Sec 8.5 row 3.
func (h *ChurnVerbHandler) ContentType() string { return "application/json" }

// ScanRunKind implements [VerbHandler]. `ingest.churn` is
// `external_per_row` per e2e-scenarios.md line 687 and
// tech-spec Sec 4.11.
func (h *ChurnVerbHandler) ScanRunKind() string {
	return metric_ingestor.ScanRunKindExternalPerRow
}

// SHABinding implements [VerbHandler]. `external_per_row`
// leaves `scan_run.to_sha` NULL because each emitted
// metric_sample carries its own SHA. Migration 0001's
// scan_run_sha_binding_consistent CHECK enforces this.
func (h *ChurnVerbHandler) SHABinding() string {
	return metric_ingestor.SHABindingPerRow
}

// ExtractMetadata implements [VerbHandler]. Decodes the
// churn payload sufficient to surface the parent
// (RepoID, SHA) -- SHA is empty for `external_per_row`
// because each emitted row carries its own SHA, and the
// scan_run row leaves `to_sha` NULL.
//
// Validation surface mirrors the legacy
// [churn.Payload.Validate] check at the
// repo_id-only level; full per-row validation runs inside
// [Handle] so the Router can still classify a malformed
// body as 400. The double-decode (here + in [Handle]) is a
// deliberate trade-off: ExtractMetadata MUST run BEFORE
// the durable scan_run claim, but decoding the body twice
// is preferable to leaking the parsed payload through
// the [VerbHandler] interface (which would couple the
// Router to per-verb body shapes).
func (h *ChurnVerbHandler) ExtractMetadata(ctx context.Context, body []byte) (VerbPayloadMetadata, error) {
	var payload churn.Payload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return VerbPayloadMetadata{}, fmt.Errorf("%w: %v", errChurnJSONDecode, err)
	}
	if payload.RepoID == uuid.Nil {
		return VerbPayloadMetadata{}, churn.ErrEmptyRepoID
	}
	return VerbPayloadMetadata{
		RepoID: payload.RepoID,
		// SHA intentionally empty: external_per_row leaves
		// scan_run.to_sha NULL (migration 0001 CHECK).
	}, nil
}

// Handle implements [VerbHandler]. Decodes `body` as a
// [churn.Payload] (with DisallowUnknownFields), builds a
// [metric_ingestor.ScanRunContext] stamped with
// `scanRunID`, and dispatches to the underlying Ingestor.
func (h *ChurnVerbHandler) Handle(ctx context.Context, body []byte, scanRunID uuid.UUID) (VerbHandleResult, error) {
	var payload churn.Payload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return VerbHandleResult{}, fmt.Errorf("%w: %v", errChurnJSONDecode, err)
	}

	scanRun := metric_ingestor.ScanRunContext{
		ID:     scanRunID,
		Kind:   metric_ingestor.ScanRunKindExternalPerRow,
		RepoID: payload.RepoID,
	}
	res, runErr := h.ingestor.Run(ctx, metric_ingestor.RunRequest{
		ScanRun: scanRun,
		Churn:   &payload,
	})
	if runErr != nil {
		return VerbHandleResult{}, runErr
	}

	detail, err := json.Marshal(struct {
		ChurnSamplesWritten int `json:"churn_samples_written"`
		ChurnRowsHydrated   int `json:"churn_rows_hydrated"`
	}{
		ChurnSamplesWritten: res.ChurnSamplesWritten,
		ChurnRowsHydrated:   res.ChurnRowsHydrated,
	})
	if err != nil {
		return VerbHandleResult{}, fmt.Errorf("marshalling churn detail: %w", err)
	}

	return VerbHandleResult{
		ScanRunID:            scanRunID,
		FoundationDispatched: res.FoundationDispatched,
		Detail:               json.RawMessage(detail),
	}, nil
}

// ClassifyError implements [VerbErrorClassifier]. Mirrors
// the legacy [ChurnIngestHandler] mapping so a Router-driven
// request returns the same status / code shape as a direct
// `/v1/ingest/churn` mount. The closed set:
//
//   - [churn.ErrEmptyRepoID]            -> 400 / EMPTY_REPO_ID
//   - [churn.ErrEmptyRows]              -> 400 / EMPTY_ROWS
//   - [churn.ErrEmptySHA]               -> 400 / EMPTY_SHA
//   - [churn.ErrInvalidSHA]             -> 400 / INVALID_SHA
//   - [churn.ErrEmptyFilePath]          -> 400 / EMPTY_FILE_PATH
//   - [churn.ErrZeroModifiedAt]         -> 400 / ZERO_MODIFIED_AT
//   - [churn.ErrScopeResolutionFailed]  -> 422 / SCOPE_RESOLUTION_FAILED
//   - [metric_ingestor.ErrRepoIDMismatch]-> 400 / REPO_ID_MISMATCH
//   - [metric_ingestor.ErrZeroRepoID]   -> 400 / EMPTY_REPO_ID
//   - [metric_ingestor.ErrWriterFailure]-> 500 / WRITER_FAILURE
//   - any other error                   -> (0, "") -- defer to Router default
func (h *ChurnVerbHandler) ClassifyError(err error) (int, string) {
	switch {
	case errors.Is(err, churn.ErrEmptyRepoID), errors.Is(err, metric_ingestor.ErrZeroRepoID):
		return http.StatusBadRequest, "EMPTY_REPO_ID"
	case errors.Is(err, churn.ErrEmptyRows):
		return http.StatusBadRequest, "EMPTY_ROWS"
	case errors.Is(err, churn.ErrEmptySHA):
		return http.StatusBadRequest, "EMPTY_SHA"
	case errors.Is(err, churn.ErrInvalidSHA):
		return http.StatusBadRequest, "INVALID_SHA"
	case errors.Is(err, churn.ErrEmptyFilePath):
		return http.StatusBadRequest, "EMPTY_FILE_PATH"
	case errors.Is(err, churn.ErrZeroModifiedAt):
		return http.StatusBadRequest, "ZERO_MODIFIED_AT"
	case errors.Is(err, churn.ErrScopeResolutionFailed):
		return http.StatusUnprocessableEntity, "SCOPE_RESOLUTION_FAILED"
	case errors.Is(err, metric_ingestor.ErrRepoIDMismatch):
		return http.StatusBadRequest, "REPO_ID_MISMATCH"
	case errors.Is(err, metric_ingestor.ErrWriterFailure):
		return http.StatusInternalServerError, "WRITER_FAILURE"
	default:
		// A JSON-decode failure surfaces as a wrapped
		// json.SyntaxError / json.UnmarshalTypeError; map
		// these to 400 BAD_REQUEST so a publisher with a
		// bad body gets the canonical client-error code.
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return http.StatusBadRequest, "BAD_REQUEST"
		}
		// A JSON-decoder unwrap-failure (e.g. EOF on empty
		// body, "unknown field" with DisallowUnknownFields)
		// arrives as a plain error; the wrapper sentinel
		// is exposed so the Router can recognise it without
		// regexping the message text.
		if errors.Is(err, errChurnJSONDecode) {
			return http.StatusBadRequest, "BAD_REQUEST"
		}
		return 0, ""
	}
}

// Compile-time interface assertions so a future signature
// drift surfaces at build time, not at first request.
var (
	_ VerbHandler         = (*ChurnVerbHandler)(nil)
	_ VerbErrorClassifier = (*ChurnVerbHandler)(nil)
)
