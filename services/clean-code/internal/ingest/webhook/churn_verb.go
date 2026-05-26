package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
)

// ChurnVerbHandler is the [VerbHandler] implementation for
// the `ingest.churn` verb (architecture Sec 6.4 / tech-spec
// Sec 8.5 row 3 / implementation-plan Stage 4.4 lines 410-425).
//
// # Stage 4.4 rewire (this iter)
//
// The handler now depends on a [ChurnIngester] -- the small
// interface satisfied by [churn.Ingester] from the
// `internal/ingest/churn` package -- INSTEAD of the legacy
// `*metric_ingestor.Ingestor` chain. The contract pinned by
// the brief is "the verb writes ZERO `metric_sample` rows
// directly; it ONLY feeds the `modification_count_in_window`
// materialiser via the `clean_code.churn_event` staging
// table" (Stage 4.4 lines 410-425, e2e-scenarios.md lines
// 658-664). [ChurnIngester] depends on [churn.ChurnEventWriter]
// only -- it has NO `metric_sample` writer in its type
// signature, so the import graph of THIS handler proves the
// contract at the type level: `churn_verb.go` does not import
// `metric_ingestor` after the rewire.
//
// # Why a separate type from ChurnIngestHandler
//
// [ChurnIngestHandler] is the legacy direct mount at the
// fixed path `/v1/ingest/churn` (iter 5 structural fix --
// see handler.go doc-comment) and continues to use the
// inline metric_sample path for now. The Router introduced
// by Stage 4.1 sits at `/v1/ingest/{verb}` and is the path
// the production composition root mounts via
// [mountIngestRouter] in `cmd/clean-code-metric-ingestor/main.go`.
// This Router-facing type is the seam through which Stage 4.4
// swaps the inline path for the staging path WITHOUT
// disturbing the legacy mount.
type ChurnVerbHandler struct {
	ingester ChurnIngester
	now      func() time.Time
}

// ChurnIngester is the minimal interface [ChurnVerbHandler]
// depends on -- satisfied by [churn.Ingester]. Defined as a
// package-local interface (rather than reaching into the
// `churn` package's concrete type) so a future test fake or
// alternate implementation can swap in without changing the
// handler.
//
// The interface surface intentionally mirrors
// [churn.Ingester.Ingest] verbatim: one method, value-type
// handle, pointer payload, structured result.
type ChurnIngester interface {
	Ingest(ctx context.Context, handle churn.ScanRunHandle, payload *churn.Payload) (churn.IngestResult, error)
}

// errChurnJSONDecode is the sentinel the verb wraps every
// JSON-decode failure in so [ClassifyError] can pattern-
// match without parsing the inner json.* error's free-form
// text. NOT exported: callers consume the classified status
// code, not this sentinel directly.
var errChurnJSONDecode = errors.New("webhook/churn: JSON decode failed")

// NewChurnVerbHandler constructs a [ChurnVerbHandler] bound
// to `ingester`. PANICS on a nil ingester -- a verb handler
// without a writer cannot service any request and the
// composition-root misconfig should fail loudly at startup.
//
// The clock defaults to [time.Now]. Tests that need a
// deterministic [churn.ScanRunHandle.OpenedAt] use
// [NewChurnVerbHandlerWithClock].
func NewChurnVerbHandler(ingester ChurnIngester) *ChurnVerbHandler {
	if ingester == nil {
		panic("webhook: NewChurnVerbHandler received nil ChurnIngester")
	}
	return &ChurnVerbHandler{ingester: ingester, now: time.Now}
}

// NewChurnVerbHandlerWithClock is the test-friendly
// constructor. PANICS on any nil argument.
func NewChurnVerbHandlerWithClock(ingester ChurnIngester, now func() time.Time) *ChurnVerbHandler {
	if ingester == nil {
		panic("webhook: NewChurnVerbHandlerWithClock received nil ChurnIngester")
	}
	if now == nil {
		panic("webhook: NewChurnVerbHandlerWithClock received nil now()")
	}
	return &ChurnVerbHandler{ingester: ingester, now: now}
}

// Verb implements [VerbHandler].
func (h *ChurnVerbHandler) Verb() string { return churn.Verb }

// ContentType implements [VerbHandler]. `ingest.churn` is
// pinned to `application/json` per tech-spec Sec 8.5 row 3.
func (h *ChurnVerbHandler) ContentType() string { return "application/json" }

// ScanRunKind implements [VerbHandler]. `ingest.churn` is
// `external_per_row` per e2e-scenarios.md line 687 and
// tech-spec Sec 4.11. Sourced from the canonical
// [churn.Kind] constant so a refactor that rebrands the
// scan_run kind enum surfaces here as a build break.
func (h *ChurnVerbHandler) ScanRunKind() string {
	return churn.Kind
}

