package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/test_balance"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// TestBalanceVerbHandler is the [VerbHandler] implementation
// for the `ingest.test_balance` verb (architecture Sec 1.4.2
// / Sec 6.4 lines 1367-1368 / line 1395, tech-spec Sec 4.1.1
// / Sec 4.11 lines 429-432 / Sec 8.5 lines 987-992,
// implementation-plan Stage 4.3). The handler:
//
//  1. Reads `(repo_id, sha)` from the [RepoIDHeader] +
//     [SHAHeader] request headers (architecture line 1395:
//     `ingest.test_balance(repo_id, sha, payload)` -- three
//     logical args, with the bare row-array body carrying
//     `payload`).
//  2. Parses the JSON body as a [test_balance.Payload] (a
//     bare row-array `[{scope_id, attempt_count, pass_count},
//     ...]`) with DisallowUnknownFields. The shape matches
//     `e2e-scenarios.md:648` and
//     `implementation-plan.md:396,400`. Iter-1 incorrectly
//     wrapped the rows in an envelope; iter-2 corrects to
//     the documented bare-array shape.
//  3. Returns the (repo_id, sha) tuple for the Router's
//     scan_run claim. test_balance is `external_single` so
//     both MUST be non-empty.
//  4. Stamps each emitted `metric_sample` with the
//     Router-supplied `scan_run_id` via
//     [test_balance.Writer.Run].
//
// Like [ChurnVerbHandler] the handler owns its sentinel-to-
// HTTP-code mapping through the optional
// [VerbErrorClassifier] interface.
//
// # Authenticated target metadata
//
// `(repo_id, sha)` are header-borne; the HMAC-SHA256
// signature ([HMACSignatureHeader]) covers the canonical
// signed material returned by [CanonicalRequest]:
//
//	canonical = body || 0x00 || normalised RepoID header
//	         || 0x00 || normalised SHA header
//
// where "normalised" means trimmed leading/trailing
// whitespace + lower-cased. The same canonical bytes feed
// `payload_hash`, so two requests with byte-identical
// bodies but different `(repo, sha)` targets cannot share
// a signature (HMAC-MISMATCH) and cannot collide on the
// in-process or durable idempotency claim.
//
// An attacker who captures a valid HMAC over canonical_A
// cannot retarget the request to (repo_B, sha_B) by
// swapping headers -- the recomputed canonical bytes
// differ and the HMAC verification fails. Publishers MUST
// therefore compute their HMAC over canonical bytes (use
// [BuildTestBalanceCanonicalRequest] or its open-source
// re-implementation per the runbook).
type TestBalanceVerbHandler struct {
	writer *test_balance.Writer
}

// errTestBalanceJSONDecode is the sentinel the verb wraps
// every JSON-decode failure in so [ClassifyError] can
// pattern-match without parsing the inner json.* error's
// free-form text.
var errTestBalanceJSONDecode = errors.New("webhook/test_balance: JSON decode failed")

// errTestBalanceMissingRepoIDHeader is returned when the
// caller omitted [RepoIDHeader].
var errTestBalanceMissingRepoIDHeader = errors.New("webhook/test_balance: " + RepoIDHeader + " header is missing or empty")

// errTestBalanceMissingSHAHeader is returned when the
// caller omitted [SHAHeader].
var errTestBalanceMissingSHAHeader = errors.New("webhook/test_balance: " + SHAHeader + " header is missing or empty")

// errTestBalanceInvalidRepoIDHeader is returned when
// [RepoIDHeader] is non-empty but does not parse as a UUID.
var errTestBalanceInvalidRepoIDHeader = errors.New("webhook/test_balance: " + RepoIDHeader + " header is not a valid UUID")

// errTestBalanceInvalidSHAHeader is returned when [SHAHeader]
// is non-empty but does not match the canonical 40-char
// hex commit SHA shape.
var errTestBalanceInvalidSHAHeader = errors.New("webhook/test_balance: " + SHAHeader + " header is not a 40-character hex commit SHA")

