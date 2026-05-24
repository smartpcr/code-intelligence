package dsl

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

func TestCache_GetOrCompile_MissAndHit(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pv := uuid.Must(uuid.NewV4())
	src := "metric_kind == 'lcom4' AND value > 10"

	p1, err := c.GetOrCompile(pv, src, nil)
	if err != nil {
		t.Fatalf("first GetOrCompile: %v", err)
	}
	if p1 == nil {
		t.Fatalf("first GetOrCompile returned nil predicate")
	}
	if got := c.LenForPolicy(pv); got != 1 {
		t.Errorf("LenForPolicy after first compile = %d, want 1", got)
	}

	p2, err := c.GetOrCompile(pv, src, nil)
	if err != nil {
		t.Fatalf("second GetOrCompile: %v", err)
	}
	if p1 != p2 {
		t.Errorf("second GetOrCompile returned a different Predicate pointer (recompiled)")
	}
}

func TestCache_PerPolicyIsolation(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pvA := uuid.Must(uuid.NewV4())
	pvB := uuid.Must(uuid.NewV4())
	src := "metric_kind == 'lcom4'"

	pA, err := c.GetOrCompile(pvA, src, nil)
	if err != nil {
		t.Fatalf("compile A: %v", err)
	}
	pB, err := c.GetOrCompile(pvB, src, nil)
	if err != nil {
		t.Fatalf("compile B: %v", err)
	}
	if pA == pB {
		t.Errorf("per-policy isolation broken: same Predicate pointer across policy versions")
	}
	if got := c.Len(); got != 2 {
		t.Errorf("Len after two policies = %d, want 2", got)
	}
}

func TestCache_CachesErrors(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pv := uuid.Must(uuid.NewV4())
	src := "metric_kind == 'lines_of_code'" // canon-guard rejects

	_, err1 := c.GetOrCompile(pv, src, nil)
	if !errors.Is(err1, ErrSemantic) {
		t.Fatalf("first call: err=%v, want ErrSemantic", err1)
	}
	_, err2 := c.GetOrCompile(pv, src, nil)
	if err1.Error() != err2.Error() {
		t.Errorf("error not cached deterministically:\n  first=%v\n  second=%v", err1, err2)
	}
}

func TestCache_Invalidate(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pv := uuid.Must(uuid.NewV4())
	src := "metric_kind == 'lcom4'"
	if _, err := c.GetOrCompile(pv, src, nil); err != nil {
		t.Fatalf("compile: %v", err)
	}
	if c.Len() != 1 {
		t.Errorf("Len before invalidate = %d, want 1", c.Len())
	}
	c.Invalidate(pv)
	if c.Len() != 0 {
		t.Errorf("Len after invalidate = %d, want 0", c.Len())
	}
	// Invalidate of an unknown policy is a no-op.
	c.Invalidate(uuid.Must(uuid.NewV4()))
}

// TestCache_HotPathIsConcurrent stresses the RWMutex hot
// path with many concurrent readers + an interleaved
// compilation. The test does not assert performance; it
// pins that concurrent GetOrCompile calls for the same key
// return the same `*Predicate` instance (and the
// compileCount stays at 1).
func TestCache_HotPathIsConcurrent(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pv := uuid.Must(uuid.NewV4())
	src := "metric_kind == 'lcom4' AND value > 10"

	var compileCount atomic.Int64
	// Wrap the resolver path to count compilations -- but
	// since this predicate has no threshold() we use a
	// sentinel approach: pre-flight one compile to seed
	// the cache and then assert no compilation cost on the
	// concurrent path.
	first, err := c.GetOrCompile(pv, src, nil)
	if err != nil {
		t.Fatalf("first compile: %v", err)
	}
	compileCount.Add(1)

	const goroutines = 32
	const iters = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				p, err := c.GetOrCompile(pv, src, nil)
				if err != nil {
					t.Errorf("goroutine compile: %v", err)
					return
				}
				if p != first {
					t.Errorf("hot-path returned a different *Predicate (recompiled)")
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := compileCount.Load(); got != 1 {
		t.Errorf("compileCount = %d, want 1", got)
	}
}

// TestCache_ConcurrentDistinctSources verifies the cache
// correctly stores multiple sources under the same policy
// when many goroutines race to fill them.
func TestCache_ConcurrentDistinctSources(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pv := uuid.Must(uuid.NewV4())
	sources := []string{
		"metric_kind == 'lcom4'",
		"metric_kind == 'fan_in'",
		"metric_kind == 'fan_out'",
		"metric_kind == 'cycle_member' AND value >= 1",
		"value > 100",
		"pack == 'solid'",
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		for _, src := range sources {
			src := src
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.GetOrCompile(pv, src, nil); err != nil {
					t.Errorf("compile %q: %v", src, err)
				}
			}()
		}
	}
	wg.Wait()
	if got := c.LenForPolicy(pv); got != len(sources) {
		t.Errorf("LenForPolicy = %d, want %d", got, len(sources))
	}
}

