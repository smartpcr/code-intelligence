package mgmtapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// RouteSpans is the path the Stage 7.2 `mgmt.ingest_spans`
// verb is mounted on. Cmd binaries that compose this handler
// MUST mount BOTH `/v1/spans` and `/v1/spans/` on the public
// mux (mirrors the /v1/repos pattern).
const RouteSpans = "/v1/spans"

// DefaultSpansMaxBodyBytes caps the inbound `/v1/spans`
// request body. Defaults to 4 MiB to match the
// `internal/spaningestor/otlphttp.go` receiver's default,
// since the same OTLP/HTTP payloads flow through both
// surfaces. Larger than DefaultMaxBodyBytes (64 KiB) because
// a single OTel export request commonly carries hundreds of
// spans; smaller than a process's working-set budget so a
// pathological body cannot exhaust memory.
const DefaultSpansMaxBodyBytes int64 = 4 << 20

// otelTraceIDHexLen is the canonical 16-byte trace ID
// hex-string length. OTLP/HTTP JSON transports trace_id as
// lower-case hex; the receiver rejects anything else.
const otelTraceIDHexLen = 32

// otelSpanIDHexLen is the canonical 8-byte span ID hex-string
// length.
const otelSpanIDHexLen = 16

// forbiddenSpanFields is the closed set of JSON keys / OTel
// attribute keys whose presence on the `mgmt.ingest_spans`
// payload is a ┬º6.2.2 protocol violation. These belong on
// `mgmt.feedback`, NOT on spans (see architecture.md ┬º6.2.2).
// Checked at EVERY semantic level: root, resourceSpans,
// resource, resource attributes, scopeSpans, scope, scope
// attributes, span, span attributes.
var forbiddenSpanFields = map[string]struct{}{
	"outcome":          {},
	"corrected_action": {},
}

// IngestSpansResponse is the success-shape of `POST /v1/spans`.
// Returned with 202 Accepted because the spans are durably
// forwarded but their downstream processing (resolution,
// observation writes) is asynchronous.
//
// `AcceptedSpans` is the total span count across every
// resource group in the inbound payload. Because the
// forwarder is atomic per request (single POST of the
// original bytes), this is always equal to the validated
// span count.
type IngestSpansResponse struct {
	AcceptedSpans int `json:"accepted_spans"`
}

// handleIngestSpans implements `POST /v1/spans`
// (`mgmt.ingest_spans`). See architecture.md ┬º6.2.1 / ┬º6.2.2
// and implementation-plan.md Stage 7.2 for the contract.
//
// Error precedence (fail-fast on first match):
//  1. Content-Type guard ΓåÆ 415 unsupported_media_type.
//  2. Body cap + JSON parse ΓåÆ 413 / 400.
//  3. Forbidden field (`outcome`, `corrected_action`) at
//     ANY level (root / resourceSpans / resource / resource
//     attributes / scopeSpans / scope / scope attributes /
//     span / span attributes) ΓåÆ 400 forbidden_field.
//  4. OTel schema validation (trace_id, span_id, valid
//     start/end timestamps, hex shape) ΓåÆ 400
//     validation_failed.
//  5. Unknown service (any resource group whose
//     `service.name` does not resolve via the configured
//     [ServiceNameToRepoID]) ΓåÆ 400 unknown_service.
//  6. Forwarder error ΓåÆ 502 forward_failed or 503
//     forwarder_not_configured.
//
// On the happy path: the ORIGINAL validated body bytes are
// forwarded once to the configured [SpanForwarder] (no
// re-serialization). The handler then increments
// `mgmt_ingest_spans_total{status="accepted"}` per repo
// (partitioned via the per-repo span counts computed during
// validation).
func (h *Handler) handleIngestSpans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Step 1: content-type guard. Validating as JSON below
	// would happily decode a body sent with `text/plain`,
	// but the downstream receiver would then 415 on the
	// forwarded request and mgmt-api would surface that as
	// a misleading 502 forward_failed. Fail fast at the
	// edge so the operator gets the right diagnosis.
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return
	}

	body, ok := h.readSpansBody(w, r)
	if !ok {
		return
	}

	// Step 2: validation walker. Produces per-repo span
	// counts on success; emits the HTTP error + metric
	// internally and returns ok=false on any failure.
	counts, totalSpans, validationOK := h.validateSpansPayload(w, body)
	if !validationOK {
		return
	}

	// Empty payload (zero spans across all resource groups)
	// is valid OTLP but pointless. Return 202 with zero
	// count so retries are no-ops and the operator can
	// see the no-op in the metrics surface (no metric
	// fired because nothing happened).
	if totalSpans == 0 {
		writeJSONResponse(w, http.StatusAccepted, IngestSpansResponse{
			AcceptedSpans: 0,
		})
		return
	}

	// Step 3: forward the ORIGINAL validated body bytes,
	// once, atomically. The forwarder MUST NOT touch the
	// body; it just POSTs it at the OTLP/HTTP receiver.
	forwarder := h.spanForwarder
	if forwarder == nil {
		forwarder = notConfiguredForwarder{}
	}
	batch := SpansBatch{
		Body:        body,
		ContentType: "application/json",
	}
	if err := forwarder.Forward(ctx, batch); err != nil {
		h.handleForwardError(w, err, counts, totalSpans)
		return
	}

	// Step 4: success. Emit per-repo accepted counter.
	for repoID, n := range counts {
		h.spanMetrics.IncIngestSpansTotal(repoID, SpanStatusAccepted, int64(n))
	}
	subject, _ := SubjectFromContext(ctx)
	h.logger.Info("mgmtapi.ingest_spans.ok",
		slog.String("op", "ingest_spans"),
		slog.Int("accepted_spans", totalSpans),
		slog.Int("repo_groups", len(counts)),
		slog.String("subject", subject),
	)
	writeJSONResponse(w, http.StatusAccepted, IngestSpansResponse{
		AcceptedSpans: totalSpans,
	})
}

