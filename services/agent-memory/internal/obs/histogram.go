package obs

// Histogram is a hand-rolled, fixed-bucket, concurrency-safe
// Prometheus-text-format histogram. It satisfies the §8.3
// surface for the four `*_duration_seconds` metrics declared
// in metrics.go without pulling in
// github.com/prometheus/client_golang.
//
// Contract (matches the Prometheus exposition format 0.0.4):
//
//   - Buckets are cumulative: a sample with `value=0.42`
//     increments every bucket whose `le` is `≥ 0.42`,
//     including the implicit `+Inf` bucket which always
//     equals `_count`.
//   - The exposed series are `<name>_bucket{le="<v>"}`,
//     `<name>_sum`, `<name>_count`.
//   - Bucket boundaries are immutable for the lifetime of
//     the Histogram instance; reconfiguring requires a fresh
//     Histogram (the operator's TSDB sees the change as a
//     new series).
//   - Observe(v) is safe to call from any goroutine; Write
//     takes a brief read lock to snapshot the bucket counts +
//     sum atomically per series. Two concurrent observations
//     and a concurrent scrape are therefore consistent (the
//     scrape sees a partial-but-coherent state, never a
//     bucket count that under-reports its le-predecessor).
//
// Why fixed buckets. The §8.3 SLO targets are point-thresholds
// (1.5 s for recall p95, 400 ms for observe, 4 s for
// summarize). We MUST have a bucket boundary at exactly
// `0.4`, `1.5`, `2.0`, `4.0` so `histogram_quantile()` can
// return a value that is precise at the SLO line; absent
// those, the quantile-interpolator straddles the threshold
// and the alert oscillates. DefaultDurationBuckets covers
// every §8.3 SLO line, plus the standard Prometheus default
// boundaries (5 ms, 10 ms, …) so faster-than-SLO traffic also
// produces a useful distribution.
//
// Why not sparse / native histograms. The agent-memory
// scrape pipeline is Prometheus text-format (the rest of the
// /metrics surface already commits to it). Native histograms
// require the protobuf-format exposition; keeping fixed
// buckets keeps the wire compatible.

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
)

// DefaultDurationBuckets is the bucket set every
// `*_duration_seconds` histogram in this package uses. The
// boundaries align with the §8.3 SLO thresholds (0.4 s for
// agent.observe p95, 1.5 s for agent.recall / agent.expand
// p95 and agent.observe p99, 2 s for mgmt.ingest_spans p95,
// 4 s for agent.summarize p95 and agent.recall / agent.expand
// p99, 5 s for mgmt.ingest_spans p99, 10 s for agent.summarize
// p99) and otherwise mirror the Prometheus default histogram
// boundary set so faster traffic still shows a usable
// distribution. The `+Inf` bucket is appended automatically
// by [NewHistogram]; it MUST NOT appear in this slice (the
// constructor panics if it does).
var DefaultDurationBuckets = []float64{
	0.005,
	0.01,
	0.025,
	0.05,
	0.1,
	0.25,
	0.4,
	0.5,
	1,
	1.5,
	2,
	2.5,
	4,
	5,
	10,
}

// Histogram is the in-process aggregator. Construct via
// [NewHistogram]; call [Histogram.Observe] from the
// hot-path; render the cumulative buckets + `_sum` + `_count`
// via [Histogram.Write] from inside an HTTP /metrics handler.
//
// All fields are package-private. The mutex is held only for
// the very short window of an Observe (counter increment) or
// Write (snapshot copy); it does NOT serialise the actual
// network write so a slow scraper cannot stall the recall
// hot-path.
type Histogram struct {
	name string
	help string

	// upperBounds is sorted ascending and is owned by the
	// Histogram (the constructor copies the caller's slice
	// so a later mutation cannot poison the bucket table).
	// The final `+Inf` is implicit — bucketCounts[len(bucketCounts)-1]
	// is the `+Inf` cell and equals total count by
	// definition.
	upperBounds []float64

	mu sync.RWMutex
	// bucketCounts[i] is the cumulative count of samples
	// observed at or below upperBounds[i] (with the last
	// cell carrying the `+Inf` total).
	bucketCounts []uint64
	sumSeconds   float64
	count        uint64
}

