package mgmtapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SpansBatch is the ATOMIC unit a [SpanForwarder] receives:
// the *original* validated OTLP/HTTP request bytes plus the
// MIME type the caller posted them with. Stage 7.2
// (`mgmt.ingest_spans`) deliberately forwards the inbound
// body byte-for-byte; the handler does NOT re-serialize the
// payload, so:
//
//   - `service.name` is preserved verbatim and the downstream
//     OTLP receiver's existing `service.name → repo_id`
//     mapping continues to work without re-mapping.
//   - Non-string attribute types (intValue, boolValue,
//     doubleValue, arrayValue, kvlistValue, bytesValue) and
//     OTLP span fields the handler doesn't model (events,
//     links, status, droppedXCount, etc.) are forwarded
//     intact instead of being silently dropped by a
//     narrowing typed re-marshal.
//
// Atomicity contract:
//
//   - The handler hands exactly ONE [SpansBatch] per
//     POST /v1/spans request — not a slice — so a forwarder
//     can never partially accept one repo and fail another.
//     This is `Forward(ctx, batch) error`; a non-nil return
//     fails the whole call (502 from the handler), and the
//     operator's retry posts the SAME body again.
//   - Delivery semantics are **at-least-once**: if the
//     downstream receiver durably accepted the payload but
//     the response was lost in flight (read timeout,
//     connection reset, transient 5xx), mgmt-api will 502
//     and an operator retry will duplicate the spans on the
//     receiver side. The downstream pipeline must be
//     idempotent on `(trace_id, span_id)` to absorb this,
//     OR the operator accepts duplicates. This package does
//     NOT inject an idempotency key; the spaningestor
//     receiver does not currently consume one.
type SpansBatch struct {
	// Body is the exact validated request body the
	// handler received. Forwarders MUST NOT rewrite it.
	Body []byte
	// ContentType mirrors the inbound Content-Type after
	// the handler's content-type guard normalized it
	// (currently always "application/json"; protobuf
	// support is a future extension).
	ContentType string
}

// SpanForwarder forwards a validated OTLP batch to the Span
// Ingestor input queue. The implementation owns the transport
// (single HTTP POST today; a future protobuf or gRPC bridge
// would replace the HTTPSpanForwarder behind this interface).
//
// `Forward` returns nil on full success and a non-nil error
// otherwise; the handler translates a non-nil return into 502
// `forward_failed`. To signal "the binary was started without
// a forwarder wired" (vs. an upstream outage), return
// [ErrForwarderNotConfigured] — the handler maps that
// sentinel onto 503 `forwarder_not_configured` so the
// operator can dashboard misconfiguration distinctly from
// real upstream failures.
//
// The signature is intentionally singular ([SpansBatch], not
// []SpansBatch). Iter-1 used a slice and let the HTTP forwarder
// POST per-repo bodies in a loop, which silently broke
// atomicity when a later POST in the same loop failed after
// earlier POSTs were already enqueued downstream. The
// singular signature eliminates that class of bug at the
// interface boundary.
type SpanForwarder interface {
	Forward(ctx context.Context, batch SpansBatch) error
}

// ErrForwarderNotConfigured is the sentinel a forwarder
// returns when the binary was started without a real
// downstream wired up. The handler classifies this as 503
// (not 502 / 500) so:
//   - 502 stays "the real ingestor said no" (a transient
//     upstream fault the operator should page on),
//   - 503 stays "this surface isn't configured to serve" (an
//     operator-side config bug — fix the env, restart).
var ErrForwarderNotConfigured = errors.New(
	"mgmtapi: span forwarder not configured")

// notConfiguredForwarder is the default that ships when
// Options.SpanForwarder is nil. EVERY call returns
// [ErrForwarderNotConfigured]; the handler 503s and counts
// `mgmt_ingest_spans_total{status="forwarder_not_configured"}`.
// This is fail-CLOSED — a previous draft used a silent no-op
// here, which would have returned 202 while silently
// dropping every span. (Caught by the iter-1 design review.)
type notConfiguredForwarder struct{}

// Forward implements [SpanForwarder].
func (notConfiguredForwarder) Forward(context.Context, SpansBatch) error {
	return ErrForwarderNotConfigured
}