// handleForwardError classifies a forwarder return value and
// emits the matching HTTP envelope + metric per repo. The
// per-repo counter attribution preserves the "we know who
// was affected" property even when the whole batch is
// dropped at the transport layer (operator dashboards can
// then group `forward_failed` by `repo_id` to see which
// services are most impacted by the outage).
func (h *Handler) handleForwardError(w http.ResponseWriter, err error, counts map[string]int, totalSpans int) {
	switch {
	case errors.Is(err, ErrForwarderNotConfigured):
		for repoID, n := range counts {
			h.spanMetrics.IncIngestSpansTotal(repoID, SpanStatusForwarderNotConfigured, int64(n))
		}
		writeJSONError(w, http.StatusServiceUnavailable, "forwarder_not_configured",
			"span forwarder is not configured; AGENT_MEMORY_SPAN_FORWARD_URL is unset")
		h.logger.Warn("mgmtapi.ingest_spans.forwarder_not_configured",
			slog.String("op", "ingest_spans"),
			slog.Int("dropped_spans", totalSpans),
		)
	default:
		for repoID, n := range counts {
			h.spanMetrics.IncIngestSpansTotal(repoID, SpanStatusForwardFailed, int64(n))
		}
		writeJSONError(w, http.StatusBadGateway, "forward_failed",
			"failed to forward batch to Span Ingestor input queue")
		h.logger.Warn("mgmtapi.ingest_spans.forward_failed",
			slog.String("op", "ingest_spans"),
			slog.Int("dropped_spans", totalSpans),
			slog.String("error", err.Error()),
		)
	}
}

// isJSONContentType reports whether `ct` is an
// application/json variant (parameters such as charset are
// allowed; case is canonicalised). An empty value is also
// accepted to keep the door open for OTel exporters that
// omit Content-Type ΓÇö the body is still parsed as JSON, so a
// malformed body produces a 400 invalid_json downstream.
//
// Other content types (text/plain, application/x-protobuf,
// application/octet-stream, etc.) are rejected with 415
// because the JSON walker would either accept garbage or
// the downstream receiver would 415, both of which produce
// misleading error envelopes.
func isJSONContentType(ct string) bool {
	if strings.TrimSpace(ct) == "" {
		// An OTel exporter that omits Content-Type is
		// common; treat as JSON and let the parser reject
		// a non-JSON body with 400 invalid_json.
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

// readSpansBody applies the configured body cap and returns
// the raw bytes. Failures write the appropriate 4xx envelope
// and return ok=false. Distinct from the existing
// [Handler.decodeJSONBody] because this path needs the raw
// bytes (for the no-re-serialize forward AND for the
// raw-key forbidden-field walker) AND a separate larger cap.
func (h *Handler) readSpansBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	cap := h.maxSpansBody
	if cap == 0 {
		cap = DefaultSpansMaxBodyBytes
	}
	if cap > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, cap)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mxErr *http.MaxBytesError
		if errors.As(err, &mxErr) {
			h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("request body exceeds %d bytes", cap))
			return nil, false
		}
		h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
		writeJSONError(w, http.StatusBadRequest, "bad_request",
			"failed to read request body")
		return nil, false
	}
	if len(body) == 0 {
		h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
		writeJSONError(w, http.StatusBadRequest, "invalid_json",
			"request body is required")
		return nil, false
	}
	return body, true
}

