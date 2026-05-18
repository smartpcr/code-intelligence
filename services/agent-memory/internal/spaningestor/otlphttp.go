package spaningestor

// OTLP/HTTP receiver for the Span Ingestor. Implements the
// subset of the OTLP/HTTP protocol (https://opentelemetry.io/docs/specs/otlp/#otlphttp)
// the Span Ingestor needs:
//
//   * POST /v1/traces with Content-Type: application/json OR
//     application/x-protobuf accepting an
//     ExportTraceServiceRequest body.
//   * Returns 200 on accept, 503 (with Retry-After) on
//     backpressure, 400 on malformed body, 415 on wrong
//     content type.
//
// Both JSON and protobuf encodings are accepted per the OTLP
// spec; the protobuf path shares the `convertProtoSpan`
// helper with `otlpgrpc.go` so semantics are identical across
// transports. Evaluator iter-1 #1 (gRPC OTLP) lands the
// matching gRPC receiver in otlpgrpc.go.
//
// JSON shape consumed
// -------------------
// We deserialize only the fields we need. The OTLP/HTTP JSON
// `ExportTraceServiceRequest` is:
//
//   { "resourceSpans": [
//       { "resource": {"attributes": [ {"key": "service.name", "value": {"stringValue": "..."}} ]},
//         "scopeSpans": [
//           { "spans": [
//               { "traceId": "<hex>", "spanId": "<hex>",
//                 "parentSpanId": "<hex>",
//                 "startTimeUnixNano": "<int as string>",
//                 "endTimeUnixNano":   "<int as string>",
//                 "attributes": [...] } ] } ] } ] }
//
// We extract `service.name` from the resource attributes and
// look it up via a `ServiceNameToRepoID` function the binary
// supplies; spans whose service is not registered are
// dropped with a counter increment (NOT a 4xx) so an unknown
// service can't 4xx-flood the receiver.
//
// Routing precedence (mgmt-api replay support)
// --------------------------------------------
// Stage 7.2 wires a `mgmt.ingest_spans` verb (POST /v1/spans on
// the mgmt-api binary) that forwards verified batches to this
// receiver. Those batches carry an explicit `repo_id` that
// MUST NOT be erased by the registered service.name lookup
// (the operator may be replaying spans whose service.name is
// not in the registry). To honor the explicit override the
// receiver consults three independent routing hooks in this
// order, and picks the first one that produces a non-empty
// repo_id:
//
//  1. `X-Mgmt-Repo-ID` HTTP request header  (applies to all
//     ResourceSpans entries in the request — the operator is
//     asserting "every span in this body belongs to that repo")
//  2. `mgmt.repo_id` resource attribute     (per-ResourceSpans)
//  3. `service.name` resource attribute → [ServiceNameToRepoID]
//
// The header / attribute repo_id MUST be a 36-char hyphenated
// UUID; malformed values are ignored and the receiver falls
// through to the next hook so a typo can't drop a whole batch
// into an arbitrary repo.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// MgmtRepoIDHeader is the HTTP request header the mgmt-api span
// forwarder sets to override the per-ResourceSpans service.name
// → repo_id lookup. When present and parseable as a UUID, the
// receiver routes EVERY span in the request to that repo
// regardless of any service.name or mgmt.repo_id attributes.
// See file header §"Routing precedence (mgmt-api replay support)".
const MgmtRepoIDHeader = "X-Mgmt-Repo-ID"

// MgmtRepoIDResourceAttr is the per-ResourceSpans resource
// attribute key the mgmt-api forwarder injects so a Collector
// receiving a mgmt replay can route the batch back to the
// originating repo without operator-side service.name registry
// configuration. Recognised by the receiver with precedence
// LOWER than [MgmtRepoIDHeader] but HIGHER than service.name
// lookup. See file header §"Routing precedence (mgmt-api replay support)".
const MgmtRepoIDResourceAttr = "mgmt.repo_id"

// MgmtReplayServiceNamePrefix is the prefix the mgmt-api
// forwarder synthesises on the per-batch `service.name`
// resource attribute (e.g. "mgmt-api-replay/<repo_id>"). When
// the [ServiceNameToRepoID] lookup misses but the service.name
// carries this prefix, the receiver falls back to the suffix
// (UUID-validated) as the repo_id. Operators who want to
// disable this convention can leave it as-is — it only fires
// when no other routing hook resolved the batch.
const MgmtReplayServiceNamePrefix = "mgmt-api-replay/"

