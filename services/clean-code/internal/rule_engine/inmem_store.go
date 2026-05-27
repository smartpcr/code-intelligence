package rule_engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// InMemoryStore is the canonical test fake [Store]. Holds
// every required row family in goroutine-safe maps. Used by:
//
//  1. The unit tests in this package
//     (`engine_test.go`, `worker_test.go`,
//     `synchronous_test.go`).
//  2. The scaffold-mode composition root (a future Stage
//     5.8 wiring) when CLEAN_CODE_PG_URL is unset -- so a
//     developer can exercise the engine end-to-end against
//     a fixture without a database.
//
// Concurrency: all methods acquire a single [sync.Mutex];
// the engine's hot path is rule evaluation, not store IO, so
// the contention is acceptable for tests and scaffold mode.
type InMemoryStore struct {
	mu sync.Mutex

	// Policy / rules sub-store mirrors.
	policyVersions map[uuid.UUID]steward.PolicyVersion
	rules          map[ruleKey]steward.Rule
	thresholds     map[uuid.UUID]steward.Threshold
	overrides      []steward.Override

	// Measurement mirror: samples keyed by (repo, sha).
	samples map[sampleKey][]Sample

	// commitParents mirrors `clean_code.commit.parent_sha`
	// per (repo_id, sha). An entry with value "" denotes a
	// root commit (no parent); an entry MISSING from the
	// map denotes a commit row that has not been
	// registered yet (a fresh-deploy / race-with-indexer
	// state). The engine treats both cases identically:
	// "no prior" -> every firing rule produces a
	// `delta=new` finding and no resolved row is emitted.
	commitParents map[sampleKey]string

	// evalLocks is a per-(repo, sha) mutex pool used by
	// [InMemoryStore.WithEvaluationLock] to serialise the
	// read-modify-write window the engine performs across
	// the prior-finding lookup + AppendEvaluation pair.
	evalLocks map[sampleKey]*sync.Mutex

	// Audit mirrors.
	runs     []EvaluationRun
	verdicts []EvaluationVerdict
	findings []Finding

	// clock is consulted by [InMemoryStore.LookupRecentCanonicalRun]
	// to apply the `since` TTL filter against a stored
	// run's CreatedAt. When nil the InMemoryStore IGNORES
	// the TTL (back-compat with existing fixtures that
	// pin a 2026 clock while time.Now() is wall-clock --
	// see godoc on LookupRecentCanonicalRun). Tests that
	// exercise the TTL boundary (TestEngine_RunSync_
	// DedupsHonoursTTLBoundary) MUST call SetClock with
	// the same clock the engine uses so the
	// `runCreatedAt + ttl > store.clock()` predicate
	// observes the engine's notion of "now".
	clock func() time.Time
}

type ruleKey struct {
	RuleID  string
	Version int
}

type sampleKey struct {
	RepoID uuid.UUID
	SHA    string
}

// NewInMemoryStore constructs a fresh fake store. The
// returned value is ready to use; tests seed it via the
// `Insert*` / `Append*` helpers below.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		policyVersions: make(map[uuid.UUID]steward.PolicyVersion),
		rules:          make(map[ruleKey]steward.Rule),
		thresholds:     make(map[uuid.UUID]steward.Threshold),
		samples:        make(map[sampleKey][]Sample),
		commitParents:  make(map[sampleKey]string),
	}
}

// SetClock injects a clock the InMemoryStore consults for
// the TTL filter in [InMemoryStore.LookupRecentCanonicalRun].
// Tests that exercise the TTL boundary MUST set this to
// the SAME clock the engine uses so the store and the
// engine share a notion of "now"; the production SQL
// path uses PG's `now()` which is implicitly shared by
// all replicas. Pass nil to disable the TTL filter
// (default behaviour for existing fixtures).
func (s *InMemoryStore) SetClock(clock func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = clock
}

// InsertPolicyVersion seeds a policy version. Tests call
// this to register the policy under test.
func (s *InMemoryStore) InsertPolicyVersion(pv steward.PolicyVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policyVersions[pv.PolicyVersionID] = pv
}

// InsertRule seeds a rule.
func (s *InMemoryStore) InsertRule(r steward.Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[ruleKey{RuleID: r.RuleID, Version: r.Version}] = r
}

