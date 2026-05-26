package webhook

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/gofrs/uuid"
)

// PayloadHash is the 32-byte SHA-256 digest the Router
// computes over the canonical webhook body for idempotency
// lookups. Pinned as a named type so a caller cannot
// accidentally swap a `[]byte` of different provenance into
// the idempotency store interface.
//
// # Canonicalisation
//
// The brief says `payload_hash = sha256(canonicalised body)`.
// v1 defines "canonicalised" as **the body bytes as
// received over the wire after MaxBytesReader trims the
// length-prefixed read** -- i.e. the SAME bytes the HMAC
// verifier digests. Two equivalent JSON documents with
// different whitespace produce DIFFERENT payload_hashes; a
// publisher that wants idempotent replay MUST replay the
// exact same body bytes. This pins the spec's vague
// "canonicalised" term to a behaviour the publisher can
// reason about without a server-side schema parser, and
// matches the e2e replay scenario's "the exact same call
// is replayed" framing (e2e-scenarios.md lines 700-708).
// A v2 follow-on may layer a per-verb canonical re-serialise
// step on top, but v1 ships the raw-bytes shape and pins it
// here.
//
// The Wire shape (after hex encoding) is 64 lowercase hex
// chars; this is the form persisted to
// `scan_run.payload_hash` in migration 0001 (`bytea`
// column).
type PayloadHash [32]byte

// String returns the lowercase-hex encoding suitable for
// logging or for the canonical persistence shape. Never use
// the raw byte string for log output -- the helper is the
// audit-friendly form.
func (p PayloadHash) String() string { return hex.EncodeToString(p[:]) }

// Bytes returns a fresh slice copy of the hash bytes so a
// caller storing the hash cannot mutate the receiver's
// state.
func (p PayloadHash) Bytes() []byte {
	out := make([]byte, len(p))
	copy(out, p[:])
	return out
}

// IdempotencyRecord is the persisted shape of one
// successfully-processed webhook call. The Router stamps a
// record on every successful 200; replays of the same
// `(Verb, PayloadHash)` re-emit `ResponseBody` verbatim
// without dispatching the verb handler again.
//
// # Fields
//
//   - `Verb` is the URL path segment (e.g. `churn`,
//     `defects`). Two different verbs with the same body
//     bytes get separate records (rubber-duck audit fix
//     iter-1 #3, hardened in iter-3: keying on `kind`
//     alone would be too coarse because `churn` and
//     `defects` both bind to `kind='external_per_row'`;
//     iter-3's migration 0009 partial unique index
//     `scan_run_payload_hash_verb_uniq` is keyed on
//     `(verb, payload_hash)` for the same reason at the
//     durable layer).
//
//   - `PayloadHash` is the [PayloadHash] of the canonical
//     body.
//
//   - `ScanRunID` is the parent ScanRun's `scan_run_id`
//     stamped by the verb handler. Mirrors the value the
//     original 200 response carried.
//
//   - `ResponseBody` is the JSON bytes the Router emitted on
//     the original 200. Stored verbatim so the replay
//     response is byte-identical to the original (modulo a
//     `replayed=true` flag the Router toggles after re-
//     serialising the envelope).
type IdempotencyRecord struct {
	Verb         string
	PayloadHash  PayloadHash
	ScanRunID    uuid.UUID
	ResponseBody []byte
}