// testBalanceSHARegex is the strict canonical pattern for a
// commit SHA: exactly 40 hexadecimal characters. Matches the
// shape the migration-0001 CHECK constraint enforces on
// `scan_run.to_sha`.
var testBalanceSHARegex = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// NewTestBalanceVerbHandler constructs a
// [TestBalanceVerbHandler] bound to `w`. PANICS on a nil
// writer -- a verb handler without a writer cannot service
// any request and the composition-root misconfig should
// fail loudly at startup.
func NewTestBalanceVerbHandler(w *test_balance.Writer) *TestBalanceVerbHandler {
	if w == nil {
		panic("webhook: NewTestBalanceVerbHandler received nil test_balance.Writer")
	}
	return &TestBalanceVerbHandler{writer: w}
}

// Verb implements [VerbHandler].
func (h *TestBalanceVerbHandler) Verb() string { return "test_balance" }

// ContentType implements [VerbHandler]. `ingest.test_balance`
// is pinned to `application/json` per tech-spec Sec 8.5 row 2.
// The verb does NOT accept JUnit-XML bodies directly -- the
// publisher is responsible for pre-aggregating into the
// `{scope_id, attempt_count, pass_count}` shape. Posting an
// XML body to this verb surfaces as `415 Unsupported Media
// Type` from the Router's per-verb content-type check.
func (h *TestBalanceVerbHandler) ContentType() string { return "application/json" }

// ScanRunKind implements [VerbHandler]. `ingest.test_balance`
// is `external_single` per e2e-scenarios.md line 686 and
// tech-spec Sec 4.11.
func (h *TestBalanceVerbHandler) ScanRunKind() string {
	return metric_ingestor.ScanRunKindExternalSingle
}

// SHABinding implements [VerbHandler]. `external_single`
// pins `scan_run.to_sha` to the ONE SHA the caller supplied
// in the `X-Forge-SHA` header; migration 0001's
// scan_run_sha_binding_consistent CHECK enforces the binding-
// to-to_sha invariant downstream.
func (h *TestBalanceVerbHandler) SHABinding() string {
	return metric_ingestor.SHABindingSingle
}

// CanonicalRequest implements [VerbHandler]. Builds the
// canonical signed material the Router uses for BOTH HMAC
// verification AND `payload_hash` computation:
//
//	canonical = body || 0x00 || normaliseHeader(RepoIDHeader)
//	         || 0x00 || normaliseHeader(SHAHeader)
//
// The NUL byte (0x00) is not a valid character in any
// canonical UUID string OR 40-hex SHA, so the framing
// between (body, repo, sha) is unambiguous regardless of
// body content.
//
// "normaliseHeader" strips leading/trailing whitespace and
// lower-cases the result. The SAME normalisation is applied
// inside [ExtractMetadata] so the bytes [CanonicalRequest]
// builds match the value [ExtractMetadata] returns.
// Whitespace / case variants of the same logical target
// therefore (a) sign to the same canonical bytes, (b) hash
// to the same `payload_hash`, and (c) cannot duplicate-row
// the durable scan_run table.
//
// MUST NOT validate -- callable BEFORE HMAC verification.
// Missing or malformed headers produce stable canonical
// bytes with empty values; HMAC verification downstream
// rejects unauthenticated requests so a probe of the
// canonical form cannot extract verb taxonomy.
func (h *TestBalanceVerbHandler) CanonicalRequest(headers http.Header, body []byte) []byte {
	return BuildTestBalanceCanonicalRequest(headers, body)
}

// BuildTestBalanceCanonicalRequest is the package-level
// helper that builds the canonical bytes -- exported so
// publishers and tests can compute the SAME signed-material
// the Router will verify against. The runbook references
// this function as the v1 publisher recipe.
//
// Invariant: the bytes returned here MUST equal the bytes
// the [TestBalanceVerbHandler.CanonicalRequest] method
// returns for the same (headers, body) input.
func BuildTestBalanceCanonicalRequest(headers http.Header, body []byte) []byte {
	repo := normaliseTestBalanceHeader(headers.Get(RepoIDHeader))
	sha := normaliseTestBalanceHeader(headers.Get(SHAHeader))
	var buf bytes.Buffer
	buf.Grow(len(body) + 2 + len(repo) + len(sha))
	buf.Write(body)
	buf.WriteByte(0)
	buf.WriteString(repo)
	buf.WriteByte(0)
	buf.WriteString(sha)
	return buf.Bytes()
}

