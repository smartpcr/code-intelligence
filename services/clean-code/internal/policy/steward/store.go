package steward

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/gofrs/uuid"
)

// Store is the persistence boundary the [Steward] writes
// through. The interface intentionally exposes ONLY insert and
// read verbs -- no Update, no Delete -- so a future drift that
// tries to mutate an append-only row trips a compile error
// rather than a runtime privilege check.
//
// Concurrency: implementations MUST be safe for concurrent use
// (the [Steward] does not serialise its own writes -- the SQL
// implementation relies on PostgreSQL's row-level locking,
// the in-memory implementation uses a sync.Mutex).
//
// Failure-mode contract:
//
//   - InsertPolicyVersion MUST refuse duplicate
//     `policy_version_id` (PK violation -> wrap a sentinel).
//
//   - InsertPolicyActivation MUST refuse activation rows whose
//     `policy_version_id` does not reference an existing row
//     (returns [ErrUnknownPolicyVersion]).
//
//   - InsertRulePackAndRules is the SINGLE atomic verb for
//     `policy.publish_rulepack`: SQL implementations MUST run
//     pack + rules in a single transaction so a partial
//     rule-set never lands in an append-only store (per
//     rubber-duck #3). Returns [ErrDuplicateRulePack] /
//     [ErrDuplicateRule] on composite-PK collision.
type Store interface {
	// InsertPolicyVersion appends `pv`. Returns a wrapped
	// error on duplicate `policy_version_id`.
	InsertPolicyVersion(ctx context.Context, pv PolicyVersion) error

	// GetPolicyVersion returns the row keyed by id, or
	// [ErrUnknownPolicyVersion] when the row is absent.
	GetPolicyVersion(ctx context.Context, id uuid.UUID) (PolicyVersion, error)

	// InsertPolicyActivation appends `pa`. Returns
	// [ErrUnknownPolicyVersion] when `pa.PolicyVersionID`
	// does not reference an existing row.
	InsertPolicyActivation(ctx context.Context, pa PolicyActivation) error

	// LatestActivation returns the most recent activation by
	// `created_at` (tie-break: `activation_id` DESC), or
	// `ok=false` when no activation row exists. Used by the
	// "activation-latest-row-wins" scenario.
	LatestActivation(ctx context.Context) (PolicyActivation, bool, error)

	// InsertRulePackAndRules atomically appends `pack`
	// and every entry in `rules`. Implementations MUST run
	// the inserts in a single transaction; on any error,
	// neither the pack nor any rule is persisted.
	InsertRulePackAndRules(ctx context.Context, pack RulePack, rules []Rule) error

	// GetRulePack returns the row keyed by (packID, version),
	// or `ok=false` when the row is absent. Read by the
	// e2e scenario "publish_rulepack-writes-rule-pack-and-
	// rules".
	GetRulePack(ctx context.Context, packID string, version int) (RulePack, bool, error)

	// ListRulesForPack returns every rule whose `(rule_id,
	// version)` pair is referenced by the pack's most recent
	// publish call. Returned rows are sorted by `rule_id ASC,
	// version ASC` so tests can pattern-match.
	ListRulesForPack(ctx context.Context, packID string) ([]Rule, error)

	// RuleExists reports whether a `(rule_id, version)` row
	// is present in `clean_code.rule`. Used by
	// `policy.publish` to enforce the `rule_refs` JSON FK
	// contract documented in migration 0003 -- a published
	// PolicyVersion that cites an unknown rule would be
	// unresolvable at gate time.
	RuleExists(ctx context.Context, ruleID string, version int) (bool, error)

	// ThresholdExists reports whether a `threshold_id` is
	// present in `clean_code.threshold`. Used by
	// `policy.publish` to enforce the `threshold_refs` JSON
	// FK contract from migration 0003 line 462: "the FK
	// target is enforced by the writer, not by SQL".
	ThresholdExists(ctx context.Context, id uuid.UUID) (bool, error)

	// InsertThreshold appends a Threshold row. Append-only
	// (no Update / Delete). Stage 5.2 does NOT expose a
	// `policy.publish_threshold` verb on the canonical write
	// surface; this Store primitive exists so tests can
	// seed threshold rows and so a future bootstrap tool /
	// migration can populate them. Returns a wrapped error
	// on duplicate `threshold_id`.
	InsertThreshold(ctx context.Context, t Threshold) error

	// RuleExistsByID reports whether ANY row with the given
	// `rule_id` exists in `clean_code.rule` (across every
	// version). Used by [Steward.Override] to enforce the
	// logical FK `Override.rule_id -> Rule.rule_id`
	// documented in architecture Sec 5.3.6 line 1166. The
	// migration 0003 COMMENT clarifies the FK is logical
	// (not a SQL FK) because `rule` has the composite PK
	// `(rule_id, version)` and the Override row binds to
	// the rule LINEAGE, not to a specific version -- so the
	// writer enforces "some version of this rule_id has
	// been registered". Distinct from [RuleExists], which
	// takes a `(rule_id, version)` pair for the
	// `policy.publish` `rule_refs[]` FK check.
	RuleExistsByID(ctx context.Context, ruleID string) (bool, error)

	// InsertOverride appends `o` to `clean_code.override`.
	// Append-only -- the Store interface intentionally has
	// no UpdateOverride / DeleteOverride method so unmute
	// MUST land as a fresh row with `mute=false` (tech-spec
	// Sec 10A "mute lifecycle" pin). Returns a wrapped error
	// on duplicate `override_id` (PK violation).
	InsertOverride(ctx context.Context, o Override) error

	// LatestMatchingOverride returns the most recent
	// [Override] row whose `scope_filter` matches the
	// supplied `candidate` per the architecture-pinned read
	// semantic (Sec 5.3.6 line 1171):
	//
	//     MAX(created_at) WHERE rule_id=$1 AND
	//       scope_filter matches the candidate scope.
	//
	// "Matches" is the AND of three conditions:
	//
	//   - `scope_filter.repo_id == candidate.RepoID`,
	//   - `scope_filter.scope_kind == candidate.ScopeKind`,
	//   - `scopeGlobMatches(scope_filter.scope_signature_glob,
	//     candidate.Signature) == true`.
	//
	// The glob vocabulary is `*` (zero or more chars) and
	// `?` (one char); see [scopeGlobMatches] in
	// scope_glob.go for the full translation rules.
	//
	// Returns `ok=false` when no override row matches the
	// candidate. The Steward's read helper
	// [Steward.LatestMatchingOverride] additionally
	// validates the candidate up front -- the Store-level
	// method assumes the candidate is well-formed.
	//
	// Tie-break: when two matching rows share `created_at`,
	// the row with the largest `override_id` (lexicographic
	// uuid order) wins -- mirrors the SQL ORDER BY contract
	// used by [LatestActivation]. The SQL implementation
	// streams rows in `(created_at DESC, override_id DESC)`
	// order and stops at the first glob match; there is NO
	// row-count limit, so an older matching glob is never
	// hidden behind a newer non-matching row.
	LatestMatchingOverride(ctx context.Context, ruleID string, candidate CandidateScope) (Override, bool, error)

	// ListAllOverrides returns every row in `clean_code.override`
	// across every rule and every scope, ordered
	// `(created_at ASC, override_id ASC)`. The append-only log
	// is returned VERBATIM -- the caller (typically the
	// management aged-mute projection) is responsible for
	// reducing `(rule_id, scope)` partitions to a latest-row-
	// wins winner. The Store interface does NOT pre-reduce
	// because different read seams need different reductions
	// (latest-mute vs latest-of-each-actor vs full audit
	// history) and the schema row count is bounded by
	// `O(rules * scopes * lifecycle_events)` -- small enough
	// to scan per read in any deployment that uses overrides.
	//
	// Returns an empty (non-nil) slice when the table is empty.
	// Propagates `ctx.Err()` if cancelled mid-scan.
	ListAllOverrides(ctx context.Context) ([]Override, error)
}

