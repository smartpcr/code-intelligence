package rerankertrainer

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// TestSnapshot_StableShape asserts the Snapshot map carries
// every counter Metric* constant and does NOT silently leak
// the per-actor map or the gauge into the counter table.
// A regression that bolted a new counter onto the package
// would either add an entry here (intentional) or fail one of
// these assertions (accidental).
func TestSnapshot_StableShape(t *testing.T) {
	m := NewMetrics()
	got := m.Snapshot()

	want := []string{
		MetricRerankerRunsTotal,
		MetricRerankerErrorsTotal,
		MetricRerankerLockSkippedTotal,
		MetricRerankerModelsPublishedTotal,
		MetricRerankerPositivePairsTotal,
		MetricRerankerNegativePairsTotal,
		MetricRerankerEpisodesBelowMinTotal,
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("Snapshot missing %q", name)
		}
	}
	// The per-actor counter and the alias MUST NOT appear in
	// the COUNTER snapshot -- they are dimensional and live in
	// CappedActorSnapshot.
	if _, ok := got[MetricRerankerCappedActorTotal]; ok {
		t.Errorf("Snapshot leaked dimensional metric %q", MetricRerankerCappedActorTotal)
	}
	if _, ok := got[AltMetricCappedActorTotal]; ok {
		t.Errorf("Snapshot leaked dimensional metric %q", AltMetricCappedActorTotal)
	}
	// The gauge MUST NOT appear; gauges are uint64-unsafe and
	// surface through LastTrainedAt instead.
	if _, ok := got[MetricRerankerLastTrainedAtSeconds]; ok {
		t.Errorf("Snapshot leaked gauge metric %q", MetricRerankerLastTrainedAtSeconds)
	}
}

// TestSnapshot_ReflectsIncrements asserts the counters round-
// trip through Snapshot. The values are distinct primes so a
// renaming regression that confused two counters surfaces as a
// value mismatch rather than a coincidental match.
func TestSnapshot_ReflectsIncrements(t *testing.T) {
	m := NewMetrics()
	m.IncRuns()
	m.IncRuns()
	m.IncRuns()
	m.IncErrors()
	m.IncLockSkipped()
	m.IncLockSkipped()
	m.IncModelsPublished()
	m.AddPositivePairs(11)
	m.AddNegativePairs(13)
	m.IncEpisodesBelowMin()

	snap := m.Snapshot()
	want := map[string]uint64{
		MetricRerankerRunsTotal:             3,
		MetricRerankerErrorsTotal:           1,
		MetricRerankerLockSkippedTotal:      2,
		MetricRerankerModelsPublishedTotal:  1,
		MetricRerankerPositivePairsTotal:    11,
		MetricRerankerNegativePairsTotal:    13,
		MetricRerankerEpisodesBelowMinTotal: 1,
	}
	for k, v := range want {
		if snap[k] != v {
			t.Errorf("%s: got %d want %d", k, snap[k], v)
		}
	}
}

// TestCappedActorSnapshot_SortedDeterministic asserts that
// per-actor exposition is sorted alphabetically so two back-
// to-back /metrics scrapes return byte-identical bodies.
// A regression that switched the snapshot to map iteration
// would surface as a Prometheus scrape-diff alert.
func TestCappedActorSnapshot_SortedDeterministic(t *testing.T) {
	m := NewMetrics()
	m.AddCappedActor("operator", 3)
	m.AddCappedActor("consolidator", 1)
	m.AddCappedActor("system", 2)
	m.AddCappedActor("operator", 4) // accumulates

	snap1 := m.CappedActorSnapshot()
	snap2 := m.CappedActorSnapshot()

	if len(snap1) != 3 {
		t.Fatalf("expected 3 distinct actors, got %d (%v)", len(snap1), snap1)
	}
	gotOrder := make([]string, len(snap1))
	for i, s := range snap1 {
		gotOrder[i] = s.Actor
	}
	wantOrder := []string{"consolidator", "operator", "system"}
	if !sort.StringsAreSorted(gotOrder) {
		t.Fatalf("snapshot not sorted: %v", gotOrder)
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Errorf("snap[%d].Actor = %q want %q", i, gotOrder[i], want)
		}
	}
	if snap1[1].Count != 7 {
		t.Errorf("operator count: got %d want 7 (3+4)", snap1[1].Count)
	}
	// Determinism: two snapshots over identical state are
	// byte-identical.
	for i := range snap1 {
		if snap1[i] != snap2[i] {
			t.Errorf("snapshot drift at %d: %+v vs %+v", i, snap1[i], snap2[i])
		}
	}
}

