package spaningestor

import (
	"testing"
)

// TestLatencyAggregator_quantilesNearestRank validates the
// nearest-rank quantile contract on a known set so a future
// refactor cannot silently switch interpolation modes (which
// would shift dashboards).
func TestLatencyAggregator_quantilesNearestRank(t *testing.T) {
	cases := []struct {
		name        string
		samples     []float64
		wantP50     float64
		wantP95     float64
	}{
		{"single", []float64{42}, 42, 42},
		{"two", []float64{1, 100}, 1, 100},
		// nearest-rank: idx = ceil(p*N)-1, so p50 over 10 samples = idx 4 = 5
		{"ten ordered", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 5, 10},
		// 20 samples: p50 -> ceil(0.5*20)-1 = 9 -> value at idx 9 = 10;
		// p95 -> ceil(0.95*20)-1 = 18 -> value at idx 18 = 19.
		{"twenty", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, 10, 19},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotP50, gotP95 := quantiles(append([]float64(nil), tc.samples...))
			if gotP50 != tc.wantP50 {
				t.Errorf("p50 = %v, want %v", gotP50, tc.wantP50)
			}
			if gotP95 != tc.wantP95 {
				t.Errorf("p95 = %v, want %v", gotP95, tc.wantP95)
			}
		})
	}
}

// TestLatencyAggregator_observeReturnsCurrentWindowQuantiles
// confirms each Observe call returns the post-write quantiles
// across the rolling window of THAT key.
func TestLatencyAggregator_observeReturnsCurrentWindowQuantiles(t *testing.T) {
	a := NewLatencyAggregator(8, 4)
	// Observe four samples on the same key.
	p50, p95 := a.Observe("k", 10)
	if p50 != 10 || p95 != 10 {
		t.Errorf("first observe: p50=%v p95=%v, want (10,10)", p50, p95)
	}
	a.Observe("k", 20)
	a.Observe("k", 30)
	p50, p95 = a.Observe("k", 40)
	// sorted = [10,20,30,40]; p50 -> idx 1 = 20; p95 -> idx 3 = 40.
	if p50 != 20 || p95 != 40 {
		t.Errorf("after 4 observes: p50=%v p95=%v, want (20,40)", p50, p95)
	}
}

// TestLatencyAggregator_windowRolloverEvictsOldest confirms
// once the per-key window is full, the next write evicts the
// oldest sample, the windowEvictions counter increments, and
// quantiles reflect only the new window.
func TestLatencyAggregator_windowRolloverEvictsOldest(t *testing.T) {
	a := NewLatencyAggregator(8, 3)
	a.Observe("k", 100)
	a.Observe("k", 200)
	a.Observe("k", 300)
	// Window is now [100, 200, 300]. Push 1000: should evict 100.
	a.Observe("k", 1000)
	// Window logically now {200, 300, 1000}; sorted -> 200,300,1000;
	// p50 idx ceil(.5*3)-1 = 1 = 300; p95 idx ceil(.95*3)-1 = 2 = 1000.
	p50, p95 := a.Observe("k", 2000)
	// After this 5th observe (evicts 200): window {300,1000,2000};
	// sorted -> 300,1000,2000; p50 = 1000; p95 = 2000.
	if p50 != 1000 || p95 != 2000 {
		t.Errorf("after rollover: p50=%v p95=%v, want (1000,2000)", p50, p95)
	}
	we, _ := a.Snapshot()
	// 5 observes, capacity 3: 2 window evictions.
	if we != 2 {
		t.Errorf("windowEvictions = %d, want 2", we)
	}
}

// TestLatencyAggregator_keyLRUEvictionOnCapacity verifies the
// LRU policy: when the key count hits keyCap, the oldest
// (least-recently-observed) key is dropped.
func TestLatencyAggregator_keyLRUEvictionOnCapacity(t *testing.T) {
	a := NewLatencyAggregator(2, 4)
	a.Observe("k1", 1)
	a.Observe("k2", 2)
	if got := a.Size(); got != 2 {
		t.Fatalf("Size after two unique keys = %d, want 2", got)
	}
	// Re-touch k1 so k2 becomes the LRU.
	a.Observe("k1", 3)
	a.Observe("k3", 4)
	if got := a.Size(); got != 2 {
		t.Errorf("Size after third unique key = %d, want 2 (LRU eviction)", got)
	}
	_, ke := a.Snapshot()
	if ke != 1 {
		t.Errorf("keyEvictions = %d, want 1", ke)
	}
	// k1 should still be present (its window starts fresh on the
	// next observation if it was evicted; here, since we did
	// move-to-front, its window persists).
	p50, _ := a.Observe("k1", 100)
	// Existing window for k1 was [1,3] (no rollover); adding 100 → [1,3,100].
	// sorted = [1,3,100]; p50 idx 1 = 3.
	if p50 != 3 {
		t.Errorf("p50 after rejoining k1 = %v, want 3 (window preserved)", p50)
	}
}

// TestLatencyAggregator_negativeDurationClampsToZero defends
// against an emitter clock-jump injecting negative durations
// (which would distort the median).
func TestLatencyAggregator_negativeDurationClampsToZero(t *testing.T) {
	a := NewLatencyAggregator(4, 4)
	p50, _ := a.Observe("k", -5)
	if p50 != 0 {
		t.Errorf("negative observed: p50 = %v, want 0", p50)
	}
}
