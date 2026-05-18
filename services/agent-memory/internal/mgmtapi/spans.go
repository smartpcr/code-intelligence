package mgmtapi

// Stage 7.2: `mgmt.ingest_spans` -- POST /v1/spans.
//
// architecture.md §6.2.1 pins the verb signature as
// `mgmt.ingest_spans(batch[])` -- a CANONICAL OTel span batch
// (§3.3, **G3**). §6.2.2 ("Validation rules") states the verb
// validates each span against the OTel schema and -- explicitly
// -- has NO `outcome`/`corrected_action` semantics. Those fields
// belong to `mgmt.feedback`.
//
// The implementation-plan Stage 7.2 brief adds:
//
//   * Reject any payload containing `outcome` or `corrected_action`
//     with 400 + a §6.2.2 reference.
//   * Emit metric `mgmt_ingest_spans_total` partitioned by
//     `repo_id` and `status`.
//
// This verb is an ALTERNATIVE entry path. The primary OTel
// pipeline (Collector -> Span Ingestor's OTLP/HTTP receiver in
// `internal/spaningestor/otlphttp.go`) is unchanged. Operators
// and offline tools use POST /v1/spans when they can't reach the
// Collector (air-gapped replay, batch backfill, manual triage).
//
// Wire shape (CANONICAL OTel ExportTraceServiceRequest)
// -----------------------------------------------------
//
// The body is an OTLP/HTTP JSON `ExportTraceServiceRequest`
// (https://opentelemetry.io/docs/specs/otlp/#otlphttp) optionally
// extended with a top-level `repo_id` routing field. The per-span
// shape, scope hierarchy, and attribute encoding are byte-for-byte
// identical to the shape the Span Ingestor's OTLP receiver
// accepts at `internal/spaningestor/otlphttp.go` so a client
// emitting canonical OTLP can target both endpoints with the
// same serializer:
//
//   {
//     "repo_id": "<uuid>",                     // OPTIONAL — see "repo_id resolution" below
//     "resourceSpans": [                       // OTLP canonical
//       {
//         "resource": {
//           "attributes": [
//             {"key": "service.name", "value": {"stringValue": "..."}}
//           ]
//         },
//         "scopeSpans": [
//           {
//             "spans": [
//               {
//                 "traceId": "<32 hex chars, non-zero>",
//                 "spanId": "<16 hex chars, non-zero>",
//                 "parentSpanId": "<16 hex chars, optional>",
//                 "name": "<operation>",
//                 "startTimeUnixNano": "1700000000000000000",
//                 "endTimeUnixNano":   "1700000000123456000",
//                 "attributes": [
//                   {"key": "http.method",  "value": {"stringValue": "GET"}},
//                   {"key": "http.status",  "value": {"intValue":    "200"}},
//                   {"key": "feature.flag", "value": {"boolValue":   true}}
//                 ]
//               }
//             ]
//           }
//         ]
//       }
//     ]
//   }
//
// The top-level `repo_id` is the simplest routing hook for an
// operator scripting curl-style requests. Callers who want to
// POST a canonical OTLP `ExportTraceServiceRequest` AS-IS
// (e.g. a Collector forwarding a backfill batch) can omit it
// and supply the repo via either an HTTP header or an OTel
// resource attribute; see the "repo_id resolution" section
// below. Field names AT THE PER-SPAN LEVEL are the OTel
// canonical lowerCamelCase identifiers (`traceId`, `spanId`,
// ...) -- this is the shape the OTLP/HTTP spec defines and the
// shape `internal/spaningestor/otlphttp.go` already consumes.
// Validation MESSAGES use the conceptual snake_case naming
// (`trace_id required`, `span_id required`) to align with the
// architecture / implementation-plan §7.2 test scenario phrasing
// and with the way operators read about these fields in §6.2.
//
// repo_id resolution
// ------------------
// The handler resolves the target repo_id from the FIRST hook
// below that yields a UUID-shaped value; later hooks are only
// consulted when earlier ones are absent or malformed. This
// lets a caller POST a canonical OTLP body unchanged when their
// pipeline cannot decorate the body with a `repo_id` wrapper.
//
//  1. `X-Mgmt-Repo-ID` HTTP request header. Whole-request
//     override; applies to every span in the body. Matches the
//     header the `HTTPSpanForwarder` sets and the Span Ingestor
//     receiver honors (`spaningestor.MgmtRepoIDHeader`).
//  2. Top-level `repo_id` JSON field (the original mgmt-api
//     wrapper shape; still the most explicit for operator UX).
//  3. `mgmt.repo_id` resource attribute on the FIRST entry of
//     `resourceSpans` (matches `spaningestor.MgmtRepoIDResourceAttr`).
//     The handler treats the request as having a SINGLE repo
//     target — when multiple ResourceSpans carry diverging
//     `mgmt.repo_id` values the request is rejected at 400.
//
// A request that supplies no hook (no header, no field, no
// resource attribute) is rejected at 400 `repo_id_required` so
// a typo can't silently mis-route a batch.
//
// Attribute encoding follows the OTel AnyValue union: an
// attribute value is an object with EXACTLY ONE of
// `stringValue`, `intValue`, `doubleValue`, `boolValue`,
// `arrayValue`, `kvlistValue`, or `bytesValue` set. Numeric
// values are stringified by the handler (per the resolver's
// `map[string]string` contract, mirroring `convertSpan` in
// `otlphttp.go`).
//
// Backpressure
// ------------
// When the SpanForwarder reports `ErrSpanIngestorBackpressure`,
// the handler responds with HTTP 503 + `Retry-After: 5` so the
// caller can retry. The full §7.5 / §C22 degraded envelope
// (HTTP 200 + `degraded=true`) requires durable mgmt-api-side
// buffering to satisfy the "batch is queued, no spans dropped
// silently" e2e §3 invariant; that buffering lands in Stage 8.1
// (Degraded-mode contract wiring). 503 + Retry-After is the
// honest interim response: the caller knows the batch was NOT
// queued and MUST retry.
//
// SpanForwarder wiring
// --------------------
// The handler holds an optional [SpanForwarder]. When nil,
// POST /v1/spans returns 501 Not Implemented so a fresh
// deployment makes the "missing forwarder" mis-config loud at
// the first request instead of silently dropping spans. The
// production composition root (`cmd/mgmt-api`) wires
// [NewHTTPSpanForwarder] (in `forwarder_http.go`) when
// `AGENT_MEMORY_SPAN_INGESTOR_URL` is set.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// RouteSpans is the URL path the Stage 7.2 verb registers on.
// architecture.md §6.2.1 names the verb `mgmt.ingest_spans`;
// implementation-plan.md Stage 7.2 binds it to POST /v1/spans.
const RouteSpans = "/v1/spans"

