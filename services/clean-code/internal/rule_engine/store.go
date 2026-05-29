package rule_engine

import (
	"context"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/steward"
)

// Store is the persistence boundary the [Engine] writes
// through. It is intentionally a SINGLE interface (rather
// than five sub-interfaces) so the composition root wires
// ONE Store per process and the engine's read/write surface
// is co-located with the writer-ownership grants from
// architecture Sec 1.5 G1 / tech-spec Sec 7.2.
//
// Two production implementations are expected:
//
//   - [InMemoryStore] (this package) -- the test fake. Holds
//     policy / rule / threshold / override / sample / finding
//     rows in goroutine-safe maps.
//   - A future PG-backed implementation (Stage 5.8 wiring)
//     that translates each method into a SQL statement
//     against the `clean_code` schema.
//
// The single interface lets the engine treat
// [Store.AppendEvaluation] as an atomic boundary: ONE
// [EvaluationRun] + ONE [EvaluationVerdict] + N [Finding]
// rows commit together OR not at all. Splitting it would
// invite a partial-write hazard the rubber-duck critique on
// Stage 5.2's `policy.publish_rulepack` already pinned as a
// G3 violation.
//
// Concurrency: implementations MUST be safe for concurrent
// use. The engine's in-process advisory lock serialises the
// (repo_id, sha) read-modify-write window, but other read
// methods (e.g. [Store.GetPolicyVersion]) may race against
// unrelated runs.
type Store interface {
	// GetPolicyVersion resolves the policy referenced by
	// `policyVersionID`. Returns a wrapped
	// [ErrUnknownPolicyVersion] when the row is absent so
	// the engine can refuse the run rather than evaluate
	// against a synthesised zero policy.
	GetPolicyVersion(ctx context.Context, policyVersionID uuid.UUID) (steward.PolicyVersion, error)

	// GetRule resolves the rule referenced by
	// `(ruleID, version)`. Returns a wrapped
	// [ErrUnknownRuleRef] on miss.
	GetRule(ctx context.Context, ruleID string, version int) (steward.Rule, error)

	// GetThreshold resolves the threshold referenced by
	// `thresholdID`. Returns a wrapped
	// [ErrUnknownThresholdRef] on miss.
	GetThreshold(ctx context.Context, thresholdID uuid.UUID) (steward.Threshold, error)

	// LatestMatchingOverride is the gate-time override
	// lookup -- delegates to [steward.Steward.LatestMatchingOverride].
	// The engine constructs `candidate` from each [Sample]'s
	// `(repo_id, scope_kind, scope_signature)` triple.
	LatestMatchingOverride(ctx context.Context, ruleID string, candidate steward.CandidateScope) (steward.Override, bool, error)

	// ListMetricSamples returns the [Sample] rows for
	// `(repo_id, sha)`. When `scopeID` is non-nil the
	// returned set is filtered to that scope (the gate's
	// PR-diff `scope?` argument). Production implementations
	// MUST perform the `metric_sample x scope_binding` join
	// at the SQL layer so the engine sees a denormalised
	// view (architecture Sec 5.2.3 line 1046 + DSL
	// purity contract).
	//
	// Returns `(nil, nil)` only when the store has no
	// samples for the SHA AND the caller has not yet
	// triggered a scan -- an unusual but legitimate state
	// (e.g. a freshly-registered repo). The engine
	// distinguishes it via [ErrSampleSourceEmpty] only when
	// the rule set is non-empty, which avoids masking a
	// store misconfiguration.
	ListMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, scopeID *uuid.UUID) ([]Sample, error)

	// ParentSHA returns the canonical parent of `sha`
	// from `clean_code.commit.parent_sha`. The engine
	// uses this to scope the prior-finding lookup to the
	// IMMEDIATE topological ancestor rather than to a
	// wall-clock proxy -- using `created_at` would treat
	// a back-merge or a re-evaluated older SHA as "prior
	// for the newer SHA", which is semantically wrong.
	//
	// Returns `ok=false` when `sha` is a root commit (no
	// parent) OR when the commit row has not been
	// registered yet (e.g. the gate races the indexer);
	// the engine treats both cases as "no prior" so every
	// firing rule produces a `delta=new` finding and no
	// resolved row is emitted (architecture Sec 5.4.1 line
	// 1215). Returns a non-nil error only on store IO
	// failures.
	//
	// Production: a single `SELECT parent_sha FROM
	// clean_code.commit WHERE repo_id=$1 AND sha=$2`.
	ParentSHA(ctx context.Context, repoID uuid.UUID, sha string) (string, bool, error)

	// LatestPriorFinding returns the prior [Finding] row
	// at SHA `parentSHA` for the `(repo_id, scope_id,
	// rule_id, policy_version_id)` tuple. "Prior" is
	// scoped topologically by the caller-supplied parent;
	// engine code resolves `parentSHA` via [Store.ParentSHA]
	// before calling this method.
	//
	// The `policy_version_id` filter is REQUIRED by
	// implementation-plan Stage 5.7 line 556: a finding
	// emitted under policy version `P1` is NOT a prior for
	// the same `(scope, rule)` under policy version `P2`,
	// because the two policies may carry different
	// thresholds, severity defaults, or rule_versions. The
	// engine MUST NOT collapse them: doing so would label a
	// policy-change-driven verdict shift as "code regression",
	// confusing the Insights surface's regression bucket.
	//
	// Returns `ok=false` when no prior finding exists for
	// the tuple at the supplied parent (the [DeltaNew] case).
	// The engine refuses `ScopeID == uuid.Nil` or an empty
	// `ruleID` / empty `parentSHA` up front.
	LatestPriorFinding(ctx context.Context, repoID uuid.UUID, parentSHA string, scopeID uuid.UUID, ruleID string, policyVersionID uuid.UUID) (Finding, bool, error)

	// ListPriorBlockFindings returns the most-recent prior
	// finding per `(repo_id, scope_id, rule_id)` tuple at
	// the topological parent SHA `parentSHA`, filtered to
	// `policy_version_id = policyVersionID` and to rows
	// whose latest state is `severity=block AND
	// delta != resolved`. The engine scans this set
	// against the currently-firing tuples and emits a
	// [DeltaResolved] row for every tuple that was a
	// prior block but is NOT in the current-firing set.
	//
	// As with [Store.LatestPriorFinding], the
	// `policy_version_id` filter is mandatory: a resolved
	// row should reflect a code change between two SHAs
	// EVALUATED UNDER THE SAME POLICY, not a policy switch
	// that retired the rule. The engine refuses an empty
	// `parentSHA` so the SQL path does not partial-match
	// against every other SHA in the store.
	ListPriorBlockFindings(ctx context.Context, repoID uuid.UUID, parentSHA string, policyVersionID uuid.UUID) ([]Finding, error)

	// AppendEvaluation persists `run`, `verdict`, and every
	// entry in `findings` as a SINGLE atomic unit. On any
	// error NONE of the rows land; on success ALL rows
	// land. Production-PG implementations MUST run this
	// inside ONE `BEGIN` / `COMMIT` -- the surrounding
	// [Store.WithEvaluationLock] envelope is responsible
	// for acquiring `pg_advisory_xact_lock(...)`, so this
	// method assumes the (repo, sha) read-modify-write
	// window is already serialised by its caller.
	//
	// An empty `findings` slice is valid -- the gate's
	// clean-pass with no rule firings produces ONE run +
	// ONE verdict + ZERO findings, and the writer MUST
	// accept that as an idempotent insert (no rule says
	// every run must produce a finding).
	//
	// On the PG path, this is the writer-of-record for the
	// three Audit tables under the
	// `clean_code_solid_batch` grant from tech-spec Sec 7.2
	// lines 1256-1261.
	AppendEvaluation(ctx context.Context, run EvaluationRun, verdict EvaluationVerdict, findings []Finding) error

	// WithEvaluationLock serialises the read-modify-write
	// window the engine performs across a single SHA
	// evaluation. The `fn` callback receives a Store
	// scoped to the same lock holder -- on the PG path,
	// `fn`'s Store argument MUST be the transaction-bound
	// store so every read+write inside the closure runs
	// inside ONE `BEGIN; pg_advisory_xact_lock(...); ...; COMMIT;`
	// envelope.
	//
	// Rationale (rubber-duck Stage 5.7 #5): if the lock
	// lived only inside [Store.AppendEvaluation], a
	// concurrent run could overlap our prior-finding
	// snapshot read with a sibling instance's append --
	// emitting `delta=new` against a stale view of the
	// prior set. The WithEvaluationLock envelope closes
	// that window by serialising the entire read+append
	// pair.
	//
	// The in-memory implementation reuses a per-(repo,
	// sha) goroutine-local mutex; the PG implementation
	// uses `pg_advisory_xact_lock(hashtext(repo_id::text
	// || ':' || sha))`. Both share the same observable
	// contract: only one engine run for a given (repo,
	// sha) tuple runs at a time.
	//
	// Implementations MUST refuse a nil `fn`.
	WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(Store) error) error

	// LookupRecentCanonicalRun returns the most recent
	// non-degraded `evaluation_run + evaluation_verdict +
	// findings` triple for the `(repoID, sha,
	// policyVersionID, caller, scopeID)` tuple whose
	// `evaluation_run.created_at` is within `since` of
	// `now()`. When `since == 0` the implementation
	// disables the recency filter (returns the most
	// recent non-degraded row regardless of age).
	//
	// SCOPE-AWARE LOOKUP (Stage 5.7 evaluator iter-6
	// feedback #2): the lookup MUST filter on the new
	// `evaluation_run.scope_id` column added by migration
	// 0008. A `nil` `scopeID` matches rows with
	// `scope_id IS NULL` (the whole-SHA evaluation: every
	// `batch_refresh` by construction and the `eval.gate`
	// happy path when no scope was supplied); a non-nil
	// `scopeID` matches rows whose `scope_id = $5`
	// exactly. Implementations MUST use the null-safe
	// `IS NOT DISTINCT FROM` operator (or its Go
	// equivalent) so the two cases share a single code
	// path. With this change the eval_gate cross-replica
	// dedup is now schema-safe; the engine consults this
	// method for BOTH `CallerBatchRefresh` AND
	// `CallerEvalGate`.
	//
	// DEGRADED-VERDICT FILTER (rubber-duck blocker #2):
	// implementations MUST JOIN `evaluation_verdict` and
	// filter `degraded = false`, so a prior degraded
	// short-circuit (signature-invalid / samples-pending)
	// is NEVER returned as the canonical row. A degraded
	// row carries an empty findings set and would
	// mistakenly suppress a real evaluation.
	//
	// Returns `ok=false` when no matching row exists --
	// the engine proceeds with the full evaluation path
	// in that case. A non-nil error indicates a store IO
	// failure; the engine logs and falls back to the full
	// evaluation path (NOT a fatal error: dedup is an
	// optimisation, not a correctness boundary).
	//
	// Production (txStore) MUST execute this lookup
	// INSIDE the `pg_advisory_xact_lock` envelope so a
	// parallel replica that has already committed its
	// canonical row under the lock is observed by the
	// SECOND caller's RC-isolated SELECT. The
	// implementation queries `evaluation_run` joined with
	// `evaluation_verdict`, then fetches the row's
	// findings (ordered by `finding_id` for determinism).
	LookupRecentCanonicalRun(ctx context.Context, repoID uuid.UUID, sha string, policyVersionID uuid.UUID, caller Caller, scopeID *uuid.UUID, since time.Duration) (RunResult, bool, error)
}
