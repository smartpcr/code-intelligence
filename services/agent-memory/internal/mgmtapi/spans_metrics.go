package mgmtapi

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// SpanMetrics records the Stage 7.2 `mgmt_ingest_spans_total`
// counter partitioned by `repo_id` and `status`
// (implementation-plan.md §7.2; architecture.md §8.3 metric
// catalog).
//
// The interface is the seam between the handler and the
// observability backend. Production composition roots plug in
// a Prometheus-registered counter; unit tests inject a
// [DefaultSpanMetrics] and assert on Snapshot.
//
// `delta` is the number of spans counted by the event — a
// successful forward of a 50-span batch calls
// `IncIngestSpansTotal(repoID, "accepted", 50)` exactly once.
// Counting at the span (NOT request) level keeps dashboards
// honest for the §8.3 SLO that frames throughput in spans/sec.
//
// `repoID` may be the empty string when the rejected payload
// did not carry a resolvable `service.name` (or when an early
// JSON-decode failure prevented us from looking at all). The
// empty label is deliberately kept distinct from any real
// repo id so an operator can see "we rejected something but
// have no idea whose service it was" without cross-talk.
//
// `status` is drawn from the closed set below. Adding a new
// status MUST update the dashboard panel allowlist in
// `deploy/dashboards/` (Stage 8.3) or the new bucket will go
// uncounted on the visible chart.
type SpanMetrics interface {
	IncIngestSpansTotal(repoID, status string, delta int64)
}

// Closed-set values for the `status` label on
// `mgmt_ingest_spans_total`. Keeping the labels as named
// constants (rather than magic strings spread through the
// handler) guarantees the metrics emitter, the dashboard
// JSON, and the alert rules all reference the same literals.
const (
	// SpanStatusAccepted -- the span was validated AND a
	// downstream Span Ingestor forward returned nil. This is
	// the only status that counts "made it onto the input
	// queue" — every other status is a drop.
	SpanStatusAccepted = "accepted"

	// SpanStatusRejectedValidation -- the span failed an
	// OTel schema check (missing trace_id, missing span_id,
	// zero timestamp, etc). Fail-fast on the first invalid
	// span aborts the whole batch per the architecture
	// §6.2.2 "atomic batch" semantic; this counter
	// increments by 1 for the offending span.
	SpanStatusRejectedValidation = "rejected_validation"

	// SpanStatusRejectedForbiddenField -- the span carried
	// an `outcome` or `corrected_action` field. Per
	// architecture.md §6.2.2 those belong on `mgmt.feedback`,
	// NOT on `mgmt.ingest_spans`. Fail-fast on first
	// occurrence; counter increments by 1.
	SpanStatusRejectedForbiddenField = "rejected_forbidden_field"

	// SpanStatusForwardFailed -- the span passed validation
	// but the downstream Span Ingestor forward call
	// returned an error. The whole atomic batch is rejected
	// to keep retry safe; counter increments by the number
	// of spans that were dropped.
	SpanStatusForwardFailed = "forward_failed"

	// SpanStatusUnknownService -- the OTel resource's
	// `service.name` did not map to any known repo via the
	// configured [ServiceNameToRepoID]. The handler REJECTS
	// the request with HTTP 400 `unknown_service`
	// (fail-fast — aligns with the architecture's atomic
	// per-batch semantic). The counter is incremented by
	// the number of spans in the offending resource group
	// (not 1) so operator dashboards reflect the true
	// reject volume. Labelled with `repo_id=""` because no
	// repo was resolved.
	SpanStatusUnknownService = "unknown_service"

	// SpanStatusForwarderNotConfigured -- the handler
	// received a valid batch but the binary was started
	// without a real forwarder wired. The handler responds
	// 503 to signal an operational rather than client
	// fault; counter increments by the number of spans in
	// the rejected batch.
	SpanStatusForwarderNotConfigured = "forwarder_not_configured"
)

// spanMetricKey is the composite key the in-memory counter
// map is indexed on. Using a struct (rather than a
// concatenated string) avoids accidental collisions when a
// repo id legitimately contains the delimiter character.
type spanMetricKey struct {
	RepoID string
	Status string
}

// DefaultSpanMetrics is an in-memory [SpanMetrics] with a
// snapshot accessor. Used as the Stage 7.2 default when no
// metrics backend is wired (so a fresh deployment still
// produces something observable via /metrics or test hooks)
// AND as the test-time double for unit tests.
//
// Concurrency: the counter map is guarded by an embedded
// mutex. Reads via Snapshot copy the keys+values under the
// lock and return an independent map so callers can mutate
// the returned structure without racing the writer.
type DefaultSpanMetrics struct {
	mu       sync.Mutex
	counters map[spanMetricKey]int64
}