// spanValidationError is the structured outcome of the
// validation walker. Carries the HTTP status, the operator-
// facing message, AND the metric `status` label so the
// caller can emit both atomically without duplicating the
// classification logic.
type spanValidationError struct {
	httpStatus int
	code       string
	message    string
	// metricStatus is the `status` label for
	// mgmt_ingest_spans_total.
	metricStatus string
	// metricRepoID is the `repo_id` label. Empty when the
	// failure occurred before a resource was identified
	// (root forbidden field, malformed JSON, unknown
	// service).
	metricRepoID string
	// metricCount is the delta the counter should be
	// incremented by. Defaults to 1 for single-span errors
	// (forbidden_field, schema validation). For
	// `unknown_service` the walker pre-counts the resource
	// group's span population and sets this so the
	// dashboard reflects how many spans were actually
	// rejected, not just one per HTTP call.
	metricCount int64
}

func (e *spanValidationError) Error() string { return e.message }

// validateSpansPayload walks the body's JSON tree, emits the
// 4xx envelope + metric for the FIRST failure it finds, and
// returns the per-repo accepted-span counts on full success.
//
// Returns (counts, totalSpans, true) on success; (nil, 0,
// false) on failure (the error envelope has already been
// written to w).
//
// The walker uses map[string]json.RawMessage at each
// semantic level so:
//   - Forbidden keys (`outcome`, `corrected_action`) are
//     visible BEFORE any typed decode silently drops them.
//   - The original body bytes are NEVER modified ΓÇö we
//     re-decode subtrees but the request body remains the
//     ground truth for forwarding.
//   - Non-string OTLP attribute values (intValue, boolValue,
//     doubleValue, arrayValue, ...) are preserved intact
//     because the walker only inspects attribute KEYS, not
//     the typed value union.
func (h *Handler) validateSpansPayload(w http.ResponseWriter, body []byte) (map[string]int, int, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
		writeJSONError(w, http.StatusBadRequest, "invalid_json",
			"request body is not valid OTLP/HTTP JSON: "+err.Error())
		return nil, 0, false
	}

	// Iter-4 structural fix: deep recursive scan of the
	// ENTIRE decoded JSON tree. Catches forbidden fields at
	// ANY nesting depth ΓÇö including nested OTLP shapes the
	// structured walker does not explicitly traverse
	// (`span.status`, nested `kvlistValue` KeyValue entries,
	// custom non-OTel sibling objects). This is the
	// canonical implementation of the implementation-plan
	// ┬º7.2 wording "reject any payload containing an
	// `outcome` or `corrected_action` field". The
	// downstream structured forbidden-key checks remain as
	// defense-in-depth.
	//
	// We pass the already-decoded `root` (a
	// `map[string]json.RawMessage` view) rather than the
	// raw body bytes so the scan does NOT re-parse the
	// payload into a fully materialised `any` tree. For a
	// payload at the 4 MiB body cap that second parse
	// roughly doubled peak heap, because every nested
	// string/number/bool leaf got boxed into a Go
	// interface; the lazy json.RawMessage walk only
	// decodes object/array spines on the descent path and
	// leaves scalar leaves as raw bytes.
	if vErr := h.deepForbiddenScan(root); vErr != nil {
		h.writeValidationFailure(w, vErr)
		return nil, 0, false
	}

	// Forbidden keys at the ROOT object level (kept as a
	// defense-in-depth check; the deep scan above already
	// covers this).
	if vErr := forbiddenKeyCheck(root, "root"); vErr != nil {
		h.writeValidationFailure(w, vErr)
		return nil, 0, false
	}

	rsRaw, ok := root["resourceSpans"]
	if !ok {
		h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
		writeJSONError(w, http.StatusBadRequest, "validation_failed",
			"validation: resourceSpans required")
		return nil, 0, false
	}
	var resources []json.RawMessage
	if err := json.Unmarshal(rsRaw, &resources); err != nil {
		h.spanMetrics.IncIngestSpansTotal("", SpanStatusRejectedValidation, 1)
		writeJSONError(w, http.StatusBadRequest, "validation_failed",
			"validation: resourceSpans must be an array: "+err.Error())
		return nil, 0, false
	}

	counts := make(map[string]int)
	totalSpans := 0
	for resIdx, rsRawEntry := range resources {
		repoID, n, vErr := h.validateResourceSpans(rsRawEntry, resIdx)
		if vErr != nil {
			h.writeValidationFailure(w, vErr)
			return nil, 0, false
		}
		if n > 0 {
			counts[repoID] += n
		}
		totalSpans += n
	}
	return counts, totalSpans, true
}

