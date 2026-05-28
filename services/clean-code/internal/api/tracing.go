package api

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Span-attribute keys the gateway populates on every span. The
// names mirror the OTel semantic conventions where one exists
// (`http.status_code`, `http.method`) and pin the bespoke
// gateway attributes (`verb`, `caller_subject`, `repo_id`)
// from architecture Sec 8 / implementation-plan Stage 6.4.
//
// Dashboards / alerts grep these literals; keeping them as
// exported constants prevents silent drift.
const (
	// SpanAttrVerb carries the canonical dotted verb name
	// `<namespace>.<verb>` (e.g. `mgmt.register_repo`).
	SpanAttrVerb = "verb"
	// SpanAttrCallerSubject carries the verified `sub`
	// claim from the bearer token.
	SpanAttrCallerSubject = "caller_subject"
	// SpanAttrRepoID carries the optional repo_id pulled
	// from the request via [Verb.RepoIDExtractor]; empty
	// string when no extractor is registered or no
	// repo_id is present in the request.
	SpanAttrRepoID = "repo_id"
	// SpanAttrHTTPStatusCode carries the downstream
	// handler's HTTP status code. Mirrors the OTel
	// semantic-convention attribute name.
	SpanAttrHTTPStatusCode = "http.status_code"
	// SpanAttrHTTPMethod carries the HTTP method.
	SpanAttrHTTPMethod = "http.method"
	// SpanAttrHTTPRoute carries the matched route
	// (the `/v1/{namespace}/{verb}` path the verb mounted
	// at).
	SpanAttrHTTPRoute = "http.route"
)

// SpanName is the conventional span name the gateway uses for
// every request. Per OTel HTTP semantic conventions a span
// SHOULD be named after the route, not the URL -- the route
// is the cardinality-bounded form. The gateway always uses
// `http.gateway` as the base name and attaches the route via
// [SpanAttrHTTPRoute] / verb identity via [SpanAttrVerb].
const SpanName = "http.gateway"

// Tracer is the gateway's outbound-tracing seam. The
// interface is shaped to match `go.opentelemetry.io/otel/trace.Tracer`
// closely so a future composition-root wiring can swap in an
// OTel-backed tracer (or `otelhttp` middleware composition)
// without changing gateway code. For tests, the package
// provides a [RecordingTracer] that captures every span and
// attribute for assertion.
//
// StartSpan MUST return a non-nil [Span]. The returned
// context.Context is the child context propagated to the
// downstream verb handler; OTel-backed implementations use
// it to carry the active span for `propagation.TraceContext`
// extraction.
type Tracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, Span)
}

// Span is the gateway's outbound-span seam. The methods
// mirror OTel's `trace.Span` where useful: attribute setters
// (string + typed) and a terminal `End()` that ships the
// span to the configured backend. Errors recorded via
// [Span.RecordError] surface in OTel as `exception` events;
// the gateway logs them as span-level attributes when the
// backend is the [SlogTracer].
type Span interface {
	SetAttribute(key string, value any)
	RecordError(err error)
	End()
}

// NoopTracer is a Tracer that returns a Span discarding every
// call. Useful as a default for tests that do not assert on
// span shape and as an explicit "off" switch when an operator
// disables tracing in a particular environment.
type NoopTracer struct{}

func (NoopTracer) StartSpan(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetAttribute(_ string, _ any) {}
func (noopSpan) RecordError(_ error)          {}
func (noopSpan) End()                         {}

// SlogTracer is the default [Tracer] implementation when no
// OTel SDK is wired. Each call to StartSpan returns a
// [Span] that captures attributes in-memory and emits ONE
// structured log entry at End() time -- the operator gets
// the audit invariant (verb + caller_subject + repo_id +
// http.status_code on every request) without an OTel
// collector.
//
// # Why ship this at all
//
// Architecture Sec 8 / impl-plan Stage 6.4 requires spans
// "tagged with verb, caller_subject, repo_id". A production
// composition root with the OTel SDK wired in will swap in
// a real Tracer; the SlogTracer is the in-process fallback
// so the audit invariant is preserved even in
// docker-compose / unit-test deployments where no collector
// runs.
type SlogTracer struct {
	// Logger is the destination logger. nil falls back to
	// [slog.Default]. The tracer emits log entries at
	// INFO level with `event=span` so dashboards can
	// distinguish span lines from ordinary log lines.
	Logger *slog.Logger
	// Now is the time-source used to stamp span start /
	// end timestamps. nil -> [time.Now].
	Now func() time.Time
}

// StartSpan opens a new span. The returned context is the
// input context verbatim -- the [SlogTracer] does NOT
// propagate trace context (it has no trace IDs to
// propagate). When a real OTel tracer is wired the returned
// context carries the active OTel span.
func (t *SlogTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	now := time.Now
	if t != nil && t.Now != nil {
		now = t.Now
	}
	span := &slogSpan{
		tracer: t,
		name:   name,
		start:  now(),
		attrs:  map[string]any{},
	}
	return ctx, span
}

type slogSpan struct {
	tracer *SlogTracer
	name   string
	start  time.Time
	mu     sync.Mutex
	attrs  map[string]any
	errs   []error
	ended  bool
}

func (s *slogSpan) SetAttribute(key string, value any) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs[key] = value
}