// SHABinding implements [VerbHandler]. `external_per_row`
// leaves `scan_run.to_sha` NULL because each emitted
// churn_event carries its own SHA. Migration 0001's
// scan_run_sha_binding_consistent CHECK enforces this.
func (h *ChurnVerbHandler) SHABinding() string {
	return churn.SHABinding
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
// [churn.ScanRunHandle] stamped with the Router-supplied
// `scanRunID` plus the canonical verb / kind / sha-binding
// constants from the `churn` package, and dispatches to the
// staging [ChurnIngester] (NOT the legacy `metric_sample`
// path -- Stage 4.4 wires this handler to write into the
// `clean_code.churn_event` staging table; the
// `modification_count_in_window` materialiser is the SOLE
// writer of that metric_kind on a later pass).
//
// On success returns a [VerbHandleResult] with
// `FoundationDispatched=false` (external_per_row never
// dispatches foundation per tech-spec Sec 4.11) and a detail
// envelope shape `{churn_events_written, scan_run_id}`
// recording the staged-row count for operator audit.
func (h *ChurnVerbHandler) Handle(ctx context.Context, body []byte, scanRunID uuid.UUID) (VerbHandleResult, error) {
	var payload churn.Payload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return VerbHandleResult{}, fmt.Errorf("%w: %v", errChurnJSONDecode, err)
	}

	// Build the pre-opened scan_run handle the Ingester
	// validates against the canonical verb/kind matrix.
	// OpenedAt is stamped from the handler's clock for audit
	// only; the Ingester stamps a fresh clock reading for
	// every churn_event.created_at separately.
	handle := churn.ScanRunHandle{
		ScanRunID:  scanRunID,
		RepoID:     payload.RepoID,
		Verb:       churn.Verb,
		Kind:       churn.Kind,
		SHABinding: churn.SHABinding,
		ToSHA:      "",
		OpenedAt:   h.now(),
	}

	res, runErr := h.ingester.Ingest(ctx, handle, &payload)
	if runErr != nil {
		return VerbHandleResult{}, runErr
	}

	detail, err := json.Marshal(struct {
		ChurnEventsWritten int       `json:"churn_events_written"`
		ScanRunID          uuid.UUID `json:"scan_run_id"`
	}{
		ChurnEventsWritten: res.EventsWritten,
		ScanRunID:          res.ScanRunID,
	})
	if err != nil {
		return VerbHandleResult{}, fmt.Errorf("marshalling churn detail: %w", err)
	}

	return VerbHandleResult{
		ScanRunID: scanRunID,
		// FoundationDispatched is always false for
		// external_per_row -- the Stage 4.4 staging path
		// has no foundation hook at all (the materialiser
		// runs on a separate pass).
		FoundationDispatched: false,
		Detail:               json.RawMessage(detail),
	}, nil
}

// ClassifyError implements [VerbErrorClassifier]. The closed
// set maps every sentinel the [churn.Ingester] surfaces to a
// canonical (status, code) pair so the Router emits the same
// shapes the legacy direct mount returns (a publisher that
// migrates from `/v1/ingest/churn` to `/v1/ingest/{verb}/churn`
// sees the same client-error vocabulary).
//
//   - [churn.ErrEmptyRepoID]              -> 400 / EMPTY_REPO_ID
//   - [churn.ErrEmptyRows]                -> 400 / EMPTY_ROWS
//   - [churn.ErrEmptySHA]                 -> 400 / EMPTY_SHA
//   - [churn.ErrInvalidSHA]               -> 400 / INVALID_SHA
//   - [churn.ErrEmptyFilePath]            -> 400 / EMPTY_FILE_PATH
//   - [churn.ErrZeroModifiedAt]           -> 400 / ZERO_MODIFIED_AT
//   - [churn.ErrScopeResolutionFailed]    -> 422 / SCOPE_RESOLUTION_FAILED
//   - [churn.ErrRepoIDMismatch]           -> 400 / REPO_ID_MISMATCH
//   - [churn.ErrChurnEventWriteFailed]    -> 500 / WRITER_FAILURE
//   - JSON decode failure                 -> 400 / BAD_REQUEST
//   - any other error                     -> (0, "") -- defer to Router default
func (h *ChurnVerbHandler) ClassifyError(err error) (int, string) {
	switch {
	case errors.Is(err, churn.ErrEmptyRepoID):
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
	case errors.Is(err, churn.ErrRepoIDMismatch):
		return http.StatusBadRequest, "REPO_ID_MISMATCH"
	case errors.Is(err, churn.ErrChurnEventWriteFailed):
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
	// The production [churn.Ingester] MUST satisfy our
	// minimal [ChurnIngester] interface. A drift surfaces
	// here, not at composition-root wiring time.
	_ ChurnIngester = (*churn.Ingester)(nil)
)

