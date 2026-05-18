package mgmtapi

// HTTPSpanForwarder is the production [SpanForwarder]
// implementation the `cmd/mgmt-api` binary wires when
// AGENT_MEMORY_SPAN_INGESTOR_URL is configured.
//
// The forwarder rebuilds a canonical OTLP/HTTP JSON
// `ExportTraceServiceRequest` from the flattened [ForwardedSpan]
// slice and POSTs it at `<base>/v1/traces` with
// `Content-Type: application/json` -- byte-for-byte the same
// shape the Span Ingestor's OTLP receiver consumes in
// `internal/spaningestor/otlphttp.go`. This guarantees the
// mgmt-api alternative entry path (architecture.md §6.2.1) and
// the Collector primary path queue spans through the same
// resolver -> ingestor pipeline; the only difference is the
// "where did the batch enter the system" provenance carried in
// the resource attributes.
//
// Routing
// -------
// The Span Ingestor's receiver maps spans to a `repo_id` by
// looking up `service.name` in its [ServiceNameToRepoID]
// registry. To preserve the operator's explicit `repo_id` (so
// mgmt-api-replayed spans land in the right repo even when the
// caller did not know the registered service name), the
// forwarder INJECTS two resource attributes into every batch:
//
//	service.name    = "mgmt-api-replay/<repo_id>"
//	mgmt.repo_id    = "<repo_id>"
//
// Deployments whose Span Ingestor registry recognises either
// the `mgmt-api-replay/<uuid>` service.name convention OR the
// `mgmt.repo_id` resource attribute will pick the routing up
// without further wiring. Deployments that need a different
// convention can subclass via the [HTTPSpanForwarderConfig.ServiceNamePrefix]
// hook.
//
// Backpressure
// ------------
// The receiver returns 503 with `Retry-After` when its input
// queue is at capacity (see `otlphttp.go` writeOTLPBackpressure).
// The forwarder maps that response to [ErrSpanIngestorBackpressure]
// so the handler can surface the agreed §C22 envelope.
//
// Errors
// ------
// All other non-2xx responses become a wrapped error the handler
// renders as 500 internal_error. The forwarder caps the error
// body it reads / returns to keep a misbehaving downstream from
// blowing memory.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultSpanIngestorPath is the OTLP/HTTP traces sub-path
// that the Span Ingestor's receiver registers at. Joined onto
// the configured base URL when [HTTPSpanForwarderConfig.Path]
// is empty.
const DefaultSpanIngestorPath = "/v1/traces"

// defaultMgmtReplayServiceNamePrefix is the service.name prefix
// the forwarder synthesizes for batches submitted via the
// mgmt-api alternative entry path. Picked to be DISTINCT from
// any real service name an OTel SDK would emit so operators can
// filter on it in their tracing UI ("show me the spans that
// landed via manual replay").
const defaultMgmtReplayServiceNamePrefix = "mgmt-api-replay/"

// defaultForwarderHTTPTimeout caps the per-request timeout on
// the forward call. Sized so a saturated Span Ingestor that
// stalls accepting the body doesn't hold the mgmt-api handler
// goroutine open indefinitely. The OTLP receiver's body-read
// timeout is on the order of seconds; we give it a generous 30s.
const defaultForwarderHTTPTimeout = 30 * time.Second

// maxForwarderErrorBodyBytes caps the bytes the forwarder
// reads off an error response. Keeps a misbehaving downstream
// from streaming megabytes of HTML into the audit log.
const maxForwarderErrorBodyBytes = 4 * 1024

// HTTPSpanForwarderConfig carries the production forwarder's
// tunables. All fields are optional; the constructor applies
// safe defaults.
type HTTPSpanForwarderConfig struct {
	// BaseURL is the Span Ingestor's HTTP base URL, e.g.
	// "http://span-ingestor:4318". REQUIRED. The forwarder
	// joins [Path] onto this URL to form the POST target.
	BaseURL string

	// Path is the OTLP/HTTP traces sub-path. Defaults to
	// [DefaultSpanIngestorPath] when empty.
	Path string

	// HTTPClient is the http.Client the forwarder uses.
	// Defaults to a fresh client with [defaultForwarderHTTPTimeout]
	// when nil.
	HTTPClient *http.Client

	// Logger receives structured forwarder events. Defaults
	// to the package's [silentLogger] when nil.
	Logger *slog.Logger

	// ServiceNamePrefix is prepended to the per-batch
	// `service.name` resource attribute the forwarder
	// synthesizes (the suffix is the repo_id). Defaults to
	// [defaultMgmtReplayServiceNamePrefix] when empty.
	ServiceNamePrefix string
}