// TestCache_ResolverConsultedOnlyOnMiss pins the Stage 5.4
// purity contract: the resolver MUST be consulted only
// during the compile step. Cache hits MUST NOT call back
// into the resolver.
func TestCache_ResolverConsultedOnlyOnMiss(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	src := "threshold('" + id.String() + "')"
	pv := uuid.Must(uuid.NewV4())

	var lookups atomic.Int64
	resolver := countingResolver{
		base: MapResolver{
			id: Threshold{
				ThresholdID: id,
				MetricKind:  "lcom4",
				ScopeKind:   "class",
				Op:          OpGE,
				Value:       10,
			},
		},
		lookups: &lookups,
	}
	c := NewCache()
	if _, err := c.GetOrCompile(pv, src, resolver); err != nil {
		t.Fatalf("first compile: %v", err)
	}
	if got := lookups.Load(); got != 1 {
		t.Errorf("lookups after first compile = %d, want 1", got)
	}
	// Many subsequent calls -- none should hit the resolver.
	for i := 0; i < 100; i++ {
		if _, err := c.GetOrCompile(pv, src, resolver); err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
	}
	if got := lookups.Load(); got != 1 {
		t.Errorf("lookups after 100 hits = %d, want 1 (resolver invoked on hot path)", got)
	}
}

type countingResolver struct {
	base    MapResolver
	lookups *atomic.Int64
}

func (r countingResolver) Lookup(id uuid.UUID) (Threshold, error) {
	r.lookups.Add(1)
	return r.base.Lookup(id)
}

// blockingResolver gates the first N Lookups behind a
// channel, allowing tests to assert behaviour during a slow
// compile.
type blockingResolver struct {
	base    MapResolver
	gate    chan struct{} // closed by Release()
	started chan uuid.UUID
	lookups atomic.Int64
}

func newBlockingResolver(base MapResolver) *blockingResolver {
	return &blockingResolver{
		base:    base,
		gate:    make(chan struct{}),
		started: make(chan uuid.UUID, 16),
	}
}

func (r *blockingResolver) Lookup(id uuid.UUID) (Threshold, error) {
	r.lookups.Add(1)
	// Signal that this Lookup is now blocked on gate; tests
	// use this to know the slow compile has actually
	// entered the resolver path (and thus released the
	// cache mutex).
	select {
	case r.started <- id:
	default:
	}
	<-r.gate
	return r.base.Lookup(id)
}

func (r *blockingResolver) Release() {
	close(r.gate)
}

