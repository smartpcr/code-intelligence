package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

// knownMetricFamilies is the closed set of metric NAMES the
// dashboard / alert files may reference. Mirrors the §8.3 brief
// plus a small list of established Stage-8.1 / Stage-7.2 / §6.4
// metric names that the dashboard composes alongside the §8.3
// additions. Any panel or alert expression that mentions a name
// NOT in this set is a regression — either the dashboard / alert
// is stale (a metric was renamed) or the binary stopped emitting
// the metric.
//
// The list is split into "owned by this stage" (the §8.3 pinned
// names from internal/obs/metrics.go) and "owned by an earlier
// stage" (cross-stage references). The split is documentation,
// not enforcement — both halves are treated identically by the
// validator.
func knownMetricFamilies() map[string]struct{} {
	out := map[string]struct{}{}
	// Stage 8.3 -- pinned names.
	for _, n := range []string{
		obs.MetricRecallFilterUnpublishedTotal,
		obs.MetricSpanUnresolvedTotal,
		obs.MetricTrainerCappedActorTotal,
		obs.MetricMgmtIngestSpansTotal,
		obs.MetricSnapshotPublishedTotal,
		obs.MetricObserveWALBufferDepth,
		obs.MetricConsolidatorEpisodeLag,
		obs.MetricAgentRecallDurationSeconds,
		obs.MetricAgentObserveDurationSeconds,
		obs.MetricAgentExpandDurationSeconds,
		obs.MetricAgentSummarizeDurationSeconds,
		obs.MetricMgmtIngestSpansBatchDurationSeconds,
		obs.MetricPartitionProvisionLag,
		obs.MetricRerankerLastTrainedAt,
		obs.MetricRerankerLastTrainedAtSeconds,
		// Iter-3 evaluator fix #4: surface the sidecar/binary
		// metrics that iter-2 added but left out of the
		// dashboard + alert allowlist. Without these the
		// validator would reject any panel/alert that
		// references them.
		obs.MetricQdrantBootstrapRunsTotal,
		obs.MetricQdrantBootstrapDurationSeconds,
		obs.MetricQdrantBootstrapLastCompletedAt,
		obs.MetricRerankerSidecarInferenceTotal,
		obs.MetricRerankerSidecarInferenceDurationSeconds,
		// Iter-3 evaluator fix #2: webhook-receiver request
		// counter -- emitted by internal/webhookreceiver/metrics.go,
		// surfaced on /metrics by cmd/webhook-receiver/main.go.
		obs.MetricWebhookReceiverRequestsTotal,
	} {
		out[n] = struct{}{}
	}
	// Cross-stage references the §8.3 dashboard surfaces.
	for _, n := range []string{
		"agent_memory_degraded_total", // Stage 8.1
	} {
		out[n] = struct{}{}
	}
	return out
}

// metricRefRE matches a Prometheus metric name as it appears
// inside an expression: bare identifier or with the
// histogram-companion suffixes `_bucket`, `_sum`, `_count`. The
// final `\b` prevents `agent_recall_duration_seconds_bucket`
// from matching just `agent_recall_duration_seconds`.
var metricRefRE = regexp.MustCompile(`\b([a-z][a-z0-9_]+?)(?:_bucket|_sum|_count)?\b`)

// stripHistogramSuffix returns the histogram base name when n
// ends in one of the three companion suffixes Prometheus emits
// for a histogram (`_bucket`, `_sum`, `_count`); otherwise it
// returns n unchanged. The validator uses this to fold a
// `agent_recall_duration_seconds_bucket` reference back to the
// `agent_recall_duration_seconds` family registered in
// knownMetricFamilies.
func stripHistogramSuffix(n string) string {
	for _, sfx := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(n, sfx) {
			return strings.TrimSuffix(n, sfx)
		}
	}
	return n
}

// promQLBuiltinIdents are PromQL functions / keywords / unary
// operators that look like identifiers but are NOT metric names.
// The validator filters these out before treating the remaining
// identifiers as candidate metric references.
var promQLBuiltinIdents = map[string]struct{}{
	"sum": {}, "rate": {}, "irate": {}, "max": {}, "min": {},
	"avg": {}, "count": {}, "histogram_quantile": {},
	"absent": {}, "absent_over_time": {}, "by": {}, "without": {},
	"on": {}, "ignoring": {}, "group_left": {}, "group_right": {},
	"and": {}, "or": {}, "unless": {}, "if": {}, "le": {},
	"verb": {}, "reason": {}, "status": {}, "actor": {},
	"true": {}, "false": {}, "bool": {},
	"time": {}, "now": {}, "vector": {}, "scalar": {}, "humanizeDuration": {},
	"increase": {}, "delta": {}, "deriv": {}, "stddev": {},
	"stdvar": {}, "topk": {}, "bottomk": {}, "quantile": {},
}