// HTTPSpanForwarder is a production [SpanForwarder] backed by
// the Span Ingestor's OTLP/HTTP endpoint.
type HTTPSpanForwarder struct {
	targetURL         string
	httpClient        *http.Client
	logger            *slog.Logger
	serviceNamePrefix string
}

// NewHTTPSpanForwarder validates the config and returns a
// ready-to-use forwarder. Returns an error when BaseURL is
// missing or unparseable so the boot path fails closed.
func NewHTTPSpanForwarder(cfg HTTPSpanForwarderConfig) (*HTTPSpanForwarder, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("HTTPSpanForwarder: BaseURL is required")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("HTTPSpanForwarder: BaseURL %q: %w", cfg.BaseURL, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("HTTPSpanForwarder: BaseURL %q must be http or https", cfg.BaseURL)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("HTTPSpanForwarder: BaseURL %q is missing host", cfg.BaseURL)
	}

	path := cfg.Path
	if path == "" {
		path = DefaultSpanIngestorPath
	}
	target := strings.TrimRight(base.String(), "/") + "/" + strings.TrimLeft(path, "/")

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultForwarderHTTPTimeout}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	prefix := cfg.ServiceNamePrefix
	if prefix == "" {
		prefix = defaultMgmtReplayServiceNamePrefix
	}

	return &HTTPSpanForwarder{
		targetURL:         target,
		httpClient:        client,
		logger:            logger,
		serviceNamePrefix: prefix,
	}, nil
}

// TargetURL exposes the resolved POST target for diagnostics
// and tests. The returned string is the BaseURL + Path with a
// single slash separator, e.g. `http://span-ingestor:4318/v1/traces`.
func (f *HTTPSpanForwarder) TargetURL() string { return f.targetURL }

// ForwardSpans implements [SpanForwarder]. Builds an OTLP/HTTP
// JSON ExportTraceServiceRequest from the flattened
// [ForwardedSpan] slice and POSTs it at the configured
// target URL.
//
// Response handling:
//   - 2xx               -> nil
//   - 503 with Retry-After -> [ErrSpanIngestorBackpressure]
//   - 4xx               -> wrapped error (handler returns 500)
//   - 5xx (not 503)     -> wrapped error
//   - transport error   -> wrapped error
func (f *HTTPSpanForwarder) ForwardSpans(ctx context.Context, repoID string, spans []ForwardedSpan) error {
	if len(spans) == 0 {
		return nil
	}

	payload := f.buildOTLPRequest(repoID, spans)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("HTTPSpanForwarder: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("HTTPSpanForwarder: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Carry the operator-supplied repo_id as an HTTP header
	// so a custom receiver that wants to route on it (instead
	// of on service.name) has a fast path. The Span Ingestor's
	// current receiver ignores unknown headers, so this is a
	// safe extension.
	req.Header.Set("X-Mgmt-Repo-ID", repoID)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTPSpanForwarder: POST %s: %w", f.targetURL, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusServiceUnavailable:
		// Honour the Retry-After header if present so the
		// caller's polling loop can synchronize, but the
		// sentinel error is what triggers the handler's
		// 503-passthrough.
		f.logger.Warn("mgmtapi.forwarder.backpressure",
			slog.String("repo_id", repoID),
			slog.Int("span_count", len(spans)),
			slog.String("retry_after", resp.Header.Get("Retry-After")),
		)
		return ErrSpanIngestorBackpressure
	default:
		// Bounded read so a misbehaving downstream can't
		// stream megabytes into our audit log.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxForwarderErrorBodyBytes))
		f.logger.Error("mgmtapi.forwarder.bad_status",
			slog.String("repo_id", repoID),
			slog.Int("span_count", len(spans)),
			slog.Int("status", resp.StatusCode),
			slog.String("body_snippet", string(body)),
		)
		return fmt.Errorf("HTTPSpanForwarder: POST %s: HTTP %d", f.targetURL, resp.StatusCode)
	}
}

