package rule_engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// Config wires the engine's dependencies. Required field:
// [Config.Store]. Optional fields:
//
//   - [Config.Cache] -- the [dsl.Cache] memoising compiled
//     predicates per `(policy_version_id, source)`. Defaults
//     to a freshly-allocated cache when nil.
//   - [Config.Clock] -- canonical wall-clock. Defaults to
//     [time.Now].
//   - [Config.NewID] -- uuid generator. Defaults to
//     [uuid.NewV4].
//
// Tests inject deterministic shims via the optional fields
// to pin row IDs and timestamps under assertion.
type Config struct {
	Store Store
	Cache *dsl.Cache
	Clock func() time.Time
	NewID func() (uuid.UUID, error)
	// RunDedupTTL is the time window during which the
	// engine returns the SAME canonical run+verdict+findings
	// for repeated calls with identical
	// `(repoID, sha, policyVersionID, scopeID, caller)`
	// arguments. Defaults to [DefaultRunDedupTTL] when zero.
	//
	// Per implementation-plan Stage 5.7 line 559: "two
	// parallel `eval.gate` calls for the same `(repo_id,
	// sha)` produce a single canonical run+verdict". The
	// TTL covers the narrow "parallel" window (one engine
	// run's wall-clock + a small grace period); requests
	// outside the window still write distinct audit rows
	// per the architecture's `every gate call is
	// audit-stamped` contract.
	RunDedupTTL time.Duration
}

// DefaultRunDedupTTL is the engine's "parallel call"
// dedup window. 30 seconds comfortably covers any
// reasonable engine-run wall-clock under load while still
// allowing the next "logical" gate call (e.g. after a
// developer pushes a follow-up commit minutes later) to
// produce a distinct audit row.
const DefaultRunDedupTTL = 30 * time.Second

// Engine is the in-process actor implementing the SOLID
// Rule Engine. Construct one via [New] at the composition
// root and share it across concurrent `eval.gate` requests
// and the batch-refresh dispatcher.
//
// Concurrency: the engine is safe for concurrent use across
// any number of goroutines. The in-process advisory lock
// (a per-`(repo, sha)` [sync.Mutex] in [Engine.locks])
// serialises read-modify-write windows so two concurrent
// `RunSync` calls for the same SHA do not race on the prior-
// finding snapshot. Within the lock window an in-process
// dedup cache ([Engine.recentRuns]) ensures the SECOND
// caller observes the FIRST caller's just-written canonical
// row set rather than minting its own duplicate (iter-5
// evaluator item #3, implementation-plan Stage 5.7 line 559).
type Engine struct {
	store       Store
	cache       *dsl.Cache
	clock       func() time.Time
	newID       func() (uuid.UUID, error)
	runDedupTTL time.Duration

	// locks holds a per-`(repo_id, sha)` [sync.Mutex] used
	// to serialise concurrent read-modify-write windows
	// (the advisory-lock contract from the Stage 5.7
	// brief). The map itself is guarded by [Engine.lockMu];
	// the per-key mutex is acquired AFTER releasing
	// [Engine.lockMu] so unrelated SHAs proceed in
	// parallel.
	lockMu sync.Mutex
	locks  map[string]*sync.Mutex

	// recentRuns memoises the most recent successful
	// [RunResult] per `(repoID, sha, policyVersionID,
	// scopeID, caller)` tuple within [Engine.runDedupTTL].
	// Guarded by [Engine.recentMu]; entries are evicted
	// lazily on lookup when older than the TTL.
	//
	// Used to satisfy implementation-plan Stage 5.7 line
	// 559's "two parallel `eval.gate` calls produce a
	// SINGLE canonical run+verdict" contract: the second
	// caller acquires the advisory lock AFTER the first
	// caller has released it (and stored its result in
	// the cache), so the second caller short-circuits to
	// the cached IDs rather than re-running the engine.
	recentMu   sync.Mutex
	recentRuns map[runCacheKey]runCacheEntry
}

// runCacheKey identifies a unique canonical run for the
// in-process dedup cache. The key includes `caller` so that
// a `batch_refresh` run and an `eval_gate` run for the
// same SHA do NOT collide -- they are distinct audit-
// stamped rows by spec.
type runCacheKey struct {
	repoID          uuid.UUID
	sha             string
	policyVersionID uuid.UUID
	scopeID         uuid.UUID // uuid.Nil when scope filter is nil
	caller          Caller
}

