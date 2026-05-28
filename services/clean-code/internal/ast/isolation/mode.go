package isolation

import (
	"context"
	"fmt"
	"sync"

	"github.com/gofrs/uuid"
)

// Mode is the AST adapter mode for a single repo. Mirrors
// `clean_code.repo.mode` from the management/repo_store; pinned
// here as its own type so the isolation package is independent
// of (and cycle-free with respect to) internal/management.
type Mode string

const (
	// ModeEmbedded is the default mode. The Clean Code service
	// runs standalone tree-sitter parses; no agent-memory
	// dependency.
	ModeEmbedded Mode = "embedded"

	// ModeLinked is the cross-repo composition mode. The AST
	// Adapter reuses agent-memory.GraphReader for cross-repo
	// edges.
	ModeLinked Mode = "linked"
)

// AllowedModes is the closed list of legal [Mode] values.
var AllowedModes = []Mode{ModeEmbedded, ModeLinked}

// IsAllowedMode reports whether `m` is one of [AllowedModes].
func IsAllowedMode(m Mode) bool {
	for _, allowed := range AllowedModes {
		if m == allowed {
			return true
		}
	}
	return false
}

// ScanToken is the opaque handle returned by
// [ModeCoordinator.BeginScan]. The caller MUST pass it to
// [ModeCoordinator.EndScan] (typically via `defer`) when the
// scan completes so the coordinator's in-flight count decays.
// Calling EndScan more times than BeginScan panics -- a
// double-EndScan would underflow the in-flight count and
// silently break the drain contract.
type ScanToken struct {
	state *repoState
	mode  Mode
}

// Mode returns the mode the scan was admitted under. Holding
// the token guarantees the coordinator will not advertise a
// different mode for new scans on the same repo until EndScan
// is called.
func (t ScanToken) Mode() Mode { return t.mode }

// Valid reports whether the token came from a real BeginScan
// call (vs the zero value). Used by EndScan to reject the
// zero-token without panicking.
func (t ScanToken) Valid() bool { return t.state != nil }

// ModeCoordinator is the per-repo admission + drain primitive
// that backs the `mgmt.set_mode(repo_id, mode)` "drain before
// flip" contract.
//
// # Brief (implementation-plan line 804)
//
//	"On `mgmt.set_mode(repo_id, mode)` transitions between
//	 `embedded` and `linked`, drain in-flight scans for the
//	 repo before flipping; new scans pick up the new mode."
//
// # Algorithm
//
//   - [BeginScan] admits a scan iff no flip is in progress for
//     the repo. If a flip is in progress, BeginScan blocks
//     (channel-based; ctx-cancellable) until the flip clears,
//     then admits the scan under the NEW mode.
//   - [EndScan] decrements the in-flight count; when it reaches
//     zero, any waiter (notably a pending SetMode) is woken.
//   - [SetMode] (a) marks the repo as flipping (new BeginScan
//     calls block), (b) waits for in-flight scans to drain to
//     zero, (c) runs the caller-supplied `applyFn` (the
//     catalog mutation), (d) on success swaps the in-memory
//     mode, (e) clears the flipping flag and wakes waiters.
//     Every error path clears the flipping flag so a failed
//     flip cannot leave new scans permanently blocked
//     (rubber-duck iter-1 finding #3).
//
// # Mode source of truth
//
// The coordinator's in-memory mode is a CACHE, not the source
// of truth. Callers MUST invoke [HydrateMode] (typically at
// startup or on first-touch) with the value read from
// `clean_code.repo.mode`. BeginScan returns [ErrModeNotHydrated]
// for an unknown repo so a coordinator cold-start cannot
// silently default to `embedded` and disagree with a persisted
// `linked` row (rubber-duck iter-1 finding #2).
type ModeCoordinator struct {
	mu     sync.Mutex
	states map[uuid.UUID]*repoState
}

