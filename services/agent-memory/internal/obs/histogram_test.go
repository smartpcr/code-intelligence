package obs_test

import (
	"bytes"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

// TestHistogram_Default_Buckets_Cover_All_SLO_Lines pins the
// SLO-line boundaries — the alert rules in
// `deploy/alerts/agent-memory.rules.yml` rely on exact bucket
// boundaries at 0.4, 1.5, 2, 4, 5, 10. A regression that
// drops one of these silently breaks the alert because
// `histogram_quantile()` straddles the threshold.
func TestHistogram_Default_Buckets_Cover_All_SLO_Lines(t *testing.T) {
	required := []float64{0.4, 1.5, 2, 4, 5, 10}
	have := map[float64]bool{}
	for _, b := range obs.DefaultDurationBuckets {
		have[b] = true
	}
	for _, want := range required {
		if !have[want] {
			t.Errorf("DefaultDurationBuckets missing SLO-line %v", want)
		}
	}
}

func TestHistogram_Observe_Counts_Cumulatively(t *testing.T) {
	h := obs.NewHistogram("test_duration_seconds", "", []float64{0.1, 0.5, 1, 5})
	h.Observe(0.05)
	h.Observe(0.3)
	h.Observe(0.7)
	h.Observe(3)
	h.Observe(10) // overflow -- only the +Inf bucket
	buckets, sum, count := h.Snapshot()
	// 5 cells: 0.1, 0.5, 1, 5, +Inf
	if got := len(buckets); got != 5 {
		t.Fatalf("expected 5 cells (4 finite + +Inf), got %d", got)
	}
	wantBuckets := []uint64{1, 2, 3, 4, 5}
	for i, w := range wantBuckets {
		if buckets[i] != w {
			t.Errorf("cell %d: want %d, got %d", i, w, buckets[i])
		}
	}
	if count != 5 {
		t.Errorf("count: want 5, got %d", count)
	}
	wantSum := 0.05 + 0.3 + 0.7 + 3 + 10
	if math.Abs(sum-wantSum) > 1e-9 {
		t.Errorf("sum: want %v, got %v", wantSum, sum)
	}
}

func TestHistogram_Write_PrometheusFormat(t *testing.T) {
	h := obs.NewHistogram(
		"agent_recall_duration_seconds",
		"recall latency",
		[]float64{0.5, 1.5, 4},
	)
	h.Observe(0.2)
	h.Observe(1.0)
	h.Observe(2.0)

	var buf bytes.Buffer
	h.Write(&buf)
	body := buf.String()

	mustContain := []string{
		"# HELP agent_recall_duration_seconds recall latency",
		"# TYPE agent_recall_duration_seconds histogram",
		`agent_recall_duration_seconds_bucket{le="0.5"} 1`,
		`agent_recall_duration_seconds_bucket{le="1.5"} 2`,
		`agent_recall_duration_seconds_bucket{le="4"} 3`,
		`agent_recall_duration_seconds_bucket{le="+Inf"} 3`,
		"agent_recall_duration_seconds_sum 3.2",
		"agent_recall_duration_seconds_count 3",
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q\n=== full body ===\n%s", want, body)
		}
	}

	// The Prometheus parser expects the cumulative property —
	// every bucket count is monotonically non-decreasing.
	// Verify with a quick scan.
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var prev uint64
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "agent_recall_duration_seconds_bucket{le=") {
			continue
		}
		parts := strings.Fields(ln)
		if len(parts) < 2 {
			t.Fatalf("malformed bucket line %q", ln)
		}
		var v uint64
		for _, c := range parts[1] {
			if c < '0' || c > '9' {
				t.Fatalf("non-numeric bucket value %q in line %q", parts[1], ln)
			}
			v = v*10 + uint64(c-'0')
		}
		if v < prev {
			t.Errorf("non-monotonic bucket counts: %d after %d in %q", v, prev, ln)
		}
		prev = v
	}
}

func TestHistogram_Observe_Negative_Excluded_From_Sum(t *testing.T) {
	h := obs.NewHistogram("x_seconds", "", []float64{0.1, 1})
	h.Observe(-1)
	h.Observe(0.05)
	_, sum, count := h.Snapshot()
	if math.Abs(sum-0.05) > 1e-9 {
		t.Errorf("sum should exclude negative observation, got %v", sum)
	}
	if count != 2 {
		t.Errorf("count should include negative observation, got %d", count)
	}
}

func TestHistogram_Concurrent_Observe(t *testing.T) {
	// Race-detector smoke: 50 goroutines × 200 ops should
	// produce exactly 10_000 observations with no torn
	// _count value.
	h := obs.NewHistogram("y_seconds", "", []float64{0.5, 1, 2})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Observe(float64(i%3) / 2)
			}
		}(i)
	}
	wg.Wait()
	_, _, count := h.Snapshot()
	if count != 10_000 {
		t.Fatalf("expected 10_000 observations, got %d", count)
	}
}

func TestHistogram_Constructor_PanicsOnInvalidBuckets(t *testing.T) {
	cases := []struct {
		name        string
		upperBounds []float64
		wantPanic   string
	}{
		{
			name:        "empty bounds",
			upperBounds: []float64{},
			wantPanic:   "at least one bucket",
		},
		{
			name:        "non-monotonic",
			upperBounds: []float64{1, 0.5},
			wantPanic:   "ascending",
		},
		{
			name:        "explicit +Inf",
			upperBounds: []float64{1, math.Inf(+1)},
			wantPanic:   "+Inf is implicit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected panic, got none")
				}
				if msg, ok := r.(string); !ok || !strings.Contains(msg, tc.wantPanic) {
					t.Fatalf("panic %v does not contain %q", r, tc.wantPanic)
				}
			}()
			obs.NewHistogram("z", "", tc.upperBounds)
		})
	}
}

func TestHistogram_Nil_Safe(t *testing.T) {
	var h *obs.Histogram
	h.Observe(1) // must not panic
	var buf bytes.Buffer
	h.Write(&buf)
	if buf.Len() != 0 {
		t.Errorf("nil Histogram should write nothing, got %q", buf.String())
	}
	if h.Name() != "" {
		t.Errorf("nil Histogram Name() should be empty")
	}
}