// runCacheEntry pairs a cached [RunResult] with its
// minting timestamp so lazy-eviction can prune stale rows
// during lookup.
type runCacheEntry struct {
	result RunResult
	ts     time.Time
}

// New constructs a fully wired engine. Returns
// [ErrStoreUnwired] if `cfg.Store` is nil; defaults for
// every other optional field follow the [Config] docstring.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil {
		return nil, ErrStoreUnwired
	}
	cache := cfg.Cache
	if cache == nil {
		cache = dsl.NewCache()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	newID := cfg.NewID
	if newID == nil {
		newID = uuid.NewV4
	}
	dedupTTL := cfg.RunDedupTTL
	if dedupTTL <= 0 {
		dedupTTL = DefaultRunDedupTTL
	}
	return &Engine{
		store:       cfg.Store,
		cache:       cache,
		clock:       clock,
		newID:       newID,
		runDedupTTL: dedupTTL,
		locks:       make(map[string]*sync.Mutex),
		recentRuns:  make(map[runCacheKey]runCacheEntry),
	}, nil
}

// RunSync is the synchronous mode entry point invoked by
// `eval.gate`. Writes ONE [EvaluationRun] (caller=
// `[CallerEvalGate]`) + ONE [EvaluationVerdict] + N
// [Finding] rows in a single [Store.AppendEvaluation]
// transaction and returns their IDs. `scopeID` MAY be nil
// for a repo-wide gate; passing a non-nil scope filters the
// sample set per the gate's `scope?` argument (architecture
// Sec 3.7 line 558).
//
// Concurrent invocations for the same `(repoID, sha,
// policyVersionID, scopeID)` are serialised by an
// in-process advisory lock so the prior-finding snapshot
// the delta computation sees stays consistent. Per
// implementation-plan Stage 5.7 line 559, two PARALLEL
// (same-args, near-the-same-instant) calls produce a
// SINGLE canonical run+verdict: the second caller acquires
// the lock after the first releases it, observes the
// first's just-written result via [Engine.recentRuns]
// (within [Config.RunDedupTTL]), and returns those IDs
// rather than minting a duplicate audit row. Sequential
// calls outside the dedup window still produce distinct
// audit rows per the architecture's
// `every gate call is audit-stamped` contract.
func (e *Engine) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scopeID *uuid.UUID, policyVersionID uuid.UUID) (RunResult, error) {
	return e.run(ctx, repoID, sha, scopeID, policyVersionID, CallerEvalGate)
}

// RunBatch is the batch-refresh mode entry point invoked by
// the post-scan dispatcher (architecture Sec 4.1 line 752,
// Sec 4.7). Writes the same row set as [Engine.RunSync] but
// stamps `caller=`[CallerBatchRefresh] and never filters by
// scope (the batch refresh re-runs every rule against every
// sample for the SHA).
func (e *Engine) RunBatch(ctx context.Context, repoID uuid.UUID, sha string, policyVersionID uuid.UUID) (RunResult, error) {
	return e.run(ctx, repoID, sha, nil, policyVersionID, CallerBatchRefresh)
}