// repoState holds the per-repo coordinator state. The state's
// own mutex is acquired in a leaf position (never while
// ModeCoordinator.mu is held, so cross-repo SetMode calls
// don't serialise on the coordinator-wide mutex).
type repoState struct {
	mu       sync.Mutex
	mode     Mode
	hydrated bool
	inFlight int
	flipping bool
	// waiters are notified on state changes (inFlight->0,
	// flipping cleared, mode changed). Channel-based instead
	// of sync.Cond so context cancellation composes via
	// `select`.
	waiters []chan struct{}
}

// signalAll closes every pending waiter channel and resets
// the slice. Callers MUST hold state.mu.
func (s *repoState) signalAll() {
	for _, w := range s.waiters {
		close(w)
	}
	s.waiters = nil
}

// addWaiter registers a new waiter and returns the channel
// that will be closed on the next state change. Callers MUST
// hold state.mu.
func (s *repoState) addWaiter() chan struct{} {
	w := make(chan struct{})
	s.waiters = append(s.waiters, w)
	return w
}

// NewModeCoordinator constructs an empty coordinator. Callers
// hydrate each repo's mode via [HydrateMode] before the first
// scan.
func NewModeCoordinator() *ModeCoordinator {
	return &ModeCoordinator{states: make(map[uuid.UUID]*repoState)}
}

// getOrCreate returns the state slot for `repoID`, allocating
// it on first touch. Holds the coordinator-wide mutex only
// long enough to lookup/insert; per-repo work uses state.mu.
func (c *ModeCoordinator) getOrCreate(repoID uuid.UUID) *repoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.states[repoID]
	if !ok {
		s = &repoState{}
		c.states[repoID] = s
	}
	return s
}

