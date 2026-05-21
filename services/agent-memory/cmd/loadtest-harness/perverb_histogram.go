package main

// verbLabelledHistogram is a small, harness-local wrapper that
// fans one Prometheus histogram metric NAME across N per-verb
// underlying [obs.Histogram] series, exposing each series with
// a `{verb="..."}` label. This keeps the operator-facing /metrics
// surface a single metric (queryable as
// `histogram_quantile(0.95, sum(rate(<name>_bucket[5m])) by (verb, le))`)
// while still letting Prometheus split percentiles by verb —
// the F4 fix from iter-2 evaluator feedback.
//
// The underlying obs.Histogram does not natively support labels;
// each verb gets its own *obs.Histogram seeded with the same
// bucket bounds. The vec's [Write] method emits a single HELP /
// TYPE preamble for the shared metric name, then iterates the
// per-verb histograms in a deterministic order, baking the
// `verb="..."` label into every bucket / sum / count line.
//
// Concurrency:
//   - [Observe] is safe for concurrent calls from any number of
//     goroutines for any combination of verbs. First-time use
//     of a new verb uses [sync.Map.LoadOrStore] so a concurrent
//     race for the same verb cannot lose a sample to an orphaned
//     histogram instance.
//   - [Write] takes a snapshot of every per-verb histogram via
//     the obs.Histogram's own RLock — a slow scraper does not
//     stall the hot-path Observe calls.

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

type verbLabelledHistogram struct {
	name string
	help string

	// bounds is owned by the vec; we copy at construction so a
	// later mutation by the caller cannot poison the label
	// values emitted on /metrics. The same slice is handed to
	// every per-verb obs.Histogram so the bucket grid stays
	// identical across verbs.
	bounds []float64

	// verbs is a sync.Map[string]*obs.Histogram. Reads are
	// lock-free; writes use LoadOrStore so the race for a
	// brand-new verb cannot orphan a histogram instance.
	verbs sync.Map
}

// newVerbLabelledHistogram constructs the vec. Bounds MUST
// satisfy the same constraints as [obs.NewHistogram] (strictly
// ascending, no +Inf, non-empty); the constraint is enforced by
// the obs.NewHistogram call that runs the first time a verb is
// Observe()d. We make a defensive copy of bounds so callers
// can't mutate them after construction.
func newVerbLabelledHistogram(name, help string, bounds []float64) *verbLabelledHistogram {
	if name == "" {
		panic("loadtest-harness: verbLabelledHistogram requires a non-empty name")
	}
	if len(bounds) == 0 {
		panic("loadtest-harness: verbLabelledHistogram requires at least one bucket")
	}
	b := make([]float64, len(bounds))
	copy(b, bounds)
	return &verbLabelledHistogram{
		name:   name,
		help:   help,
		bounds: b,
	}
}

// Observe records a single sample for the named verb. Unknown
// verbs lazily create their own underlying obs.Histogram via
// LoadOrStore so two goroutines racing for a brand-new verb
// converge on the same instance (the other's freshly
// constructed Histogram is GC'd when LoadOrStore returns the
// already-stored one).
//
// Cardinality safety: this method does not whitelist verb
// names; the harness wires it from per-Sample callbacks, where
// the verb is one of the five constants in
// `internal/reliability/load_profile.go` (VerbAgent*,
// VerbMgmt*). A future caller pushing arbitrary strings would
// be a Prometheus cardinality footgun — keep the call sites
// honest.
func (v *verbLabelledHistogram) Observe(verb string, seconds float64) {
	if v == nil {
		return
	}
	if h, ok := v.verbs.Load(verb); ok {
		h.(*obs.Histogram).Observe(seconds)
		return
	}
	fresh := obs.NewHistogram(v.name, v.help, v.bounds)
	actual, _ := v.verbs.LoadOrStore(verb, fresh)
	actual.(*obs.Histogram).Observe(seconds)
}

// Write renders the per-verb histogram family in Prometheus
// text-exposition format 0.0.4 to w. Exactly one HELP and one
// TYPE preamble is emitted for the shared metric name; each
// per-verb series follows in ascending verb-name order with the
// `{verb="...",le="..."}` label set on every bucket line and
// `{verb="..."}` on every _sum / _count line.
//
// Output is deterministic: verbs are sorted alphabetically so
// scrape diffs and tests stay stable across runs.
func (v *verbLabelledHistogram) Write(w io.Writer) {
	if v == nil {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n", v.name, v.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", v.name)

	var verbs []string
	v.verbs.Range(func(key, _ any) bool {
		verbs = append(verbs, key.(string))
		return true
	})
	sort.Strings(verbs)

	for _, verb := range verbs {
		raw, ok := v.verbs.Load(verb)
		if !ok {
			continue
		}
		h := raw.(*obs.Histogram)
		buckets, sum, count := h.Snapshot()
		for i, ub := range v.bounds {
			fmt.Fprintf(w, "%s_bucket{verb=%s,le=%s} %d\n",
				v.name, promLabelValue(verb), promLabelValue(formatPromFloat(ub)), buckets[i])
		}
		fmt.Fprintf(w, "%s_bucket{verb=%s,le=%s} %d\n",
			v.name, promLabelValue(verb), promLabelValue("+Inf"), buckets[len(buckets)-1])
		fmt.Fprintf(w, "%s_sum{verb=%s} %s\n",
			v.name, promLabelValue(verb), formatPromFloat(sum))
		fmt.Fprintf(w, "%s_count{verb=%s} %d\n",
			v.name, promLabelValue(verb), count)
	}
}

// promLabelValue renders a Prometheus label value with the
// minimal required escaping (backslash, double-quote, newline)
// per the text exposition format spec. The harness's current
// verb constants don't contain any of these characters, but the
// guard keeps a future caller honest.
func promLabelValue(s string) string {
	// strconv.Quote handles the same three characters Prometheus
	// requires escaped (\, ", \n) plus other non-printables we
	// don't expect but might as well render safely.
	return strconv.Quote(s)
}

// formatPromFloat mirrors the private formatPromFloat in
// `internal/obs/histogram.go`: shortest exact 'g' format for
// finite values, special tokens for the Prometheus-defined
// non-finites. We replicate it locally rather than export the
// obs helper because the vec is a thin presentation layer that
// shouldn't be the trigger for widening obs's surface.
func formatPromFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, +1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
