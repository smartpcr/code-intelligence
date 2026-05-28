package telemetry

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestAggregatorTickMetrics_WriteText_EmitsHistogramShape(t *testing.T) {
	m := NewAggregatorTickMetrics()
	m.Observe(5 * time.Millisecond)
	m.Observe(40 * time.Millisecond)
	m.Observe(2 * time.Second)

	var buf bytes.Buffer
	if _, err := m.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	text := buf.String()
	expectSubstrings(t, text,
		"# TYPE "+MetricNameAggregatorTickDurationSeconds+" histogram",
		MetricNameAggregatorTickDurationSeconds+`_bucket{le="0.005"} 1`,
		MetricNameAggregatorTickDurationSeconds+`_bucket{le="0.05"} 2`,
		MetricNameAggregatorTickDurationSeconds+`_bucket{le="2.5"} 3`,
		MetricNameAggregatorTickDurationSeconds+`_bucket{le="+Inf"} 3`,
		MetricNameAggregatorTickDurationSeconds+`_count 3`,
	)
}

func TestAggregatorTickMetrics_NilSafe(t *testing.T) {
	var m *AggregatorTickMetrics
	// Must not panic.
	m.Observe(1 * time.Second)
	var buf bytes.Buffer
	n, err := m.WriteText(&buf)
	if err != nil || n != 0 {
		t.Fatalf("nil WriteText: n=%d err=%v; want 0/nil", n, err)
	}
}

func TestWALReplayMetrics_WriteText_EmitsHistogramShape(t *testing.T) {
	m := NewWALReplayMetrics()
	m.Observe(250 * time.Millisecond)

	var buf bytes.Buffer
	if _, err := m.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	text := buf.String()
	expectSubstrings(t, text,
		"# TYPE "+MetricNameWALReplayDurationSeconds+" histogram",
		MetricNameWALReplayDurationSeconds+`_bucket{le="0.25"} 1`,
		MetricNameWALReplayDurationSeconds+`_bucket{le="+Inf"} 1`,
		MetricNameWALReplayDurationSeconds+`_count 1`,
	)
}

func TestRuleEngineMetrics_WriteText_TotalAndByVerdict(t *testing.T) {
	m := NewRuleEngineMetrics()
	m.Observe("pass")
	m.Observe("pass")
	m.Observe("warn")
	m.Observe("block")
	m.Observe("garbage") // folds to "unknown"

	var buf bytes.Buffer
	if _, err := m.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	text := buf.String()
	expectSubstrings(t, text,
		"# TYPE "+MetricNameRuleEngineEvaluationsTotal+" counter",
		MetricNameRuleEngineEvaluationsTotal+" 5",
		"# TYPE "+MetricNameRuleEngineEvaluationsByVerdictTotal+" counter",
		MetricNameRuleEngineEvaluationsByVerdictTotal+`{verdict="block"} 1`,
		MetricNameRuleEngineEvaluationsByVerdictTotal+`{verdict="pass"} 2`,
		MetricNameRuleEngineEvaluationsByVerdictTotal+`{verdict="unknown"} 1`,
		MetricNameRuleEngineEvaluationsByVerdictTotal+`{verdict="warn"} 1`,
	)
}

func TestRuleEngineMetrics_NilSafe(t *testing.T) {
	var m *RuleEngineMetrics
	m.Observe("pass") // must not panic
	var buf bytes.Buffer
	n, err := m.WriteText(&buf)
	if err != nil || n != 0 {
		t.Fatalf("nil WriteText: n=%d err=%v; want 0/nil", n, err)
	}
}

func TestHistogram_CumulativeBuckets(t *testing.T) {
	h := newHistogram(DefaultDurationBuckets)
	for i := 0; i < 100; i++ {
		h.observe(0.001) // 1ms: falls into ALL buckets >= 0.005
	}
	_, counts, total, _ := h.snapshot()
	if total != 100 {
		t.Errorf("total = %d; want 100", total)
	}
	// Every bucket boundary >= 0.005 should include all 100
	// observations -- cumulative shape.
	for i, c := range counts {
		if c != 100 {
			t.Errorf("counts[%d] (le=%v) = %d; want 100", i, DefaultDurationBuckets[i], c)
		}
	}
}

func TestCanonicalVerdictLabel(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"pass", "pass"},
		{"warn", "warn"},
		{"block", "block"},
		{"", "unknown"},
		{"PASS", "unknown"},
		{"refactor", "unknown"},
	} {
		if got := canonicalVerdictLabel(tc.in); got != tc.want {
			t.Errorf("canonicalVerdictLabel(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultDurationBuckets_MatchesArchitecturePin(t *testing.T) {
	want := []float64{
		0.005, 0.01, 0.025, 0.05, 0.1,
		0.25, 0.5, 1, 2.5, 5,
		10, 30, 60, 120, 300,
	}
	if len(DefaultDurationBuckets) != len(want) {
		t.Fatalf("len(DefaultDurationBuckets) = %d; want %d", len(DefaultDurationBuckets), len(want))
	}
	for i, v := range want {
		if DefaultDurationBuckets[i] != v {
			t.Errorf("bucket[%d] = %v; want %v", i, DefaultDurationBuckets[i], v)
		}
	}
}

// expectSubstrings asserts every wanted substring is
// present in `text`. Reports ALL misses in one call so the
// test output points the maintainer at every regression in
// one pass.
func expectSubstrings(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("output missing substring:\n  want substring: %q\n  full output: %s", w, text)
		}
	}
}