// IngestSpansMaxBatch caps the TOTAL number of spans a single
// `mgmt.ingest_spans` request may carry across ALL its
// `resourceSpans[i].scopeSpans[j].spans[k]` entries.
// tech-spec.md §8.3 / §C14 pins the sustained envelope at
// "≤ 1k spans per batch, 50 batches/min". 1000 is the hard
// ceiling; oversized batches are rejected at 400 so an operator
// scripting the verb sees the limit immediately rather than
// waiting for a downstream queue-full.
const IngestSpansMaxBatch = 1000

// IngestSpansMaxBodyBytes is the per-route body cap for
// POST /v1/spans. The mgmt-API global default
// ([DefaultMaxBodyBytes]) is 64 KiB which would reject a
// healthy 1000-span batch outright; we raise the cap on this
// route specifically to 4 MiB (the OTLP/HTTP receiver's same
// default in `internal/spaningestor/otlphttp.go`) so the two
// entry paths accept comparably-shaped payloads.
const IngestSpansMaxBodyBytes int64 = 4 << 20

// SpanIngestorBackpressureReason is the closed-set ENUM literal
// the §C22 / §6.3 degraded contract permits when the Span
// Ingestor queue is at capacity. Re-exported here as a
// package-level constant so callers wiring metrics or alerts
// can dispatch on the same string the handler emits.
const SpanIngestorBackpressureReason = "span_ingestor_backpressure"

// ingestSpansRetryAfterSeconds is the Retry-After header value
// returned alongside the 503 backpressure response. 5 seconds
// matches the OTLP/HTTP receiver's default and keeps an
// operator's polling loop in sync across both entry paths.
const ingestSpansRetryAfterSeconds = 5

// Closed-set status labels for the `mgmt_ingest_spans_total`
// metric. Kept as named constants so a typo at one call site
// can't fork the label space.
const (
	IngestSpansStatusAccepted        = "accepted"
	IngestSpansStatusValidationError = "validation_error"
	IngestSpansStatusForbiddenField  = "forbidden_field"
	IngestSpansStatusBackpressure    = "backpressure"
	IngestSpansStatusForwarderError  = "forwarder_error"
	IngestSpansStatusRepoNotFound    = "repo_not_found"
	IngestSpansStatusForwarderUnset  = "forwarder_unset"
)

// metricUnknownRepo is the label value used when a request
// fails before a valid repo_id is known (malformed body, bad
// UUID). Empty string would also work but a sentinel keeps
// the metric label distinct from "no requests for this repo".
const metricUnknownRepo = "unknown"

// MgmtRepoIDHeader is the HTTP request header an operator (or
// the mgmt-api forwarder) sets to assert the target repo for a
// canonical OTLP body that does NOT carry a top-level `repo_id`
// wrapper. Mirrors `spaningestor.MgmtRepoIDHeader` so the same
// constant flows end-to-end from the mgmt-api verb through to
// the Span Ingestor's receiver.
const MgmtRepoIDHeader = "X-Mgmt-Repo-ID"

// MgmtRepoIDResourceAttr is the resource attribute key the
// handler reads from the FIRST `resourceSpans` entry when
// neither the [MgmtRepoIDHeader] nor the top-level `repo_id`
// JSON field is supplied. Mirrors
// `spaningestor.MgmtRepoIDResourceAttr` for end-to-end
// consistency. See file header §"repo_id resolution".
const MgmtRepoIDResourceAttr = "mgmt.repo_id"

// IngestSpansForwarderUnavailableCode is the operator-facing
// error code emitted on 501 when the binary was deployed
// without a wired SpanForwarder. Aligned with the
// `cmd/mgmt-api` doc-comment so monitoring rules and runbooks
// can match a single literal.
const IngestSpansForwarderUnavailableCode = "span_forwarder_unavailable"