// run is the shared core of both entry points. The caller
// discriminator is the only operational difference between
// the synchronous and batch modes.
func (e *Engine) run(ctx context.Context, repoID uuid.UUID, sha string, scopeID *uuid.UUID, policyVersionID uuid.UUID, caller Caller) (RunResult, error) {
	if !caller.IsValid() {
		return RunResult{}, fmt.Errorf("%w: caller=%q", ErrInvalidCaller, caller)
	}
	if repoID == uuid.Nil {
		return RunResult{}, errors.New("rule_engine: run: repo_id is the zero uuid")
	}
	if sha == "" {
		return RunResult{}, errors.New("rule_engine: run: sha is empty")
	}
	if policyVersionID == uuid.Nil {
		return RunResult{}, errors.New("rule_engine: run: policy_version_id is the zero uuid")
	}

	// The engine acquires TWO locks around the (repo, sha)
	// read-modify-write window:
	//
	//   - The Store's own [Store.WithEvaluationLock]
	//     envelope, which is `pg_advisory_xact_lock(...)`
	//     in production -- the cross-process fence.
	//   - The engine's in-process [Engine.lockFor] mutex,
	//     which is the cross-goroutine fence inside a
	//     single service instance. We acquire it BEFORE
	//     entering the Store envelope so a tight burst of
	//     RunSync calls from the same instance does not
	//     thrash the database advisory lock.
	mu := e.lockFor(repoID, sha)
	mu.Lock()
	defer mu.Unlock()

	// Parallel-call dedup (iter-5 evaluator item #3 /
	// implementation-plan Stage 5.7 line 559): consult
	// the in-process cache AFTER acquiring the lock so
	// the second of two parallel callers observes the
	// first caller's just-written canonical IDs. The
	// cache is keyed by the FULL identity tuple
	// (including scopeID and caller) so a scoped sync
	// run cannot be reused as the canonical row for an
	// unscoped batch run, and vice versa.
	cacheKey := e.makeRunCacheKey(repoID, sha, policyVersionID, scopeID, caller)
	if cached, ok := e.lookupRecentRun(cacheKey); ok {
		return cached, nil
	}

	var result RunResult
	lockErr := e.store.WithEvaluationLock(ctx, repoID, sha, func(s Store) error {
		r, err := e.runLocked(ctx, s, repoID, sha, scopeID, policyVersionID, caller)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if lockErr != nil {
		return RunResult{}, lockErr
	}
	e.storeRecentRun(cacheKey, result)
	return result, nil
}

// makeRunCacheKey assembles the dedup-cache key from the
// full call identity. A nil scopeID maps to [uuid.Nil] so
// scoped vs unscoped calls never collide on the cache.
func (e *Engine) makeRunCacheKey(repoID uuid.UUID, sha string, policyVersionID uuid.UUID, scopeID *uuid.UUID, caller Caller) runCacheKey {
	var s uuid.UUID
	if scopeID != nil {
		s = *scopeID
	}
	return runCacheKey{
		repoID:          repoID,
		sha:             sha,
		policyVersionID: policyVersionID,
		scopeID:         s,
		caller:          caller,
	}
}

// lookupRecentRun returns the cached [RunResult] for `key`
// when present AND still within [Engine.runDedupTTL]. Expired
// entries are evicted lazily.
func (e *Engine) lookupRecentRun(key runCacheKey) (RunResult, bool) {
	e.recentMu.Lock()
	defer e.recentMu.Unlock()
	entry, ok := e.recentRuns[key]
	if !ok {
		return RunResult{}, false
	}
	if e.clock().Sub(entry.ts) > e.runDedupTTL {
		delete(e.recentRuns, key)
		return RunResult{}, false
	}
	return entry.result, true
}

// storeRecentRun records a successful run in the dedup
// cache so a subsequent parallel call for the SAME args
// observes its IDs instead of minting duplicates.
func (e *Engine) storeRecentRun(key runCacheKey, result RunResult) {
	e.recentMu.Lock()
	defer e.recentMu.Unlock()
	e.recentRuns[key] = runCacheEntry{
		result: result,
		ts:     e.clock(),
	}
}

// runLocked performs the read-modify-write under both the
// in-process mutex and the Store's evaluation-lock envelope.
// All reads + the AppendEvaluation write go through the
// store passed by [Store.WithEvaluationLock]; on the PG path
// this is the transaction-bound Store, so prior-finding
// snapshots and the canonical append commit atomically.
func (e *Engine) runLocked(ctx context.Context, s Store, repoID uuid.UUID, sha string, scopeID *uuid.UUID, policyVersionID uuid.UUID, caller Caller) (RunResult, error) {
	// Cross-replica dedup for BOTH callers (iter-7
	// evaluator item #2; iter-6 evaluator item #3): a
	// parallel replica that has already committed its
	// canonical row under the same `pg_advisory_xact_lock`
	// is observed by THIS caller's RC-isolated SELECT
	// because the lock-release happens-before the SELECT.
	// The first replica wins; the second replica returns
	// the same (run_id, verdict_id, finding_ids) instead
	// of minting duplicates -- the in-process recentRuns
	// cache is the within-process layer above this.
	//
	// SCOPE-AWARE (iter-7 evaluator item #2): migration
	// 0008 adds a nullable `evaluation_run.scope_id`
	// column. The lookup filters with `IS NOT DISTINCT
	// FROM $5` (PostgreSQL null-safe equality), so a
	// scoped eval_gate call NEVER matches an unscoped
	// eval_gate row (or vice versa); both `nil` and
	// non-nil scope arguments share a single code path.
	// This closes the iter-6 #2 gap where eval_gate
	// cross-replica parallel calls could mint duplicate
	// run+verdict rows because the Store-level lookup was
	// gated to batch_refresh only.
	if cached, ok, err := s.LookupRecentCanonicalRun(ctx, repoID, sha, policyVersionID, caller, scopeID, e.runDedupTTL); err != nil {
		// Dedup is an OPTIMISATION, not a correctness
		// boundary; a lookup failure just falls through
		// to the full path. We don't log here -- the
		// production txStore wraps the underlying error
		// with enough detail for an upstream INFO log if
		// the composition root chooses to surface it.
		_ = err
	} else if ok {
		return cached, nil
	}

	pv, err := s.GetPolicyVersion(ctx, policyVersionID)
	if err != nil {
		// Wrap unconditionally -- the Store implementation
		// is responsible for marking the error with
		// [ErrUnknownPolicyVersion]; we just propagate.
		return RunResult{}, fmt.Errorf("rule_engine: run: load policy: %w", err)
	}

	resolver, err := e.buildThresholdResolverWith(ctx, s, pv)
	if err != nil {
		return RunResult{}, err
	}

	rules, err := e.loadRulesWith(ctx, s, pv)
	if err != nil {
		return RunResult{}, err
	}

	samples, err := s.ListMetricSamples(ctx, repoID, sha, scopeID)
	if err != nil {
		return RunResult{}, fmt.Errorf("rule_engine: run: list samples: %w", err)
	}

	// Enforce the [ErrSampleSourceEmpty] contract documented
	// on the sentinel (types.go) and on [Store.ListMetricSamples]
	// (store.go): a `(nil, nil)` return combined with a
	// non-empty rule set is a strong signal of a misconfigured
	// store, NOT a legitimate "no samples yet" outcome.
	// Legitimate "no samples" is a non-nil empty slice
	// (`make([]Sample, 0)`) which both InMemoryStore and the
	// SQL path already return; that case falls through to
	// verdict=pass with zero findings. We only refuse the
	// (nil, nil) shape so a wiring bug in a custom Store
	// implementation surfaces loudly instead of masquerading
	// as a successful no-op run.
	if samples == nil && len(rules) > 0 {
		return RunResult{}, fmt.Errorf("%w: repo_id=%s sha=%s rules=%d",
			ErrSampleSourceEmpty, repoID, sha, len(rules))
	}

	// Resolve the topological parent for the prior-finding
	// snapshot. A missing parent (root commit or
	// unregistered SHA) collapses every delta to
	// [DeltaNew] and skips resolved-row emission.
	parentSHA, parentOK, err := s.ParentSHA(ctx, repoID, sha)
	if err != nil {
		return RunResult{}, fmt.Errorf("rule_engine: run: resolve parent sha: %w", err)
	}

	// Compile every rule's predicate up front via the
	// shared cache. Compilation failure here is fatal for
	// the run -- a malformed predicate in the active policy
	// is a publish-time bug and refusing the run surfaces
	// it loudly.
	preds := make([]ruleBinding, 0, len(rules))
	for _, r := range rules {
		pred, err := e.cache.GetOrCompile(policyVersionID, r.PredicateDSL, resolver)
		if err != nil {
			return RunResult{}, fmt.Errorf("%w: rule_id=%s version=%d: %v",
				ErrPredicateCompile, r.RuleID, r.Version, err)
		}
		preds = append(preds, ruleBinding{rule: r, predicate: pred})
	}

	now := e.clock().UTC()
	runID, err := e.newID()
	if err != nil {
		return RunResult{}, fmt.Errorf("rule_engine: run: mint run_id: %w", err)
	}

	firing, err := e.evaluate(ctx, s, repoID, sha, parentSHA, parentOK, policyVersionID, runID, now, preds, samples)
	if err != nil {
		return RunResult{}, err
	}

	resolved, err := e.computeResolved(ctx, s, repoID, sha, parentSHA, parentOK, policyVersionID, runID, now, firing)
	if err != nil {
		return RunResult{}, err
	}

	// Compose the final finding set. Sort by FindingID so
	// the caller observes a deterministic order; the SQL
	// path's `INSERT ... RETURNING finding_id` would
	// otherwise return them in INSERT order, which is
	// implementation-defined.
	allFindings := make([]Finding, 0, len(firing)+len(resolved))
	for _, f := range firing {
		allFindings = append(allFindings, *f)
	}
	allFindings = append(allFindings, resolved...)
	sort.Slice(allFindings, func(i, j int) bool {
		return uuidCompare(allFindings[i].FindingID, allFindings[j].FindingID) < 0
	})

	verdictID, err := e.newID()
	if err != nil {
		return RunResult{}, fmt.Errorf("rule_engine: run: mint verdict_id: %w", err)
	}

	run := EvaluationRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             sha,
		PolicyVersionID: policyVersionID,
		Caller:          caller,
		// ScopeID makes the canonical row scope-aware so
		// the Store-level dedup lookup (above) can safely
		// distinguish scoped vs unscoped eval_gate runs
		// at the row level (migration 0008; iter-7
		// evaluator item #2). nil for whole-SHA runs
		// (every batch_refresh by construction, plus
		// eval.gate calls with no scope argument).
		ScopeID:   scopeID,
		CreatedAt: now,
	}
	verdict := EvaluationVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         e.computeVerdict(allFindings),
		Degraded:        false,
		DegradedReason:  "",
		CreatedAt:       now,
	}

	if err := s.AppendEvaluation(ctx, run, verdict, allFindings); err != nil {
		return RunResult{}, fmt.Errorf("rule_engine: run: append: %w", err)
	}

	findingIDs := make([]uuid.UUID, len(allFindings))
	for i, f := range allFindings {
		findingIDs[i] = f.FindingID
	}
	return RunResult{
		EvaluationRunID:     runID,
		EvaluationVerdictID: verdictID,
		FindingIDs:          findingIDs,
		Verdict:             verdict.Verdict,
	}, nil
}