// NewDefaultSpanMetrics returns an empty [DefaultSpanMetrics].
// The zero-value Mutex is usable so callers may also embed
// DefaultSpanMetrics directly; this constructor is exposed
// for the common case.
func NewDefaultSpanMetrics() *DefaultSpanMetrics {
	return &DefaultSpanMetrics{counters: make(map[spanMetricKey]int64)}
}

// IncIngestSpansTotal implements [SpanMetrics].
func (m *DefaultSpanMetrics) IncIngestSpansTotal(repoID, status string, delta int64) {
	if delta == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters == nil {
		m.counters = make(map[spanMetricKey]int64)
	}
	m.counters[spanMetricKey{RepoID: repoID, Status: status}] += delta
}

// Snapshot returns an independent copy of the counter map
// keyed by repo_id then by status. The caller may mutate the
// returned maps without affecting the live counters.
//
// Useful in tests (`assert.Equal(metrics.Snapshot()[...], 1)`)
// and as a /metrics-style scrape endpoint when a real
// Prometheus collector is not wired.
func (m *DefaultSpanMetrics) Snapshot() map[string]map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]map[string]int64, len(m.counters))
	// Sort keys so the output is deterministic across
	// callers (helpful for golden-file tests; small cost on
	// what is a debug/test path).
	keys := make([]spanMetricKey, 0, len(m.counters))
	for k := range m.counters {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].RepoID != keys[j].RepoID {
			return keys[i].RepoID < keys[j].RepoID
		}
		return keys[i].Status < keys[j].Status
	})
	for _, k := range keys {
		if _, ok := out[k.RepoID]; !ok {
			out[k.RepoID] = make(map[string]int64)
		}
		out[k.RepoID][k.Status] = m.counters[k]
	}
	return out
}

// noopSpanMetrics is the safe default when the Options struct
// supplies no SpanMetrics implementation. Counters go to /dev/
// null. The handler never panics on a nil metrics field.
type noopSpanMetrics struct{}

func (noopSpanMetrics) IncIngestSpansTotal(string, string, int64) {}

// WritePrometheus emits the live counter as a single
// Prometheus text-format payload, suitable for direct
// publication on a `/metrics` endpoint. The output is
// deterministically sorted by (repo_id, status) so a
// `curl /metrics | diff` between scrapes is meaningful.
//
// Format (matches the Prometheus exposition spec):
//
//	# HELP mgmt_ingest_spans_total ...
//	# TYPE mgmt_ingest_spans_total counter
//	mgmt_ingest_spans_total{repo_id="...",status="..."} N
//	...
//
// HELP/TYPE are emitted exactly once even when the counter
// map is empty so an operator's `grep mgmt_ingest_spans_total`
// works on a freshly-started binary that hasn't taken
// traffic yet.
//
// Concurrency: takes a snapshot under the mutex, then writes
// the formatted output WITHOUT holding the lock — so a slow
// scrape can't block the hot ingest path.
func (m *DefaultSpanMetrics) WritePrometheus(w io.Writer) (int, error) {
	snap := m.Snapshot()
	var buf strings.Builder
	buf.WriteString("# HELP mgmt_ingest_spans_total Total spans processed by mgmt.ingest_spans (Stage 7.2), partitioned by repo_id and status. status is one of: accepted, rejected_validation, rejected_forbidden_field, forward_failed, unknown_service, forwarder_not_configured.\n")
	buf.WriteString("# TYPE mgmt_ingest_spans_total counter\n")
	// Deterministic order: outer keys sorted by repo_id,
	// inner by status.
	repoIDs := make([]string, 0, len(snap))
	for id := range snap {
		repoIDs = append(repoIDs, id)
	}
	sort.Strings(repoIDs)
	for _, id := range repoIDs {
		statuses := snap[id]
		keys := make([]string, 0, len(statuses))
		for k := range statuses {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, status := range keys {
			fmt.Fprintf(&buf, "mgmt_ingest_spans_total{repo_id=%s,status=%s} %d\n",
				escapePromLabel(id), escapePromLabel(status), statuses[status])
		}
	}
	return io.WriteString(w, buf.String())
}

// escapePromLabel renders `v` as a Prometheus label-value
// literal: enclosed in double quotes, with `\`, `"`, and `\n`
// escaped per the exposition spec. Repo IDs are expected to
// be UUIDs (no escaping needed) but a misconfigured operator
// could plant a quote in the service map; we never want the
// /metrics payload to be unparseable.
func escapePromLabel(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