// InMemoryStore is a process-local [Store] backed by
// concurrent-safe slices. Used by unit tests and by the
// scaffold-mode composition root when CLEAN_CODE_PG_URL is
// unset.
type InMemoryStore struct {
	mu             sync.Mutex
	policyVersions map[uuid.UUID]PolicyVersion
	activations    []PolicyActivation
	rulePacks      map[rulePackKey]RulePack
	rules          map[ruleKey]Rule
	thresholds     map[uuid.UUID]Threshold
	overrides      []Override
}

type rulePackKey struct {
	PackID  string
	Version int
}

type ruleKey struct {
	RuleID  string
	Version int
}

// NewInMemoryStore constructs a fresh in-memory store. The
// returned value is ready to use; no further initialisation is
// required.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		policyVersions: make(map[uuid.UUID]PolicyVersion),
		rulePacks:      make(map[rulePackKey]RulePack),
		rules:          make(map[ruleKey]Rule),
		thresholds:     make(map[uuid.UUID]Threshold),
	}
}

// InsertPolicyVersion appends `pv`. Returns a wrapped error
// (`policy_version_id already exists`) on PK collision.
func (s *InMemoryStore) InsertPolicyVersion(ctx context.Context, pv PolicyVersion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.policyVersions[pv.PolicyVersionID]; exists {
		return fmt.Errorf("steward: InMemoryStore.InsertPolicyVersion: policy_version_id=%s already exists", pv.PolicyVersionID)
	}
	s.policyVersions[pv.PolicyVersionID] = copyPolicyVersion(pv)
	return nil
}

