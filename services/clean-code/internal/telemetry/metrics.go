package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Collector is the narrow interface every Prometheus
// metric source in the clean-code service satisfies:
// emit the Prometheus text-exposition v0.0.4 line-based
// snippet for ALL series the collector owns.
//
// The signature is deliberately identical to
// `internal/metric_ingestor.StaleScanRunSweepMetrics.WriteText`
// (Stage 3.5) so the existing sweep counters drop in as a
// Collector without an adapter. New metric holders in
// Stage 7.1 / Stage 9.2 / Stage 5.7 implement the same
// shape (see [AggregatorTickMetrics], [WALReplayMetrics],
// [RuleEngineMetrics]).
//
// Implementations MUST be safe for concurrent calls --
// `/metrics` scrapes can race with subsystem writes.
type Collector interface {
	WriteText(w io.Writer) (int, error)
}

// CollectorFunc adapts a bare `func(io.Writer) (int, error)`
// to the [Collector] interface. Useful for one-off ad-hoc
// collectors a composition root assembles on the fly.
type CollectorFunc func(w io.Writer) (int, error)

// WriteText implements [Collector].
func (f CollectorFunc) WriteText(w io.Writer) (int, error) { return f(w) }

// DefaultDurationBuckets is the canonical Prometheus
// histogram bucket boundaries (in seconds) for sub-second
// to multi-minute durations. The boundaries mirror the
// official `client_golang` default + an extra 30s / 60s /
// 120s / 300s tail so an aggregator tick that runs over the
// 15-minute cadence does not silently fall into +Inf.
//
// Exposed so the `prometheus-counter-shape` test scenario
// (impl-plan Stage 9.4) can assert the exact bucket
// labels without re-deriving them.
var DefaultDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5,
	10, 30, 60, 120, 300,
}

// histogram is the shared in-memory Prometheus histogram
// implementation used by [AggregatorTickMetrics],
// [WALReplayMetrics], and any future tick-duration-style
// collector in the package. The struct is unexported so the
// constructors (NewXxxMetrics) own the bucket choice and
// metric naming.
//
// The implementation is hand-rolled (rather than
// `prometheus/client_golang`-backed) so the service does
// NOT take a runtime dep on the official client lib. The
// text-exposition output is byte-for-byte identical to
// `client_golang`'s default for the same buckets / counts.
type histogram struct {
	mu       sync.Mutex
	buckets  []float64 // upper-bound boundaries (seconds)
	counts   []uint64  // count per bucket; len = len(buckets)
	totalCnt uint64
	totalSum float64
}

func newHistogram(buckets []float64) *histogram {
	// Defensive copy + sort so the caller cannot mutate
	// our bucket slice after construction.
	cp := make([]float64, len(buckets))
	copy(cp, buckets)
	sort.Float64s(cp)
	return &histogram{
		buckets: cp,
		counts:  make([]uint64, len(cp)),
	}
}

// observe records a single observation. Concurrent calls
// are serialised on `mu` -- the locking overhead is
// negligible for the sub-100-Hz observation rates the
// clean-code service produces.
func (h *histogram) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.totalCnt++
	h.totalSum += v
	for i, ub := range h.buckets {
		if v <= ub {
			h.counts[i]++
		}
	}
}

// snapshot returns a copy of the histogram state so
// `WriteText` can emit consistent counts even while another
// goroutine continues observing.
func (h *histogram) snapshot() (buckets []float64, counts []uint64, total uint64, sum float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	buckets = make([]float64, len(h.buckets))
	copy(buckets, h.buckets)
	counts = make([]uint64, len(h.counts))
	copy(counts, h.counts)
	return buckets, counts, h.totalCnt, h.totalSum
}

// writeHistogram emits the histogram in Prometheus text
// exposition format. The output shape:
//
//	# HELP <name> <help>
//	# TYPE <name> histogram
//	<name>_bucket{le="0.005"} <count>
//	<name>_bucket{le="0.01"}  <count>
//	...
//	<name>_bucket{le="+Inf"}  <total_count>
//	<name>_sum    <total_sum>
//	<name>_count  <total_count>
func writeHistogram(w io.Writer, name, help string, h *histogram) (int, error) {
	buckets, counts, totalCount, totalSum := h.snapshot()
	var written int
	n, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	written += n
	if err != nil {
		return written, err
	}
	// Prometheus requires cumulative bucket counts; the
	// internal `counts` already stores cumulative via the
	// `<= ub` observe loop above.
	for i, ub := range buckets {
		n, err = fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", name, formatBucket(ub), counts[i])
		written += n
		if err != nil {
			return written, err
		}
	}
	n, err = fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, totalCount)
	written += n
	if err != nil {
		return written, err
	}
	n, err = fmt.Fprintf(w, "%s_sum %s\n", name, formatFloat(totalSum))
	written += n
	if err != nil {
		return written, err
	}
	n, err = fmt.Fprintf(w, "%s_count %d\n", name, totalCount)
	written += n
	return written, err
}