// IngestSpansRepoIDRequiredCode is the operator-facing error
// code emitted on 400 when a request supplied no repo hook
// (no [MgmtRepoIDHeader] header, no top-level `repo_id`, no
// [MgmtRepoIDResourceAttr] resource attribute).
const IngestSpansRepoIDRequiredCode = "repo_id_required"

// IngestSpansRepoIDConflictCode is the operator-facing error
// code emitted on 400 when two ResourceSpans entries carry
// CONFLICTING `mgmt.repo_id` resource attributes. Conflicting
// resource-attribute targets within a single body would force
// the handler to split the batch across repos -- a behaviour
// that surfaces silently and can mis-route. The handler
// rejects up front so the caller can split the batch
// explicitly.
const IngestSpansRepoIDConflictCode = "repo_id_conflict"

// SpanForwarder is the narrow surface the `mgmt.ingest_spans`
// handler uses to hand a verified batch off to the Span
// Ingestor input queue. Production wires this to
// [NewHTTPSpanForwarder] which posts an OTLP/HTTP frame at the
// span-ingestor binary's /v1/traces endpoint; tests pass an
// in-memory fake.
//
// The handler always calls ForwardSpans with `repoID` matching
// the request's `repo_id` field AFTER validating the request.
// Implementations MUST NOT mutate the `spans` slice or its
// elements (the handler retains the slice for logging after
// the call returns).
//
// On a full Span Ingestor queue, implementations MUST return
// [ErrSpanIngestorBackpressure] so the handler can surface
// the right operator-facing envelope. Any other error is
// treated as a 500 internal_error.
type SpanForwarder interface {
	ForwardSpans(ctx context.Context, repoID string, spans []ForwardedSpan) error
}

// ForwardedSpan is the per-span shape the handler hands to the
// forwarder after the §6.2.2 validation step. Field names align
// with the conceptual OTel naming; types are normalized (uint64
// for nanos, `map[string]string` for attributes) so a forwarder
// need not re-parse JSON.
//
// The `Attributes` map is a single string-typed view per the
// resolver contract in `internal/spaningestor/resolver.go`
// (`Span.Attributes map[string]string`). The OTel
// union-typed `AnyValue` (string / int / bool / double) is
// stringified by [stringifyOTelAnyValue] before forwarding;
// `arrayValue` / `kvlistValue` / `bytesValue` are dropped at
// stringification time with a deterministic representation so
// the resolver gets a stable signal.
type ForwardedSpan struct {
	TraceID           string
	SpanID            string
	ParentSpanID      string
	Name              string
	StartTimeUnixNano uint64
	EndTimeUnixNano   uint64
	Attributes        map[string]string
}

// ErrSpanIngestorBackpressure is the sentinel a [SpanForwarder]
// implementation returns when the downstream Span Ingestor's
// input queue is at capacity. The handler maps it onto an HTTP
// 503 response with a `Retry-After: 5` header (see file header
// for the rationale on choosing 503 + Retry-After over the
// degraded-envelope shape for Stage 7.2).
var ErrSpanIngestorBackpressure = errors.New("mgmtapi: span ingestor backpressure")

// IngestSpansMetrics is the per-`(status, repo_id)` counter
// ledger backing the `mgmt_ingest_spans_total` metric called
// out in implementation-plan.md Stage 7.2. Per-batch counting
// (each request increments once with its final outcome) keeps
// the metric shape aligned with standard Prometheus HTTP-request
// counter conventions; per-span volume can be added on a
// separate metric in a future stage if operators need it.
//
// Zero value is NOT ready for use; allocate via
// [NewIngestSpansMetrics] so the inner maps are initialised.
// The constructor avoids a lazy-init nil-check on every Inc
// call.
type IngestSpansMetrics struct {
	mu sync.RWMutex
	// counters: status -> repo_id -> count
	counters map[string]map[string]*atomic.Int64
}

// NewIngestSpansMetrics returns a ready-to-use metrics ledger.
// Cheap (one map allocation) so the composition root can call
// it unconditionally at boot.
func NewIngestSpansMetrics() *IngestSpansMetrics {
	return &IngestSpansMetrics{
		counters: make(map[string]map[string]*atomic.Int64),
	}
}

// Inc atomically bumps the counter at `(status, repoID)` by 1.
// Safe for concurrent use from any goroutine. A nil receiver is
// a no-op so callers can pass nil to opt out of metric
// recording in tests without separate code paths.
func (m *IngestSpansMetrics) Inc(status, repoID string) {
	if m == nil {
		return
	}
	if repoID == "" {
		repoID = metricUnknownRepo
	}
	m.mu.RLock()
	inner, ok := m.counters[status]
	var c *atomic.Int64
	if ok {
		c = inner[repoID]
	}
	m.mu.RUnlock()
	if c != nil {
		c.Add(1)
		return
	}
	m.mu.Lock()
	inner, ok = m.counters[status]
	if !ok {
		inner = make(map[string]*atomic.Int64)
		m.counters[status] = inner
	}
	c, ok = inner[repoID]
	if !ok {
		c = new(atomic.Int64)
		inner[repoID] = c
	}
	m.mu.Unlock()
	c.Add(1)
}