// validateResourceSpans validates one resourceSpans entry,
// resolves its repo_id, and returns the entry's span count.
//
// Returns (repoID, spanCount, nil) on success; (_, 0, vErr)
// on any failure. The caller emits the 4xx envelope based
// on vErr.
func (h *Handler) validateResourceSpans(raw json.RawMessage, resIdx int) (string, int, *spanValidationError) {
	var rs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rs); err != nil {
		return "", 0, &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: resourceSpans[%d]: invalid JSON object: %s", resIdx, err.Error()),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if vErr := forbiddenKeyCheck(rs, fmt.Sprintf("resourceSpans[%d]", resIdx)); vErr != nil {
		return "", 0, vErr
	}

	// Resource object: forbidden keys + attribute lookup.
	serviceName := ""
	if resourceRaw, ok := rs["resource"]; ok && len(resourceRaw) > 0 && string(resourceRaw) != "null" {
		var resource map[string]json.RawMessage
		if err := json.Unmarshal(resourceRaw, &resource); err != nil {
			return "", 0, &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "validation_failed",
				message:      fmt.Sprintf("validation: resourceSpans[%d].resource: invalid JSON object: %s", resIdx, err.Error()),
				metricStatus: SpanStatusRejectedValidation,
			}
		}
		if vErr := forbiddenKeyCheck(resource, fmt.Sprintf("resourceSpans[%d].resource", resIdx)); vErr != nil {
			return "", 0, vErr
		}
		// Walk resource attributes: forbidden keys + read
		// service.name's stringValue.
		var rerr *spanValidationError
		serviceName, rerr = walkAttributes(resource["attributes"], "service.name",
			fmt.Sprintf("resourceSpans[%d].resource.attributes", resIdx))
		if rerr != nil {
			return "", 0, rerr
		}
	}

	// Walk scopeSpans ΓåÆ scope + scope attributes + spans.
	totalSpans := 0
	if scopeRaw, ok := rs["scopeSpans"]; ok && len(scopeRaw) > 0 && string(scopeRaw) != "null" {
		var scopeSpans []json.RawMessage
		if err := json.Unmarshal(scopeRaw, &scopeSpans); err != nil {
			return "", 0, &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "validation_failed",
				message:      fmt.Sprintf("validation: resourceSpans[%d].scopeSpans: must be an array: %s", resIdx, err.Error()),
				metricStatus: SpanStatusRejectedValidation,
			}
		}
		for ssIdx, ssRaw := range scopeSpans {
			n, vErr := validateScopeSpans(ssRaw, resIdx, ssIdx)
			if vErr != nil {
				return "", 0, vErr
			}
			totalSpans += n
		}
	}

	// Resolve repo_id only when the resource actually had
	// spans. An empty resource group with zero spans is
	// allowed even if its service.name doesn't map ΓÇö there
	// is no data to attribute and rejecting it would be
	// overly strict.
	if totalSpans == 0 {
		return "", 0, nil
	}
	repoID := ""
	if h.spanLookup != nil {
		repoID = h.spanLookup(serviceName)
	}
	if repoID == "" {
		return "", 0, &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "unknown_service",
			message:      fmt.Sprintf("validation: resourceSpans[%d]: service.name=%q has no configured repo_id mapping; see AGENT_MEMORY_SPAN_SERVICE_MAP", resIdx, serviceName),
			metricStatus: SpanStatusUnknownService,
			metricRepoID: "",
			// Pre-count: every span in this resource
			// group is rejected, not just one. Without
			// this the metric undercounts when a
			// misconfigured emitter ships a batch with N
			// spans under an unmapped service.name.
			metricCount: int64(totalSpans),
		}
	}
	return repoID, totalSpans, nil
}

// validateScopeSpans walks one scopeSpans entry: forbidden
// keys, scope object, scope attributes, and each span.
// Returns the span count for the entry.
func validateScopeSpans(raw json.RawMessage, resIdx, ssIdx int) (int, *spanValidationError) {
	var ss map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ss); err != nil {
		return 0, &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: resourceSpans[%d].scopeSpans[%d]: invalid JSON object: %s", resIdx, ssIdx, err.Error()),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if vErr := forbiddenKeyCheck(ss, fmt.Sprintf("resourceSpans[%d].scopeSpans[%d]", resIdx, ssIdx)); vErr != nil {
		return 0, vErr
	}
	if scopeRaw, ok := ss["scope"]; ok && len(scopeRaw) > 0 && string(scopeRaw) != "null" {
		var scope map[string]json.RawMessage
		if err := json.Unmarshal(scopeRaw, &scope); err != nil {
			return 0, &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "validation_failed",
				message:      fmt.Sprintf("validation: resourceSpans[%d].scopeSpans[%d].scope: invalid JSON object: %s", resIdx, ssIdx, err.Error()),
				metricStatus: SpanStatusRejectedValidation,
			}
		}
		if vErr := forbiddenKeyCheck(scope, fmt.Sprintf("resourceSpans[%d].scopeSpans[%d].scope", resIdx, ssIdx)); vErr != nil {
			return 0, vErr
		}
		if _, vErr := walkAttributes(scope["attributes"], "",
			fmt.Sprintf("resourceSpans[%d].scopeSpans[%d].scope.attributes", resIdx, ssIdx)); vErr != nil {
			return 0, vErr
		}
	}

	count := 0
	if spansRaw, ok := ss["spans"]; ok && len(spansRaw) > 0 && string(spansRaw) != "null" {
		var spans []json.RawMessage
		if err := json.Unmarshal(spansRaw, &spans); err != nil {
			return 0, &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "validation_failed",
				message:      fmt.Sprintf("validation: resourceSpans[%d].scopeSpans[%d].spans: must be an array: %s", resIdx, ssIdx, err.Error()),
				metricStatus: SpanStatusRejectedValidation,
			}
		}
		for spIdx, spRaw := range spans {
			if vErr := validateSpan(spRaw, resIdx, ssIdx, spIdx); vErr != nil {
				return 0, vErr
			}
			count++
		}
	}
	return count, nil
}