// buildOTLPRequest serializes [ForwardedSpan] back into the
// canonical OTLP/HTTP `ExportTraceServiceRequest` shape the
// Span Ingestor's receiver expects, injecting the routing
// `service.name` + `mgmt.repo_id` resource attributes.
//
// We bundle ALL spans in a SINGLE resourceSpans / scopeSpans
// pair because [ForwardedSpan] has lost the original per-span
// scope grouping. The Span Ingestor's receiver iterates the
// flattened span list (it does not key off scope), so this
// flattening is byte-equivalent at the resolver's input.
func (f *HTTPSpanForwarder) buildOTLPRequest(repoID string, spans []ForwardedSpan) otlpExportTraceServiceRequestWire {
	otlpSpans := make([]otlpOutgoingSpan, 0, len(spans))
	for _, s := range spans {
		attrs := make([]otlpOutgoingKV, 0, len(s.Attributes))
		// Sort attribute keys for deterministic serialization
		// so the body bytes (and any operator-side request-
		// hash) are stable across runs.
		keys := make([]string, 0, len(s.Attributes))
		for k := range s.Attributes {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			attrs = append(attrs, otlpOutgoingKV{
				Key:   k,
				Value: otlpOutgoingValue{StringValue: stringPtr(s.Attributes[k])},
			})
		}
		otlpSpans = append(otlpSpans, otlpOutgoingSpan{
			TraceID:           s.TraceID,
			SpanID:            s.SpanID,
			ParentSpanID:      s.ParentSpanID,
			Name:              s.Name,
			StartTimeUnixNano: strconv.FormatUint(s.StartTimeUnixNano, 10),
			EndTimeUnixNano:   strconv.FormatUint(s.EndTimeUnixNano, 10),
			Attributes:        attrs,
		})
	}

	resourceAttrs := []otlpOutgoingKV{
		{
			Key:   "service.name",
			Value: otlpOutgoingValue{StringValue: stringPtr(f.serviceNamePrefix + repoID)},
		},
		{
			Key:   "mgmt.repo_id",
			Value: otlpOutgoingValue{StringValue: stringPtr(repoID)},
		},
	}

	return otlpExportTraceServiceRequestWire{
		ResourceSpans: []otlpOutgoingResourceSpans{
			{
				Resource: otlpOutgoingResource{Attributes: resourceAttrs},
				ScopeSpans: []otlpOutgoingScopeSpans{
					{Spans: otlpSpans},
				},
			},
		},
	}
}

// -----------------------------------------------------------
// Outgoing OTLP/JSON encode types. SEPARATE from the decode
// side ([otlpResourceSpansWire] etc) so the field tags can use
// `omitempty` aggressively on the encode side (smaller wire
// payload) while staying tolerant on the decode side (we
// accept any non-zero value).
// -----------------------------------------------------------

type otlpExportTraceServiceRequestWire struct {
	ResourceSpans []otlpOutgoingResourceSpans `json:"resourceSpans"`
}

type otlpOutgoingResourceSpans struct {
	Resource   otlpOutgoingResource     `json:"resource"`
	ScopeSpans []otlpOutgoingScopeSpans `json:"scopeSpans"`
}

type otlpOutgoingResource struct {
	Attributes []otlpOutgoingKV `json:"attributes"`
}

type otlpOutgoingScopeSpans struct {
	Spans []otlpOutgoingSpan `json:"spans"`
}

type otlpOutgoingSpan struct {
	TraceID           string           `json:"traceId"`
	SpanID            string           `json:"spanId"`
	ParentSpanID      string           `json:"parentSpanId,omitempty"`
	Name              string           `json:"name"`
	StartTimeUnixNano string           `json:"startTimeUnixNano"`
	EndTimeUnixNano   string           `json:"endTimeUnixNano"`
	Attributes        []otlpOutgoingKV `json:"attributes,omitempty"`
}

type otlpOutgoingKV struct {
	Key   string            `json:"key"`
	Value otlpOutgoingValue `json:"value"`
}

// otlpOutgoingValue carries the OTLP AnyValue union. The
// forwarder only ever emits `stringValue` (since the forwarded
// shape has already been stringified by [validateSpan]), but
// the struct keeps the other variants as optional fields so a
// future stage that wants typed attributes can populate them
// without a wire-shape break.
type otlpOutgoingValue struct {
	StringValue *string `json:"stringValue,omitempty"`
}

// stringPtr is a tiny helper so the literal-attribute construction
// stays readable without a one-off &-of-temporary dance per field.
func stringPtr(s string) *string { return &s }

// sortStrings is a tiny wrapper around the stdlib's sort.Strings
// kept inline so this file does not pick up an extra import just
// to sort a single attribute-key slice.
func sortStrings(s []string) {
	// Insertion sort — attribute count per span is tiny
	// (single digits in practice); avoids pulling in
	// `sort` for a one-off call.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