// Snapshot returns a flat copy of the {status -> {repo_id ->
// count}} map for the operator dashboard / tests. Reads acquire
// the rw-mutex briefly; the returned values are detached so the
// caller can mutate freely.
func (m *IngestSpansMetrics) Snapshot() map[string]map[string]int64 {
	if m == nil {
		return map[string]map[string]int64{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]map[string]int64, len(m.counters))
	for status, inner := range m.counters {
		sub := make(map[string]int64, len(inner))
		for repoID, c := range inner {
			sub[repoID] = c.Load()
		}
		out[status] = sub
	}
	return out
}

// Count returns the current count at `(status, repoID)`. Useful
// in tests that want to assert a single value without a full
// snapshot allocation. Returns 0 when the bucket is unset.
func (m *IngestSpansMetrics) Count(status, repoID string) int64 {
	if m == nil {
		return 0
	}
	if repoID == "" {
		repoID = metricUnknownRepo
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	inner, ok := m.counters[status]
	if !ok {
		return 0
	}
	c, ok := inner[repoID]
	if !ok {
		return 0
	}
	return c.Load()
}

// SpanIngestRequest is the wire shape of POST /v1/spans. See
// the file header for the rationale: a canonical OTLP
// `ExportTraceServiceRequest` optionally wrapped by a top-level
// `repo_id` routing field. Callers who can't decorate the body
// with the wrapper can instead supply the repo via the
// [MgmtRepoIDHeader] HTTP header or the [MgmtRepoIDResourceAttr]
// resource attribute (see file header §"repo_id resolution").
type SpanIngestRequest struct {
	// RepoID is the target repo the operator is submitting
	// spans for. OPTIONAL on the wire (the handler may
	// resolve the same value from the [MgmtRepoIDHeader]
	// header or the [MgmtRepoIDResourceAttr] resource
	// attribute on the first `resourceSpans` entry). The
	// resolved repo_id MUST be a valid UUID and MUST reference
	// a row in the `repo` table; otherwise the handler returns
	// 400 (malformed) or 404 (unknown).
	RepoID string `json:"repo_id"`
	// ResourceSpans is the canonical OTLP per-resource span
	// hierarchy. REQUIRED. Decoded with the per-scope `spans`
	// kept as []json.RawMessage so [validateSpan] can do a
	// two-pass parse: pass 1 detects forbidden top-level keys
	// (`outcome`, `corrected_action`) precisely; pass 2
	// decodes the typed [spanWire] shape.
	ResourceSpans []otlpResourceSpansWire `json:"resourceSpans"`
}

// SpanIngestResponse is the wire shape of a successful POST
// /v1/spans. `AcceptedSpans` echoes the total span count the
// handler forwarded so the caller can sanity-check on the wire.
//
// The `Degraded` / `DegradedReason` fields are included for
// forward-compat with the §6.3 envelope; Stage 7.2 always
// returns `degraded=false` on success (backpressure is surfaced
// as HTTP 503, not as a degraded 200 -- see file header). The
// fields stay in the struct so a future Stage 8.1 wire-up that
// promotes backpressure to a degraded 200 doesn't need a
// breaking response-shape change.
type SpanIngestResponse struct {
	RepoID         string `json:"repo_id"`
	AcceptedSpans  int    `json:"accepted_spans"`
	Degraded       bool   `json:"degraded"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// otlpResourceSpansWire mirrors the OTLP `ResourceSpans`
// message. Field tags match the OTLP/JSON canonical shape
// (lowerCamelCase) consumed by the Span Ingestor's receiver in
// `internal/spaningestor/otlphttp.go`.
type otlpResourceSpansWire struct {
	Resource   otlpResourceWire    `json:"resource"`
	ScopeSpans []otlpScopeSpanWire `json:"scopeSpans"`
}

// otlpResourceWire mirrors the OTLP `Resource` message. The
// `attributes` array is OPTIONAL; when absent the handler
// treats the resource as carrying no attributes (it is the
// operator's prerogative to omit them when batch-replaying
// spans whose service.name routing is already encoded via the
// top-level `repo_id` field).
type otlpResourceWire struct {
	Attributes []otlpKeyValueWire `json:"attributes"`
}

// otlpScopeSpanWire mirrors the OTLP `ScopeSpans` message. The
// `scope` field is intentionally OMITTED on the decode side --
// the mgmt verb has no scope semantics (operators usually batch
// across scopes for replay). `Spans` is kept as raw JSON so
// validation can do precise forbidden-key detection without
// being fooled by a substring scan.
type otlpScopeSpanWire struct {
	Spans []json.RawMessage `json:"spans"`
}

// otlpKeyValueWire mirrors the OTLP `KeyValue` message. Decoded
// once and then stringified into the resolver's
// `map[string]string` view by [stringifyOTelAnyValue].
type otlpKeyValueWire struct {
	Key   string           `json:"key"`
	Value otlpAnyValueWire `json:"value"`
}

// otlpAnyValueWire mirrors the OTLP `AnyValue` union. Exactly
// one of the value fields should be set per the spec; we use
// pointer / sentinel checks (rather than a oneof discriminator)
// so a degenerate payload with multiple fields still produces
// a deterministic stringification.
//
// Numeric fields use [otlpStringOrInt] because the OTLP spec
// mandates string encoding for int64/uint64 values (precision)
// but several SDKs emit raw numbers; the handler accepts both.
//
// `arrayValue` / `kvlistValue` / `bytesValue` are KEPT as raw
// JSON so a future stage can decode them; [stringifyOTelAnyValue]
// emits a stable token for them today so the resolver always
// sees a non-empty string for any value the operator provided.
type otlpAnyValueWire struct {
	StringValue *string         `json:"stringValue,omitempty"`
	IntValue    *otlpStringOrInt `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	ArrayValue  json.RawMessage `json:"arrayValue,omitempty"`
	KvlistValue json.RawMessage `json:"kvlistValue,omitempty"`
	BytesValue  string          `json:"bytesValue,omitempty"`
}

// spanWire is the per-span typed decode target. Field tags use
// the OTel canonical lowerCamelCase shape (matching
// `otlphttp.go`'s `otlpSpan`) so a client emitting OTLP/HTTP
// can target both entry paths with the same serializer.
type spanWire struct {
	TraceID           string             `json:"traceId"`
	SpanID            string             `json:"spanId"`
	ParentSpanID      string             `json:"parentSpanId"`
	Name              string             `json:"name"`
	StartTimeUnixNano otlpStringOrInt    `json:"startTimeUnixNano"`
	EndTimeUnixNano   otlpStringOrInt    `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValueWire `json:"attributes"`
}

// otlpStringOrInt is a uint64 that accepts either a JSON number
// or a JSON string. OTel's OTLP/JSON spec mandates string
// encoding for uint64 fields (start/end_time_unix_nano, intValue)
// to preserve >2^53 precision across browsers; some SDKs
// (notably older OpenTelemetry-Go versions) emit raw numbers.
// Accept both so an operator's curl-style payload works
// regardless of which encoding their tool of choice produces.
type otlpStringOrInt uint64

// UnmarshalJSON handles both `"123"` and `123`. Empty / null
// values decode to 0; the handler treats 0 as "missing"
// downstream (see [validateSpan]).
func (s *otlpStringOrInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		if len(b) < 2 || b[len(b)-1] != '"' {
			return fmt.Errorf("otlpStringOrInt: malformed quoted value %q", string(b))
		}
		b = b[1 : len(b)-1]
		if len(b) == 0 {
			*s = 0
			return nil
		}
	}
	v, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("otlpStringOrInt: %w", err)
	}
	*s = otlpStringOrInt(v)
	return nil
}

// stringifyOTelAnyValue projects an OTLP AnyValue union onto a
// single deterministic string for the resolver's
// `map[string]string` contract. Mirrors `stringifyAnyValue` in
// `internal/spaningestor/otlphttp.go` so the two entry paths
// produce IDENTICAL attribute strings for the same input span.
//
// Resolution order (first non-empty wins):
//
//  1. stringValue (the most common attribute shape)
//  2. intValue / doubleValue / boolValue (typed scalars,
//     formatted with Go's default conversion)
//  3. arrayValue / kvlistValue / bytesValue (compound shapes,
//     emitted as the raw JSON bytes so downstream consumers
//     can inspect them and so the value is never silently
//     dropped)
//
// Returns "" only when EVERY field is unset; the handler emits
// a "" attribute value rather than dropping the key so
// downstream consumers can detect the empty-value case.
func stringifyOTelAnyValue(v otlpAnyValueWire) string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return strconv.FormatUint(uint64(*v.IntValue), 10)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case len(v.ArrayValue) > 0:
		return string(v.ArrayValue)
	case len(v.KvlistValue) > 0:
		return string(v.KvlistValue)
	case v.BytesValue != "":
		return v.BytesValue
	default:
		return ""
	}
}

// resolveIngestSpansRepoID applies the documented repo_id
// resolution precedence to a decoded POST /v1/spans request:
//
//  1. [MgmtRepoIDHeader] HTTP request header (most explicit).
//  2. Top-level `repo_id` JSON field on the request body.
//  3. [MgmtRepoIDResourceAttr] resource attribute on the
//     resourceSpans entries.
//
// At step 3 the helper scans every `resourceSpans[i].resource`
// entry: if MORE THAN ONE distinct mgmt.repo_id value is
// present the request is rejected at 400 with
// [IngestSpansRepoIDConflictCode] -- the handler refuses to
// split a single body across repos because that obscures the
// per-batch parent-map invariants the Span Ingestor relies on.
//
// Returns (repoID, "", "", true) on success; (zero, code, msg,
// false) when no hook resolved a valid UUID or two attributes
// conflicted. The caller renders the error envelope.
func resolveIngestSpansRepoID(r *http.Request, req *SpanIngestRequest) (string, string, string, bool) {
	if h := strings.TrimSpace(r.Header.Get(MgmtRepoIDHeader)); h != "" {
		normalized := strings.ToLower(h)
		if !reUUID.MatchString(normalized) {
			return "", "invalid_repo_id",
				MgmtRepoIDHeader + ": must be a valid UUID", false
		}
		return normalized, "", "", true
	}
	if body := strings.TrimSpace(req.RepoID); body != "" {
		normalized := strings.ToLower(body)
		if !reUUID.MatchString(normalized) {
			return "", "invalid_repo_id",
				"repo_id: must be a valid UUID", false
		}
		return normalized, "", "", true
	}
	// Walk every resourceSpans[i].resource looking for an
	// `mgmt.repo_id` attribute. Collect distinct non-empty
	// values; reject when two diverge.
	var fromAttr string
	for _, rs := range req.ResourceSpans {
		for _, kv := range rs.Resource.Attributes {
			if kv.Key != MgmtRepoIDResourceAttr {
				continue
			}
			val := strings.ToLower(strings.TrimSpace(stringifyOTelAnyValue(kv.Value)))
			if val == "" {
				continue
			}
			if !reUUID.MatchString(val) {
				return "", "invalid_repo_id",
					MgmtRepoIDResourceAttr + " resource attribute: must be a valid UUID", false
			}
			if fromAttr == "" {
				fromAttr = val
				continue
			}
			if fromAttr != val {
				return "", IngestSpansRepoIDConflictCode,
					"resourceSpans entries carry different " +
						MgmtRepoIDResourceAttr + " values; split the batch", false
			}
		}
	}
	if fromAttr != "" {
		return fromAttr, "", "", true
	}
	return "", IngestSpansRepoIDRequiredCode,
		"repo_id: required; set the top-level field, the " +
			MgmtRepoIDHeader + " header, or a " +
			MgmtRepoIDResourceAttr + " resource attribute", false
}

// handleIngestSpans is the POST /v1/spans dispatcher. Ordering:
//  1. Forwarder presence check (501 if unwired).
//  2. Body decode (with route-specific 4 MiB cap).
//  3. repo_id resolution + UUID validation (header > top-level
//     field > first resourceSpans' [MgmtRepoIDResourceAttr]).
//  4. Total-span-count + per-span validation (cheap, no DB) --
//     catches missing trace_id + forbidden outcome /
//     corrected_action early so a malformed request doesn't
//     burn a DB roundtrip.
//  5. Repo existence check (loadRepo) -- 404 on unknown.
//  6. Forward to ingestor.
//  7. Emit metric + log + response.
func (h *Handler) handleIngestSpans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.spanForwarder == nil {
		h.spansMetrics.Inc(IngestSpansStatusForwarderUnset, metricUnknownRepo)
		h.logger.Error("mgmtapi.ingest_spans.forwarder_unset",
			slog.String("op", "ingest_spans"),
			slog.String("path", r.URL.Path),
		)
		writeJSONError(w, http.StatusNotImplemented, IngestSpansForwarderUnavailableCode,
			"mgmt.ingest_spans is not enabled in this deployment: "+
				"configure a SpanForwarder on the handler Options "+
				"(set AGENT_MEMORY_SPAN_INGESTOR_URL)")
		return
	}

	// Decode with the per-route larger body cap (4 MiB) so a
	// healthy 1000-span batch isn't rejected by the global
	// 64 KiB default. We do this manually instead of using
	// decodeJSONBody so the cap stays route-local.
	var req SpanIngestRequest
	if !h.decodeJSONBodyWithCap(w, r, &req, IngestSpansMaxBodyBytes) {
		h.spansMetrics.Inc(IngestSpansStatusValidationError, metricUnknownRepo)
		return
	}

	repoID, repoCode, repoMsg, ok := resolveIngestSpansRepoID(r, &req)
	if !ok {
		h.spansMetrics.Inc(IngestSpansStatusValidationError, metricUnknownRepo)
		writeJSONError(w, http.StatusBadRequest, repoCode, repoMsg)
		return
	}

	if len(req.ResourceSpans) == 0 {
		h.spansMetrics.Inc(IngestSpansStatusValidationError, repoID)
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"resourceSpans: at least one ResourceSpans entry required")
		return
	}

	// Total span count BEFORE per-span decoding so an
	// over-sized batch fails fast.
	total := 0
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			total += len(ss.Spans)
		}
	}
	if total == 0 {
		h.spansMetrics.Inc(IngestSpansStatusValidationError, repoID)
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"spans: at least one span required")
		return
	}
	if total > IngestSpansMaxBatch {
		h.spansMetrics.Inc(IngestSpansStatusValidationError, repoID)
		writeJSONError(w, http.StatusBadRequest, "batch_too_large",
			fmt.Sprintf("spans: at most %d spans per batch (tech-spec §8.3)", IngestSpansMaxBatch))
		return
	}

	// Cheap validation pass first so malformed payloads do not
	// pay the DB-existence-check round trip. Walk every span
	// across every scope; report errors using a stable global
	// span index so an operator looking at the §6.2.2 message
	// can find the offending entry.
	forwarded := make([]ForwardedSpan, 0, total)
	idx := 0
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, raw := range ss.Spans {
				fs, code, msg, ok := validateSpan(raw, idx)
				if !ok {
					status := IngestSpansStatusValidationError
					if code == "forbidden_field" {
						status = IngestSpansStatusForbiddenField
					}
					h.spansMetrics.Inc(status, repoID)
					writeJSONError(w, http.StatusBadRequest, code, msg)
					return
				}
				forwarded = append(forwarded, fs)
				idx++
			}
		}
	}

	// Repo existence check. Mirrors the handleIngest pattern.
	if _, _, _, err := h.loadRepo(ctx, repoID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.spansMetrics.Inc(IngestSpansStatusRepoNotFound, repoID)
			writeJSONError(w, http.StatusNotFound, "repo_not_found",
				"no repo with the supplied repo_id")
			return
		}
		h.spansMetrics.Inc(IngestSpansStatusForwarderError, repoID)
		h.logger.Error("mgmtapi.ingest_spans.load_repo_failed",
			slog.String("op", "ingest_spans"),
			slog.String("repo_id", repoID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Forward to the Span Ingestor.
	if err := h.spanForwarder.ForwardSpans(ctx, repoID, forwarded); err != nil {
		switch {
		case errors.Is(err, ErrSpanIngestorBackpressure):
			h.spansMetrics.Inc(IngestSpansStatusBackpressure, repoID)
			w.Header().Set("Retry-After", strconv.Itoa(ingestSpansRetryAfterSeconds))
			writeJSONError(w, http.StatusServiceUnavailable,
				SpanIngestorBackpressureReason,
				"span ingestor queue is full; retry shortly")
			h.logger.Warn("mgmtapi.ingest_spans.backpressure",
				slog.String("op", "ingest_spans"),
				slog.String("repo_id", repoID),
				slog.Int("span_count", len(forwarded)),
			)
			return
		default:
			h.spansMetrics.Inc(IngestSpansStatusForwarderError, repoID)
			h.logger.Error("mgmtapi.ingest_spans.forwarder_failed",
				slog.String("op", "ingest_spans"),
				slog.String("repo_id", repoID),
				slog.Int("span_count", len(forwarded)),
				slog.String("error", err.Error()),
			)
			writeJSONError(w, http.StatusInternalServerError, "internal_error",
				"failed to forward spans to ingestor")
			return
		}
	}

	h.spansMetrics.Inc(IngestSpansStatusAccepted, repoID)
	subject, _ := SubjectFromContext(ctx)
	h.logger.Info("mgmtapi.ingest_spans.ok",
		slog.String("op", "ingest_spans"),
		slog.String("repo_id", repoID),
		slog.Int("span_count", len(forwarded)),
		slog.String("subject", subject),
		slog.Time("at", h.clock()),
	)
	writeJSONResponse(w, http.StatusAccepted, SpanIngestResponse{
		RepoID:        repoID,
		AcceptedSpans: len(forwarded),
	})
}

// validateSpan applies the §6.2.2 OTel-schema rules to one
// span. Returns (forwarded, "", "", true) on success;
// otherwise returns (zero, code, msg, false) with the error
// envelope the handler should write.
//
// Validation does TWO passes over the raw bytes:
//
//  1. Decode into `map[string]json.RawMessage` so we can
//     precisely detect a forbidden TOP-LEVEL key WITHOUT false
//     positives that a substring scan of the body bytes would
//     produce (e.g. an attribute value happening to contain the
//     word "outcome"). The forbidden keys are checked under
//     BOTH the architecture-doc snake_case spelling (`outcome`,
//     `corrected_action`) and the OTel lowerCamelCase
//     equivalent (`correctedAction`) so neither serializer can
//     sneak the field past the gate.
//  2. Decode into the typed [spanWire] shape so the per-field
//     checks have proper types.
//
// Per-span attribute KEYS named "outcome" or "corrected_action"
// are NOT rejected -- the §6.2.2 rule is about the span's
// top-level shape (the verb has no outcome semantics), not
// about every word in the payload.
func validateSpan(raw json.RawMessage, idx int) (ForwardedSpan, string, string, bool) {
	prefix := fmt.Sprintf("spans[%d]", idx)

	// Pass 1: forbidden-key detection.
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return ForwardedSpan{}, "invalid_request",
			fmt.Sprintf("%s: span must be a JSON object: %v", prefix, err), false
	}
	for _, forbidden := range []string{"outcome", "corrected_action", "correctedAction"} {
		if _, has := asMap[forbidden]; has {
			return ForwardedSpan{}, "forbidden_field",
				fmt.Sprintf(
					"%s: %q is not a span field (architecture.md §6.2.2: "+
						"mgmt.ingest_spans has no outcome/corrected_action semantics; "+
						"use mgmt.feedback for those)", prefix, forbidden), false
		}
	}

	// Pass 2: typed decode.
	var sp spanWire
	if err := json.Unmarshal(raw, &sp); err != nil {
		return ForwardedSpan{}, "invalid_request",
			fmt.Sprintf("%s: %v", prefix, err), false
	}

	// Required-field checks. Order chosen so the most-likely-
	// missing-from-a-typo field (trace_id) is reported first,
	// matching the §7.2 test scenario phrasing
	// ("validation: trace_id required"). Note: the wire field
	// name is OTel canonical `traceId`; the error MESSAGE uses
	// the conceptual snake_case naming so operators reading
	// architecture.md §6.2 recognize it.
	if strings.TrimSpace(sp.TraceID) == "" {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: trace_id required", prefix), false
	}
	if strings.TrimSpace(sp.SpanID) == "" {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: span_id required", prefix), false
	}
	if strings.TrimSpace(sp.Name) == "" {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: name required", prefix), false
	}
	if sp.StartTimeUnixNano == 0 {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: start_time_unix_nano required", prefix), false
	}
	if sp.EndTimeUnixNano == 0 {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: end_time_unix_nano required", prefix), false
	}

	// W3C Trace Context shape checks: trace_id is 16 bytes
	// (32 hex chars), span_id is 8 bytes (16 hex chars), and
	// neither may be all-zero (the "invalid" sentinel per
	// W3C). Lower-case hex is canonical for OTel-emitted IDs;
	// we accept both cases and normalize to lower for the
	// downstream resolver, which keys off the lower-case
	// fingerprint.
	traceID, ok := normalizeOTelID(sp.TraceID, 32)
	if !ok {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf(
				"%s: trace_id must be 32 hex characters (W3C Trace Context)", prefix), false
	}
	spanID, ok := normalizeOTelID(sp.SpanID, 16)
	if !ok {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf(
				"%s: span_id must be 16 hex characters (W3C Trace Context)", prefix), false
	}
	parentSpanID := ""
	if strings.TrimSpace(sp.ParentSpanID) != "" {
		ps, ok := normalizeOTelID(sp.ParentSpanID, 16)
		if !ok {
			return ForwardedSpan{}, "invalid_span",
				fmt.Sprintf(
					"%s: parent_span_id must be 16 hex characters when present", prefix), false
		}
		parentSpanID = ps
	}

	// Timestamp sanity. Reject values that would overflow
	// when converted to time.Unix's int64-nanos shape
	// downstream -- the resolver / aggregator rely on
	// time.Time arithmetic that would silently wrap negative
	// for nanos > MaxInt64.
	if uint64(sp.StartTimeUnixNano) > math.MaxInt64 {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: start_time_unix_nano out of range", prefix), false
	}
	if uint64(sp.EndTimeUnixNano) > math.MaxInt64 {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: end_time_unix_nano out of range", prefix), false
	}
	if sp.EndTimeUnixNano < sp.StartTimeUnixNano {
		return ForwardedSpan{}, "invalid_span",
			fmt.Sprintf("%s: end_time_unix_nano must be >= start_time_unix_nano", prefix), false
	}

	// Stringify attributes per the OTel AnyValue union. The
	// resolver contract wants `map[string]string`; numbers,
	// bools, and compound shapes are flattened via
	// [stringifyOTelAnyValue] which mirrors `otlphttp.go`'s
	// `stringifyAnyValue` so both entry paths produce
	// identical attribute strings for the same input span.
	var attrs map[string]string
	if len(sp.Attributes) > 0 {
		attrs = make(map[string]string, len(sp.Attributes))
		for _, kv := range sp.Attributes {
			if kv.Key == "" {
				continue
			}
			attrs[kv.Key] = stringifyOTelAnyValue(kv.Value)
		}
	}

	return ForwardedSpan{
		TraceID:           traceID,
		SpanID:            spanID,
		ParentSpanID:      parentSpanID,
		Name:              sp.Name,
		StartTimeUnixNano: uint64(sp.StartTimeUnixNano),
		EndTimeUnixNano:   uint64(sp.EndTimeUnixNano),
		Attributes:        attrs,
	}, "", "", true
}

// normalizeOTelID validates an OTel trace_id / span_id /
// parent_span_id shape and returns the lower-case form.
// Requirements:
//
//   - Exact length match (`n` hex chars).
//   - Every character in [0-9a-fA-F].
//   - NOT all-zero (the W3C "invalid id" sentinel).
//
// Returns the lower-case normalized id and true on success;
// returns ("", false) otherwise.
func normalizeOTelID(s string, n int) (string, bool) {
	if len(s) != n {
		return "", false
	}
	out := make([]byte, n)
	allZero := true
	for i := 0; i < n; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			out[i] = c
		case c >= 'a' && c <= 'f':
			out[i] = c
		case c >= 'A' && c <= 'F':
			out[i] = c + ('a' - 'A')
		default:
			return "", false
		}
		if out[i] != '0' {
			allZero = false
		}
	}
	if allZero {
		return "", false
	}
	return string(out), true
}

// decodeJSONBodyWithCap is decodeJSONBody with a per-call body
// cap. The Stage 7.2 `mgmt.ingest_spans` route needs a larger
// cap (4 MiB) than the mgmt-api global default (64 KiB) to
// accept the §8.3-sized 1000-span batches; we factor this as a
// separate helper rather than mutate decodeJSONBody so the
// other verbs keep the tighter cap as a defence-in-depth
// guard.
//
// Implemented as a stateless copy of decodeJSONBody so the per-
// call cap does NOT race against concurrent requests on other
// verbs (mutating h.maxBody under a defer would corrupt the
// global default for in-flight requests since [Handler] is
// documented as safe for concurrent use).
func (h *Handler) decodeJSONBodyWithCap(w http.ResponseWriter, r *http.Request, dst any, capBytes int64) bool {
	if capBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, capBytes)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mxErr *http.MaxBytesError
		if errors.As(err, &mxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge,
				"body_too_large",
				fmt.Sprintf("request body exceeds %d bytes", capBytes))
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request",
			"failed to read request body")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json",
			"request body is not valid JSON")
		return false
	}
	return true
}