// validateSpan runs the ┬º6.2.2 OTel schema checks against a
// raw span JSON message AND checks the span object + its
// attributes for forbidden field names.
//
// `resIdx`, `ssIdx`, `spIdx` are zero-based positions used
// only in the human-readable error message.
func validateSpan(raw json.RawMessage, resIdx, ssIdx, spIdx int) *spanValidationError {
	loc := fmt.Sprintf("resourceSpans[%d].scopeSpans[%d].spans[%d]", resIdx, ssIdx, spIdx)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: invalid JSON object: %s", loc, err.Error()),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if vErr := forbiddenKeyCheck(keys, loc); vErr != nil {
		return vErr
	}

	// Required fields presence check (before typed decode).
	if _, ok := keys["traceId"]; !ok {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      "validation: trace_id required",
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if _, ok := keys["spanId"]; !ok {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      "validation: span_id required",
			metricStatus: SpanStatusRejectedValidation,
		}
	}

	// Typed decode for schema validation. The typed view
	// is intentionally narrow (only fields we validate);
	// other OTLP fields stay in the body untouched.
	var typed typedSpanForValidation
	if err := json.Unmarshal(raw, &typed); err != nil {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: invalid OTel span JSON: %s", loc, err.Error()),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if typed.TraceID == "" {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      "validation: trace_id required",
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if !isLowerHex(typed.TraceID, otelTraceIDHexLen) {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: trace_id must be a %d-char lower-case hex string", loc, otelTraceIDHexLen),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	// Iter-4: W3C trace-context (and OTel) reserve the
	// all-zero trace_id as the "invalid trace" sentinel ΓÇö
	// it must never appear in an exported payload. The
	// `isLowerHex` check above passes "000...0" because it
	// is technically lower-case hex; this explicit reject
	// closes the gap.
	if isAllZeroHex(typed.TraceID) {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: trace_id must not be all zeros (W3C trace-context reserves the all-zero trace_id as the 'invalid trace' sentinel)", loc),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if typed.SpanID == "" {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      "validation: span_id required",
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if !isLowerHex(typed.SpanID, otelSpanIDHexLen) {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: span_id must be a %d-char lower-case hex string", loc, otelSpanIDHexLen),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	// Iter-4: same sentinel rule for span_id. The all-zero
	// span_id is reserved for "no span" ΓÇö only legitimate
	// as the implicit parent of a root span. Reject it on
	// the span_id field where it must be a real identifier.
	if isAllZeroHex(typed.SpanID) {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: span_id must not be all zeros (W3C trace-context reserves the all-zero span_id as the 'no-span' sentinel)", loc),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	// parent_span_id is allowed to be all-zero (or empty)
	// for a root span. Only check the hex shape if present
	// and non-empty.
	if typed.ParentSpanID != "" && !isLowerHex(typed.ParentSpanID, otelSpanIDHexLen) {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: parent_span_id must be empty or a %d-char lower-case hex string", loc, otelSpanIDHexLen),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if typed.StartTimeUnixNano == 0 {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: start_time_unix_nano required and must be > 0", loc),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if typed.EndTimeUnixNano == 0 {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: end_time_unix_nano required and must be > 0", loc),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	if typed.EndTimeUnixNano < typed.StartTimeUnixNano {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: end_time_unix_nano must be >= start_time_unix_nano", loc),
			metricStatus: SpanStatusRejectedValidation,
		}
	}

	// Span attributes ΓÇö forbidden-key check.
	if _, vErr := walkAttributes(keys["attributes"], "", loc+".attributes"); vErr != nil {
		return vErr
	}

	// Span `events` ΓÇö each event object AND its attributes
	// are walked for forbidden keys. OTel `SpanEvent` is
	// `{timeUnixNano, name, attributes, droppedAttributesCount}`
	// ΓÇö a payload smuggling `outcome` as either the event
	// object key OR an event-attribute key would otherwise
	// bypass the ┬º6.2.2 contract.
	if vErr := walkEventsOrLinks(keys["events"], loc+".events", "event"); vErr != nil {
		return vErr
	}

	// Span `links` ΓÇö same treatment. OTel `SpanLink` is
	// `{traceId, spanId, traceState, attributes, droppedAttributesCount,
	// flags}`.
	if vErr := walkEventsOrLinks(keys["links"], loc+".links", "link"); vErr != nil {
		return vErr
	}
	return nil
}

// walkEventsOrLinks validates the OTLP `events` or `links`
// array on a span: each entry's object keys are
// forbidden-checked, AND its `attributes` key-value array is
// walked. The `kind` parameter is `"event"` or `"link"` and
// is woven into the location string for operator-facing
// error messages.
//
// A nil / "null" / empty raw payload is a no-op (both fields
// are optional on OTel spans). Non-array shapes surface as
// `validation_failed` 400.
func walkEventsOrLinks(raw json.RawMessage, where, kind string) *spanValidationError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: %ss must be an array: %s", where, kind, err.Error()),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	for i, entryRaw := range entries {
		entryLoc := fmt.Sprintf("%s[%d]", where, i)
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(entryRaw, &entry); err != nil {
			return &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "validation_failed",
				message:      fmt.Sprintf("validation: %s: invalid JSON object: %s", entryLoc, err.Error()),
				metricStatus: SpanStatusRejectedValidation,
			}
		}
		if vErr := forbiddenKeyCheck(entry, entryLoc); vErr != nil {
			return vErr
		}
		if _, vErr := walkAttributes(entry["attributes"], "", entryLoc+".attributes"); vErr != nil {
			return vErr
		}
	}
	return nil
}