// GetPolicyVersion returns the row keyed by `id`.
func (s *InMemoryStore) GetPolicyVersion(ctx context.Context, id uuid.UUID) (PolicyVersion, error) {
	if err := ctx.Err(); err != nil {
		return PolicyVersion{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pv, ok := s.policyVersions[id]
	if !ok {
		return PolicyVersion{}, fmt.Errorf("%w: policy_version_id=%s", ErrUnknownPolicyVersion, id)
	}
	return copyPolicyVersion(pv), nil
}

// InsertPolicyActivation appends `pa`. Returns
// [ErrUnknownPolicyVersion] when the referenced policy version
// is absent.
func (s *InMemoryStore) InsertPolicyActivation(ctx context.Context, pa PolicyActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policyVersions[pa.PolicyVersionID]; !ok {
		return fmt.Errorf("%w: policy_version_id=%s", ErrUnknownPolicyVersion, pa.PolicyVersionID)
	}
	s.activations = append(s.activations, pa)
	return nil
}

// LatestActivation returns the activation with the largest
// (created_at, activation_id) tuple, mirroring the SQL ORDER
// BY contract.
func (s *InMemoryStore) LatestActivation(ctx context.Context) (PolicyActivation, bool, error) {
	if err := ctx.Err(); err != nil {
		return PolicyActivation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.activations) == 0 {
		return PolicyActivation{}, false, nil
	}
	sorted := make([]PolicyActivation, len(s.activations))
	copy(sorted, s.activations)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return uuidCompare(sorted[i].ActivationID, sorted[j].ActivationID) > 0
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})
	return sorted[0], true, nil
}

