package degraded

// Stage 8.1 step 4 — operator-facing Prometheus exposition
// for the per-verb degraded counter.
//
// The shape mirrors mgmtapi's PrometheusMetricsHandler: text
// exposition format 0.0.4, deterministic ordering, no
// dependency on the upstream prometheus client library so the
// agent-memory binaries stay light. The single metric is:
//
//	# HELP agent_memory_degraded_total Number of degraded
//	#      verb responses by verb and closed-set reason.
//	# TYPE agent_memory_degraded_total counter
//	agent_memory_degraded_total{verb="agent.observe",reason="consolidator_backpressure"} N
//
// Pre-registration: the handler always emits a row at 0 for
// every (verb in `KnownVerbs`) × (reason in `AllReasons()`)
// pair the Snapshot does not cover. This is the iter-3 fix for
// evaluator finding #3: an operator's dashboard needs the
// label series to exist at scrape time even if the verb has
// never gone degraded — otherwise the time-series database
// emits gaps that read as "metric missing" rather than
// "value=0". Recorded pairs that lie outside the pre-
// registration table (e.g. a future verb the test seam
// flipped) are still emitted, sorted after the known pairs.

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// MetricsContentType is the Prometheus text exposition format
// media type. Mirrors the one mgmtapi uses so the two handlers
// can share a /metrics surface.
const MetricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// MetricName is the single Prometheus counter the
// agent-memory service exposes for the §8.1 dashboard.
const MetricName = "agent_memory_degraded_total"

// MetricHelp is the HELP line that accompanies MetricName in
// the exposition output. Operators see this verbatim when
// inspecting the metric in the Prometheus UI.
const MetricHelp = "Number of degraded responses by verb and closed-set reason (architecture.md §8.2, implementation-plan.md Stage 8.1 step 4)."

// KnownVerbs is the explicit list of verb identifiers wired
// to the [Counter] in production. Pre-registering this list
// keeps operator dashboards in sync with the Stage 8.1 audit
// surface — every verb that funnels through `Enforce` MUST
// have a row in the pre-registration table so the time series
// exists at scrape time.
//
// Agent verbs come from `internal/agentapi`:
//   - agent.observe
//   - agent.recall
//   - agent.expand
//   - agent.summarize
//
// Management verbs come from `internal/mgmtapi/read.go`:
//   - mgmt.read.repos / commits / episodes / observations /
//     context / concepts / concept_supports / graph_node /
//     trace_observation
//
// (We embed these as strings rather than importing the
// constants because the agentapi/mgmtapi packages import this
// package, not the other way round — cycle avoidance.)
var KnownVerbs = []string{
	"agent.observe",
	"agent.recall",
	"agent.expand",
	"agent.summarize",
	"mgmt.read.repos",
	"mgmt.read.commits",
	"mgmt.read.episodes",
	"mgmt.read.observations",
	"mgmt.read.context",
	"mgmt.read.concepts",
	"mgmt.read.concept_supports",
	"mgmt.read.graph_node",
	"mgmt.read.trace_observation",
}

// PrometheusHandler renders a [Counter] as Prometheus text.
// Construct via [NewPrometheusHandler]. ServeHTTP is safe
// for concurrent use across goroutines (Snapshot uses an
// RLock).
type PrometheusHandler struct {
	c *Counter
}

// NewPrometheusHandler returns a handler that exposes c at
// the standard /metrics location. A nil `c` is OK — the
// handler emits only the HELP / TYPE lines plus the pre-
// registered zero rows.
func NewPrometheusHandler(c *Counter) *PrometheusHandler {
	return &PrometheusHandler{c: c}
}

// ServeHTTP implements http.Handler.
func (h *PrometheusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"error":"method_not_allowed"}`)
		return
	}
	w.Header().Set("Content-Type", MetricsContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	h.Write(w)
}

// Write emits the metric body to w. Factored out so the
// caller can chain multiple metric families into a single
// /metrics response (mgmtapi composes this with its own
// handler) and so tests can render into a buffer without
// faking an http.ResponseWriter.
//
// The output begins with the HELP / TYPE prelude. When chaining
// inside another /metrics body, write this body BEFORE writing
// any other metric family — the Prometheus parser tolerates
// either ordering as long as a given metric name is contiguous.
func (h *PrometheusHandler) Write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", MetricName, MetricHelp)
	fmt.Fprintf(w, "# TYPE %s counter\n", MetricName)

	var snap map[string]map[string]int64
	if h != nil && h.c != nil {
		snap = h.c.Snapshot()
	}

	type row struct {
		verb, reason string
		value        int64
	}
	rows := make([]row, 0, len(KnownVerbs)*len(AllReasons()))
	registered := make(map[string]map[string]struct{}, len(KnownVerbs))
	// Pre-registered (verb × reason) rows at zero (or the
	// recorded value if the pair fired).
	for _, v := range KnownVerbs {
		registered[v] = make(map[string]struct{}, len(AllReasons()))
		for _, r := range AllReasons() {
			registered[v][r] = struct{}{}
			val := int64(0)
			if inner, ok := snap[v]; ok {
				if c, ok := inner[r]; ok {
					val = c
				}
			}
			rows = append(rows, row{verb: v, reason: r, value: val})
		}
	}
	// Recorded rows that fall outside the pre-registration
	// table (e.g. a new verb / new reason via the test seam).
	// Emit them too so operators see the unexpected label
	// rather than silently dropping it.
	for v, inner := range snap {
		known, _ := registered[v]
		for r, c := range inner {
			if known != nil {
				if _, ok := known[r]; ok {
					continue
				}
			}
			rows = append(rows, row{verb: v, reason: r, value: c})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].verb != rows[j].verb {
			return rows[i].verb < rows[j].verb
		}
		return rows[i].reason < rows[j].reason
	})
	for _, r := range rows {
		fmt.Fprintf(w, "%s{reason=\"%s\",verb=\"%s\"} %d\n",
			MetricName,
			escapePromLabelValue(r.reason),
			escapePromLabelValue(r.verb),
			r.value,
		)
	}
}

// escapePromLabelValue applies the three character escapes the
// Prometheus text exposition format requires inside label
// values: backslash, double quote, newline. Identical shape to
// the helper in mgmtapi/metrics_handler.go; duplicated here so
// this package stays import-independent.
func escapePromLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