// deepForbiddenScan walks the decoded JSON tree once and
// returns a `forbidden_field` validation error on the first
// occurrence of any forbidden field anywhere in the payload.
//
// What it checks (at any nesting depth):
//  1. Any JSON OBJECT KEY equal to a forbidden name
//     (`outcome`, `corrected_action`). This catches
//     `outcome` placed on the root, on `resource`, on
//     `scope`, on a span, on a span event, on a span link,
//     on `span.status`, or on any non-OTel sibling object
//     an operator might have included.
//  2. Any OTel `KeyValue` whose `key` STRING VALUE equals a
//     forbidden name. OTel attribute names are encoded as
//     the string value of a JSON field called `"key"`, not
//     as JSON object keys ΓÇö so the pure-JSON-key check
//     above is necessary but not sufficient. This second
//     leg catches forbidden attribute names anywhere
//     KeyValue lists appear, including nested in
//     `kvlistValue.values[]` and `arrayValue.values[]`.
//
// Returns nil if the payload contains no forbidden fields.
// The error message includes the full JSON path of the
// first violation so an operator can locate the offending
// field in a multi-MB OTLP body.
//
// This is the iter-4 structural fix for the
// implementation-plan.md ┬º7.2 wording "reject any payload
// containing an `outcome` or `corrected_action` field" ΓÇö
// previous iterations enumerated specific OTLP levels and
// missed `span.status` and nested kvlistValue cases.
//
// Memory note: this walker reuses the
// `map[string]json.RawMessage` view the caller already
// decoded from the body. It NEVER materialises the full
// payload as an `any` tree, so scalar leaves
// (strings, numbers, booleans, null) remain as raw bytes
// and are not boxed into Go interfaces. On a payload at
// the 4 MiB body cap this avoids roughly doubling peak
// heap relative to a `json.Unmarshal(body, &any)` scan.
// Only object / array spines on the current descent path
// are decoded one level at a time.
func (h *Handler) deepForbiddenScan(root map[string]json.RawMessage) *spanValidationError {
	if path, key, found := containsForbiddenKeyMap(root, ""); found {
		return &spanValidationError{
			httpStatus: http.StatusBadRequest,
			code:       "forbidden_field",
			message: fmt.Sprintf(
				"validation: forbidden field %q found at %s; %q and %q are not allowed on mgmt.ingest_spans ΓÇö see architecture.md ┬º6.2.2 (these belong on mgmt.feedback)",
				key, path, "outcome", "corrected_action"),
			metricStatus: SpanStatusRejectedForbiddenField,
		}
	}
	return nil
}