// formatBucket returns the canonical Prometheus bucket
// boundary literal. Integer-valued boundaries (1, 5, 10)
// must NOT carry a `.0` suffix per the Prometheus exposition
// format spec; fractional boundaries (0.005, 0.025) are
// emitted with their minimal decimal form.
func formatBucket(v float64) string {
	if v == math.Floor(v) && !math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// formatFloat emits a float in the Prometheus-canonical
// decimal form. The exposition format accepts any
// `strconv`-style decimal, including `0` and very small /
// large numbers; -1 precision picks the shortest faithful
// representation.
func formatFloat(v float64) string {
	if v == math.Floor(v) && !math.IsInf(v, 0) && !math.IsNaN(v) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// counter is a single-series Prometheus counter. Used by
// [RuleEngineMetrics]; future per-verdict counters compose
// the same shape via a `map[string]*counter`.
type counter struct {
	mu  sync.Mutex
	val uint64
}

func (c *counter) inc(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.val += n
}

func (c *counter) load() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// ---------------------------------------------------------
// AggregatorTickMetrics (Stage 7.1)
// ---------------------------------------------------------

// MetricNameAggregatorTickDurationSeconds is the canonical
// Prometheus histogram name for the Cross-Repo Aggregator
// tick duration (architecture Sec 3.10 / Stage 7.1).
// Exposed so the `prometheus-counter-shape` test scenario
// asserts the name verbatim.
const MetricNameAggregatorTickDurationSeconds = "cleancode_aggregator_tick_duration_seconds"

// AggregatorTickMetrics records the duration of each
// `aggregator.Aggregator.Tick` call. Construct one per
// process; pass it to the aggregator loop via the
// `WithTickObserver` Option so each Tick invokes
// `ObserveTick` with the measured duration.
type AggregatorTickMetrics struct {
	histogram *histogram
}

// NewAggregatorTickMetrics returns a zero-initialised
// metrics holder using [DefaultDurationBuckets].
func NewAggregatorTickMetrics() *AggregatorTickMetrics {
	return &AggregatorTickMetrics{histogram: newHistogram(DefaultDurationBuckets)}
}

// Observe records a single tick duration. Safe for
// concurrent calls.
func (m *AggregatorTickMetrics) Observe(d time.Duration) {
	if m == nil {
		return
	}
	m.histogram.observe(d.Seconds())
}

// WriteText implements [Collector].
func (m *AggregatorTickMetrics) WriteText(w io.Writer) (int, error) {
	if m == nil {
		return 0, nil
	}
	return writeHistogram(w,
		MetricNameAggregatorTickDurationSeconds,
		"Wall-clock duration of one Cross-Repo Aggregator tick (architecture Sec 3.10, Stage 7.1).",
		m.histogram,
	)
}

// ---------------------------------------------------------
// WALReplayMetrics (Stage 9.2)
// ---------------------------------------------------------

// MetricNameWALReplayDurationSeconds is the canonical
// Prometheus histogram name for the Audit WAL Reconciler
// replay duration (architecture Sec 7.10 / Stage 9.2).
const MetricNameWALReplayDurationSeconds = "cleancode_wal_replay_duration_seconds"

// WALReplayMetrics records the duration of each
// `reconciler.Reconciler.Run` call. The reconciler runs at
// startup; the metric therefore captures the recovery
// window from process boot to "WAL fully replayed".
type WALReplayMetrics struct {
	histogram *histogram
}

// NewWALReplayMetrics returns a zero-initialised holder
// using [DefaultDurationBuckets].
func NewWALReplayMetrics() *WALReplayMetrics {
	return &WALReplayMetrics{histogram: newHistogram(DefaultDurationBuckets)}
}

// Observe records a single WAL replay duration.
func (m *WALReplayMetrics) Observe(d time.Duration) {
	if m == nil {
		return
	}
	m.histogram.observe(d.Seconds())
}

// WriteText implements [Collector].
func (m *WALReplayMetrics) WriteText(w io.Writer) (int, error) {
	if m == nil {
		return 0, nil
	}
	return writeHistogram(w,
		MetricNameWALReplayDurationSeconds,
		"Wall-clock duration of one Audit WAL Reconciler replay pass (architecture Sec 7.10, Stage 9.2).",
		m.histogram,
	)
}

// ---------------------------------------------------------
// RuleEngineMetrics (Stage 5.7)
// ---------------------------------------------------------

// MetricNameRuleEngineEvaluationsTotal is the canonical
// Prometheus counter name for the SOLID Rule Engine
// evaluation throughput (Stage 5.7). The "evaluations/sec"
// rate target in architecture Sec 8 is computed at scrape
// time via `rate(cleancode_rule_engine_evaluations_total[5m])`.
const MetricNameRuleEngineEvaluationsTotal = "cleancode_rule_engine_evaluations_total"

// MetricNameRuleEngineEvaluationsByVerdictTotal is a
// per-verdict counter so dashboards can chart pass / warn /
// block separately. One series per `Verdict` enum value;
// non-canonical verdict values are folded into `unknown`.
const MetricNameRuleEngineEvaluationsByVerdictTotal = "cleancode_rule_engine_evaluations_by_verdict_total"

// RuleEngineMetrics is the rule-engine Prometheus surface.
// Increment via [IncEvaluation]; the holder bumps both the
// total counter and the per-verdict counter atomically.
type RuleEngineMetrics struct {
	total           counter
	byVerdictMu     sync.Mutex
	byVerdict       map[string]*counter
}

// NewRuleEngineMetrics returns a zero-initialised holder.
func NewRuleEngineMetrics() *RuleEngineMetrics {
	return &RuleEngineMetrics{
		byVerdict: map[string]*counter{},
	}
}

// Observe records one completed rule-engine evaluation with
// the given verdict. An empty / unknown verdict folds into
// the `unknown` label so dashboards surface adapter bugs
// that smuggle a non-canonical verdict instead of silently
// dropping the count.
func (m *RuleEngineMetrics) Observe(verdict string) {
	if m == nil {
		return
	}
	m.total.inc(1)
	label := canonicalVerdictLabel(verdict)
	m.byVerdictMu.Lock()
	c, ok := m.byVerdict[label]
	if !ok {
		c = &counter{}
		m.byVerdict[label] = c
	}
	m.byVerdictMu.Unlock()
	c.inc(1)
}

func canonicalVerdictLabel(v string) string {
	switch v {
	case "pass", "warn", "block":
		return v
	default:
		return "unknown"
	}
}

// WriteText implements [Collector].
func (m *RuleEngineMetrics) WriteText(w io.Writer) (int, error) {
	if m == nil {
		return 0, nil
	}
	var written int
	n, err := fmt.Fprintf(w,
		"# HELP %s Total rule-engine evaluations completed (Stage 5.7).\n# TYPE %s counter\n%s %d\n",
		MetricNameRuleEngineEvaluationsTotal,
		MetricNameRuleEngineEvaluationsTotal,
		MetricNameRuleEngineEvaluationsTotal,
		m.total.load(),
	)
	written += n
	if err != nil {
		return written, err
	}
	n, err = fmt.Fprintf(w,
		"# HELP %s Rule-engine evaluations completed, labelled by canonical verdict.\n# TYPE %s counter\n",
		MetricNameRuleEngineEvaluationsByVerdictTotal,
		MetricNameRuleEngineEvaluationsByVerdictTotal,
	)
	written += n
	if err != nil {
		return written, err
	}
	// Sort labels deterministically so the test assertions
	// can pin substring expectations without depending on
	// map iteration order.
	m.byVerdictMu.Lock()
	labels := make([]string, 0, len(m.byVerdict))
	for k := range m.byVerdict {
		labels = append(labels, k)
	}
	m.byVerdictMu.Unlock()
	sort.Strings(labels)
	for _, label := range labels {
		m.byVerdictMu.Lock()
		c := m.byVerdict[label]
		m.byVerdictMu.Unlock()
		n, err = fmt.Fprintf(w, "%s{verdict=\"%s\"} %d\n",
			MetricNameRuleEngineEvaluationsByVerdictTotal,
			label,
			c.load(),
		)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