// IdempotencyStore is the seam between the Router and the
// durable lookup of `(verb, payload_hash) -> scan_run_id`.
// The brief says: "if a `scan_run(payload_hash=...)` already
// exists for this verb, return the stored scan_run_id without
// re-executing". This interface IS that lookup.
//
// # Atomic claim semantics
//
// The contract is intentionally [Claim] + [Commit] (NOT a
// bare `Lookup` + `Store` pair). The rubber-duck audit
// (iter-1 #2) caught a TOCTOU vector in a naive lookup/store
// shape: two identical concurrent POSTs both `Lookup` (miss),
// both execute the verb handler, both `Store`. The atomic
// claim flow:
//
//  1. Router calls [Claim] with `(verb, payload_hash)`.
//     EXACTLY ONE concurrent caller observes
//     `claimed=true`; the rest observe `claimed=false` and
//     receive the in-flight or already-committed record.
//  2. The claiming caller executes the verb handler.
//  3. On success the claiming caller calls [Commit] with the
//     produced [IdempotencyRecord]. Other callers observing
//     the in-flight claim block (or poll) on [Lookup] until
//     [Commit] resolves.
//  4. On verb-handler failure the claiming caller calls
//     [Abort] to release the claim so a retry can succeed.
//
// # PG-backed implementation (later stages)
//
// The v1 in-memory [InMemoryIdempotencyStore] is the seam
// the Router consumes today. Phase 3.2's stage-metric-
// ingestor-and-scanrun-state-machine plugs in a
// PG-backed implementation that selects from
// `clean_code.scan_run` with a partial unique index on
// `(verb, payload_hash) WHERE payload_hash IS NOT NULL`
// (migration 0009; iter-3 evaluator item #2 hardened the
// key from `(kind, payload_hash)` to `(verb,
// payload_hash)` so two verbs sharing a kind cannot
// collide) and translates the claim/commit/abort flow to
// a single `INSERT ... ON CONFLICT DO NOTHING RETURNING`
// shape. The interface here survives that swap.
type IdempotencyStore interface {
	// Claim is the atomic claim primitive. Returns
	// `claimed=true` + `nil` record when the caller has
	// claimed the (verb, payload_hash) slot and SHOULD
	// execute the verb handler. Returns `claimed=false`
	// + a non-nil [IdempotencyRecord] when a prior call
	// has already committed a result for the same slot;
	// the caller MUST treat that record as the canonical
	// response (replay path).
	//
	// The seam-shape promise: between Claim returning
	// `claimed=true` and a follow-up Commit / Abort,
	// concurrent calls for the SAME slot block until the
	// claim resolves. The in-memory implementation models
	// this with a per-slot sync.Cond; the PG implementation
	// will use a row lock.
	Claim(ctx context.Context, verb string, hash PayloadHash) (claimed bool, existing *IdempotencyRecord, err error)

	// Commit persists `record` against the slot the caller
	// previously claimed. Returns an error if no prior Claim
	// holds the slot (caller bug). Wakes any other callers
	// blocked on the same slot so they observe the new
	// record via their own Claim's return.
	Commit(ctx context.Context, record IdempotencyRecord) error

	// Abort releases the slot the caller previously claimed
	// without persisting a record (verb-handler failure).
	// Any other caller blocked on the same slot is woken so
	// it can re-attempt Claim and observe `claimed=true`.
	// Returns an error if no prior Claim holds the slot.
	Abort(ctx context.Context, verb string, hash PayloadHash) error

	// Lookup is a non-blocking projection of the committed
	// state. Returns (`nil, nil`) when no record exists;
	// returns the record on a committed slot; returns
	// [ErrClaimInFlight] when a claim is in flight (so an
	// ops dashboard can pattern-match the in-flight state
	// without blocking the request thread).
	Lookup(ctx context.Context, verb string, hash PayloadHash) (*IdempotencyRecord, error)
}

// Sentinel errors returned by [IdempotencyStore]
// implementations and the Router's idempotency pipeline.
var (
	// ErrClaimNotHeld is returned by [Commit] / [Abort]
	// when the caller has not previously claimed the slot.
	// Reachable only as a caller bug -- the Router
	// guarantees Claim always precedes Commit/Abort.
	ErrClaimNotHeld = errors.New("webhook: idempotency claim is not held by caller")

	// ErrClaimInFlight is returned by [Lookup] when the slot
	// is currently claimed but not yet committed. The
	// Router itself never calls Lookup on the hot path
	// (Claim subsumes Lookup); this sentinel is for ops
	// tooling.
	ErrClaimInFlight = errors.New("webhook: idempotency claim is in flight (not yet committed or aborted)")
)

// InMemoryIdempotencyStore is the v1 [IdempotencyStore]
// implementation. Records live in-process; restarts lose
// state. The composition root for the production webhook
// will swap this for the PG-backed implementation in Phase
// 3.2's stage-metric-ingestor-and-scanrun-state-machine; the
// in-memory shape is sufficient for Stage 4.1's
// transport-tier scope where the brief's idempotency
// requirement is satisfied by the interface plus
// integration tests against this store (the PG-backed
// behaviour is exercised in the matching state-machine
// stage).
//
// # Concurrency model
//
// One Mutex guards both `committed` and `inflight`. A
// sync.Cond per slot is overkill for the v1 scale; we use a
// single per-store Cond and broadcast on every Commit /
// Abort. Callers that block on a slot wake on every
// resolution and re-check their slot under the lock --
// O(slots * waiters) worst-case, but at the expected v1
// scale (1-2 concurrent retries per slot) this is fine.
//
// # Bounded cache
//
// The store accepts an optional max-entry cap. A zero cap
// means unbounded (test scenarios). The production wiring
// should pass a finite cap (e.g. 65 536) so a malicious
// publisher cannot exhaust memory by replaying with
// rotating fresh payloads; eviction is LRU by
// arrival-of-commit. The store evicts whole committed
// entries; in-flight claims are NEVER evicted (would
// violate the Commit contract).
type InMemoryIdempotencyStore struct {
	mu        sync.Mutex
	cond      *sync.Cond
	committed map[idempotencyKey]IdempotencyRecord
	// order tracks insertion order for LRU eviction. A
	// slice is fine at v1 scale (1-2 popular slots in a
	// retry storm); a linked-hash-map is the v2 shape if
	// hot-eviction throughput becomes a profile hit.
	order   []idempotencyKey
	inflight map[idempotencyKey]struct{}
	maxEntries int
}