// HydrateMode seeds the coordinator's per-repo mode cache with
// the value read from the authoritative catalog
// (`clean_code.repo.mode`). MUST be called before the first
// [BeginScan] on `repoID`. Calling HydrateMode while a flip is
// in progress is rejected with `ErrModeNotHydrated`-shaped
// guard (the rare race where the catalog read crosses a live
// flip should be resolved by re-reading after SetMode returns).
//
// `mode` MUST be in [AllowedModes]; otherwise [ErrInvalidMode]
// is returned.
//
// Calling HydrateMode again with a different value is a no-op
// when a flip is not in progress -- the catalog read is the
// source of truth, and a SetMode would arrive in flight to
// reconcile any drift. We deliberately do NOT signal waiters
// on hydrate; only BeginScan/EndScan/SetMode mutate the
// drain-relevant state.
func (c *ModeCoordinator) HydrateMode(repoID uuid.UUID, mode Mode) error {
	if repoID == uuid.Nil {
		return fmt.Errorf("isolation: ModeCoordinator.HydrateMode: zero repoID")
	}
	if !IsAllowedMode(mode) {
		return fmt.Errorf("%w: got %q", ErrInvalidMode, mode)
	}
	s := c.getOrCreate(repoID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flipping {
		// A flip is in flight; let SetMode finish and update
		// the cache from its applyFn result rather than
		// racing here.
		return fmt.Errorf("isolation: ModeCoordinator.HydrateMode: cannot hydrate while flip in progress for repo_id=%s", repoID)
	}
	s.mode = mode
	s.hydrated = true
	return nil
}

// CurrentMode returns the coordinator's cached mode for the
// repo. Test helper; production callers should rely on
// [BeginScan] which performs the same lookup atomically with
// admission.
func (c *ModeCoordinator) CurrentMode(repoID uuid.UUID) (Mode, bool) {
	s := c.getOrCreate(repoID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hydrated {
		return "", false
	}
	return s.mode, true
}

// InFlight returns the current in-flight count for the repo.
// Test helper for assertions about drain semantics.
func (c *ModeCoordinator) InFlight(repoID uuid.UUID) int {
	s := c.getOrCreate(repoID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// BeginScan admits a scan for the repo, returning a token the
// caller MUST pass to [EndScan] when finished (typically via
// `defer`). The returned [ScanToken] snapshots the mode at
// admission time; the scan body MAY rely on this mode for its
// entire lifetime even if a SetMode runs concurrently (it will
// block, waiting for this scan to EndScan).
//
// Blocks if a flip is in progress for `repoID`; returns
// `ctx.Err()` if the context is cancelled while blocked.
// Returns [ErrModeNotHydrated] if the caller hasn't called
// [HydrateMode] for this repo yet.
func (c *ModeCoordinator) BeginScan(ctx context.Context, repoID uuid.UUID) (ScanToken, error) {
	if err := ctx.Err(); err != nil {
		return ScanToken{}, err
	}
	if repoID == uuid.Nil {
		return ScanToken{}, fmt.Errorf("isolation: ModeCoordinator.BeginScan: zero repoID")
	}
	s := c.getOrCreate(repoID)
	for {
		s.mu.Lock()
		if !s.hydrated {
			s.mu.Unlock()
			return ScanToken{}, fmt.Errorf("%w: repo_id=%s", ErrModeNotHydrated, repoID)
		}
		if !s.flipping {
			s.inFlight++
			tok := ScanToken{state: s, mode: s.mode}
			s.mu.Unlock()
			return tok, nil
		}
		w := s.addWaiter()
		s.mu.Unlock()
		select {
		case <-w:
			// State changed -- loop and re-check.
		case <-ctx.Done():
			return ScanToken{}, ctx.Err()
		}
	}
}

// EndScan releases the scan admission represented by `tok`.
// Idempotent on the zero-token (returns without effect).
// Calling EndScan more times than BeginScan on a given repo
// panics -- a missing BeginScan/EndScan pairing would
// underflow the in-flight count and silently break the drain
// contract; surfacing it as a panic forces the bug to the
// surface.
func (c *ModeCoordinator) EndScan(tok ScanToken) {
	if !tok.Valid() {
		return
	}
	s := tok.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight == 0 {
		panic("isolation: ModeCoordinator.EndScan: in-flight count is zero (programmer bug; calling EndScan more times than BeginScan, or releasing the same token twice)")
	}
	s.inFlight--
	if s.inFlight == 0 {
		s.signalAll()
	}
}

// SetMode performs the drain-before-flip mode transition.
//
//   - If the coordinator's cached mode equals `target`, this is
//     a NO-OP: applyFn is still invoked (so the catalog can
//     append a canonical no-op event if it wishes), but the
//     coordinator does NOT take the flip lock and does NOT
//     drain in-flight scans. This avoids the rubber-duck
//     iter-1 finding #1 ("same-mode set_mode unnecessarily
//     drains scans").
//   - Otherwise: (1) acquire the flip lock (block while another
//     flip on the same repo finishes), (2) wait for in-flight
//     scans to drain to zero, (3) run applyFn outside the
//     per-repo lock, (4) on success swap the in-memory mode,
//     (5) clear the flip lock and wake waiters.
//
// Every error path (ctx cancel, applyFn error) clears the flip
// lock and wakes waiters so a failed flip cannot leave new
// scans permanently blocked.
//
// Returns the previous mode and `changed=true` iff a real flip
// occurred (applyFn ran AND in-memory mode was swapped).
// `changed=false` indicates the no-op same-mode path.
//
// Returns [ErrModeNotHydrated] if the repo has not been
// hydrated. Returns [ErrInvalidMode] if `target` is not in
// [AllowedModes]. Returns a wrapped [ErrModeFlipApplyFailed]
// if applyFn fails. Returns ctx.Err() if the context cancels
// while waiting for drain.
func (c *ModeCoordinator) SetMode(ctx context.Context, repoID uuid.UUID, target Mode, applyFn func(ctx context.Context) error) (previous Mode, changed bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if repoID == uuid.Nil {
		return "", false, fmt.Errorf("isolation: ModeCoordinator.SetMode: zero repoID")
	}
	if !IsAllowedMode(target) {
		return "", false, fmt.Errorf("%w: got %q", ErrInvalidMode, target)
	}
	s := c.getOrCreate(repoID)

	// Same-mode no-op fast path: short-circuit ONLY when no
	// flip is in progress. If a flip is mid-stream the cached
	// mode is stale w.r.t. the in-flight target, so we MUST
	// queue behind the flip lock and re-evaluate after the
	// flip's mode swap. Skipping that check would let a
	// concurrent `SetMode(repo, current_mode)` short-circuit
	// to a no-op while an in-flight flip is about to move the
	// repo to a different mode, leaving the caller's "ensure
	// mode is X" intent silently violated.
	s.mu.Lock()
	if !s.hydrated {
		s.mu.Unlock()
		return "", false, fmt.Errorf("%w: repo_id=%s", ErrModeNotHydrated, repoID)
	}
	if !s.flipping && s.mode == target {
		prev := s.mode
		s.mu.Unlock()
		if applyFn != nil {
			if err := applyFn(ctx); err != nil {
				return prev, false, fmt.Errorf("%w: %v", ErrModeFlipApplyFailed, err)
			}
		}
		return prev, false, nil
	}
	s.mu.Unlock()

	// Real flip: acquire the flip lock, wait for drain, mutate,
	// release. The for-loop handles both "queued behind another
	// flip" and "waiting for inFlight->0" via the same waiter
	// channel.
	if err := c.acquireFlipLock(ctx, s); err != nil {
		return "", false, err
	}
	// From here on every return path MUST clear the flip lock.
	if err := c.waitForDrain(ctx, s); err != nil {
		c.releaseFlipLock(s)
		return "", false, err
	}

	// Run the catalog mutation outside the per-repo lock so the
	// store's own mutex / DB roundtrip doesn't block coordinator
	// reads. The flip flag still blocks new BeginScan calls.
	if applyFn != nil {
		if err := applyFn(ctx); err != nil {
			c.releaseFlipLock(s)
			return "", false, fmt.Errorf("%w: %v", ErrModeFlipApplyFailed, err)
		}
	}

	// Swap the cached mode under the per-repo lock and release
	// the flip atomically with the mode-change so the next
	// BeginScan after release sees the new mode. If the cached
	// mode already equals `target` (e.g. a prior flip we queued
	// behind moved us there), report changed=false so the
	// caller's `changed` flag tells the truth.
	s.mu.Lock()
	previous = s.mode
	if previous == target {
		changed = false
	} else {
		s.mode = target
		changed = true
	}
	s.flipping = false
	s.signalAll()
	s.mu.Unlock()
	return previous, changed, nil
}

// acquireFlipLock blocks until no other flip is in progress on
// the state, then sets `flipping=true`. Returns ctx.Err() if
// the context cancels while waiting.
func (c *ModeCoordinator) acquireFlipLock(ctx context.Context, s *repoState) error {
	for {
		s.mu.Lock()
		if !s.flipping {
			s.flipping = true
			s.mu.Unlock()
			return nil
		}
		w := s.addWaiter()
		s.mu.Unlock()
		select {
		case <-w:
			// Some state change -- re-check.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForDrain blocks until `inFlight == 0` on the state.
// Caller MUST already have set `flipping=true` (so no new
// scans can be admitted). Returns ctx.Err() if ctx cancels.
func (c *ModeCoordinator) waitForDrain(ctx context.Context, s *repoState) error {
	for {
		s.mu.Lock()
		if s.inFlight == 0 {
			s.mu.Unlock()
			return nil
		}
		w := s.addWaiter()
		s.mu.Unlock()
		select {
		case <-w:
			// inFlight may have decremented -- re-check.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// releaseFlipLock clears the flipping flag and wakes any
// waiter (notably BeginScan callers blocked on the flip).
// Used on every error path after the flip lock has been
// acquired so a failed flip never leaves new scans
// permanently blocked.
func (c *ModeCoordinator) releaseFlipLock(s *repoState) {
	s.mu.Lock()
	s.flipping = false
	s.signalAll()
	s.mu.Unlock()
}
