package webhook_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
)

func hashOf(body string) webhook.PayloadHash {
	return webhook.PayloadHash(sha256.Sum256([]byte(body)))
}

// TestInMemoryIdempotencyStore_ClaimReturnsClaimedOnFirstCall
// pins the basic claim contract: a fresh slot is claimable.
func TestInMemoryIdempotencyStore_ClaimReturnsClaimedOnFirstCall(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{"a":1}`)
	claimed, existing, err := s.Claim(context.Background(), "churn", hash)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatalf("Claim: want claimed=true on fresh slot, got false")
	}
	if existing != nil {
		t.Fatalf("Claim: want existing=nil on fresh slot, got %+v", existing)
	}
}

// TestInMemoryIdempotencyStore_CommitThenReplay pins the
// post-commit replay shape: a second Claim against the same
// (verb, hash) returns the committed record.
func TestInMemoryIdempotencyStore_CommitThenReplay(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{"a":1}`)
	scanRunID := uuid.Must(uuid.NewV7())
	respBody := []byte(`{"scan_run_id":"x"}`)

	claimed, _, err := s.Claim(context.Background(), "churn", hash)
	if err != nil || !claimed {
		t.Fatalf("first Claim: claimed=%v err=%v", claimed, err)
	}
	if err := s.Commit(context.Background(), webhook.IdempotencyRecord{
		Verb:         "churn",
		PayloadHash:  hash,
		ScanRunID:    scanRunID,
		ResponseBody: respBody,
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	claimed, existing, err := s.Claim(context.Background(), "churn", hash)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed {
		t.Errorf("second Claim: want claimed=false (replay), got true")
	}
	if existing == nil {
		t.Fatalf("second Claim: want existing non-nil (replay), got nil")
	}
	if existing.ScanRunID != scanRunID {
		t.Errorf("replay ScanRunID: want %s, got %s", scanRunID, existing.ScanRunID)
	}
	if string(existing.ResponseBody) != string(respBody) {
		t.Errorf("replay body: want %q, got %q", respBody, existing.ResponseBody)
	}
}

// TestInMemoryIdempotencyStore_AbortReleasesSlot pins the
// abort-on-failure path: a retry after Abort gets a fresh
// claim.
func TestInMemoryIdempotencyStore_AbortReleasesSlot(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{"a":1}`)
	if claimed, _, err := s.Claim(context.Background(), "churn", hash); err != nil || !claimed {
		t.Fatalf("first Claim: claimed=%v err=%v", claimed, err)
	}
	if err := s.Abort(context.Background(), "churn", hash); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	claimed, existing, err := s.Claim(context.Background(), "churn", hash)
	if err != nil {
		t.Fatalf("post-Abort Claim: %v", err)
	}
	if !claimed {
		t.Errorf("post-Abort Claim: want claimed=true (slot released), got false")
	}
	if existing != nil {
		t.Errorf("post-Abort Claim: want existing=nil, got %+v", existing)
	}
}

// TestInMemoryIdempotencyStore_CommitWithoutClaimErrors pins
// the caller-bug guard.
func TestInMemoryIdempotencyStore_CommitWithoutClaimErrors(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	err := s.Commit(context.Background(), webhook.IdempotencyRecord{
		Verb:        "churn",
		PayloadHash: hashOf(`{}`),
	})
	if !errors.Is(err, webhook.ErrClaimNotHeld) {
		t.Errorf("Commit without Claim: want ErrClaimNotHeld, got %v", err)
	}
}

// TestInMemoryIdempotencyStore_AbortWithoutClaimErrors pins
// the symmetric guard.
func TestInMemoryIdempotencyStore_AbortWithoutClaimErrors(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	err := s.Abort(context.Background(), "churn", hashOf(`{}`))
	if !errors.Is(err, webhook.ErrClaimNotHeld) {
		t.Errorf("Abort without Claim: want ErrClaimNotHeld, got %v", err)
	}
}

// TestInMemoryIdempotencyStore_VerbScopesIsolated pins the
// rubber-duck iter-1 #3 contract: two DIFFERENT verbs posting
// the same body get separate slots.
func TestInMemoryIdempotencyStore_VerbScopesIsolated(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{"shared":true}`)

	churnClaimed, _, _ := s.Claim(context.Background(), "churn", hash)
	defectsClaimed, _, _ := s.Claim(context.Background(), "defects", hash)
	if !churnClaimed {
		t.Errorf("churn claim: want true, got false")
	}
	if !defectsClaimed {
		t.Errorf("defects claim: want true, got false (verbs must be isolated)")
	}
	_ = s.Abort(context.Background(), "churn", hash)
	_ = s.Abort(context.Background(), "defects", hash)
}

// TestInMemoryIdempotencyStore_DifferentHashesIsolated pins
// the hash discriminator.
func TestInMemoryIdempotencyStore_DifferentHashesIsolated(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	a := hashOf(`{"a":1}`)
	b := hashOf(`{"b":2}`)
	claimedA, _, _ := s.Claim(context.Background(), "churn", a)
	claimedB, _, _ := s.Claim(context.Background(), "churn", b)
	if !claimedA || !claimedB {
		t.Errorf("distinct payloads must each claim: a=%v b=%v", claimedA, claimedB)
	}
	_ = s.Abort(context.Background(), "churn", a)
	_ = s.Abort(context.Background(), "churn", b)
}