// ruleBinding pairs a rule with its compiled predicate.
type ruleBinding struct {
	rule      steward.Rule
	predicate *dsl.Predicate
}

// buildThresholdResolverWith populates a [dsl.MapResolver]
// from the policy version's [steward.PolicyVersion.ThresholdRefs]
// slice via the supplied [Store] handle. Every reference
// MUST resolve; otherwise the bound predicate would carry a
// dangling threshold node.
//
// The Store argument is the [Store.WithEvaluationLock]-
// scoped handle so production reads share the engine's
// transaction.
func (e *Engine) buildThresholdResolverWith(ctx context.Context, s Store, pv steward.PolicyVersion) (dsl.MapResolver, error) {
	resolver := make(dsl.MapResolver, len(pv.ThresholdRefs))
	for i, ref := range pv.ThresholdRefs {
		t, err := s.GetThreshold(ctx, ref.ThresholdID)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: threshold_refs[%d]={threshold_id=%s}: %w", i, ref.ThresholdID, err)
		}
		op := dsl.ThresholdOp(t.Op)
		if !op.IsValid() {
			return nil, fmt.Errorf("rule_engine: threshold_refs[%d]: op=%q is not in {gt,ge,lt,le,eq}", i, t.Op)
		}
		resolver[t.ThresholdID] = dsl.Threshold{
			ThresholdID: t.ThresholdID,
			MetricKind:  t.MetricKind,
			ScopeKind:   t.ScopeKind,
			Op:          op,
			Value:       t.Value,
		}
	}
	return resolver, nil
}