// normaliseTestBalanceHeader is the single source of truth
// for the canonical form of a test_balance target-metadata
// header value. Used by BOTH [BuildTestBalanceCanonicalRequest]
// AND [TestBalanceVerbHandler.ExtractMetadata] so case /
// whitespace variants of the same logical target produce
// the same signed bytes AND the same persisted (RepoID, SHA).
func normaliseTestBalanceHeader(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// ExtractMetadata implements [VerbHandler]. Reads
// [RepoIDHeader] and [SHAHeader] from `headers`, validates
// each, and returns the resolved [VerbPayloadMetadata]. The
// body is NOT consulted -- test_balance bodies are bare row
// arrays with no envelope, so the Router-supplied
// [RepoIDHeader] / [SHAHeader] are the authoritative source
// for the (repo, sha) target. Both are REQUIRED for
// `external_single`:
//
//   - missing RepoID -> 400 / EMPTY_REPO_ID
//   - missing SHA -> 400 / EMPTY_SHA
//   - malformed RepoID UUID -> 400 / INVALID_REPO_ID
//   - non-40-hex SHA -> 400 / INVALID_SHA
//
// The Router invokes [ExtractMetadata] AFTER HMAC has
// already authenticated the canonical signed material so
// any of these classification codes ONLY surfaces to a
// caller with a valid signing key.
//
// Header values are normalised via the SAME helper that
// builds [CanonicalRequest] bytes -- whitespace and case
// variants of the same logical (repo, sha) produce the
// same persisted scan_run row AND the same payload_hash.
func (h *TestBalanceVerbHandler) ExtractMetadata(_ context.Context, headers http.Header, _ []byte) (VerbPayloadMetadata, error) {
	normRepoID := normaliseTestBalanceHeader(headers.Get(RepoIDHeader))
	if normRepoID == "" {
		return VerbPayloadMetadata{}, errTestBalanceMissingRepoIDHeader
	}
	repoID, err := uuid.FromString(normRepoID)
	if err != nil {
		return VerbPayloadMetadata{}, fmt.Errorf("%w: %v", errTestBalanceInvalidRepoIDHeader, err)
	}
	if repoID == uuid.Nil {
		return VerbPayloadMetadata{}, errTestBalanceMissingRepoIDHeader
	}
	normSHA := normaliseTestBalanceHeader(headers.Get(SHAHeader))
	if normSHA == "" {
		return VerbPayloadMetadata{}, errTestBalanceMissingSHAHeader
	}
	if !testBalanceSHARegex.MatchString(normSHA) {
		return VerbPayloadMetadata{}, fmt.Errorf("%w (got %q)", errTestBalanceInvalidSHAHeader, normSHA)
	}
	return VerbPayloadMetadata{
		RepoID: repoID,
		SHA:    normSHA,
	}, nil
}

// Handle implements [VerbHandler]. Decodes `body` as a
// [test_balance.Payload] (a bare JSON row array, with
// DisallowUnknownFields), builds a
// [metric_ingestor.ScanRunContext] stamped with `scanRunID`
// and the Router-supplied `(metadata.RepoID, metadata.SHA)`,
// and dispatches to the underlying [test_balance.Writer].
//
// `metadata` is the (RepoID, SHA) tuple
// [ExtractMetadata] returned for THIS request; the handler
// does NOT re-read headers from the request. The body bytes
// are the bare row array -- the (repo, sha) live entirely
// in metadata.
//
// The handler does NOT dispatch any foundation-tier recipe:
// `external_single` is store-only at the test_balance verb
// (tech-spec Sec 4.11). [VerbHandleResult.FoundationDispatched]
// is therefore always `false`.
func (h *TestBalanceVerbHandler) Handle(ctx context.Context, metadata VerbPayloadMetadata, body []byte, scanRunID uuid.UUID) (VerbHandleResult, error) {
	var rows test_balance.Payload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rows); err != nil {
		return VerbHandleResult{}, fmt.Errorf("%w: %v", errTestBalanceJSONDecode, err)
	}

	scanRun := metric_ingestor.ScanRunContext{
		ID:     scanRunID,
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: metadata.RepoID,
		SHA:    metadata.SHA,
	}
	res, runErr := h.writer.Run(ctx, scanRun, rows)
	if runErr != nil {
		return VerbHandleResult{}, runErr
	}

	detail, err := json.Marshal(struct {
		TestBalanceSamplesWritten int `json:"test_balance_samples_written"`
		TestBalanceRowsSkipped    int `json:"test_balance_rows_skipped"`
	}{
		TestBalanceSamplesWritten: res.SamplesWritten,
		TestBalanceRowsSkipped:    res.RowsSkipped,
	})
	if err != nil {
		return VerbHandleResult{}, fmt.Errorf("marshalling test_balance detail: %w", err)
	}

	return VerbHandleResult{
		ScanRunID:            scanRunID,
		FoundationDispatched: false,
		Detail:               json.RawMessage(detail),
	}, nil
}

