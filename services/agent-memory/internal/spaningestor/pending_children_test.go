package spaningestor

import (
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

func mkChild(trace, parent, span string) PendingChild {
	return PendingChild{
		RepoID:       "r",
		TraceID:      trace,
		SpanID:       span,
		ParentSpanID: parent,
		DstTarget:    ObservationTarget{NodeID: "n-" + span, Kind: "method"},
		Obs:          graphwriter.ObservationInput{TraceID: trace, SpanID: span, DurationMs: 1.0},
	}
}

func TestPendingChildIndex_drainReturnsAllChildrenForKey(t *testing.T) {
	p := newPendingChildIndex(8, time.Minute, time.Now)
	p.Add(mkChild("t1", "p1", "c1"))
	p.Add(mkChild("t1", "p1", "c2"))
	p.Add(mkChild("t1", "p1", "c3"))
	p.Add(mkChild("t1", "other", "c4"))

	got := p.Drain("t1", "p1")
	if len(got) != 3 {
		t.Errorf("Drain returned %d, want 3", len(got))
	}
	// Subsequent drain on the same key returns nothing.
	if again := p.Drain("t1", "p1"); len(again) != 0 {
		t.Errorf("second Drain = %d, want 0", len(again))
	}
	// Other key still parked.
	if got := p.Drain("t1", "other"); len(got) != 1 {
		t.Errorf("Drain other = %d, want 1", len(got))
	}
}

func TestPendingChildIndex_traceScopedKey(t *testing.T) {
	p := newPendingChildIndex(8, time.Minute, time.Now)
	p.Add(mkChild("traceA", "shared-parent-id", "ca"))
	p.Add(mkChild("traceB", "shared-parent-id", "cb"))

	gotA := p.Drain("traceA", "shared-parent-id")
	if len(gotA) != 1 || gotA[0].SpanID != "ca" {
		t.Errorf("Drain traceA = %+v, want only ca", gotA)
	}
	gotB := p.Drain("traceB", "shared-parent-id")
	if len(gotB) != 1 || gotB[0].SpanID != "cb" {
		t.Errorf("Drain traceB = %+v, want only cb", gotB)
	}
}

func TestPendingChildIndex_ttlExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	p := newPendingChildIndex(8, 100*time.Millisecond, clock)
	p.Add(mkChild("t1", "p1", "c1"))
	p.Add(mkChild("t1", "p2", "c2"))

	// Advance clock past TTL.
	now = now.Add(200 * time.Millisecond)
	var evicted []PendingChild
	flushed := p.FlushExpired(func(c PendingChild) {
		evicted = append(evicted, c)
	})
	if flushed != 2 {
		t.Errorf("FlushExpired returned %d, want 2", flushed)
	}
	if len(evicted) != 2 {
		t.Errorf("callback fired %d times, want 2", len(evicted))
	}
	if p.Size() != 0 {
		t.Errorf("Size after flush = %d, want 0", p.Size())
	}
}

func TestPendingChildIndex_lruEvictionReturnsChildren(t *testing.T) {
	p := newPendingChildIndex(2, time.Hour, time.Now)
	p.Add(mkChild("t1", "a", "ca"))
	p.Add(mkChild("t1", "b", "cb"))
	evicted := p.Add(mkChild("t1", "c", "cc"))
	if len(evicted) != 1 {
		t.Fatalf("LRU evicted %d, want 1", len(evicted))
	}
	if evicted[0].ParentSpanID != "a" {
		t.Errorf("evicted parent = %q, want a (oldest)", evicted[0].ParentSpanID)
	}
	if p.Size() != 2 {
		t.Errorf("Size = %d, want 2", p.Size())
	}
}

func TestPendingChildIndex_disabledIsNoOp(t *testing.T) {
	p := newPendingChildIndex(0, time.Minute, time.Now)
	if got := p.Add(mkChild("t", "p", "c")); got != nil {
		t.Errorf("Add evicted = %v, want nil (disabled)", got)
	}
	if got := p.Drain("t", "p"); got != nil {
		t.Errorf("Drain = %v, want nil (disabled)", got)
	}
	if p.Size() != 0 {
		t.Errorf("Size = %d, want 0 (disabled)", p.Size())
	}
}

func TestPendingChildIndex_zeroTTLIsNoOp(t *testing.T) {
	p := newPendingChildIndex(8, 0, time.Now)
	if got := p.Add(mkChild("t", "p", "c")); got != nil {
		t.Errorf("Add evicted = %v, want nil (zero TTL)", got)
	}
	if got := p.Drain("t", "p"); got != nil {
		t.Errorf("Drain = %v, want nil (zero TTL)", got)
	}
}