// loadRulesWith resolves every [steward.RuleRef] in the
// policy to a [steward.Rule] row via the supplied [Store]
// handle (see [Engine.buildThresholdResolverWith] for the
// transaction-scope rationale).
func (e *Engine) loadRulesWith(ctx context.Context, s Store, pv steward.PolicyVersion) ([]steward.Rule, error) {
	rules := make([]steward.Rule, 0, len(pv.RuleRefs))
	for i, ref := range pv.RuleRefs {
		r, err := s.GetRule(ctx, ref.RuleID, ref.Version)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: rule_refs[%d]={rule_id=%s, version=%d}: %w", i, ref.RuleID, ref.Version, err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// firingKey identifies a `(rule_id, scope_id)` tuple in the
// firing map.
type firingKey struct {
	RuleID  string
	ScopeID uuid.UUID
}

// evaluate walks every (rule, scope) pair and accumulates
// the set of "currently firing" findings. A rule fires at a
// scope when its predicate's [dsl.Predicate.EvalAtScope]
// returns matched=true for the scope's sample set. The
// finding's `metric_sample_ids` carries the EXACT
// [dsl.Sample.SampleID]s the scope evaluator attributed to
// the match (G4 + Stage 5.7 brief: "the EXACT MetricSample
// row(s) that triggered the rule").
//
// Switching to per-scope evaluation (vs the prior per-sample
// loop) is what enables SOLID composite recipes such as SRP
// `threshold(lcom4) AND threshold(interface_width)` to fire
// when a class has BOTH a high-LCOM4 sample AND a wide-
// interface sample, even though no single sample carries
// both metric_kinds. The semantics for the per-sample
// reading (e.g. `metric_kind == 'lcom4' AND value > 5`) are
// preserved by the evaluator's Phase 1 / Phase 2 split --
// see [dsl.Predicate.EvalAtScope] for the contract.
//
// Mute is checked per `(rule, candidate)` against the
// override matcher; a positive match with `mute=true`
// SUPPRESSES the finding entirely per the Stage 5.7 brief
// "muted-scope-skipped" scenario.
//
// Delta is computed via [Store.LatestPriorFinding] against
// the topological parent SHA's finding for the same
// `(repo, scope, rule, policy_version)` tuple. When the
// commit has no registered parent (root commit or
// indexer-race), the prior lookup is skipped and every
// firing finding lands as [DeltaNew].
func (e *Engine) evaluate(ctx context.Context, s Store, repoID uuid.UUID, sha string, parentSHA string, hasParent bool, policyVersionID uuid.UUID, runID uuid.UUID, now time.Time, preds []ruleBinding, samples []Sample) (map[firingKey]*Finding, error) {
	firing := make(map[firingKey]*Finding)

	// Group samples by scope_id. We retain scope_kind +
	// scope_signature per scope so the downstream override-
	// matcher candidate is constructible without re-joining.
	type scopeBucket struct {
		scopeKind      string
		scopeSignature string
		dslSamples     []dsl.Sample
	}
	scopeIdx := make(map[uuid.UUID]*scopeBucket)
	scopeOrder := make([]uuid.UUID, 0)
	for _, sm := range samples {
		b, ok := scopeIdx[sm.ScopeID]
		if !ok {
			b = &scopeBucket{
				scopeKind:      sm.ScopeKind,
				scopeSignature: sm.ScopeSignature,
				dslSamples:     make([]dsl.Sample, 0, 1),
			}
			scopeIdx[sm.ScopeID] = b
			scopeOrder = append(scopeOrder, sm.ScopeID)
		}
		b.dslSamples = append(b.dslSamples, sm.Sample)
	}

	// Stable scope iteration order so the finding row IDs
	// the engine mints are deterministic across replays.
	sort.Slice(scopeOrder, func(i, j int) bool {
		return uuidCompare(scopeOrder[i], scopeOrder[j]) < 0
	})

	// muteCache memoises the override matcher's verdict
	// per `(rule_id, scope_id)` so the engine does not
	// re-issue the same `LatestMatchingOverride` call.
	type muteCacheKey struct {
		RuleID  string
		ScopeID uuid.UUID
	}
	muteCache := make(map[muteCacheKey]bool)

	for _, b := range preds {
		for _, scopeID := range scopeOrder {
			bucket := scopeIdx[scopeID]
			match, witnessIDs, err := b.predicate.EvalAtScope(dsl.ScopeContext{Samples: bucket.dslSamples})
			if err != nil {
				return nil, fmt.Errorf("%w: rule_id=%s scope_id=%s: %v",
					ErrPredicateEval, b.rule.RuleID, scopeID, err)
			}
			if !match {
				continue
			}

			mk := muteCacheKey{RuleID: b.rule.RuleID, ScopeID: scopeID}
			muted, cached := muteCache[mk]
			if !cached {
				candidate := steward.CandidateScope{
					RepoID:    repoID.String(),
					ScopeKind: steward.ScopeKind(bucket.scopeKind),
					Signature: bucket.scopeSignature,
				}
				ov, found, err := s.LatestMatchingOverride(ctx, b.rule.RuleID, candidate)
				if err != nil {
					return nil, fmt.Errorf("rule_engine: override lookup for rule_id=%s scope_id=%s: %w",
						b.rule.RuleID, scopeID, err)
				}
				muted = found && ov.Mute
				muteCache[mk] = muted
			}
			if muted {
				continue
			}

			fk := firingKey{RuleID: b.rule.RuleID, ScopeID: scopeID}
			id, err := e.newID()
			if err != nil {
				return nil, fmt.Errorf("rule_engine: mint finding_id: %w", err)
			}
			firing[fk] = &Finding{
				FindingID:       id,
				EvaluationRunID: runID,
				RepoID:          repoID,
				SHA:             sha,
				ScopeID:         scopeID,
				RuleID:          b.rule.RuleID,
				RuleVersion:     b.rule.Version,
				PolicyVersionID: policyVersionID,
				MetricSampleIDs: witnessIDs,
				Severity:        b.rule.SeverityDefault,
				ExplanationMD:   "",
				CreatedAt:       now,
			}
		}
	}

	// Compute delta per firing finding. Stable key order
	// for deterministic prior-finding lookup sequence.
	keys := make([]firingKey, 0, len(firing))
	for k := range firing {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].RuleID != keys[j].RuleID {
			return keys[i].RuleID < keys[j].RuleID
		}
		return uuidCompare(keys[i].ScopeID, keys[j].ScopeID) < 0
	})
	for _, k := range keys {
		f := firing[k]
		if !hasParent {
			// Root commit / indexer-race: no parent SHA
			// to look up against. Every firing rule is
			// "new" at this SHA by construction.
			f.Delta = computeDelta(f.Severity, false, Finding{})
			continue
		}
		prior, hasPrior, err := s.LatestPriorFinding(ctx, repoID, parentSHA, f.ScopeID, f.RuleID, policyVersionID)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: latest prior finding for rule_id=%s scope_id=%s: %w",
				f.RuleID, f.ScopeID, err)
		}
		f.Delta = computeDelta(f.Severity, hasPrior, prior)
	}
	return firing, nil
}