// containsForbiddenKey is the recursive worker behind
// [deepForbiddenScan]. It returns
// `(jsonPath, offendingKey, true)` on the first hit and
// `("", "", false)` otherwise.
//
// The walker is "lazy" in the json.RawMessage sense:
// it peeks the first non-whitespace byte of `raw` and only
// pays for a full json.Unmarshal when that byte is `{` or
// `[`. Scalar leaves (strings, numbers, booleans, null)
// short-circuit immediately, which is the property that
// keeps peak heap bounded on a 4 MiB OTLP body.
func containsForbiddenKey(raw json.RawMessage, path string) (string, string, bool) {
	first, ok := firstNonSpaceByte(raw)
	if !ok {
		return "", "", false
	}
	switch first {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			// json.Unmarshal validated the parent during
			// the caller's decode, so a well-formed
			// subtree here should always re-decode. If
			// it somehow does not, defer to the
			// structured decoder for the actual error.
			return "", "", false
		}
		return containsForbiddenKeyMap(obj, path)
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return "", "", false
		}
		for i, item := range arr {
			if p, key, ok := containsForbiddenKey(item, fmt.Sprintf("%s[%d]", path, i)); ok {
				return p, key, ok
			}
		}
	}
	return "", "", false
}

// containsForbiddenKeyMap is the per-object pass. Split
// out from [containsForbiddenKey] so [deepForbiddenScan]
// can reuse the already-decoded root map without paying
// for an extra round-trip through json.RawMessage.
//
// Object key iteration is alphabetically sorted so that
// error messages are deterministic across the Go runtime's
// randomised map iteration order (otherwise the same input
// could produce different paths run-to-run, breaking test
// stability).
func containsForbiddenKeyMap(obj map[string]json.RawMessage, path string) (string, string, bool) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Pass 1: JSON object keys. Catches `outcome` as
	// a literal object field at any depth.
	for _, k := range keys {
		if _, bad := forbiddenSpanFields[k]; bad {
			return joinJSONPath(path, k), k, true
		}
	}

	// Pass 2: KeyValue shape. OTLP encodes attribute
	// names as the STRING VALUE of a `"key"` JSON field.
	// We only pay for the unquote when the raw bytes
	// actually look like a JSON string (cheap first-byte
	// check), so non-string `key` values short-circuit.
	if rawKey, ok := obj["key"]; ok {
		if ks, isStr := decodeJSONString(rawKey); isStr {
			if _, bad := forbiddenSpanFields[ks]; bad {
				return joinJSONPath(path, "key"), ks, true
			}
		}
	}

	// Pass 3: recurse into values. Sorted-key iteration
	// keeps the JSON path of the first violation stable.
	for _, k := range keys {
		if p, key, ok := containsForbiddenKey(obj[k], joinJSONPath(path, k)); ok {
			return p, key, ok
		}
	}
	return "", "", false
}

// firstNonSpaceByte returns the first byte of `raw` that
// is not JSON whitespace (space, tab, CR, LF), along with
// a boolean indicating whether such a byte exists. Used
// by [containsForbiddenKey] and [decodeJSONString] to
// classify a json.RawMessage subtree by its leading
// structural character without paying for a full decode
// of scalar leaves.
func firstNonSpaceByte(raw json.RawMessage) (byte, bool) {
	for _, b := range raw {
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return b, true
		}
	}
	return 0, false
}

// decodeJSONString returns the unquoted string value of a
// json.RawMessage iff its leading non-whitespace byte is
// `"`. Other shapes (numbers, booleans, null, objects,
// arrays) return `("", false)` without a decode, which is
// what keeps the OTel `KeyValue.key` check cheap when the
// `key` field happens to be a non-string value (rare, but
// possible for malformed exporters).
func decodeJSONString(raw json.RawMessage) (string, bool) {
	first, ok := firstNonSpaceByte(raw)
	if !ok || first != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// joinJSONPath concatenates a JSON-path base with a child
// segment. An empty base produces just the segment (no
// leading dot).
func joinJSONPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// typedSpanForValidation is the narrow typed view used to
// validate OTel schema fields. Decoded from the raw span
// JSON but DELIBERATELY does not model the full OTel span
// shape ΓÇö the original bytes carry all data on the forward
// path; this struct only exists to type-check the handful
// of fields the validator cares about.
type typedSpanForValidation struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId"`
	StartTimeUnixNano otlpJSONUint64 `json:"startTimeUnixNano"`
	EndTimeUnixNano   otlpJSONUint64 `json:"endTimeUnixNano"`
}

// otlpJSONUint64 decodes both "12345" (spec form) and 12345
// (numeric form). Local to this file so the mgmtapi package
// stays free of cross-package dependencies on
// internal/spaningestor.
type otlpJSONUint64 uint64

// UnmarshalJSON implements [json.Unmarshaler].
func (s *otlpJSONUint64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		if len(b) < 2 || b[len(b)-1] != '"' {
			return fmt.Errorf("otlpJSONUint64: malformed quoted value %q", string(b))
		}
		b = b[1 : len(b)-1]
		if len(b) == 0 {
			*s = 0
			return nil
		}
	}
	v, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("otlpJSONUint64: %w", err)
	}
	*s = otlpJSONUint64(v)
	return nil
}