// extractMetricRefs returns the unique candidate metric names
// referenced by a PromQL expression. The walker is intentionally
// regex-based — Prometheus does not ship a Go parser as a
// reusable library at this version and a regex + builtin filter
// catches drift just as effectively for the dashboard / rule
// surfaces this repo emits.
func extractMetricRefs(expr string) []string {
	seen := map[string]struct{}{}
	var out []string
	// Quick reject: drop quoted literals so a label-value
	// containing an identifier-shaped string can't masquerade as
	// a metric reference.
	expr = stripQuoted(expr)
	for _, m := range metricRefRE.FindAllStringSubmatch(expr, -1) {
		full := m[0]
		if _, ok := promQLBuiltinIdents[full]; ok {
			continue
		}
		base := stripHistogramSuffix(full)
		if _, ok := promQLBuiltinIdents[base]; ok {
			continue
		}
		if _, dup := seen[base]; dup {
			continue
		}
		seen[base] = struct{}{}
		out = append(out, base)
	}
	sort.Strings(out)
	return out
}

// stripQuoted returns `expr` with every `"..."` substring
// blanked out. Used by extractMetricRefs to avoid picking up
// identifiers from inside label-value literals.
func stripQuoted(expr string) string {
	var b strings.Builder
	b.Grow(len(expr))
	inQ := false
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if c == '"' && (i == 0 || expr[i-1] != '\\') {
			inQ = !inQ
			b.WriteByte(c)
			continue
		}
		if inQ {
			b.WriteByte(' ')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestDashboardJSONValid parses the dashboard JSON and asserts
// that every panel target's `expr` references only metric
// families this binary actually emits.
func TestDashboardJSONValid(t *testing.T) {
	t.Parallel()

	path := filepath.Join("dashboards", "agent-memory.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dashboard JSON: %v", err)
	}

	// Shape we care about — strict enough to catch a
	// hand-edited dashboard that loses its panels block, lax
	// enough to tolerate Grafana version drift in unrelated
	// fields.
	type target struct {
		Expr    string `json:"expr"`
		LegendF string `json:"legendFormat"`
		RefID   string `json:"refId"`
	}
	type panel struct {
		ID      json.Number `json:"id"`
		Type    string      `json:"type"`
		Title   string      `json:"title"`
		Targets []target    `json:"targets"`
	}
	type dashboard struct {
		Title  string  `json:"title"`
		UID    string  `json:"uid"`
		Panels []panel `json:"panels"`
	}
	var d dashboard
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("dashboard JSON malformed: %v", err)
	}
	if d.Title == "" {
		t.Fatalf("dashboard JSON missing title")
	}
	if d.UID == "" {
		t.Fatalf("dashboard JSON missing uid")
	}
	if len(d.Panels) == 0 {
		t.Fatalf("dashboard JSON declares no panels")
	}

	known := knownMetricFamilies()
	var problems []string
	usedNames := map[string]struct{}{}
	for _, p := range d.Panels {
		if len(p.Targets) == 0 {
			problems = append(problems, fmt.Sprintf("panel %q has no targets", p.Title))
			continue
		}
		for _, tg := range p.Targets {
			if strings.TrimSpace(tg.Expr) == "" {
				problems = append(problems,
					fmt.Sprintf("panel %q target %q has empty expr", p.Title, tg.RefID))
				continue
			}
			for _, ref := range extractMetricRefs(tg.Expr) {
				if _, ok := known[ref]; !ok {
					problems = append(problems,
						fmt.Sprintf("panel %q references unknown metric %q (expr: %s)",
							p.Title, ref, tg.Expr))
				}
				usedNames[ref] = struct{}{}
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("dashboard validation failed:\n  - %s",
			strings.Join(problems, "\n  - "))
	}

	// Sanity: the dashboard MUST mention every §8.3-pinned
	// latency histogram and the snapshot counter, otherwise the
	// dashboard is incomplete vs the brief.
	required := []string{
		obs.MetricAgentRecallDurationSeconds,
		obs.MetricAgentObserveDurationSeconds,
		obs.MetricAgentExpandDurationSeconds,
		obs.MetricAgentSummarizeDurationSeconds,
		obs.MetricMgmtIngestSpansBatchDurationSeconds,
		obs.MetricSnapshotPublishedTotal,
		obs.MetricRecallFilterUnpublishedTotal,
		obs.MetricSpanUnresolvedTotal,
		obs.MetricTrainerCappedActorTotal,
		obs.MetricMgmtIngestSpansTotal,
		obs.MetricObserveWALBufferDepth,
		obs.MetricConsolidatorEpisodeLag,
		obs.MetricPartitionProvisionLag,
		obs.MetricRerankerLastTrainedAt,
	}
	for _, n := range required {
		if _, ok := usedNames[n]; !ok {
			t.Errorf("dashboard MUST surface metric %q (Stage 8.3 brief), but no panel references it", n)
		}
	}
}