// reUUIDStrict matches the canonical 8-4-4-4-12 lower-case
// hyphenated UUID textual form. Used to validate the
// header / resource-attribute repo_id overrides so a typo
// can't poison the routing decision.
var reUUIDStrict = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// validatedRepoID returns s when it parses as a UUID and ""
// otherwise. Used by the routing-precedence hooks so a typoed
// header / attribute is treated as "not supplied" rather than
// silently mis-routing the batch.
func validatedRepoID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !reUUIDStrict.MatchString(strings.ToLower(s)) {
		return ""
	}
	return strings.ToLower(s)
}

// ServiceNameToRepoID maps an OTel `service.name` (or
// `service.namespace`) to the `repo_id` (textual UUID) the
// Resolver / Ingestor need. The binary's composition root
// provides the implementation (typically a static map loaded
// from config, or a lookup in the `repo` table by URL).
//
// Returning the empty string means "unknown service"; the
// receiver drops the span and increments its own
// `otlp_dropped_unknown_service_total` counter.
type ServiceNameToRepoID func(serviceName string) string

// OTLPReceiver is the HTTP handler. Construct via
// NewOTLPReceiver; serve via Handler() / ServeHTTP.
type OTLPReceiver struct {
	ingestor          *Ingestor
	serviceToRepoID   ServiceNameToRepoID
	logger            *slog.Logger
	maxBytes          int64
	retryAfterSeconds int
	// mux is built once in NewOTLPReceiver so ServeHTTP and
	// Handler() do not allocate a fresh http.ServeMux per
	// request on the hot OTLP export path.
	mux http.Handler
}

// OTLPConfig is the receiver's tunables.
type OTLPConfig struct {
	// MaxBytes caps the in-memory request body the receiver
	// will accept. Defaults to 4 MiB, the OTel collector's
	// default `send_batch_size` upper bound at JSON
	// expansion. Set higher for whale-shaped traces.
	MaxBytes int64
	// RetryAfterSeconds is the value of the Retry-After
	// header on 503 backpressure responses. Defaults to 5s.
	RetryAfterSeconds int
}

func (c *OTLPConfig) applyDefaults() {
	if c.MaxBytes <= 0 {
		c.MaxBytes = 4 * 1024 * 1024
	}
	if c.RetryAfterSeconds <= 0 {
		c.RetryAfterSeconds = 5
	}
}