// TestCappedActorTotal_PerActorLookup asserts the convenience
// accessor returns the per-actor count, or zero for unknown.
func TestCappedActorTotal_PerActorLookup(t *testing.T) {
	m := NewMetrics()
	m.AddCappedActor("operator", 5)
	if got := m.CappedActorTotal("operator"); got != 5 {
		t.Errorf("CappedActorTotal(operator) = %d want 5", got)
	}
	if got := m.CappedActorTotal("nobody"); got != 0 {
		t.Errorf("CappedActorTotal(unknown) = %d want 0", got)
	}
}

// TestCappedActor_Concurrent asserts AddCappedActor is safe
// under contention. A regression that dropped the mutex would
// surface as a race-detector report (run via `go test -race`)
// and most likely as a flaky count.
func TestCappedActor_Concurrent(t *testing.T) {
	m := NewMetrics()
	const goroutines = 16
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				m.AddCappedActor("operator", 1)
			}
		}()
	}
	wg.Wait()
	if got := m.CappedActorTotal("operator"); got != goroutines*perGoroutine {
		t.Errorf("CappedActorTotal: got %d want %d", got, goroutines*perGoroutine)
	}
}

// TestLastTrainedAt_ZeroValueWhenUnset asserts the gauge
// returns Go's zero time before any record. The recall.go
// staleness gate interprets `ok=false` identically to
// `(_, false, nil)` from the freshness source.
func TestLastTrainedAt_ZeroValueWhenUnset(t *testing.T) {
	m := NewMetrics()
	got := m.LastTrainedAt()
	if !got.IsZero() {
		t.Fatalf("LastTrainedAt should be zero before any record, got %v", got)
	}
}

// TestLastTrainedAt_RecordsAndReads asserts SetLastTrainedAt
// round-trips through LastTrainedAt at second granularity (the
// gauge stores Unix seconds).
func TestLastTrainedAt_RecordsAndReads(t *testing.T) {
	m := NewMetrics()
	when := time.Date(2026, 6, 1, 12, 30, 45, 0, time.UTC)
	m.SetLastTrainedAt(when)
	got := m.LastTrainedAt()
	if !got.Equal(when) {
		t.Errorf("LastTrainedAt: got %v want %v", got, when)
	}
}

// TestAliasCanonicalIdentity asserts the two metric-name
// strings are NOT accidentally the same -- the §6.4 scenario
// document spells the per-actor cap counter with the
// `trainer_` prefix while the package-canonical name is
// `reranker_`, and the binary's /metrics handler emits BOTH
// families. A regression that collapsed them into one symbol
// would silently break either the scenario test or the
// dashboards.
func TestAliasCanonicalIdentity(t *testing.T) {
	if MetricRerankerCappedActorTotal == AltMetricCappedActorTotal {
		t.Fatalf("canonical and alias must differ: got both = %q",
			MetricRerankerCappedActorTotal)
	}
	if MetricRerankerCappedActorTotal != "reranker_capped_actor_total" {
		t.Errorf("canonical name drift: %q", MetricRerankerCappedActorTotal)
	}
	if AltMetricCappedActorTotal != "trainer_capped_actor_total" {
		t.Errorf("alias name drift: %q", AltMetricCappedActorTotal)
	}
}
