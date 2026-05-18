package degraded

import (
	"sync"
	"testing"
)

func TestCounter_basicIncrement(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	c.IncDegraded("agent.recall", ReasonGraphStoreUnavailable)
	c.IncDegraded("agent.recall", ReasonGraphStoreUnavailable)
	c.IncDegraded("agent.recall", ReasonEmbeddingIndexUnavailable)
	c.IncDegraded("agent.observe", ReasonConsolidatorBackpressure)

	if got := c.Count("agent.recall", ReasonGraphStoreUnavailable); got != 2 {
		t.Errorf("recall/graph = %d; want 2", got)
	}
	if got := c.Count("agent.recall", ReasonEmbeddingIndexUnavailable); got != 1 {
		t.Errorf("recall/embed = %d; want 1", got)
	}
	if got := c.Count("agent.observe", ReasonConsolidatorBackpressure); got != 1 {
		t.Errorf("observe/cons = %d; want 1", got)
	}
	if got := c.Count("agent.observe", ReasonGraphStoreUnavailable); got != 0 {
		t.Errorf("observe/graph = %d; want 0 (never incremented)", got)
	}
}

func TestCounter_concurrentIncrement(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	const goroutines = 32
	const perGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				c.IncDegraded("agent.observe", ReasonConsolidatorBackpressure)
			}
		}()
	}
	wg.Wait()
	want := int64(goroutines * perGoroutine)
	if got := c.Count("agent.observe", ReasonConsolidatorBackpressure); got != want {
		t.Errorf("count = %d; want %d", got, want)
	}
}

func TestCounter_snapshotDeepCopy(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	c.IncDegraded("agent.recall", ReasonGraphStoreUnavailable)
	snap := c.Snapshot()
	if snap["agent.recall"][ReasonGraphStoreUnavailable] != 1 {
		t.Fatalf("snapshot missing entry: %+v", snap)
	}
	// Mutating the snapshot must not affect the counter.
	snap["agent.recall"][ReasonGraphStoreUnavailable] = 99
	if c.Count("agent.recall", ReasonGraphStoreUnavailable) != 1 {
		t.Errorf("snapshot mutation leaked back to counter")
	}
}

func TestCounter_reset(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	c.IncDegraded("agent.recall", ReasonGraphStoreUnavailable)
	c.Reset()
	if got := c.Count("agent.recall", ReasonGraphStoreUnavailable); got != 0 {
		t.Errorf("after Reset count = %d; want 0", got)
	}
}

func TestCounter_nilSafe(t *testing.T) {
	t.Parallel()
	var c *Counter
	c.IncDegraded("agent.recall", ReasonGraphStoreUnavailable)
	if got := c.Count("agent.recall", ReasonGraphStoreUnavailable); got != 0 {
		t.Errorf("nil count = %d; want 0", got)
	}
	c.Reset() // must not panic
	if snap := c.Snapshot(); snap != nil {
		t.Errorf("nil snapshot = %v; want nil", snap)
	}
}

func TestNopMetric_doesNothing(t *testing.T) {
	t.Parallel()
	NopMetric.IncDegraded("any", "thing") // must not panic
}