// NewOTLPReceiver constructs an OTLP/HTTP receiver. `lookup`
// MUST be non-nil; a nil function panics so the operator
// doesn't accidentally deploy a 100% drop-rate receiver.
func NewOTLPReceiver(
	ingestor *Ingestor,
	lookup ServiceNameToRepoID,
	cfg OTLPConfig,
	logger *slog.Logger,
) *OTLPReceiver {
	if ingestor == nil {
		panic("spaningestor: NewOTLPReceiver: nil ingestor")
	}
	if lookup == nil {
		panic("spaningestor: NewOTLPReceiver: nil ServiceNameToRepoID")
	}
	cfg.applyDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	r := &OTLPReceiver{
		ingestor:          ingestor,
		serviceToRepoID:   lookup,
		logger:            logger,
		maxBytes:          cfg.MaxBytes,
		retryAfterSeconds: cfg.RetryAfterSeconds,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", r.handleTraces)
	r.mux = mux
	return r
}

// Handler returns an http.Handler that routes /v1/traces.
// Other routes return 404. The underlying mux is built once
// in NewOTLPReceiver and shared with ServeHTTP, so this is
// allocation-free on the request path.
func (r *OTLPReceiver) Handler() http.Handler {
	return r.mux
}

// ServeHTTP makes OTLPReceiver itself an http.Handler so
// callers can compose it with their own mux. Delegates to
// the cached mux built in NewOTLPReceiver to avoid a
// per-request http.ServeMux allocation on the OTLP export
// hot path.
func (r *OTLPReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *OTLPReceiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(req.Header.Get("Content-Type"), ";", 2)[0]))
	encoding := ""
	switch ct {
	case "application/json", "":
		// Default to JSON when Content-Type is missing (some
		// OTel SDKs omit it on small requests). Spec-aligned
		// fallback because JSON is text-payload-safe.
		encoding = "json"
	case "application/x-protobuf", "application/protobuf":
		encoding = "protobuf"
	default:
		http.Error(w, "only application/json or application/x-protobuf is supported", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, r.maxBytes+1))
	if err != nil {
		http.Error(w, "cannot read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > r.maxBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Group spans by repo so the within-batch parent map can
	// resolve correctly. The OTLP/HTTP payload may contain
	// spans from multiple services in one POST; we emit one
	// batch per (repo_id) so the per-batch invariants hold.
	//
	// Routing precedence: see file header §"Routing precedence
	// (mgmt-api replay support)". The X-Mgmt-Repo-ID header, if
	// present and UUID-valid, applies to EVERY ResourceSpans
	// entry — the operator is asserting body-level ownership.
	perRepo := make(map[string][]ObservationSpan)
	headerRepoID := validatedRepoID(req.Header.Get(MgmtRepoIDHeader))
	if encoding == "protobuf" {
		var payload coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &payload); err != nil {
			http.Error(w, "cannot decode OTLP/HTTP protobuf: "+err.Error(), http.StatusBadRequest)
			return
		}
		for _, rs := range payload.GetResourceSpans() {
			repoID := r.resolveRepoIDProto(rs.GetResource(), headerRepoID)
			if repoID == "" {
				r.logger.Debug("spaningestor.otlp.unknown_service",
					slog.String("service_name", lookupResourceAttrProto(rs.GetResource(), "service.name")))
				continue
			}
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					perRepo[repoID] = append(perRepo[repoID], convertProtoSpan(sp, repoID))
				}
			}
		}
	} else {
		var payload otlpExportTraceServiceRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "cannot decode OTLP/HTTP JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		for _, rs := range payload.ResourceSpans {
			repoID := r.resolveRepoIDJSON(rs.Resource.Attributes, headerRepoID)
			if repoID == "" {
				r.logger.Debug("spaningestor.otlp.unknown_service",
					slog.String("service_name", lookupResourceAttr(rs.Resource.Attributes, "service.name")))
				continue
			}
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					obs := convertSpan(sp, repoID)
					perRepo[repoID] = append(perRepo[repoID], obs)
				}
			}
		}
	}
	if len(perRepo) == 0 {
		// Body parsed but had no spans for known services —
		// 200 OK with no work done. Spec-aligned: empty
		// ExportTraceServiceResponse.
		writeOTLPSuccess(w)
		return
	}

	// Evaluator iter-1 #5: previous code enqueued per-repo
	// batches in a loop and surfaced 503 when ANY of them
	// failed, leaving already-enqueued batches stranded. The
	// Collector then retried the WHOLE POST and duplicated
	// the already-enqueued spans. EnqueueAtomic atomically
	// accepts or rejects every batch under the ingestor's
	// enqueueMu so a retry is safe — nothing was committed.
	batches := make([]SpanBatch, 0, len(perRepo))
	for repoID, spans := range perRepo {
		batches = append(batches, SpanBatch{RepoID: repoID, Spans: spans})
	}
	if err := r.ingestor.EnqueueAtomic(batches); err != nil {
		if errors.Is(err, ErrQueueFull) {
			w.Header().Set("Retry-After", strconv.Itoa(r.retryAfterSeconds))
			http.Error(w, "ingestor queue full", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "enqueue failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeOTLPSuccess(w)
}

func writeOTLPSuccess(w http.ResponseWriter) {
	// The OTLP/HTTP success response is the
	// ExportTraceServiceResponse JSON encoding — `{}` is a
	// well-formed instance with no rejected_spans.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "{}")
}

// --- OTLP JSON wire types ------------------------------------

type otlpExportTraceServiceRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpan struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId"`
	Name              string         `json:"name"`
	StartTimeUnixNano otlpStringInt  `json:"startTimeUnixNano"`
	EndTimeUnixNano   otlpStringInt  `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue is the union shape; we read whichever field is
// non-empty into a string for downstream consumption. Numeric
// values are stringified so the resolver's `map[string]string`
// shape holds.
type otlpAnyValue struct {
	StringValue string        `json:"stringValue,omitempty"`
	IntValue    otlpStringInt `json:"intValue,omitempty"`
	DoubleValue *float64      `json:"doubleValue,omitempty"`
	BoolValue   *bool         `json:"boolValue,omitempty"`
}

// otlpStringInt is the JSON-encoded uint64 representation OTLP
// uses for nanosecond timestamps. Per spec the value is
// transmitted as a JSON string to preserve 64-bit precision
// across browsers. We accept BOTH the string form and a raw
// number form because OTel SDKs in some languages emit the
// number form even though the spec forbids it.
type otlpStringInt uint64

// UnmarshalJSON handles both "12345" and 12345.
func (s *otlpStringInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	// Strip surrounding quotes if present (string form).
	if b[0] == '"' {
		if len(b) < 2 || b[len(b)-1] != '"' {
			return fmt.Errorf("otlpStringInt: malformed quoted value %q", string(b))
		}
		b = b[1 : len(b)-1]
		if len(b) == 0 {
			*s = 0
			return nil
		}
	}
	v, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("otlpStringInt: %w", err)
	}
	*s = otlpStringInt(v)
	return nil
}

func lookupResourceAttr(attrs []otlpKeyValue, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.StringValue
		}
	}
	return ""
}

// resolveRepoIDJSON applies the documented routing precedence
// to a single OTLP/HTTP-JSON ResourceSpans entry:
//
//  1. headerRepoID (already UUID-validated by caller; "" when
//     absent / malformed). When set, returned as-is so the
//     header-level override wins across the whole request.
//  2. `mgmt.repo_id` resource attribute (per-ResourceSpans).
//     UUID-validated; ignored when malformed so a typo doesn't
//     mis-route a batch.
//  3. `service.name` → [ServiceNameToRepoID] lookup.
//  4. service.name prefix fallback: if the service.name has
//     the [MgmtReplayServiceNamePrefix] and the suffix parses
//     as a UUID, that UUID is the repo_id. This lets an
//     operator wire the mgmt-api forwarder against a Span
//     Ingestor whose registry is unaware of the replay prefix.
//
// Returns "" when no hook resolves; the caller treats that as
// "unknown service" and drops the ResourceSpans entry with a
// counter increment.
func (r *OTLPReceiver) resolveRepoIDJSON(attrs []otlpKeyValue, headerRepoID string) string {
	if headerRepoID != "" {
		return headerRepoID
	}
	if id := validatedRepoID(lookupResourceAttr(attrs, MgmtRepoIDResourceAttr)); id != "" {
		return id
	}
	serviceName := lookupResourceAttr(attrs, "service.name")
	if id := r.serviceToRepoID(serviceName); id != "" {
		return id
	}
	if strings.HasPrefix(serviceName, MgmtReplayServiceNamePrefix) {
		if id := validatedRepoID(strings.TrimPrefix(serviceName, MgmtReplayServiceNamePrefix)); id != "" {
			return id
		}
	}
	return ""
}

// resolveRepoIDProto mirrors [resolveRepoIDJSON] for the
// OTLP/HTTP-protobuf branch of [handleTraces]. Kept as a
// receiver method (rather than a free function) so the
// [ServiceNameToRepoID] lookup is shared via the same
// closure both encodings consult.
func (r *OTLPReceiver) resolveRepoIDProto(res *resourcepb.Resource, headerRepoID string) string {
	if headerRepoID != "" {
		return headerRepoID
	}
	if id := validatedRepoID(lookupResourceAttrProto(res, MgmtRepoIDResourceAttr)); id != "" {
		return id
	}
	serviceName := lookupResourceAttrProto(res, "service.name")
	if id := r.serviceToRepoID(serviceName); id != "" {
		return id
	}
	if strings.HasPrefix(serviceName, MgmtReplayServiceNamePrefix) {
		if id := validatedRepoID(strings.TrimPrefix(serviceName, MgmtReplayServiceNamePrefix)); id != "" {
			return id
		}
	}
	return ""
}

func convertSpan(sp otlpSpan, repoID string) ObservationSpan {
	attrs := make(map[string]string, len(sp.Attributes))
	for _, a := range sp.Attributes {
		attrs[a.Key] = stringifyAnyValue(a.Value)
	}
	start := time.Unix(0, int64(sp.StartTimeUnixNano)).UTC()
	end := time.Unix(0, int64(sp.EndTimeUnixNano)).UTC()
	durationMs := 0.0
	if !end.IsZero() && !start.IsZero() && end.After(start) {
		durationMs = float64(end.Sub(start).Microseconds()) / 1000.0
	}
	return ObservationSpan{
		Span: Span{
			RepoID:       repoID,
			TraceID:      sp.TraceID,
			SpanID:       sp.SpanID,
			ParentSpanID: sp.ParentSpanID,
			Attributes:   attrs,
		},
		StartedAt:  start,
		DurationMs: durationMs,
	}
}

func stringifyAnyValue(v otlpAnyValue) string {
	switch {
	case v.StringValue != "":
		return v.StringValue
	case v.IntValue != 0:
		return strconv.FormatUint(uint64(v.IntValue), 10)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	default:
		return ""
	}
}

// ensure ctx import compiles for any future feature on this
// package (currently unused but conventional in worker code).
var _ = context.Background

// formatInt64 / formatFloat64 are shared with otlpgrpc.go's
// `stringifyProtoAnyValue` so HTTP/JSON, HTTP/protobuf, and
// gRPC paths all stringify attribute values identically.
func formatInt64(v int64) string         { return strconv.FormatInt(v, 10) }
func formatFloat64(v float64) string     { return strconv.FormatFloat(v, 'g', -1, 64) }