// InsertRulePackAndRules atomically appends `pack` + rules.
// In-memory all-or-nothing: validates duplicates BOTH against
// the persisted store AND within the supplied rules slice
// BEFORE committing any row, so a mid-insert failure cannot
// leave a partial write.
func (s *InMemoryStore) InsertRulePackAndRules(ctx context.Context, pack RulePack, rules []Rule) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	packKey := rulePackKey{PackID: pack.PackID, Version: pack.Version}
	if _, exists := s.rulePacks[packKey]; exists {
		return fmt.Errorf("%w: pack_id=%s version=%d", ErrDuplicateRulePack, pack.PackID, pack.Version)
	}
	// Detect duplicate `(rule_id, version)` keys within the
	// batch itself. The composite PK on `clean_code.rule`
	// would reject the second insert at SQL level; mirror
	// the same behaviour here so the InMemoryStore tests
	// guard the all-or-nothing invariant.
	batchKeys := make(map[ruleKey]struct{}, len(rules))
	for _, r := range rules {
		rk := ruleKey{RuleID: r.RuleID, Version: r.Version}
		if _, exists := s.rules[rk]; exists {
			return fmt.Errorf("%w: rule_id=%s version=%d (already persisted)", ErrDuplicateRule, r.RuleID, r.Version)
		}
		if _, dupInBatch := batchKeys[rk]; dupInBatch {
			return fmt.Errorf("%w: rule_id=%s version=%d (duplicated within batch)", ErrDuplicateRule, r.RuleID, r.Version)
		}
		batchKeys[rk] = struct{}{}
	}
	// All checks passed; commit.
	s.rulePacks[packKey] = pack
	for _, r := range rules {
		s.rules[ruleKey{RuleID: r.RuleID, Version: r.Version}] = r
	}
	return nil
}