// InsertThreshold seeds a threshold.
func (s *InMemoryStore) InsertThreshold(t steward.Threshold) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thresholds[t.ThresholdID] = t
}

// InsertOverride appends an override row. Latest-row-wins
// semantics match [steward.InMemoryStore].
func (s *InMemoryStore) InsertOverride(o steward.Override) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides = append(s.overrides, o)
}

// InsertSamples appends sample rows for the given
// `(repo_id, sha)` partition.
func (s *InMemoryStore) InsertSamples(repoID uuid.UUID, sha string, samples []Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sampleKey{RepoID: repoID, SHA: sha}
	s.samples[key] = append(s.samples[key], samples...)
}

// GetPolicyVersion implements [Store].
func (s *InMemoryStore) GetPolicyVersion(ctx context.Context, id uuid.UUID) (steward.PolicyVersion, error) {
	if err := ctx.Err(); err != nil {
		return steward.PolicyVersion{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pv, ok := s.policyVersions[id]
	if !ok {
		return steward.PolicyVersion{}, fmt.Errorf("%w: policy_version_id=%s", ErrUnknownPolicyVersion, id)
	}
	return pv, nil
}

// GetRule implements [Store].
func (s *InMemoryStore) GetRule(ctx context.Context, ruleID string, version int) (steward.Rule, error) {
	if err := ctx.Err(); err != nil {
		return steward.Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rules[ruleKey{RuleID: ruleID, Version: version}]
	if !ok {
		return steward.Rule{}, fmt.Errorf("%w: rule_id=%s version=%d", ErrUnknownRuleRef, ruleID, version)
	}
	return r, nil
}

// GetThreshold implements [Store].
func (s *InMemoryStore) GetThreshold(ctx context.Context, id uuid.UUID) (steward.Threshold, error) {
	if err := ctx.Err(); err != nil {
		return steward.Threshold{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.thresholds[id]
	if !ok {
		return steward.Threshold{}, fmt.Errorf("%w: threshold_id=%s", ErrUnknownThresholdRef, id)
	}
	return t, nil
}

// LatestMatchingOverride implements [Store]. Scans the
// in-memory override log under the same `(ruleID,
// candidate)` semantics as [steward.InMemoryStore.LatestMatchingOverride].
func (s *InMemoryStore) LatestMatchingOverride(ctx context.Context, ruleID string, candidate steward.CandidateScope) (steward.Override, bool, error) {
	if err := ctx.Err(); err != nil {
		return steward.Override{}, false, err
	}
	// Delegate to a fresh InMemoryStore so the matching
	// semantic (including the glob translation) stays in
	// one place. We copy the override slice under the
	// engine-store lock so the delegate sees a stable
	// snapshot.
	s.mu.Lock()
	snapshot := make([]steward.Override, len(s.overrides))
	copy(snapshot, s.overrides)
	s.mu.Unlock()

	delegate := steward.NewInMemoryStore()
	for _, o := range snapshot {
		if err := delegate.InsertOverride(ctx, o); err != nil {
			// The delegate's only failure modes are zero-
			// uuid override_id or a duplicate. Both signal
			// a seeding bug in the test fixture; propagate
			// rather than silently swallow.
			return steward.Override{}, false, fmt.Errorf("rule_engine: InMemoryStore: replay override into delegate: %w", err)
		}
	}
	return delegate.LatestMatchingOverride(ctx, ruleID, candidate)
}

// ListMetricSamples implements [Store]. Returns a fresh
// slice on every call so the caller can mutate it (e.g. to
// sort for determinism) without affecting the stored set.
func (s *InMemoryStore) ListMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, scopeID *uuid.UUID) ([]Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.samples[sampleKey{RepoID: repoID, SHA: sha}]
	out := make([]Sample, 0, len(rows))
	for _, sm := range rows {
		if scopeID != nil && sm.ScopeID != *scopeID {
			continue
		}
		out = append(out, sm)
	}
	return out, nil
}

// LatestPriorFinding implements [Store]. Scans the finding
// log for the most recent row matching `(repo_id, scope_id,
// rule_id, policy_version_id)` AND `sha == parentSHA`.
//
// Tie-break: when two prior rows share `created_at`, the
// row with the largest `finding_id` (lexicographic uuid
// order) wins -- mirrors the SQL `ORDER BY created_at DESC,
// finding_id DESC` contract.
//
// `parentSHA` is the topological parent of the SHA being
// evaluated (resolved by the caller via [Store.ParentSHA]).
// An empty `parentSHA` is refused: a root-commit case must
// be short-circuited by the engine before this method is
// called.
func (s *InMemoryStore) LatestPriorFinding(ctx context.Context, repoID uuid.UUID, parentSHA string, scopeID uuid.UUID, ruleID string, policyVersionID uuid.UUID) (Finding, bool, error) {
	if err := ctx.Err(); err != nil {
		return Finding{}, false, err
	}
	if scopeID == uuid.Nil {
		return Finding{}, false, errors.New("rule_engine: InMemoryStore.LatestPriorFinding: scopeID is the zero uuid")
	}
	if ruleID == "" {
		return Finding{}, false, errors.New("rule_engine: InMemoryStore.LatestPriorFinding: ruleID is empty")
	}
	if policyVersionID == uuid.Nil {
		return Finding{}, false, errors.New("rule_engine: InMemoryStore.LatestPriorFinding: policyVersionID is the zero uuid")
	}
	if parentSHA == "" {
		return Finding{}, false, errors.New("rule_engine: InMemoryStore.LatestPriorFinding: parentSHA is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var best Finding
	var found bool
	for _, f := range s.findings {
		if f.RepoID != repoID {
			continue
		}
		if f.SHA != parentSHA {
			continue
		}
		if f.ScopeID != scopeID {
			continue
		}
		if f.RuleID != ruleID {
			continue
		}
		if f.PolicyVersionID != policyVersionID {
			continue
		}
		if !found {
			best = f
			found = true
			continue
		}
		switch {
		case f.CreatedAt.After(best.CreatedAt):
			best = f
		case f.CreatedAt.Equal(best.CreatedAt) && uuidCompare(f.FindingID, best.FindingID) > 0:
			best = f
		}
	}
	return best, found, nil
}

// ListPriorBlockFindings implements [Store]. Returns the
// most-recent prior block finding per `(repo_id, scope_id,
// rule_id)` tuple at SHA == `parentSHA` whose latest non-
// resolved row is severity='block', filtered to
// `policy_version_id == policyVersionID`. Filters out
// tuples that are ALREADY resolved (the engine should not
// emit a second resolved row for an already-resolved tuple).
func (s *InMemoryStore) ListPriorBlockFindings(ctx context.Context, repoID uuid.UUID, parentSHA string, policyVersionID uuid.UUID) ([]Finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policyVersionID == uuid.Nil {
		return nil, errors.New("rule_engine: InMemoryStore.ListPriorBlockFindings: policyVersionID is the zero uuid")
	}
	if parentSHA == "" {
		return nil, errors.New("rule_engine: InMemoryStore.ListPriorBlockFindings: parentSHA is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Group prior findings by (scope_id, rule_id) and keep
	// the latest row per tuple.
	type tupleKey struct {
		ScopeID uuid.UUID
		RuleID  string
	}
	latest := make(map[tupleKey]Finding)
	for _, f := range s.findings {
		if f.RepoID != repoID {
			continue
		}
		if f.SHA != parentSHA {
			continue
		}
		if f.PolicyVersionID != policyVersionID {
			continue
		}
		k := tupleKey{ScopeID: f.ScopeID, RuleID: f.RuleID}
		cur, ok := latest[k]
		if !ok {
			latest[k] = f
			continue
		}
		switch {
		case f.CreatedAt.After(cur.CreatedAt):
			latest[k] = f
		case f.CreatedAt.Equal(cur.CreatedAt) && uuidCompare(f.FindingID, cur.FindingID) > 0:
			latest[k] = f
		}
	}

	out := make([]Finding, 0, len(latest))
	for _, f := range latest {
		// Skip already-resolved tuples: emitting a second
		// resolved row would inflate the resolution count
		// every run until the operator re-introduces the
		// rule.
		if f.Delta == DeltaResolved {
			continue
		}
		if f.Severity != steward.SeverityBlock {
			continue
		}
		out = append(out, f)
	}
	// Sort by (rule_id, scope_id) for deterministic output.
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return uuidCompare(out[i].ScopeID, out[j].ScopeID) < 0
	})
	return out, nil
}

// ParentSHA implements [Store]. Returns the parent of the
// given SHA as registered via [InMemoryStore.RegisterCommit].
// A registered entry with parent="" denotes a root commit;
// an unregistered SHA returns `ok=false`. The engine treats
// both cases identically -- "no prior, every firing rule is
// delta=new".
func (s *InMemoryStore) ParentSHA(ctx context.Context, repoID uuid.UUID, sha string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if sha == "" {
		return "", false, errors.New("rule_engine: InMemoryStore.ParentSHA: sha is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.commitParents[sampleKey{RepoID: repoID, SHA: sha}]
	if !ok {
		return "", false, nil
	}
	if parent == "" {
		// Root commit: registered, but no parent. Same
		// "no prior" semantic as unregistered.
		return "", false, nil
	}
	return parent, true, nil
}

// RegisterCommit seeds the parent-SHA mirror for the given
// commit row. Pass `parentSHA=""` to register a root commit.
// Subsequent [InMemoryStore.ParentSHA] calls for `sha`
// return the registered value (or `ok=false` if the parent
// is empty / unregistered).
//
// Tests MUST call this before evaluating a non-root SHA if
// they want prior-finding lookups to land. The engine
// itself does NOT auto-register commits -- the production
// SQLStore reads from `clean_code.commit`, where the
// indexer wrote the row.
func (s *InMemoryStore) RegisterCommit(repoID uuid.UUID, sha string, parentSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitParents[sampleKey{RepoID: repoID, SHA: sha}] = parentSHA
}

// WithEvaluationLock implements [Store]. Acquires a per-
// (repo, sha) goroutine-local mutex around the closure and
// passes the same store through, so reads + writes that
// the engine performs inside `fn` are serialised against
// other concurrent runs for the same (repo, sha) tuple.
//
// The production SQLStore implements this via
// `pg_advisory_xact_lock(...)` inside a `BEGIN; ...; COMMIT;`
// envelope; the in-memory implementation reuses the
// existing per-key mutex pool to give tests the same
// observable semantics without a database.
func (s *InMemoryStore) WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(Store) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("rule_engine: InMemoryStore.WithEvaluationLock: fn is nil")
	}
	mu := s.lockFor(repoID, sha)
	mu.Lock()
	defer mu.Unlock()
	return fn(s)
}

// lockFor returns a *sync.Mutex unique to the (repo, sha)
// tuple. The fake reuses a sync.Map keyed on the tuple's
// hash so the lock survives across calls.
func (s *InMemoryStore) lockFor(repoID uuid.UUID, sha string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evalLocks == nil {
		s.evalLocks = make(map[sampleKey]*sync.Mutex)
	}
	k := sampleKey{RepoID: repoID, SHA: sha}
	mu, ok := s.evalLocks[k]
	if !ok {
		mu = &sync.Mutex{}
		s.evalLocks[k] = mu
	}
	return mu
}

// AppendEvaluation implements [Store] as a single atomic
// append: every row -- the run, the verdict, every finding
// -- lands together under one mutex acquisition. A
// duplicate run / verdict / finding id is rejected so the
// fake catches a writer bug that mints two rows with the
// same uuid (which would be a 23505 in PG).
func (s *InMemoryStore) AppendEvaluation(ctx context.Context, run EvaluationRun, verdict EvaluationVerdict, findings []Finding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Pre-flight: refuse zero uuids and duplicate ids.
	if run.EvaluationRunID == uuid.Nil {
		return errors.New("rule_engine: InMemoryStore.AppendEvaluation: run.EvaluationRunID is the zero uuid")
	}
	if verdict.VerdictID == uuid.Nil {
		return errors.New("rule_engine: InMemoryStore.AppendEvaluation: verdict.VerdictID is the zero uuid")
	}
	if verdict.EvaluationRunID != run.EvaluationRunID {
		return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: verdict.EvaluationRunID=%s != run.EvaluationRunID=%s",
			verdict.EvaluationRunID, run.EvaluationRunID)
	}
	for _, existing := range s.runs {
		if existing.EvaluationRunID == run.EvaluationRunID {
			return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: duplicate evaluation_run_id=%s", run.EvaluationRunID)
		}
	}
	for _, existing := range s.verdicts {
		if existing.VerdictID == verdict.VerdictID {
			return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: duplicate verdict_id=%s", verdict.VerdictID)
		}
	}
	seen := make(map[uuid.UUID]struct{}, len(findings))
	for i, f := range findings {
		if f.FindingID == uuid.Nil {
			return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: findings[%d].FindingID is the zero uuid", i)
		}
		if f.EvaluationRunID != run.EvaluationRunID {
			return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: findings[%d].EvaluationRunID=%s != run.EvaluationRunID=%s",
				i, f.EvaluationRunID, run.EvaluationRunID)
		}
		if _, dup := seen[f.FindingID]; dup {
			return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: findings[%d].FindingID=%s duplicated within batch", i, f.FindingID)
		}
		seen[f.FindingID] = struct{}{}
		for _, existing := range s.findings {
			if existing.FindingID == f.FindingID {
				return fmt.Errorf("rule_engine: InMemoryStore.AppendEvaluation: findings[%d].FindingID=%s already exists",
					i, f.FindingID)
			}
		}
	}

	// All checks passed; commit.
	s.runs = append(s.runs, run)
	s.verdicts = append(s.verdicts, verdict)
	s.findings = append(s.findings, findings...)
	return nil
}

// LookupRecentCanonicalRun implements [Store]. Returns the
// most recent non-degraded canonical run matching
// `(repoID, sha, policyVersionID, caller, scopeID)`.
//
// Iter-6/iter-7 evaluator items #3 and #2: this is the
// cross-replica dedup helper; in the InMemoryStore it
// services the engine's runLocked path under the in-store
// mutex, matching the production txStore which holds
// `pg_advisory_xact_lock` for the same window.
//
// SCOPE-AWARE MATCH (iter-7): the lookup filters by
// `EvaluationRun.ScopeID` using null-safe equality -- a
// `nil` `scopeID` argument matches rows with `ScopeID == nil`
// (whole-SHA evaluation, every batch_refresh by
// construction); a non-nil argument matches rows whose
// `ScopeID` is non-nil and equal. This is the in-memory
// equivalent of the production `scope_id IS NOT DISTINCT
// FROM $5` predicate (migration 0008).
//
// NOTE: the `since` parameter is documented on the
// interface as a recency filter ("rows whose CreatedAt is
// within `since` of now"). The InMemoryStore IGNORES it
// because the fake has no shared clock with the engine --
// tests use a [fixtureClock] pinned to 2026 while
// `time.Now()` is wall-clock, and a time.Now()-based
// cutoff would lock the seeded run out of the dedup
// window. The production SQL path uses PG's `now()` which
// matches the row's `created_at` and DOES enforce the
// recency filter; that behaviour is covered by the live
// SQL tests when CLEAN_CODE_PG_URL is set.
//
// The engine-level [Engine.recentRuns] cache holds the
// TTL invariant for within-process dedup; the Store-level
// lookup is the cross-replica fallback whose TTL is owned
// by the production SQL impl.
func (s *InMemoryStore) LookupRecentCanonicalRun(ctx context.Context, repoID uuid.UUID, sha string, policyVersionID uuid.UUID, caller Caller, scopeID *uuid.UUID, since time.Duration) (RunResult, bool, error) {
	if err := ctx.Err(); err != nil {
		return RunResult{}, false, err
	}
	if repoID == uuid.Nil {
		return RunResult{}, false, errors.New("rule_engine: InMemoryStore.LookupRecentCanonicalRun: repoID is the zero uuid")
	}
	if sha == "" {
		return RunResult{}, false, errors.New("rule_engine: InMemoryStore.LookupRecentCanonicalRun: sha is empty")
	}
	if policyVersionID == uuid.Nil {
		return RunResult{}, false, errors.New("rule_engine: InMemoryStore.LookupRecentCanonicalRun: policyVersionID is the zero uuid")
	}
	if !caller.IsValid() {
		return RunResult{}, false, fmt.Errorf("rule_engine: InMemoryStore.LookupRecentCanonicalRun: caller=%q not in canonical set", caller)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Apply the TTL filter only when the test has wired
	// a shared clock via SetClock AND `since > 0`. When
	// no clock is set we IGNORE the filter -- the fake
	// has no notion of the engine's "now" and applying
	// time.Now() against a 2026-pinned run.CreatedAt
	// would either always pass (wall-clock < 2026) or
	// always fail unpredictably.
	var (
		cutoff       time.Time
		applyTTL     = false
	)
	if s.clock != nil && since > 0 {
		cutoff = s.clock().Add(-since)
		applyTTL = true
	}

	// Scan all matching non-degraded runs and pick the
	// newest by CreatedAt (with EvaluationRunID as a
	// deterministic tie-break). This is O(N) over the
	// fake's run set -- acceptable for tests; the
	// production txStore uses a single SQL query with
	// `ORDER BY er.created_at DESC, er.evaluation_run_id DESC LIMIT 1`.
	var (
		bestRun     EvaluationRun
		bestVerdict EvaluationVerdict
		found       bool
	)
	for _, r := range s.runs {
		if r.RepoID != repoID || r.SHA != sha || r.PolicyVersionID != policyVersionID || r.Caller != caller {
			continue
		}
		// Null-safe scope equality. Both nil -> match
		// (whole-SHA evaluation). Both non-nil and equal
		// -> match. Anything else -> skip.
		if !scopeIDsEqual(r.ScopeID, scopeID) {
			continue
		}
		// TTL filter (optional; see godoc + cutoff
		// derivation above). A run whose CreatedAt is
		// AT-OR-BEFORE the cutoff is considered expired
		// and the engine should mint a fresh row.
		if applyTTL && !r.CreatedAt.After(cutoff) {
			continue
		}
		var (
			v      EvaluationVerdict
			vfound bool
		)
		for _, vd := range s.verdicts {
			if vd.EvaluationRunID == r.EvaluationRunID {
				v = vd
				vfound = true
				break
			}
		}
		if !vfound || v.Degraded {
			continue
		}
		if !found {
			bestRun, bestVerdict, found = r, v, true
			continue
		}
		if r.CreatedAt.After(bestRun.CreatedAt) {
			bestRun, bestVerdict = r, v
		} else if r.CreatedAt.Equal(bestRun.CreatedAt) && uuidCompare(r.EvaluationRunID, bestRun.EvaluationRunID) > 0 {
			bestRun, bestVerdict = r, v
		}
	}
	if !found {
		return RunResult{}, false, nil
	}

	// Collect findings for the chosen run, ordered by
	// FindingID for determinism so two replicas observing
	// the same row return the same FindingIDs slice
	// ordering.
	var fIDs []uuid.UUID
	for _, f := range s.findings {
		if f.EvaluationRunID == bestRun.EvaluationRunID {
			fIDs = append(fIDs, f.FindingID)
		}
	}
	sort.Slice(fIDs, func(i, j int) bool {
		return uuidCompare(fIDs[i], fIDs[j]) < 0
	})
	return RunResult{
		EvaluationRunID:     bestRun.EvaluationRunID,
		EvaluationVerdictID: bestVerdict.VerdictID,
		FindingIDs:          fIDs,
		Verdict:             bestVerdict.Verdict,
	}, true, nil
}

// scopeIDsEqual is null-safe equality for the optional
// scope_id discriminator. Mirrors PostgreSQL's
// `IS NOT DISTINCT FROM` operator used by the production
// SQL lookup (migration 0008): both nil compare equal;
// both non-nil compare via uuid.UUID equality; otherwise
// not equal.
func scopeIDsEqual(a, b *uuid.UUID) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// Runs returns a snapshot of the persisted [EvaluationRun]
// rows. Test helper.
func (s *InMemoryStore) Runs() []EvaluationRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EvaluationRun, len(s.runs))
	copy(out, s.runs)
	return out
}

// Verdicts returns a snapshot of the persisted
// [EvaluationVerdict] rows. Test helper.
func (s *InMemoryStore) Verdicts() []EvaluationVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EvaluationVerdict, len(s.verdicts))
	copy(out, s.verdicts)
	return out
}

// Findings returns a snapshot of the persisted [Finding]
// rows. Test helper.
func (s *InMemoryStore) Findings() []Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Finding, len(s.findings))
	copy(out, s.findings)
	return out
}

// Compile-time check that InMemoryStore satisfies Store.
var _ Store = (*InMemoryStore)(nil)

// uuidCompare orders two UUIDs lexicographically by their
// canonical 16-byte representation. Mirrors the in-package
// helper in `steward/store.go` so the two `LatestMatching*`
// tie-break contracts stay aligned.
func uuidCompare(a, b uuid.UUID) int {
	ab := a.Bytes()
	bb := b.Bytes()
	for i := 0; i < len(ab); i++ {
		switch {
		case ab[i] < bb[i]:
			return -1
		case ab[i] > bb[i]:
			return 1
		}
	}
	return 0
}