// forbiddenKeyCheck rejects any key in `obj` that matches the
// closed set [forbiddenSpanFields]. Used at root /
// resourceSpans / resource / scopeSpans / scope / span object
// levels. `where` is a human-readable location for the error
// message (e.g. "resourceSpans[0].resource").
//
// Operator note: a legitimate use of an attribute key named
// `outcome` at the resource level is still rejected ΓÇö the
// architecture intentionally namespaces these keys to
// `mgmt.feedback`. Pick a different attribute name (e.g.
// `service.outcome`) if you need related semantics on a span.
func forbiddenKeyCheck(obj map[string]json.RawMessage, where string) *spanValidationError {
	for k := range obj {
		if _, bad := forbiddenSpanFields[k]; bad {
			return &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "forbidden_field",
				message:      fmt.Sprintf("validation: %s: %q is not allowed on mgmt.ingest_spans; see architecture.md ┬º6.2.2 (belongs on mgmt.feedback)", where, k),
				metricStatus: SpanStatusRejectedForbiddenField,
			}
		}
	}
	return nil
}

// walkAttributes inspects an OTLP `[]KeyValue` attributes
// list for forbidden keys AND returns the `stringValue` of
// the entry matching `wantKey` (or "" if not present /
// `wantKey` is empty).
//
// Crucially: we only walk attribute KEYS (always strings per
// OTel KeyValue.key), NEVER the typed value union ΓÇö so
// non-string OTLP values (intValue, boolValue, doubleValue,
// arrayValue, kvlistValue, bytesValue) are preserved intact
// on the forward path because we never touch them. This is
// the fix for iter-1's lossy AnyValue narrowing.
func walkAttributes(raw json.RawMessage, wantKey, where string) (string, *spanValidationError) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var attrs []json.RawMessage
	if err := json.Unmarshal(raw, &attrs); err != nil {
		return "", &spanValidationError{
			httpStatus:   http.StatusBadRequest,
			code:         "validation_failed",
			message:      fmt.Sprintf("validation: %s: attributes must be an array: %s", where, err.Error()),
			metricStatus: SpanStatusRejectedValidation,
		}
	}
	found := ""
	for i, attrRaw := range attrs {
		var attr struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string `json:"stringValue"`
			} `json:"value"`
		}
		if err := json.Unmarshal(attrRaw, &attr); err != nil {
			return "", &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "validation_failed",
				message:      fmt.Sprintf("validation: %s[%d]: invalid KeyValue: %s", where, i, err.Error()),
				metricStatus: SpanStatusRejectedValidation,
			}
		}
		if _, bad := forbiddenSpanFields[attr.Key]; bad {
			return "", &spanValidationError{
				httpStatus:   http.StatusBadRequest,
				code:         "forbidden_field",
				message:      fmt.Sprintf("validation: %s[%d]: attribute key %q is not allowed on mgmt.ingest_spans; see architecture.md ┬º6.2.2 (belongs on mgmt.feedback)", where, i, attr.Key),
				metricStatus: SpanStatusRejectedForbiddenField,
			}
		}
		if wantKey != "" && attr.Key == wantKey && found == "" {
			found = attr.Value.StringValue
		}
	}
	return found, nil
}

// writeValidationFailure emits the HTTP envelope + metric
// for a validation error in one shot. Centralised so the
// walker's many error returns all flow through the same
// metric-attribution path.
//
// The metric delta defaults to 1; the walker may override it
// via `metricCount` for the `unknown_service` case where the
// whole resource group's spans were rejected.
func (h *Handler) writeValidationFailure(w http.ResponseWriter, vErr *spanValidationError) {
	delta := vErr.metricCount
	if delta <= 0 {
		delta = 1
	}
	h.spanMetrics.IncIngestSpansTotal(vErr.metricRepoID, vErr.metricStatus, delta)
	writeJSONError(w, vErr.httpStatus, vErr.code, vErr.message)
}

// isLowerHex reports whether s is a lower-case hex string of
// the exact requested length. trace_id / span_id MUST be
// canonical OTel lower-case hex per the OTLP spec; allowing
// upper-case would let an emitter bug silently produce two
// distinct downstream rows for the same span.
func isLowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isAllZeroHex reports whether s is a non-empty string
// composed entirely of '0' characters. Used to reject the
// W3C trace-context sentinels: an all-zero trace_id ("the
// invalid trace") or an all-zero span_id ("no span") must
// never appear on a real exported span. The hex-shape check
// in [isLowerHex] would otherwise pass these strings.
func isAllZeroHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