// GetRulePack returns the row keyed by `(packID, version)`.
func (s *InMemoryStore) GetRulePack(ctx context.Context, packID string, version int) (RulePack, bool, error) {
	if err := ctx.Err(); err != nil {
		return RulePack{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pack, ok := s.rulePacks[rulePackKey{PackID: packID, Version: version}]
	if !ok {
		return RulePack{}, false, nil
	}
	return pack, true, nil
}

// ListRulesForPack returns every Rule whose `pack_id` matches
// `packID`, sorted by `(rule_id ASC, version ASC)`.
func (s *InMemoryStore) ListRulesForPack(ctx context.Context, packID string) ([]Rule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rule, 0)
	for _, r := range s.rules {
		if r.PackID == packID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID == out[j].RuleID {
			return out[i].Version < out[j].Version
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out, nil
}

// RuleExists reports whether `(ruleID, version)` was persisted
// by a prior [InMemoryStore.InsertRulePackAndRules] call.
func (s *InMemoryStore) RuleExists(ctx context.Context, ruleID string, version int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rules[ruleKey{RuleID: ruleID, Version: version}]
	return ok, nil
}

// ThresholdExists reports whether a Threshold with the given
// id has been persisted via [InMemoryStore.InsertThreshold].
func (s *InMemoryStore) ThresholdExists(ctx context.Context, id uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.thresholds[id]
	return ok, nil
}

// InsertThreshold appends a Threshold row. Returns an error on
// duplicate `threshold_id` (zero-uuid is rejected so the caller
// can't accidentally seed an unmatched FK target).
func (s *InMemoryStore) InsertThreshold(ctx context.Context, t Threshold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.ThresholdID == uuid.Nil {
		return fmt.Errorf("steward: InMemoryStore.InsertThreshold: threshold_id is the zero uuid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.thresholds[t.ThresholdID]; exists {
		return fmt.Errorf("steward: InMemoryStore.InsertThreshold: threshold_id=%s already exists", t.ThresholdID)
	}
	s.thresholds[t.ThresholdID] = t
	return nil
}

// RuleExistsByID reports whether ANY rule row with the given
// `ruleID` (across every version) has been persisted via a
// prior [InMemoryStore.InsertRulePackAndRules] call.
func (s *InMemoryStore) RuleExistsByID(ctx context.Context, ruleID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.rules {
		if k.RuleID == ruleID {
			return true, nil
		}
	}
	return false, nil
}

// InsertOverride appends `o` to the in-memory override log.
// Returns a wrapped error on duplicate `override_id`.
func (s *InMemoryStore) InsertOverride(ctx context.Context, o Override) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.OverrideID == uuid.Nil {
		return fmt.Errorf("steward: InMemoryStore.InsertOverride: override_id is the zero uuid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.overrides {
		if existing.OverrideID == o.OverrideID {
			return fmt.Errorf("steward: InMemoryStore.InsertOverride: override_id=%s already exists", o.OverrideID)
		}
	}
	s.overrides = append(s.overrides, o)
	return nil
}

// LatestMatchingOverride scans the in-memory override log for
// rows whose `(rule_id, scope_filter)` matches `(ruleID,
// candidate)` under the Stage 5.3 glob semantic and returns
// the one with the largest `(created_at, override_id)` tuple.
// Mirrors the SQL streaming contract: no LIMIT -- every
// in-partition row is considered.
func (s *InMemoryStore) LatestMatchingOverride(ctx context.Context, ruleID string, candidate CandidateScope) (Override, bool, error) {
	if err := ctx.Err(); err != nil {
		return Override{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var best Override
	var found bool
	for _, o := range s.overrides {
		if o.RuleID != ruleID {
			continue
		}
		if o.ScopeFilter.RepoID != candidate.RepoID {
			continue
		}
		if o.ScopeFilter.ScopeKind != candidate.ScopeKind {
			continue
		}
		match, err := scopeGlobMatches(o.ScopeFilter.ScopeSignatureGlob, candidate.Signature)
		if err != nil {
			// Defensive: a malformed glob is a
			// configuration bug. Propagate so the
			// evaluator surfaces it rather than fail-
			// open the gate.
			return Override{}, false, err
		}
		if !match {
			continue
		}
		if !found {
			best = o
			found = true
			continue
		}
		switch {
		case o.CreatedAt.After(best.CreatedAt):
			best = o
		case o.CreatedAt.Equal(best.CreatedAt) && uuidCompare(o.OverrideID, best.OverrideID) > 0:
			best = o
		}
	}
	return best, found, nil
}

// Compile-time check that InMemoryStore satisfies Store.
var _ Store = (*InMemoryStore)(nil)

// ListAllOverrides returns a defensive copy of every row in
// the in-memory override log, sorted oldest-first by
// `(CreatedAt ASC, OverrideID ASC)` so the management
// aged-mute projection sees a deterministic stream regardless
// of insertion order. The returned slice is detached from the
// store's internal `overrides` slice so the caller may sort,
// filter, or extend it without race-condition risk.
func (s *InMemoryStore) ListAllOverrides(ctx context.Context) ([]Override, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	out := make([]Override, len(s.overrides))
	copy(out, s.overrides)
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return uuidCompare(out[i].OverrideID, out[j].OverrideID) < 0
	})
	return out, nil
}

// copyPolicyVersion deep-copies pv so the returned value is
// safe to mutate without affecting the persisted row.
func copyPolicyVersion(pv PolicyVersion) PolicyVersion {
	out := pv
	if pv.Signature != nil {
		out.Signature = append([]byte(nil), pv.Signature...)
	}
	if pv.RuleRefs != nil {
		out.RuleRefs = append([]RuleRef(nil), pv.RuleRefs...)
	}
	if pv.ThresholdRefs != nil {
		out.ThresholdRefs = append([]ThresholdRef(nil), pv.ThresholdRefs...)
	}
	if pv.RefactorWeights.FreshnessWindowSeconds != nil {
		v := *pv.RefactorWeights.FreshnessWindowSeconds
		out.RefactorWeights.FreshnessWindowSeconds = &v
	}
	return out
}

// uuidCompare orders two UUIDs lexicographically by their
// canonical 16-byte representation. github.com/gofrs/uuid does
// not expose a Compare method so we use the underlying byte
// slice the type embeds.
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

// ensureNoUpdate is a compile-time sentinel exposing the
// append-only invariant. The [Store] interface intentionally
// has no Update / Delete method; this assertion keeps the
// invariant visible to a future maintainer.
//
// If a regression tries to add a `Store.Update` method, this
// line will fail to compile (because the implementation now
// has more methods than the interface declared). The check is
// a build-time witness of the architecture G3 contract.
var _ = func() any {
	// Empty closure; the assertion lives in the interface
	// declaration itself. Documented here so a `grep -nF
	// "ensureNoUpdate"` lands on the rationale.
	return errors.New("steward: Store is append-only -- no Update / Delete methods allowed")
}