// ClassifyError implements [VerbErrorClassifier]. Mirrors the
// per-sentinel mapping the [ChurnVerbHandler] uses so the
// `/v1/ingest/test_balance` response shape is uniform with
// the other verbs. The closed set:
//
//   - missing/empty RepoID header                  -> 400 / EMPTY_REPO_ID
//   - missing/empty SHA header                     -> 400 / EMPTY_SHA
//   - malformed RepoID header                      -> 400 / INVALID_REPO_ID
//   - malformed SHA header                         -> 400 / INVALID_SHA
//   - [test_balance.ErrEmptyRows]                  -> 400 / EMPTY_ROWS
//   - [test_balance.ErrEmptyScopeID]               -> 400 / EMPTY_SCOPE_ID
//   - [test_balance.ErrNegativeAttemptCount]       -> 400 / NEGATIVE_ATTEMPT_COUNT
//   - [test_balance.ErrNegativePassCount]          -> 400 / NEGATIVE_PASS_COUNT
//   - [test_balance.ErrScopeResolutionFailed]      -> 500 / SCOPE_RESOLUTION_FAILED
//   - [metric_ingestor.ErrZeroRepoID]              -> 400 / EMPTY_REPO_ID
//   - [metric_ingestor.ErrInvalidScanRunKind]      -> 500 / INVALID_SCAN_RUN_KIND
//   - [metric_ingestor.ErrWriterFailure]           -> 500 / WRITER_FAILURE
//   - wrapped [json.SyntaxError] / [json.UnmarshalTypeError] /
//     [errTestBalanceJSONDecode]                   -> 400 / BAD_REQUEST
//   - any other error                              -> (0, "") (Router default 500)
func (h *TestBalanceVerbHandler) ClassifyError(err error) (int, string) {
	switch {
	case errors.Is(err, errTestBalanceMissingRepoIDHeader), errors.Is(err, metric_ingestor.ErrZeroRepoID):
		return http.StatusBadRequest, "EMPTY_REPO_ID"
	case errors.Is(err, errTestBalanceMissingSHAHeader):
		return http.StatusBadRequest, "EMPTY_SHA"
	case errors.Is(err, errTestBalanceInvalidRepoIDHeader):
		return http.StatusBadRequest, "INVALID_REPO_ID"
	case errors.Is(err, errTestBalanceInvalidSHAHeader):
		return http.StatusBadRequest, "INVALID_SHA"
	case errors.Is(err, test_balance.ErrEmptyRows):
		return http.StatusBadRequest, "EMPTY_ROWS"
	case errors.Is(err, test_balance.ErrEmptyScopeID):
		return http.StatusBadRequest, "EMPTY_SCOPE_ID"
	case errors.Is(err, test_balance.ErrNegativeAttemptCount):
		return http.StatusBadRequest, "NEGATIVE_ATTEMPT_COUNT"
	case errors.Is(err, test_balance.ErrNegativePassCount):
		return http.StatusBadRequest, "NEGATIVE_PASS_COUNT"
	case errors.Is(err, test_balance.ErrScopeResolutionFailed):
		return http.StatusInternalServerError, "SCOPE_RESOLUTION_FAILED"
	case errors.Is(err, metric_ingestor.ErrInvalidScanRunKind):
		return http.StatusInternalServerError, "INVALID_SCAN_RUN_KIND"
	case errors.Is(err, metric_ingestor.ErrWriterFailure):
		return http.StatusInternalServerError, "WRITER_FAILURE"
	default:
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return http.StatusBadRequest, "BAD_REQUEST"
		}
		if errors.Is(err, errTestBalanceJSONDecode) {
			return http.StatusBadRequest, "BAD_REQUEST"
		}
		return 0, ""
	}
}

// Compile-time interface assertions so a future signature
// drift surfaces at build time, not at first request.
var (
	_ VerbHandler         = (*TestBalanceVerbHandler)(nil)
	_ VerbErrorClassifier = (*TestBalanceVerbHandler)(nil)
)