// computeResolved emits a [DeltaResolved] finding for every
// `(scope_id, rule_id)` that was severity='block' at the
// parent SHA but is EITHER absent at this SHA OR present at
// a strictly lower severity (`warn` or `info`). Severity on
// the resolved row is pinned to `info` so the rollup does
// not keep blocking after the bug is fixed.
//
// Iter-5 evaluator item #4: previously the function skipped
// ANY still-firing tuple, suppressing the resolved row when
// a block downgraded to warn. Per implementation-plan Stage
// 5.7 line 556 a `resolved` row MUST be emitted whenever a
// prior block is "now absent or at lower severity" -- the
// downgrade is real auditable progress, not a regression
// that should be hidden under a still-firing warn.
//
// When a tuple downgrades, the engine emits BOTH:
//   - the current lower-severity finding (delta=new), and
//   - a separate `severity=info, delta=resolved` row.
//
// The two rows carry distinct `finding_id`s; the schema
// permits multiple findings per `(run, rule, scope)` tuple.
// The verdict rollup skips delta=resolved rows by
// construction (see [Engine.computeVerdict]) so the
// downgraded warn correctly drives `verdict=warn` while the
// resolved row records the block→warn transition.
//
// When the commit has no registered parent the prior-block
// set is empty by construction -- nothing was tracked under
// a non-existent ancestor, so no resolved rows are emitted.
func (e *Engine) computeResolved(ctx context.Context, s Store, repoID uuid.UUID, sha string, parentSHA string, hasParent bool, policyVersionID uuid.UUID, runID uuid.UUID, now time.Time, firing map[firingKey]*Finding) ([]Finding, error) {
	if !hasParent {
		return nil, nil
	}
	priorBlocks, err := s.ListPriorBlockFindings(ctx, repoID, parentSHA, policyVersionID)
	if err != nil {
		return nil, fmt.Errorf("rule_engine: list prior block findings: %w", err)
	}
	out := make([]Finding, 0)
	for _, p := range priorBlocks {
		fk := firingKey{RuleID: p.RuleID, ScopeID: p.ScopeID}
		current, stillFiring := firing[fk]
		if stillFiring && current.Severity == steward.SeverityBlock {
			// Still failing at block -- not resolved.
			continue
		}
		// Either tuple is ABSENT at this SHA (classic
		// resolve) OR present at a strictly lower severity
		// (block -> warn / info downgrade). Either way the
		// spec calls for a resolved row.
		id, err := e.newID()
		if err != nil {
			return nil, fmt.Errorf("rule_engine: mint resolved finding_id: %w", err)
		}
		out = append(out, Finding{
			FindingID:       id,
			EvaluationRunID: runID,
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         p.ScopeID,
			RuleID:          p.RuleID,
			RuleVersion:     p.RuleVersion,
			PolicyVersionID: policyVersionID,
			MetricSampleIDs: []uuid.UUID{},
			Severity:        steward.SeverityInfo,
			Delta:           DeltaResolved,
			ExplanationMD:   "",
			CreatedAt:       now,
		})
	}
	return out, nil
}