func (s *slogSpan) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}

func (s *slogSpan) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	attrs := make([]slog.Attr, 0, len(s.attrs)+4)
	attrs = append(attrs,
		slog.String("event", "span"),
		slog.String("span.name", s.name),
		slog.Time("span.start", s.start),
	)
	now := time.Now
	if s.tracer != nil && s.tracer.Now != nil {
		now = s.tracer.Now
	}
	end := now()
	attrs = append(attrs,
		slog.Time("span.end", end),
		slog.Duration("span.duration", end.Sub(s.start)),
	)
	for k, v := range s.attrs {
		attrs = append(attrs, slog.Any(k, v))
	}
	for i, e := range s.errs {
		attrs = append(attrs, slog.String("span.error."+itoa(i), e.Error()))
	}
	s.mu.Unlock()
	logger := slog.Default()
	if s.tracer != nil && s.tracer.Logger != nil {
		logger = s.tracer.Logger
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "gateway span", attrs...)
}

// itoa is a tiny base-10 conversion that avoids dragging in
// `strconv` for the rare per-error attribute. Span-error
// counts are small (typically 0 or 1) so a quadratic call
// pattern is fine.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// RecordingTracer captures every span and its attributes in
// memory. Used by gateway tests to assert the canonical
// (verb, caller_subject, repo_id, http.status_code)
// attribute set lands on each span. NOT for production --
// the slice grows unbounded.
type RecordingTracer struct {
	mu    sync.Mutex
	Spans []*RecordedSpan
}

// RecordedSpan is one captured span. The fields are public so
// tests can assert directly.
type RecordedSpan struct {
	Name       string
	Attributes map[string]any
	Errors     []error
	Started    time.Time
	Ended      time.Time
}

// StartSpan returns a [Span] that appends itself to the
// tracer's [Spans] slice on End().
func (r *RecordingTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	rs := &RecordedSpan{
		Name:       name,
		Attributes: map[string]any{},
		Started:    time.Now(),
	}
	return ctx, &recordingSpan{tracer: r, span: rs}
}

// Attribute returns the value of attribute `key` on the most-
// recently-ended span. Returns nil if no span ended or the
// attribute is absent. Tests typically call this immediately
// after the request returns.
func (r *RecordingTracer) Attribute(key string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Spans) == 0 {
		return nil
	}
	return r.Spans[len(r.Spans)-1].Attributes[key]
}

// Last returns the most-recently-ended span, or nil if none
// has ended yet.
func (r *RecordingTracer) Last() *RecordedSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Spans) == 0 {
		return nil
	}
	return r.Spans[len(r.Spans)-1]
}

// Count returns the number of ended spans.
func (r *RecordingTracer) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Spans)
}

type recordingSpan struct {
	tracer *RecordingTracer
	span   *RecordedSpan
	mu     sync.Mutex
	ended  bool
}

func (s *recordingSpan) SetAttribute(key string, value any) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.span.Attributes[key] = value
}

func (s *recordingSpan) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.span.Errors = append(s.span.Errors, err)
}

func (s *recordingSpan) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.span.Ended = time.Now()
	s.mu.Unlock()
	s.tracer.mu.Lock()
	s.tracer.Spans = append(s.tracer.Spans, s.span)
	s.tracer.mu.Unlock()
}