// HTTPSpanForwarder posts the validated [SpansBatch] body to
// the downstream Span Ingestor's OTLP/HTTP `/v1/traces`
// endpoint (or any compatible OTLP receiver, e.g. the OTel
// Collector).
//
// Atomicity contract:
//
//   - Exactly ONE POST per Forward call. The handler hands a
//     single SpansBatch containing the original validated
//     body bytes; the forwarder NEVER splits or re-orders.
//     On non-2xx the whole call returns an error and the
//     handler 502s; the operator's retry posts the same
//     bytes again.
//
//   - This guarantees per-POST atomicity end-to-end with the
//     spaningestor receiver's `EnqueueAtomic` (which accepts
//     or rejects an entire OTLP/HTTP body as one unit). It
//     does NOT guarantee exact-once: see the SpansBatch doc
//     for the at-least-once delivery caveat.
//
// TLS / auth:
//   - The forwarder uses the supplied [http.Client] as-is —
//     production callers wire TLS roots + a bearer-token
//     round-tripper via that client. This package does not
//     ship a TLS helper to keep the test surface narrow.
type HTTPSpanForwarder struct {
	// URL is the OTLP/HTTP endpoint the forwarder POSTs to.
	// MUST be a full URL ending in the receiver's
	// `/v1/traces` path. Empty URL makes Forward return
	// [ErrForwarderNotConfigured].
	URL string
	// Client is the HTTP client used for outbound POSTs.
	// Nil falls back to the package-level
	// [defaultHTTPSpanForwardClient] (10-second timeout)
	// so connections — and the underlying http.Transport's
	// keep-alive pool — are reused across Forward calls.
	Client *http.Client
	// Timeout overrides Client.Timeout when non-zero.
	Timeout time.Duration
}

// defaultHTTPSpanForwardClient is the shared fallback used
// when an [HTTPSpanForwarder] is constructed without an
// explicit Client (e.g. `&HTTPSpanForwarder{URL: ...}` in
// tests or in the default `buildSpanForwarder` wiring).
//
// Sharing a SINGLE [http.Client] — and therefore a single
// [http.Transport] with its keep-alive connection pool —
// across both forwarder instances AND every Forward call is
// the whole point: under production load (one POST per span
// batch) we want repeat POSTs to the same downstream OTLP
// receiver to reuse the existing TCP+TLS session instead of
// paying a fresh handshake per call. A prior draft allocated
// `&http.Client{Timeout: 10 * time.Second}` inside Forward
// every time `f.Client == nil`, which created a brand-new
// http.Transport (with an empty pool) per call and silently
// disabled connection reuse — caught in code review.
//
// The timeout matches what that prior draft used so behavior
// for nil-Client callers is preserved.
var defaultHTTPSpanForwardClient = &http.Client{Timeout: 10 * time.Second}

// Forward implements [SpanForwarder]. Posts the entire body
// in exactly one HTTP call.
func (f *HTTPSpanForwarder) Forward(ctx context.Context, batch SpansBatch) error {
	if f.URL == "" {
		return ErrForwarderNotConfigured
	}
	if len(batch.Body) == 0 {
		// Empty body would normally be rejected upstream
		// by validation; treat a stray empty Forward as
		// a no-op rather than POSTing garbage.
		return nil
	}
	client := f.Client
	if client == nil {
		// Use the package-level shared client so the
		// underlying http.Transport's connection pool is
		// reused across Forward calls (and across
		// HTTPSpanForwarder instances). Allocating a fresh
		// http.Client here would force a TCP+TLS handshake
		// per POST under load.
		client = defaultHTTPSpanForwardClient
	}
	if f.Timeout > 0 {
		// Per-call deadline beats Client.Timeout when set;
		// give callers an explicit knob.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(batch.Body))
	if err != nil {
		return fmt.Errorf("build forward request: %w", err)
	}
	ct := batch.ContentType
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("forward to %s: %w", f.URL, err)
	}
	// Drain + close so the connection is reused. The body
	// of the forwarded response is uninteresting — the
	// receiver replies with an empty
	// ExportTraceServiceResponse on success.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("forward to %s: status %d", f.URL, resp.StatusCode)
	}
	return nil
}

// ServiceNameToRepoID maps an OTel `service.name` (or
// `service.namespace`) to the `repo_id` textual UUID the
// downstream Span Ingestor expects. Returning the empty
// string means "unknown service"; the handler rejects the
// whole batch with 400 `unknown_service` and increments
// `mgmt_ingest_spans_total{repo_id="", status="unknown_service"}`
// (architecture.md §6.2.2 fail-fast batch semantic).
//
// Shape mirrors [spaningestor.ServiceNameToRepoID]
// (internal/spaningestor/otlphttp.go) — kept duplicate-by-
// design so the mgmtapi package does NOT import spaningestor
// and the two surfaces can evolve independently. A future
// refactor may extract a shared `pkg/otelservice` helper if
// the duplication grows.
type ServiceNameToRepoID func(serviceName string) string