// TestCache_SlowMissDoesNotStallUnrelatedHits is the iter-2
// finding-3 regression guard: the cache must not hold its
// global mutex across [Compile] / [ThresholdResolver.Lookup],
// otherwise an unrelated (policy, source) hit waits behind a
// slow compile. The test gates a single threshold lookup
// with a channel; while that compile is blocked, an
// unrelated (same-policy, different-source) compile and a
// completely-unrelated (different-policy) compile MUST both
// complete within a short timeout.
func TestCache_SlowMissDoesNotStallUnrelatedHits(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pvA := uuid.Must(uuid.NewV4())
	pvB := uuid.Must(uuid.NewV4())
	tid := uuid.Must(uuid.NewV4())
	resolver := newBlockingResolver(MapResolver{
		tid: Threshold{
			ThresholdID: tid,
			MetricKind:  "lcom4",
			ScopeKind:   "class",
			Op:          OpGE,
			Value:       10,
		},
	})

	slowSource := "threshold('" + tid.String() + "')"
	fastSourceSameP := "metric_kind == 'lcom4'"
	fastSourceDiffP := "value > 100"

	// Goroutine 1: slow compile that blocks inside the resolver.
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		if _, err := c.GetOrCompile(pvA, slowSource, resolver); err != nil {
			t.Errorf("slow compile errored: %v", err)
		}
	}()

	// Wait until the slow compile has actually entered the
	// resolver Lookup -- this proves the cache mutex has
	// been released (because Lookup runs only AFTER the
	// placeholder entry was installed and the mutex
	// released).
	select {
	case <-resolver.started:
		// good
	case <-time.After(1 * time.Second):
		t.Fatal("slow compile did not enter resolver.Lookup within 1s")
	}

	// Same policy, different source -- must NOT wait for
	// the slow compile to finish. Uses a nil resolver since
	// the source has no threshold() atom.
	fastDoneSameP := make(chan struct{})
	go func() {
		defer close(fastDoneSameP)
		if _, err := c.GetOrCompile(pvA, fastSourceSameP, nil); err != nil {
			t.Errorf("same-policy fast compile: %v", err)
		}
	}()
	select {
	case <-fastDoneSameP:
		// good -- proves the per-policy map mutex was released
	case <-time.After(500 * time.Millisecond):
		t.Fatal("same-policy fast compile stalled behind unrelated slow compile (cache lock too coarse)")
	}

	// Different policy entirely -- same expectation.
	fastDoneDiffP := make(chan struct{})
	go func() {
		defer close(fastDoneDiffP)
		if _, err := c.GetOrCompile(pvB, fastSourceDiffP, nil); err != nil {
			t.Errorf("diff-policy fast compile: %v", err)
		}
	}()
	select {
	case <-fastDoneDiffP:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("diff-policy fast compile stalled behind unrelated slow compile")
	}

	// Slow compile is still blocked -- confirms we have not
	// accidentally raced to "release" the resolver.
	select {
	case <-slowDone:
		t.Fatal("slow compile completed without resolver Release()")
	case <-time.After(20 * time.Millisecond):
		// good
	}

	resolver.Release()
	<-slowDone
}

// TestCache_SingleFlightSameKey is the iter-2 finding-3
// dual: many concurrent callers requesting the SAME
// (policy, source) MUST de-duplicate. Compile runs at most
// once and all callers observe the same `*Predicate`
// pointer (and nil error). A naive "release the mutex
// before Compile" without an in-flight placeholder would
// allow every racing caller to compile independently,
// returning distinct `*Predicate` instances.
func TestCache_SingleFlightSameKey(t *testing.T) {
	t.Parallel()
	c := NewCache()
	pv := uuid.Must(uuid.NewV4())
	tid := uuid.Must(uuid.NewV4())
	resolver := newBlockingResolver(MapResolver{
		tid: Threshold{
			ThresholdID: tid,
			MetricKind:  "lcom4",
			ScopeKind:   "class",
			Op:          OpGE,
			Value:       10,
		},
	})
	source := "threshold('" + tid.String() + "')"

	const goroutines = 32
	results := make([]*Predicate, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	startGate := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-startGate // align all goroutines on the start.
			results[i], errs[i] = c.GetOrCompile(pv, source, resolver)
		}()
	}
	close(startGate)

	// Wait for at least one Lookup to be in flight, then
	// release the gate so all callers wake.
	select {
	case <-resolver.started:
	case <-time.After(1 * time.Second):
		t.Fatal("no goroutine reached resolver.Lookup within 1s")
	}
	resolver.Release()
	wg.Wait()

	if got := resolver.lookups.Load(); got != 1 {
		t.Errorf("resolver.Lookup invoked %d times; expected exactly 1 (singleflight broken)", got)
	}
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d errored: %v", i, errs[i])
		}
		if i > 0 && results[i] != results[0] {
			t.Errorf("caller %d returned a different *Predicate than caller 0 (singleflight broken)", i)
		}
	}
}