// TestInMemoryIdempotencyStore_AtomicClaimUnderConcurrency
// pins the rubber-duck iter-1 #2 contract: ten concurrent
// claimers for the same slot result in EXACTLY ONE
// `claimed=true` response; the other nine receive the
// committed record once the winner commits.
func TestInMemoryIdempotencyStore_AtomicClaimUnderConcurrency(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{"race":true}`)
	scanRunID := uuid.Must(uuid.NewV7())

	const N = 10
	var wg sync.WaitGroup
	var claimedCount int64
	var replayCount int64
	results := make(chan *webhook.IdempotencyRecord, N)

	// Pre-spawn N goroutines; gate them on a single
	// channel close so the race is genuinely concurrent.
	gate := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			claimed, existing, err := s.Claim(context.Background(), "churn", hash)
			if err != nil {
				t.Errorf("goroutine Claim: %v", err)
				return
			}
			if claimed {
				atomic.AddInt64(&claimedCount, 1)
				// Simulate verb handler work, then commit.
				time.Sleep(10 * time.Millisecond)
				_ = s.Commit(context.Background(), webhook.IdempotencyRecord{
					Verb:         "churn",
					PayloadHash:  hash,
					ScanRunID:    scanRunID,
					ResponseBody: []byte(`{"x":1}`),
				})
				return
			}
			atomic.AddInt64(&replayCount, 1)
			results <- existing
		}()
	}
	close(gate)
	wg.Wait()
	close(results)

	if got := atomic.LoadInt64(&claimedCount); got != 1 {
		t.Errorf("claimedCount: want exactly 1, got %d", got)
	}
	if got := atomic.LoadInt64(&replayCount); got != N-1 {
		t.Errorf("replayCount: want %d (N-1), got %d", N-1, got)
	}
	for rec := range results {
		if rec == nil {
			t.Errorf("replay record: want non-nil, got nil")
			continue
		}
		if rec.ScanRunID != scanRunID {
			t.Errorf("replay ScanRunID: want %s, got %s", scanRunID, rec.ScanRunID)
		}
	}
}

// TestInMemoryIdempotencyStore_DefensiveResponseBodyCopy
// asserts the committed ResponseBody is copied on store AND
// on retrieve so a caller mutating either copy does not
// poison the other.
func TestInMemoryIdempotencyStore_DefensiveResponseBodyCopy(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{}`)
	body := []byte(`{"original":true}`)

	_, _, _ = s.Claim(context.Background(), "churn", hash)
	if err := s.Commit(context.Background(), webhook.IdempotencyRecord{
		Verb:         "churn",
		PayloadHash:  hash,
		ScanRunID:    uuid.Must(uuid.NewV7()),
		ResponseBody: body,
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Mutate the caller's copy AFTER Commit.
	for i := range body {
		body[i] = 0x00
	}
	_, existing, _ := s.Claim(context.Background(), "churn", hash)
	if existing == nil {
		t.Fatalf("replay: want existing, got nil")
	}
	if string(existing.ResponseBody) != `{"original":true}` {
		t.Errorf("replay body: want %q (defensive copy), got %q", `{"original":true}`, existing.ResponseBody)
	}
}

// TestInMemoryIdempotencyStore_LookupReportsInflight pins the
// non-blocking Lookup helper.
func TestInMemoryIdempotencyStore_LookupReportsInflight(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	hash := hashOf(`{}`)
	_, _, _ = s.Claim(context.Background(), "churn", hash)
	rec, err := s.Lookup(context.Background(), "churn", hash)
	if !errors.Is(err, webhook.ErrClaimInFlight) {
		t.Errorf("Lookup during in-flight claim: want ErrClaimInFlight, got %v", err)
	}
	if rec != nil {
		t.Errorf("Lookup during in-flight claim: want nil record, got %+v", rec)
	}
}

// TestInMemoryIdempotencyStore_LRUEviction pins the bounded-
// cache semantics. With cap=2 a third commit drops the
// oldest entry.
func TestInMemoryIdempotencyStore_LRUEviction(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(2)
	commit := func(verb, body string) {
		t.Helper()
		hash := hashOf(body)
		_, _, _ = s.Claim(context.Background(), verb, hash)
		if err := s.Commit(context.Background(), webhook.IdempotencyRecord{
			Verb:         verb,
			PayloadHash:  hash,
			ScanRunID:    uuid.Must(uuid.NewV7()),
			ResponseBody: []byte(body),
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	commit("churn", `{"a":1}`)
	commit("churn", `{"b":2}`)
	commit("churn", `{"c":3}`)
	if got := s.Len(); got != 2 {
		t.Errorf("Len after eviction: want 2, got %d", got)
	}
	// The first-inserted hash should be gone.
	rec, err := s.Lookup(context.Background(), "churn", hashOf(`{"a":1}`))
	if err != nil {
		t.Errorf("Lookup evicted: unexpected err %v", err)
	}
	if rec != nil {
		t.Errorf("Lookup evicted: want nil (oldest entry dropped), got %+v", rec)
	}
}

// TestInMemoryIdempotencyStore_ClaimEmptyVerbErrors pins the
// caller-bug guard.
func TestInMemoryIdempotencyStore_ClaimEmptyVerbErrors(t *testing.T) {
	t.Parallel()
	s := webhook.NewInMemoryIdempotencyStore(0)
	_, _, err := s.Claim(context.Background(), "", hashOf(`{}`))
	if err == nil {
		t.Errorf("Claim with empty verb: want error, got nil")
	}
}
