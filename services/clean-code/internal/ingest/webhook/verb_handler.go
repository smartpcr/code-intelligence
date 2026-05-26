package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
)

// VerbHandler is the seam between the Router and a per-verb
// ingest path. Each of the four `ingest.*` verbs named in
// tech-spec Sec 8.5 -- `coverage`, `test_balance`, `churn`,
// `defects` -- registers one implementation.
//
// Stage 4.1 ships ONE implementation: [ChurnVerbHandler]. The
// remaining three verbs land in Stages 4.2-4.5 and plug into
// the same interface; the Router does NOT change to add a
// verb.
//
// # Contract
//
//   - [Verb] is the URL path segment the Router routes by
//     (`/v1/ingest/{Verb}`). MUST be a single lowercase
//     identifier from the closed set `coverage |
//     test_balance | churn | defects`. The Router refuses to
//     register a verb whose token contains anything other
//     than `[a-z_]` so a misconfigured registration is loud
//     at startup, not silent at runtime.
//
//   - [ContentType] is the media type the verb accepts (e.g.
//     `application/json` or `application/xml`). The Router
//     parses the inbound `Content-Type` header through
//     [mime.ParseMediaType] and matches against this value
//     case-insensitively (RFC 7231 §3.1.1.1). Parameters
//     like `charset=utf-8` are accepted; the verb sees only
//     the media-type-name match.
//
//   - [ScanRunKind] is the canonical `scan_run.kind` literal
//     the verb's ScanRun row carries. One of
//     `external_single` (coverage / test_balance) or
//     `external_per_row` (churn / defects). Pinned at
//     registration time so the Router can persist the kind
//     uniformly without inspecting the handler.
//
//   - [SHABinding] is the canonical `scan_run.sha_binding`
//     literal -- `single` for `external_single` kinds (where
//     the body carries one SHA per scan) and `per_row` for
//     `external_per_row` (where the body carries one SHA
//     per emitted row). The Router uses it to build the
//     [ScanRunRepositoryRequest] that opens the durable
//     scan_run row; the migration 0001
//     scan_run_sha_binding_consistent CHECK enforces the
//     binding-to-to_sha invariant downstream.
//
//   - [ExtractMetadata] is called by the Router AFTER HMAC,
//     verb, and content-type validation and BEFORE the
//     idempotency claim. It parses the validated body and
//     returns the (RepoID, SHA) tuple the Router needs to
//     INSERT a durable scan_run row with `payload_hash` set
//     (Stage 4.1 evaluator iter-1 #2). Returning an error
//     short-circuits the Router into a structured 400/422
//     response (mapped via [VerbErrorClassifier]); the
//     idempotency claim and verb dispatch are NOT attempted.
//
//   - [Handle] materialises the validated body. The Router
//     mints a fresh `scan_run_id` (returned by the
//     ScanRunRepository.OpenExternal claim) and passes it in
//     -- the handler MUST honour the id when persisting
//     downstream records so the active-row uniqueness
//     invariant (architecture Sec 5.2.1, tech-spec C1) holds.
//
// # Result envelope
//
// The handler returns a [VerbHandleResult] containing
// (a) the `scan_run_id` it persisted (must equal the
// Router-supplied id), (b) zero or more counter fields the
// Router exposes in the 200 response body, and (c) an
// opaque `Detail` JSON byte-slice the Router stamps into the
// response envelope so verb-specific metrics (e.g.
// `churn_samples_written`) survive the round trip without
// the Router knowing per-verb shapes.
type VerbHandler interface {
	// Verb returns the URL path segment.
	Verb() string

	// ContentType returns the media type the verb accepts.
	ContentType() string

	// ScanRunKind returns the canonical `scan_run.kind`.
	ScanRunKind() string

	// SHABinding returns the canonical
	// `scan_run.sha_binding` (one of `single` | `per_row`).
	// MUST be consistent with ScanRunKind (the migration
	// 0001 scan_run_sha_binding_consistent CHECK enforces).
	SHABinding() string

	// ExtractMetadata parses `body` and returns the
	// [VerbPayloadMetadata] tuple the Router needs to open
	// a durable scan_run row BEFORE dispatching Handle.
	// MUST validate body shape sufficient to extract the
	// metadata; deeper writer-side validation is the
	// responsibility of [Handle]. Returns the verb's
	// sentinel error on a missing / malformed metadata
	// field so the Router's [VerbErrorClassifier] mapping
	// produces the canonical 400 / 422.
	ExtractMetadata(ctx context.Context, body []byte) (VerbPayloadMetadata, error)

	// Handle materialises `body` under the supplied
	// `scanRunID`. Returns the per-verb counter envelope on
	// success; any error short-circuits the Router into a
	// structured response (status / code resolved via
	// [VerbErrorClassifier] when implemented, else generic
	// 500 / `INTERNAL_ERROR`).
	Handle(ctx context.Context, body []byte, scanRunID uuid.UUID) (VerbHandleResult, error)
}