// NewHistogram constructs a histogram with the given metric
// name, HELP text, and bucket upper bounds. The bounds slice
// MUST be:
//
//   - strictly ascending (the constructor panics on a
//     non-monotonic input — caller bug);
//   - free of `math.Inf(+1)` (the `+Inf` bucket is appended
//     internally — caller bug to pass it explicitly);
//   - non-empty.
//
// A nil HELP collapses to a generic "Duration histogram"
// string so the exposition is still well-formed.
func NewHistogram(name, help string, upperBounds []float64) *Histogram {
	if name == "" {
		panic("obs: NewHistogram requires a non-empty name")
	}
	if len(upperBounds) == 0 {
		panic("obs: NewHistogram requires at least one bucket")
	}
	bounds := make([]float64, len(upperBounds))
	copy(bounds, upperBounds)
	for i, b := range bounds {
		if math.IsInf(b, +1) {
			panic(fmt.Sprintf("obs: NewHistogram %q: +Inf is implicit, must not appear in upperBounds", name))
		}
		if i > 0 && b <= bounds[i-1] {
			panic(fmt.Sprintf("obs: NewHistogram %q: upperBounds must be strictly ascending (bucket %d=%v ≤ bucket %d=%v)", name, i, b, i-1, bounds[i-1]))
		}
	}
	if help == "" {
		help = "Duration histogram (seconds)."
	}
	// One extra cell for the implicit `+Inf` row.
	return &Histogram{
		name:         name,
		help:         help,
		upperBounds:  bounds,
		bucketCounts: make([]uint64, len(bounds)+1),
	}
}

// Observe records a single sample. Negative or NaN values
// silently increment the `+Inf` bucket but NOT _sum (we
// deliberately do not poison _sum with NaN — the operator
// would see the entire series collapse to NaN forever).
// Practical Observe values are positive durations, so this
// safety net is for defensive coding only.
func (h *Histogram) Observe(seconds float64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	// `+Inf` cell ALWAYS increments (it's the total count).
	h.bucketCounts[len(h.bucketCounts)-1]++
	// Bisect for the smallest cumulative bucket whose le ≥
	// seconds; every cell from there up to (but excluding)
	// `+Inf` also increments.
	if !math.IsNaN(seconds) && seconds >= 0 {
		idx := sort.SearchFloat64s(h.upperBounds, seconds)
		for i := idx; i < len(h.upperBounds); i++ {
			h.bucketCounts[i]++
		}
		h.sumSeconds += seconds
	}
	h.count++
	h.mu.Unlock()
}

// ObserveSince is a convenience wrapper that records the
// number of seconds elapsed between `start` and "now" via
// the caller-provided `nowSeconds` (a `time.Since(start).Seconds()`
// in production code; test code can inject a deterministic
// value). Passing the duration as a float lets the call site
// keep the import surface minimal — most call sites already
// have a `time.Since` value at hand.
func (h *Histogram) ObserveSince(elapsedSeconds float64) {
	h.Observe(elapsedSeconds)
}

// Snapshot copies the histogram's current state for an
// out-of-band inspector (tests, mostly — the live /metrics
// path goes through [Histogram.Write] which has a faster
// fast-path because it streams directly to the response).
//
// The returned bucket-count slice has one entry per
// upperBound plus a final `+Inf` cell, and is owned by the
// caller (a later Observe() does not mutate it).
func (h *Histogram) Snapshot() (buckets []uint64, sumSeconds float64, count uint64) {
	if h == nil {
		return nil, 0, 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	b := make([]uint64, len(h.bucketCounts))
	copy(b, h.bucketCounts)
	return b, h.sumSeconds, h.count
}

// Name returns the metric base name (no `_bucket` / `_sum` /
// `_count` suffix).
func (h *Histogram) Name() string {
	if h == nil {
		return ""
	}
	return h.name
}

// Write renders the histogram in Prometheus text-exposition
// format 0.0.4 to w. The output begins with HELP / TYPE
// preamble lines, then emits the cumulative `_bucket{le="…"}`
// rows in ascending bound order, then the implicit `+Inf`
// bucket, then `_sum` and `_count`. The trailing newline is
// included.
//
// Callers may chain Write into a multi-metric /metrics body;
// the per-name HELP / TYPE preamble keeps the Prometheus
// parser happy regardless of where in the body the histogram
// lands as long as the metric name appears contiguously.
func (h *Histogram) Write(w io.Writer) {
	if h == nil {
		return
	}
	buckets, sum, count := h.Snapshot()
	fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
	for i, ub := range h.upperBounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n",
			h.name, formatPromFloat(ub), buckets[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n",
		h.name, buckets[len(buckets)-1])
	fmt.Fprintf(w, "%s_sum %s\n", h.name, formatPromFloat(sum))
	fmt.Fprintf(w, "%s_count %d\n", h.name, count)
}

// formatPromFloat renders a float in the canonical
// Prometheus exposition format: trailing zeros trimmed, no
// exponent for the usual magnitude range, special values
// spelled out per the spec.
func formatPromFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, +1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	// `'g'` lets the runtime pick fixed vs scientific; -1
	// asks for the shortest exact representation. This is
	// what the prometheus/common library uses internally.
	return strconv.FormatFloat(v, 'g', -1, 64)
}