// computeVerdict implements the severity-rollup contract
// from architecture Sec 4.2 step 5 (lines 770-772): the
// verdict is `block` if any unmuted finding has
// `severity='block'`, `warn` if any has `severity='warn'`,
// `pass` otherwise. [DeltaResolved] rows are excluded
// because they are 'info' severity by construction and
// represent a regression that has been fixed at this SHA --
// they MUST NOT keep blocking the run.
func (e *Engine) computeVerdict(findings []Finding) Verdict {
	v := VerdictPass
	for _, f := range findings {
		if f.Delta == DeltaResolved {
			continue
		}
		var cand Verdict
		switch f.Severity {
		case steward.SeverityBlock:
			cand = VerdictBlock
		case steward.SeverityWarn:
			cand = VerdictWarn
		default:
			cand = VerdictPass
		}
		if cand.rank() > v.rank() {
			v = cand
		}
	}
	return v
}

// computeDelta returns the [Delta] enum for a currently-
// firing finding given the prior-SHA finding for the same
// `(repo, scope, rule)` tuple.
//
// Decision matrix (architecture Sec 5.4.1 line 1215):
//
//	no prior            -> new
//	prior severity=block, current severity=block -> unchanged
//	prior severity!=block, current severity=block -> newly_failing
//	otherwise (current not block)                 -> new
//
// The "otherwise" branch covers the case where the current
// firing is severity='warn' or 'info' regardless of the
// prior state -- this is a "fresh" surfacing at the lower
// severity. The architecture does not pin a name for
// "warn-to-warn" carry-over; we treat it as `new` because
// the alternative (`unchanged` with `severity='warn'`)
// would conflict with the Sec 5.4.1 language pinning
// `unchanged` to block-to-block specifically.
func computeDelta(currentSeverity steward.Severity, hasPrior bool, prior Finding) Delta {
	if !hasPrior {
		return DeltaNew
	}
	priorWasBlock := prior.Severity == steward.SeverityBlock && prior.Delta != DeltaResolved
	currentIsBlock := currentSeverity == steward.SeverityBlock
	switch {
	case priorWasBlock && currentIsBlock:
		return DeltaUnchanged
	case !priorWasBlock && currentIsBlock:
		return DeltaNewlyFailing
	default:
		return DeltaNew
	}
}

// lockFor returns the per-`(repoID, sha)` advisory mutex.
// The first caller for a given key allocates the mutex; all
// subsequent callers receive the same pointer and contend on
// it. The map mutex [Engine.lockMu] is held only for the map
// lookup so unrelated SHAs never block on each other.
//
// We do NOT prune the locks map on completion. The map grows
// proportionally to the number of distinct
// `(repo_id, sha)` pairs the service has ever evaluated; a
// 64-byte mutex per pair is acceptable for the v1 throughput
// (millions of SHAs across years of operation is well under
// 100MB). A follow-up could TTL stale entries via a sweep,
// but the production-PG path already has
// `pg_advisory_xact_lock` doing the actual cross-process
// coordination so the in-process map is a defence-in-depth
// surface, not the primary correctness boundary.
func (e *Engine) lockFor(repoID uuid.UUID, sha string) *sync.Mutex {
	key := repoID.String() + "@" + sha
	e.lockMu.Lock()
	defer e.lockMu.Unlock()
	mu, ok := e.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		e.locks[key] = mu
	}
	return mu
}