// VerbPayloadMetadata is the tuple [VerbHandler.ExtractMetadata]
// returns -- the parts of the validated body the Router needs
// to INSERT a durable scan_run row. The Router does NOT
// inspect anything else from the body; per-verb writers
// re-parse it in [VerbHandler.Handle].
type VerbPayloadMetadata struct {
	// RepoID is the parent repo the scan_run row points
	// at (scan_run.repo_id FK). MUST be non-zero.
	RepoID uuid.UUID

	// SHA is the commit SHA for `external_single` verbs
	// (where the run targets one SHA). Empty for
	// `external_per_row` verbs (where each emitted row
	// carries its own SHA on the metric_sample row;
	// scan_run.to_sha is NULL).
	SHA string
}

// VerbErrorClassifier is an OPTIONAL interface a VerbHandler
// may implement to map its sentinel errors to HTTP status
// codes and structured error-code strings the Router emits
// to the caller. A handler that does NOT implement it
// surfaces every Handle error as `500 / INTERNAL_ERROR`.
//
// Each VerbHandler owns its own sentinel-to-code mapping
// (e.g. [ChurnVerbHandler] maps `churn.ErrInvalidSHA` to
// `400 / INVALID_SHA`); the Router never has to know per-
// verb error shapes.
type VerbErrorClassifier interface {
	// ClassifyError returns (status, code) for `err`. A
	// non-zero status MUST be a valid HTTP status code; a
	// non-empty code SHOULD be one of the runbook-named
	// canonical strings. Returning (0, "") means "use the
	// Router default 500 / INTERNAL_ERROR".
	ClassifyError(err error) (status int, code string)
}

// VerbHandleResult is the return shape of [VerbHandler.Handle].
// The Router serialises this into a [RouterResponse] envelope
// for the 200 response and stores the marshalled bytes in the
// idempotency record so future replays return the same body.
type VerbHandleResult struct {
	// ScanRunID is the `scan_run_id` the handler persisted.
	// MUST equal the `scanRunID` argument supplied to
	// [VerbHandler.Handle]; the Router asserts equality and
	// surfaces a 500 mismatch (defensive against a buggy
	// handler that mints its own id).
	ScanRunID uuid.UUID

	// FoundationDispatched is true iff the verb's underlying
	// pipeline dispatched any foundation-tier recipe runs.
	// For Stage 4.1 the churn verb's value is always false
	// (external_per_row never dispatches foundation per
	// tech-spec Sec 4.11); the field is here for shape
	// uniformity with later verbs that may.
	FoundationDispatched bool

	// Detail is the verb-specific JSON object (e.g.
	// `{"churn_samples_written":2,"churn_rows_hydrated":2}`)
	// the Router lifts into the response envelope's
	// `detail` field. MAY be nil for verbs with no extra
	// fields (e.g. defects/v1).
	Detail json.RawMessage
}

// ValidateVerbToken returns nil iff `verb` is a syntactically
// valid path segment for `/v1/ingest/{verb}`. Used by the
// Router at registration time AND at request-handling time
// to fail loudly on path inputs the operator did not
// intentionally register.
//
// Rules (closed set):
//
//   - Length 1-64 bytes.
//   - Every byte in `[a-z]` or `_`.
//
// The closed set is intentionally strict so a future
// "ingest.coverageV2" rename has to deliberately ship a new
// verb registration rather than land via an inadvertent
// path-template typo.
func ValidateVerbToken(verb string) error {
	if verb == "" {
		return errors.New("webhook: verb token is empty")
	}
	if len(verb) > 64 {
		return fmt.Errorf("webhook: verb token %q exceeds 64-byte limit", verb)
	}
	for i := 0; i < len(verb); i++ {
		c := verb[i]
		if (c < 'a' || c > 'z') && c != '_' {
			return fmt.Errorf("webhook: verb token %q contains illegal byte at offset %d (allowed: a-z, _)", verb, i)
		}
	}
	return nil
}

// canonicalScanRunKindForVerb returns the canonical
// `scan_run.kind` the per-verb registration MUST quote, for
// the closed set of four verbs named in tech-spec Sec 8.5
// and e2e-scenarios.md lines 684-688. Used by the Router at
// Register time to assert a registration's [VerbHandler.ScanRunKind]
// agrees with the spec pin; a mismatched registration is a
// composition-root bug that should fail loudly at startup.
//
// Returns ("", false) for verbs not in the closed set; the
// caller treats that as "no static pin" (a future verb that
// did not exist when this map was written).
func canonicalScanRunKindForVerb(verb string) (string, bool) {
	switch verb {
	case "coverage":
		return "external_single", true
	case "test_balance":
		return "external_single", true
	case "churn":
		return "external_per_row", true
	case "defects":
		return "external_per_row", true
	default:
		return "", false
	}
}

// canonicalSHABindingForKind returns the canonical
// `scan_run.sha_binding` literal for a given scan_run.kind.
// Used by the Router at Register time to assert a verb's
// [VerbHandler.SHABinding] is consistent with its
// ScanRunKind. Returns ("", false) for kinds outside the
// external-ingest set.
func canonicalSHABindingForKind(kind string) (string, bool) {
	switch kind {
	case "external_single":
		return "single", true
	case "external_per_row":
		return "per_row", true
	default:
		return "", false
	}
}