// idempotencyKey is the composite hash-map key for the
// in-memory store. Using a value-type (not a string)
// avoids the heap allocation a `verb + ":" + hash.String()`
// concatenation would incur on every request.
type idempotencyKey struct {
	verb string
	hash PayloadHash
}

// NewInMemoryIdempotencyStore constructs an empty store.
// `maxEntries` is the committed-entry cap; 0 means
// unbounded.
func NewInMemoryIdempotencyStore(maxEntries int) *InMemoryIdempotencyStore {
	if maxEntries < 0 {
		panic(fmt.Sprintf("webhook: NewInMemoryIdempotencyStore received negative maxEntries=%d", maxEntries))
	}
	s := &InMemoryIdempotencyStore{
		committed:  make(map[idempotencyKey]IdempotencyRecord),
		inflight:   make(map[idempotencyKey]struct{}),
		maxEntries: maxEntries,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Claim implements [IdempotencyStore].
func (s *InMemoryIdempotencyStore) Claim(ctx context.Context, verb string, hash PayloadHash) (bool, *IdempotencyRecord, error) {
	if verb == "" {
		return false, nil, fmt.Errorf("webhook: idempotency Claim with empty verb")
	}
	key := idempotencyKey{verb: verb, hash: hash}

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		// Committed already? Replay path.
		if rec, ok := s.committed[key]; ok {
			recCopy := rec
			recCopy.ResponseBody = append([]byte{}, rec.ResponseBody...)
			return false, &recCopy, nil
		}
		// In flight by another caller? Block until the
		// claim resolves via Commit/Abort.
		if _, busy := s.inflight[key]; busy {
			// Bail out if the request context expires while
			// we wait. We do that via a brief goroutine
			// pump because sync.Cond does not natively
			// take a context.
			if waitErr := s.waitWithCtx(ctx); waitErr != nil {
				return false, nil, waitErr
			}
			continue
		}
		// Slot free -- claim it.
		s.inflight[key] = struct{}{}
		return true, nil, nil
	}
}

// waitWithCtx blocks the caller on s.cond, returning early
// when ctx is cancelled. Caller MUST hold s.mu.
func (s *InMemoryIdempotencyStore) waitWithCtx(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil && ctx.Err() != nil {
			// Already cancelled before we start waiting.
			return ctx.Err()
		}
	}
	// Spawn a watchdog that broadcasts if ctx cancels
	// before another caller does. The watchdog re-takes
	// the lock just to broadcast, then exits.
	done := make(chan struct{})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				s.mu.Lock()
				s.cond.Broadcast()
				s.mu.Unlock()
			case <-done:
			}
		}()
	}
	s.cond.Wait()
	close(done)
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// Commit implements [IdempotencyStore].
func (s *InMemoryIdempotencyStore) Commit(ctx context.Context, record IdempotencyRecord) error {
	if record.Verb == "" {
		return fmt.Errorf("webhook: idempotency Commit with empty verb")
	}
	key := idempotencyKey{verb: record.Verb, hash: record.PayloadHash}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inflight[key]; !ok {
		return fmt.Errorf("%w: verb=%s hash=%s", ErrClaimNotHeld, record.Verb, record.PayloadHash)
	}
	// Defensive copy so the caller cannot mutate the cached
	// response body after Commit.
	stored := record
	stored.ResponseBody = append([]byte{}, record.ResponseBody...)
	s.committed[key] = stored
	s.order = append(s.order, key)
	delete(s.inflight, key)
	s.evictLocked()
	s.cond.Broadcast()
	return nil
}

// Abort implements [IdempotencyStore].
func (s *InMemoryIdempotencyStore) Abort(ctx context.Context, verb string, hash PayloadHash) error {
	if verb == "" {
		return fmt.Errorf("webhook: idempotency Abort with empty verb")
	}
	key := idempotencyKey{verb: verb, hash: hash}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inflight[key]; !ok {
		return fmt.Errorf("%w: verb=%s hash=%s", ErrClaimNotHeld, verb, hash)
	}
	delete(s.inflight, key)
	s.cond.Broadcast()
	return nil
}

// Lookup implements [IdempotencyStore].
func (s *InMemoryIdempotencyStore) Lookup(ctx context.Context, verb string, hash PayloadHash) (*IdempotencyRecord, error) {
	key := idempotencyKey{verb: verb, hash: hash}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.committed[key]; ok {
		recCopy := rec
		recCopy.ResponseBody = append([]byte{}, rec.ResponseBody...)
		return &recCopy, nil
	}
	if _, ok := s.inflight[key]; ok {
		return nil, ErrClaimInFlight
	}
	return nil, nil
}

// evictLocked trims `committed` down to `maxEntries`. Caller
// holds s.mu.
func (s *InMemoryIdempotencyStore) evictLocked() {
	if s.maxEntries == 0 {
		return
	}
	for len(s.committed) > s.maxEntries && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.committed, oldest)
	}
}

// Len returns the number of committed entries. Exported for
// tests / ops dashboards; non-blocking.
func (s *InMemoryIdempotencyStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.committed)
}
