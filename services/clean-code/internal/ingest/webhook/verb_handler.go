package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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
//     payload_hash + idempotency claim. It receives the
//     validated request headers (so verbs that carry
//     `(repo_id, sha)` as HTTP headers -- e.g. test_balance,
//     architecture Sec 6.4 line 1395 `ingest.test_balance(repo_id,
//     sha, payload)`) can resolve the (RepoID, SHA) tuple
//     without forcing those fields into the body. Returning
//     an error short-circuits the Router into a structured
//     400/422 response (mapped via [VerbErrorClassifier]); the
//     idempotency claim and verb dispatch are NOT attempted.
//     ExtractMetadata is the layer that validates UUID +
//     SHA shapes and raises 400 sentinels.
//
//   - [CanonicalRequest] is called by the Router BEFORE
//     HMAC verification and BEFORE [ExtractMetadata]. It MUST
//     NOT validate metadata (validation lives in
//     [ExtractMetadata], which runs AFTER auth). Each verb
//     declares the canonical byte sequence the publisher
//     signs and that the Router hashes for idempotency:
//       * body-borne verbs (churn, defects): return `body`
//         verbatim -- the publisher's HMAC over body is
//         the canonical signature.
//       * header-borne verbs (test_balance, coverage):
//         return `body || 0x00 || normalised RepoID
//         header || 0x00 || normalised SHA header`. The
//         publisher MUST sign this canonical form so
//         attempts to retarget the body to a different
//         (repo, sha) by swapping headers fail HMAC.
//     The Router uses the SAME bytes for `payload_hash`
//     so two POSTs with identical bodies but different
//     header targets do NOT collide on the idempotency
//     claim. CanonicalRequest is intentionally lenient
//     (no error return): it MUST emit bytes for any input
//     (including malformed/missing headers) so that
//     pre-auth probes cannot learn header taxonomy
//     without a valid HMAC.
//
//   - [Handle] materialises the validated body under the
//     metadata [ExtractMetadata] returned. The Router mints a
//     fresh `scan_run_id` (returned by the
//     ScanRunRepository.OpenExternal claim) and passes it in
//     -- the handler MUST honour the id when persisting
//     downstream records so the active-row uniqueness
//     invariant (architecture Sec 5.2.1, tech-spec C1) holds.
//     Passing `metadata` explicitly removes the double-decode
//     trap iter-1 fell into (Handle had to re-parse the body
//     just to find the RepoID); Handle now sees the resolved
//     metadata directly.
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

	// ExtractMetadata parses `headers` and/or `body` and
	// returns the [VerbPayloadMetadata] tuple the Router
	// needs to open a durable scan_run row BEFORE
	// dispatching Handle. Verbs whose body carries
	// `(repo_id, sha)` (e.g. churn, defects) consult `body`;
	// verbs whose body is a bare row-array (e.g.
	// test_balance, coverage) consult `headers` for
	// [RepoIDHeader] and [SHAHeader]. MUST validate the
	// metadata sufficient for the Router to open the
	// scan_run row; deeper writer-side validation is the
	// responsibility of [Handle]. Returns the verb's
	// sentinel error on a missing / malformed metadata
	// field so the Router's [VerbErrorClassifier] mapping
	// produces the canonical 400 / 422.
	ExtractMetadata(ctx context.Context, headers http.Header, body []byte) (VerbPayloadMetadata, error)

	// CanonicalRequest returns the canonical byte sequence
	// the publisher HMAC-signs and the Router hashes for
	// `payload_hash`. Body-borne verbs return `body`
	// verbatim; header-borne verbs MUST fold the target
	// metadata (e.g. [RepoIDHeader] + [SHAHeader]) into the
	// returned bytes using a stable canonicalisation
	// (normalised case + trimmed whitespace) so the same
	// logical target hashes to the same value.
	//
	// MUST NOT validate the headers or body (validation
	// is owned by [ExtractMetadata] which runs AFTER auth).
	// MUST NOT return an error: the Router calls
	// CanonicalRequest BEFORE HMAC verification, so any
	// pre-auth error path here would leak verb taxonomy to
	// unauthenticated probes. Missing / malformed headers
	// must produce stable canonical bytes (typically with
	// empty header values) and fail HMAC verification
	// downstream.
	CanonicalRequest(headers http.Header, body []byte) []byte

	// Handle materialises `body` under the supplied
	// `metadata` (the [VerbPayloadMetadata] tuple
	// [ExtractMetadata] already validated) and the
	// Router-minted `scanRunID`. Passing metadata into
	// Handle removes the iter-1 double-decode trap: Handle
	// no longer has to re-parse the body to find the
	// RepoID/SHA. Returns the per-verb counter envelope on
	// success; any error short-circuits the Router into a
	// structured response (status / code resolved via
	// [VerbErrorClassifier] when implemented, else generic
	// 500 / `INTERNAL_ERROR`).
	Handle(ctx context.Context, metadata VerbPayloadMetadata, body []byte, scanRunID uuid.UUID) (VerbHandleResult, error)
}

// RepoIDHeader is the canonical HTTP header carrying the
// `clean_code.repo.repo_id` (UUID) the request targets.
// Used by verbs whose body shape is a bare row-array (no
// envelope) -- the architecture-level call signatures
// (e.g. `ingest.test_balance(repo_id, sha, payload)` at
// architecture Sec 6.4 line 1395) require both repo_id and
// sha alongside the payload; the v1 transport surfaces
// them as request headers so the body can match the
// `e2e-scenarios.md` row-array shape verbatim.
//
// # Why a header (vs a body envelope)
//
// The docs (`e2e-scenarios.md:648`, `implementation-plan.md:396,400`)
// specify the test_balance body as the bare JSON array
// `[{"scope_id":...,"attempt_count":...,"pass_count":...}]`.
// Putting `(repo_id, sha)` in headers preserves that body
// shape verbatim. Header-borne verbs fold the canonical
// (trimmed + lowercased) header value into the bytes
// returned by [VerbHandler.CanonicalRequest], which the
// Router uses for BOTH HMAC verification AND
// `payload_hash`. Two POSTs with identical bodies but
// different (repo, sha) targets therefore (a) cannot share
// the same HMAC signature (the signed material differs),
// and (b) cannot collide on the in-process idempotency
// claim or the durable `(verb, payload_hash)` index.
const RepoIDHeader = "X-Forge-Repo-ID"

// SHAHeader is the canonical HTTP header carrying the
// commit SHA the request targets. Required for verbs whose
// scan_run carries `sha_binding='single'` (one SHA per
// call: `ingest.coverage`, `ingest.test_balance`). Empty
// for `sha_binding='per_row'` verbs (`ingest.churn`,
// `ingest.defects`) -- each emitted row carries its own
// SHA in the body, and `scan_run.to_sha` is NULL.
//
// # Format
//
// The publisher SHOULD send the canonical 40-character
// lowercase hex commit SHA; per-verb validation may reject
// non-canonical shapes (e.g. test_balance enforces the
// `^[0-9a-fA-F]{40}$` regex).
const SHAHeader = "X-Forge-SHA"

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

