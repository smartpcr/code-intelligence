package degraded

import "sync"

// FaultInjector is the §8.1 test-only seam that flips a
// verb's response into a degraded shape on demand.
// Production binaries leave the injector nil; the unit-test
// composition root constructs one and pins per-verb fault
// rules to exercise the §6.3 contract end-to-end.
//
// Behavioural contract
// --------------------
// Inject(verb, repoID) returns (true, reason) when the
// injector has been configured to flip `verb` for the
// supplied `repoID` (or for the wildcard repo).  The reason
// returned is the verbatim string the injector wants on the
// response — it may be a closed-set value (so the response
// passes Enforce) OR a non-closed string like "oops" (so
// the response triggers the closed-set guard and the
// adapter returns Internal).
//
// The injector is consulted AFTER any real degraded signal
// (consolidator backpressure, episodic-log fallback, etc.)
// so an injected reason cannot overwrite a real outage; see
// the per-verb overlay logic in `agentapi/observe.go` and
// `agentapi/recall.go`.
type FaultInjector interface {
	// Inject reports whether the test bench wants `verb`
	// (called for `repoID`) flipped into a degraded shape.
	// When (true, reason) is returned the caller overlays
	// the response with `degraded=true` and that reason.
	// When (false, _) is returned the response is left
	// untouched.
	//
	// Implementations MUST be safe for concurrent calls —
	// the Observe burst test issues 100 concurrent requests
	// through the same injector.
	Inject(verb, repoID string) (degraded bool, reason string)
}

// FaultInjectorFunc adapts a plain function into a
// [FaultInjector]. Used when a test wants an inline rule.
type FaultInjectorFunc func(verb, repoID string) (bool, string)

// Inject implements [FaultInjector].
func (f FaultInjectorFunc) Inject(verb, repoID string) (bool, string) {
	return f(verb, repoID)
}

// MapFaultInjector is a simple per-(verb, repoID) injector
// the unit tests use.  Rules support a wildcard repo
// (`"*"`); the wildcard matches any non-empty repoID AND
// the empty repoID, so a test that forgets to set a repo
// still trips the rule.
//
// Concurrent-safety: a single sync.RWMutex guards rules;
// reads (the hot path) take only the read lock.  Tests may
// mutate rules between scenarios without restarting the
// service.
type MapFaultInjector struct {
	mu    sync.RWMutex
	rules map[string]map[string]string // verb -> repoID -> reason
}

// NewMapFaultInjector returns a fresh injector with no
// rules.
func NewMapFaultInjector() *MapFaultInjector {
	return &MapFaultInjector{
		rules: make(map[string]map[string]string),
	}
}

// SetForVerb pins `reason` as the response for every
// invocation of `verb` (across every repoID).  Overrides
// any prior wildcard or per-repo rule for `verb`.
func (m *MapFaultInjector) SetForVerb(verb, reason string) {
	m.SetForVerbRepo(verb, "*", reason)
}

// SetForVerbRepo pins `reason` for the exact `(verb,
// repoID)` pair.  Use `"*"` for `repoID` to match any
// repo.
func (m *MapFaultInjector) SetForVerbRepo(verb, repoID, reason string) {
	if verb == "" {
		return
	}
	if repoID == "" {
		repoID = "*"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	inner, ok := m.rules[verb]
	if !ok {
		inner = make(map[string]string, 1)
		m.rules[verb] = inner
	}
	inner[repoID] = reason
}

// ClearVerb removes every rule for `verb`.
func (m *MapFaultInjector) ClearVerb(verb string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rules, verb)
}

// Clear removes every rule.
func (m *MapFaultInjector) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = make(map[string]map[string]string)
}

// Inject implements [FaultInjector].  Lookup order:
//
//  1. Exact (verb, repoID) pair.
//  2. (verb, "*") wildcard pair.
//
// Returns (false, "") when neither is set.  A rule that
// resolves to an empty reason returns (false, "") too —
// callers MUST NOT inject an empty reason because that
// fails [Enforce]'s degraded-with-empty-reason check, which
// would mask the no-rule case behind a 500.
func (m *MapFaultInjector) Inject(verb, repoID string) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inner, ok := m.rules[verb]
	if !ok {
		return false, ""
	}
	if reason, ok := inner[repoID]; ok && reason != "" {
		return true, reason
	}
	if reason, ok := inner["*"]; ok && reason != "" {
		return true, reason
	}
	return false, ""
}
