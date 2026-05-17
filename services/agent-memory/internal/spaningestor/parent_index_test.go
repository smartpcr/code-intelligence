package spaningestor

import (
	"context"
	"testing"
	"time"
)

// TestParentIndex_lookupAfterRememberHits is the basic hit path:
// remember a (trace, span) → target mapping, lookup returns the
// same target.
func TestParentIndex_lookupAfterRememberHits(t *testing.T) {
	p := newParentIndex(8, time.Minute, time.Now)
	p.Remember("t1", "s1", ObservationTarget{NodeID: "n1", Kind: "method"})
	got, ok := p.Lookup("t1", "s1")
	if !ok {
		t.Fatalf("Lookup miss")
	}
	if got.NodeID != "n1" || got.Kind != "method" {
		t.Errorf("Lookup = %+v", got)
	}
	hits, _, _, _ := p.Snapshot()
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}

// TestParentIndex_ttlExpiresEntries verifies the TTL eviction
// path: with a virtual clock past the expiry, the entry is
// evicted and Lookup returns a miss.
func TestParentIndex_ttlExpiresEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	p := newParentIndex(8, 100*time.Millisecond, clock)
	p.Remember("t1", "s1", ObservationTarget{NodeID: "n1"})
	now = now.Add(200 * time.Millisecond)
	if _, ok := p.Lookup("t1", "s1"); ok {
		t.Errorf("Lookup hit after TTL expired")
	}
	_, _, _, ttlEv := p.Snapshot()
	if ttlEv == 0 {
		t.Errorf("ttlEvictions = 0, want > 0")
	}
}

// TestParentIndex_lruEvictsOldestOnCap verifies the bounded LRU
// behaviour when keyCap is hit.
func TestParentIndex_lruEvictsOldestOnCap(t *testing.T) {
	p := newParentIndex(2, time.Minute, time.Now)
	p.Remember("t", "a", ObservationTarget{NodeID: "na"})
	p.Remember("t", "b", ObservationTarget{NodeID: "nb"})
	p.Remember("t", "c", ObservationTarget{NodeID: "nc"})

	if _, ok := p.Lookup("t", "a"); ok {
		t.Errorf("oldest entry 'a' still present after cap=2 insert of 'c'")
	}
	if _, ok := p.Lookup("t", "b"); !ok {
		t.Errorf("entry 'b' should still be present")
	}
	if _, ok := p.Lookup("t", "c"); !ok {
		t.Errorf("entry 'c' should be present")
	}
	_, _, keyEv, _ := p.Snapshot()
	if keyEv != 1 {
		t.Errorf("keyEvictions = %d, want 1", keyEv)
	}
}

// TestParentIndex_disabledByZeroCap verifies cache disable.
func TestParentIndex_disabledByZeroCap(t *testing.T) {
	p := newParentIndex(0, time.Minute, time.Now)
	p.Remember("t", "a", ObservationTarget{NodeID: "na"})
	if _, ok := p.Lookup("t", "a"); ok {
		t.Errorf("Lookup hit with disabled cache")
	}
}

// TestParentIndex_disabledByZeroTTL verifies cache disable via
// TTL == 0.
func TestParentIndex_disabledByZeroTTL(t *testing.T) {
	p := newParentIndex(8, 0, time.Now)
	p.Remember("t", "a", ObservationTarget{NodeID: "na"})
	if _, ok := p.Lookup("t", "a"); ok {
		t.Errorf("Lookup hit with TTL=0")
	}
}

// shaReaderFunc adapts a function to the SHAReader interface
// for tests.
type shaReaderFunc func(repoID string) (string, error)

func (f shaReaderFunc) CurrentHeadSHA(_ context.Context, repoID string) (string, error) {
	return f(repoID)
}

// TestSHACache_returnsCachedValueWithinTTL verifies hits skip
// the underlying reader.
func TestSHACache_returnsCachedValueWithinTTL(t *testing.T) {
	calls := 0
	r := shaReaderFunc(func(repoID string) (string, error) {
		calls++
		return "deadbeef", nil
	})
	c := newSHACache(r, time.Minute, time.Now)
	for i := 0; i < 5; i++ {
		sha, err := c.Get(context.Background(), "repo-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if sha != "deadbeef" {
			t.Errorf("Get = %q, want deadbeef", sha)
		}
	}
	if calls != 1 {
		t.Errorf("reader calls = %d, want 1 (4 cache hits expected)", calls)
	}
}

// TestSHACache_refetchesAfterTTL verifies expired entries
// re-query the reader.
func TestSHACache_refetchesAfterTTL(t *testing.T) {
	calls := 0
	r := shaReaderFunc(func(repoID string) (string, error) {
		calls++
		return "sha-v" + string(rune('0'+calls)), nil
	})
	now := time.Unix(1, 0)
	clock := func() time.Time { return now }
	c := newSHACache(r, 10*time.Millisecond, clock)
	sha, _ := c.Get(context.Background(), "r")
	if sha != "sha-v1" {
		t.Fatalf("first = %q", sha)
	}
	now = now.Add(50 * time.Millisecond)
	sha, _ = c.Get(context.Background(), "r")
	if sha != "sha-v2" {
		t.Errorf("after TTL = %q, want sha-v2", sha)
	}
}

// TestSHACache_nilReaderReturnsEmpty verifies the fallback
// path: with no reader installed, Get returns ("", nil) so
// the ingestor can fall back to the "observed" sentinel.
func TestSHACache_nilReaderReturnsEmpty(t *testing.T) {
	c := newSHACache(nil, time.Minute, time.Now)
	sha, err := c.Get(context.Background(), "r")
	if err != nil {
		t.Errorf("Get with nil reader returned err: %v", err)
	}
	if sha != "" {
		t.Errorf("Get = %q, want empty", sha)
	}
}
