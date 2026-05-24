package steward

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// Signer is the narrow subset of [keys.Manager] the [Steward]
// consumes. Defined here so tests can inject a fake without
// pulling the full Manager dependency tree.
//
// Production wiring passes a real `*keys.Manager`; the unit
// tests in `steward_test.go` use the `*keys.Manager` directly
// against an in-memory KMS+Store.
type Signer interface {
	// Sign produces a signature over payload using the
	// newest active key. Returns the key id used so callers
	// who need to record signing metadata elsewhere can do
	// so. The Stage 5.2 brief uses ONLY the signature bytes
	// (the architecture's PolicyVersion.signature column is
	// bytea); the key_id is logged but not persisted.
	Sign(ctx context.Context, payload []byte) (uuid.UUID, []byte, error)

	// VerifyAny tries every active key and returns the
	// matching key id on success, or [keys.ErrUnknownKey]
	// on miss. Used by tests to sanity-check the
	// just-published signature.
	VerifyAny(ctx context.Context, payload []byte, signature []byte) (uuid.UUID, error)

	// ListActive returns the active key views. The
	// Steward uses `len(ListActive()) > 0` as the "valid
	// signing key" precondition for `policy.activate` and
	// `policy.publish_rulepack` -- both verbs require the
	// signing layer to be wired but produce no signature
	// column of their own (per the Stage 5.2 brief
	// interpretation; see rubber-duck #5 for rationale).
	ListActive(ctx context.Context) ([]keys.ActiveKeyView, error)
}

// Steward is the in-process actor that owns the three Stage
// 5.2 write verbs. Every method:
//
//  1. Validates the inbound request shape.
//
//  2. Refuses when the [Signer] has no active key.
//
//  3. Appends an immutable row to the [Store] (transactionally
//     for `policy.publish_rulepack`).
//
//  4. Returns the persisted row so the HTTP layer can echo
//     it to the caller.
//
// Concurrency: the Steward is safe for concurrent use; it
// delegates persistence-side locking to the [Store] impl.
type Steward struct {
	store  Store
	signer Signer
	clock  func() time.Time
	newID  func() (uuid.UUID, error)
}

// Config wires the Steward dependencies. Required fields:
// [Config.Store]. Optional: [Config.Signer], a clock, and a
// uuid generator.
//
// **Signer is optional**, encoding the Stage 5.3 kill-switch
// contract: `mgmt.override` is the operator's emergency mute
// for a noisy or broken rule and MUST remain operable when
// the signing-key cache is unwired (architecture Sec 4.6 + Sec
// 1.5.1 row 5; runbook "`mgmt.override` write verb" section).
// When `Signer == nil`, the constructor installs a
// [noActiveSigner] null object so the field is never literally
// nil; the Stage 5.2 verbs (Publish, Activate,
// PublishRulepack) then refuse with [ErrNoActiveSigningKey]
// because the null signer reports an empty active-key set --
// which is exactly the contract those verbs already enforce.
// Override bypasses the signing-key precondition entirely and
// works against a null-signer Steward.
//
// Tests inject deterministic shims via the optional fields.
type Config struct {
	Store  Store
	Signer Signer
	// Clock returns the canonical wall-clock time. Defaults
	// to `time.Now` when nil. Tests use a controllable
	// closure to exercise the "latest-row-wins" tie-break.
	Clock func() time.Time
	// UUIDGen mints a fresh uuid. Defaults to `uuid.NewV4`.
	// Tests use a deterministic generator to pin the
	// row id columns in test assertions.
	UUIDGen func() (uuid.UUID, error)
}

// New constructs a Steward. Returns an error only if Store is
// nil. A nil Signer is replaced by a [noActiveSigner] null
// object that reports an empty active-key set so the Stage 5.2
// write verbs surface [ErrNoActiveSigningKey] in scaffold mode
// while `Steward.Override` (Stage 5.3) keeps serving 200 --
// the kill-switch contract pinned by
// [TestSteward_Override_NoSigningKeyAccepted] and the
// composition-root test
// [TestRootMux_ScaffoldModeOverrideMounted_200].
func New(cfg Config) (*Steward, error) {
	if cfg.Store == nil {
		return nil, errors.New("steward: New: Store is required")
	}
	signer := cfg.Signer
	if signer == nil {
		signer = noActiveSigner{}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	newID := cfg.UUIDGen
	if newID == nil {
		newID = uuid.NewV4
	}
	return &Steward{
		store:  cfg.Store,
		signer: signer,
		clock:  clock,
		newID:  newID,
	}, nil
}

// noActiveSigner is the null object the Steward installs when
// the composition root constructs a Steward without a Signer
// (scaffold mode: `CLEAN_CODE_KMS_PROVIDER` is unset). Every
// method behaves as if no signing key is loaded:
//
//   - [Sign] returns [keys.ErrNoActiveKey], which the Publish
//     verb already translates into [ErrNoActiveSigningKey].
//   - [ListActive] returns the empty slice, so the
//     [checkSigningKey] precondition the other Stage 5.2 verbs
//     run surfaces [ErrNoActiveSigningKey] via the `len==0`
//     branch.
//   - [VerifyAny] returns [keys.ErrUnknownKey] so a caller
//     that somehow lands on the verify path during scaffold
//     mode hits a definite "this key is not available" error
//     instead of a nil-pointer panic.
//
// The null object lives in this package (not in
// `cmd/clean-coded`) so the Steward's invariant "`s.signer` is
// never literally nil" stays internal to the steward package
// and no production wiring code can accidentally bypass it.
type noActiveSigner struct{}

func (noActiveSigner) Sign(context.Context, []byte) (uuid.UUID, []byte, error) {
	return uuid.Nil, nil, keys.ErrNoActiveKey
}

func (noActiveSigner) VerifyAny(context.Context, []byte, []byte) (uuid.UUID, error) {
	return uuid.Nil, keys.ErrUnknownKey
}

func (noActiveSigner) ListActive(context.Context) ([]keys.ActiveKeyView, error) {
	return nil, nil
}

// Publish implements the canonical `policy.publish` verb. The
// steps:
//
//  1. Validate the inbound payload (name non-empty,
//     `rule_refs` non-empty, `refactor_weights` self-consistent).
//
//  2. Canonical-JSON-encode `(rule_refs, threshold_refs,
//     refactor_weights)` and call [Signer.Sign].
//
//  3. INSERT the row.
//
//  4. Return the persisted [PolicyVersion].
//
// Per architecture Sec 5.3.3 + tech-spec Sec 8.4, the column
// is signed at publish time AND signature verification is the
// evaluator's responsibility on every `eval.gate` call. The
// Stage 5.2 deliverable here ends at "row inserted with valid
// signature"; the evaluator-side verification lives in a
// later stage.
func (s *Steward) Publish(ctx context.Context, req PublishRequest) (PolicyVersion, error) {
	if err := s.checkSigningKey(ctx); err != nil {
		return PolicyVersion{}, err
	}
	if err := validatePublishRequest(req); err != nil {
		return PolicyVersion{}, err
	}
	// Enforce the application-layer FK contract from
	// migration 0003 BEFORE we spend signing material.
	// Migration 0003 line 462: "the FK target is enforced by
	// the writer, not by SQL, since the reference lives
	// inside a JSON document". Signing first then discovering
	// an unknown ref would either commit the service to an
	// unresolvable policy or burn a signature on a request
	// we're about to reject.
	if err := s.checkRuleRefs(ctx, req.RuleRefs); err != nil {
		return PolicyVersion{}, err
	}
	if err := s.checkThresholdRefs(ctx, req.ThresholdRefs); err != nil {
		return PolicyVersion{}, err
	}
	payload := canonicalSignedPayload{
		RuleRefs:        req.RuleRefs,
		ThresholdRefs:   req.ThresholdRefs,
		RefactorWeights: req.RefactorWeights,
	}
	signed, err := canonicalJSON(payload)
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("steward.Publish: canonical JSON: %w", err)
	}
	_, sig, err := s.signer.Sign(ctx, signed)
	if err != nil {
		// Translate the Stage 5.1 sentinel into our local
		// sentinel so callers can branch on a single
		// `errors.Is(err, ErrNoActiveSigningKey)` check.
		if errors.Is(err, keys.ErrNoActiveKey) {
			return PolicyVersion{}, fmt.Errorf("%w: %v", ErrNoActiveSigningKey, err)
		}
		return PolicyVersion{}, fmt.Errorf("steward.Publish: signing: %w", err)
	}
	id, err := s.newID()
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("steward.Publish: uuid: %w", err)
	}
	pv := PolicyVersion{
		PolicyVersionID: id,
		Name:            req.Name,
		RuleRefs:        req.RuleRefs,
		ThresholdRefs:   req.ThresholdRefs,
		RefactorWeights: req.RefactorWeights,
		Signature:       sig,
		CreatedAt:       s.clock().UTC(),
	}
	if err := s.store.InsertPolicyVersion(ctx, pv); err != nil {
		return PolicyVersion{}, fmt.Errorf("steward.Publish: insert: %w", err)
	}
	return pv, nil
}

// Activate implements the canonical `policy.activate` verb.
// Appends a [PolicyActivation] row -- no `scope` parameter
// (v1 single-tenant pin) and no `deactivated_at` flag on prior
// rows.
func (s *Steward) Activate(ctx context.Context, req ActivateRequest) (PolicyActivation, error) {
	if err := s.checkSigningKey(ctx); err != nil {
		return PolicyActivation{}, err
	}
	if err := validateActivateRequest(req); err != nil {
		return PolicyActivation{}, err
	}
	// Belt-and-braces: the SQL FK on `policy_activation.
	// policy_version_id` rejects unknown values, but the
	// application-layer check ensures the same behaviour
	// against the [InMemoryStore] tests and surfaces a
	// canonical sentinel (rubber-duck #6).
	if _, err := s.store.GetPolicyVersion(ctx, req.PolicyVersionID); err != nil {
		return PolicyActivation{}, err
	}
	id, err := s.newID()
	if err != nil {
		return PolicyActivation{}, fmt.Errorf("steward.Activate: uuid: %w", err)
	}
	pa := PolicyActivation{
		ActivationID:    id,
		PolicyVersionID: req.PolicyVersionID,
		ActivatedBy:     req.ActivatedBy,
		CreatedAt:       s.clock().UTC(),
	}
	if err := s.store.InsertPolicyActivation(ctx, pa); err != nil {
		return PolicyActivation{}, fmt.Errorf("steward.Activate: insert: %w", err)
	}
	return pa, nil
}

// PublishRulepack implements the canonical
// `policy.publish_rulepack` verb. Persists one [RulePack] row
// + len(req.Rules) [Rule] rows in a single transaction (per
// rubber-duck #3).
func (s *Steward) PublishRulepack(ctx context.Context, req PublishRulepackRequest) (RulePack, []Rule, error) {
	if err := s.checkSigningKey(ctx); err != nil {
		return RulePack{}, nil, err
	}
	if err := validatePublishRulepackRequest(req); err != nil {
		return RulePack{}, nil, err
	}
	now := s.clock().UTC()
	pack := RulePack{
		PackID:        req.PackID,
		Version:       req.Version,
		DisplayName:   req.DisplayName,
		DescriptionMD: req.DescriptionMD,
		CreatedAt:     now,
	}
	rules := make([]Rule, len(req.Rules))
	for i, r := range req.Rules {
		rules[i] = Rule{
			RuleID:          r.RuleID,
			Version:         r.Version,
			PackID:          req.PackID,
			PredicateDSL:    r.PredicateDSL,
			SeverityDefault: r.SeverityDefault,
			DescriptionMD:   r.DescriptionMD,
			CreatedAt:       now,
		}
	}
	if err := s.store.InsertRulePackAndRules(ctx, pack, rules); err != nil {
		return RulePack{}, nil, fmt.Errorf("steward.PublishRulepack: insert: %w", err)
	}
	return pack, rules, nil
}

// LatestActivation is a read helper used by the evaluator
// stage and by the steward's own "latest-row-wins" tests.
// Returns `ok=false` when no activation row exists yet.
func (s *Steward) LatestActivation(ctx context.Context) (PolicyActivation, bool, error) {
	return s.store.LatestActivation(ctx)
}

// ActivePolicyVersion resolves the canonical evaluator-pickup
// query: read the latest [PolicyActivation] row, then dereference
// to the [PolicyVersion] it pins. This is the same lookup a
// future `eval.gate` call will perform when it needs to verify
// rules against the currently-active policy.
//
// Returns `(zero, false, nil)` when no activation has been
// recorded yet (a fresh-deploy steady state). Returns
// `(zero, false, error)` when the activation row points at a
// policy version the store can no longer resolve -- this
// shouldn't happen given the SQL FK on `policy_activation.
// policy_version_id`, but the application layer surfaces it
// rather than panicking.
func (s *Steward) ActivePolicyVersion(ctx context.Context) (PolicyVersion, bool, error) {
	pa, ok, err := s.store.LatestActivation(ctx)
	if err != nil {
		return PolicyVersion{}, false, fmt.Errorf("steward.ActivePolicyVersion: latest activation: %w", err)
	}
	if !ok {
		return PolicyVersion{}, false, nil
	}
	pv, err := s.store.GetPolicyVersion(ctx, pa.PolicyVersionID)
	if err != nil {
		return PolicyVersion{}, false, fmt.Errorf("steward.ActivePolicyVersion: resolve %s: %w", pa.PolicyVersionID, err)
	}
	return pv, true, nil
}

// VerifyPolicyVersionSignature canonicalises the signed
// payload from a persisted [PolicyVersion] and delegates to
// [Signer.VerifyAny]. Helper used by integration tests and by
// a future evaluator stage to verify a freshly-read row.
func (s *Steward) VerifyPolicyVersionSignature(ctx context.Context, pv PolicyVersion) error {
	payload := canonicalSignedPayload{
		RuleRefs:        pv.RuleRefs,
		ThresholdRefs:   pv.ThresholdRefs,
		RefactorWeights: pv.RefactorWeights,
	}
	signed, err := canonicalJSON(payload)
	if err != nil {
		return fmt.Errorf("steward: VerifyPolicyVersionSignature: canonical JSON: %w", err)
	}
	if _, err := s.signer.VerifyAny(ctx, signed, pv.Signature); err != nil {
		return fmt.Errorf("steward: VerifyPolicyVersionSignature: %w", err)
	}
	return nil
}

// checkSigningKey is the precondition every write verb runs
// before touching the store. Refuses when the [Signer] has no
// active key in its cache.
func (s *Steward) checkSigningKey(ctx context.Context) error {
	views, err := s.signer.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("%w: ListActive failed: %v", ErrNoActiveSigningKey, err)
	}
	if len(views) == 0 {
		return ErrNoActiveSigningKey
	}
	return nil
}

// checkRuleRefs enforces the rule_refs JSON-FK contract from
// migration 0003 line 280: "The Policy Steward enforces the
// reference at write time." Returns [ErrUnknownRuleRef] wrapped
// with the offending `(rule_id, version)` pair so the HTTP
// layer can surface a precise 400.
func (s *Steward) checkRuleRefs(ctx context.Context, refs []RuleRef) error {
	for i, r := range refs {
		ok, err := s.store.RuleExists(ctx, r.RuleID, r.Version)
		if err != nil {
			return fmt.Errorf("steward.Publish: rule_refs[%d] existence check: %w", i, err)
		}
		if !ok {
			return fmt.Errorf("%w: rule_refs[%d]={rule_id=%q, version=%d}", ErrUnknownRuleRef, i, r.RuleID, r.Version)
		}
	}
	return nil
}

// checkThresholdRefs is the analogous helper for threshold_refs
// (migration 0003 line 462: "the FK target is enforced by the
// writer, not by SQL, since the reference lives inside a JSON
// document").
func (s *Steward) checkThresholdRefs(ctx context.Context, refs []ThresholdRef) error {
	for i, t := range refs {
		ok, err := s.store.ThresholdExists(ctx, t.ThresholdID)
		if err != nil {
			return fmt.Errorf("steward.Publish: threshold_refs[%d] existence check: %w", i, err)
		}
		if !ok {
			return fmt.Errorf("%w: threshold_refs[%d]={threshold_id=%s}", ErrUnknownThresholdRef, i, t.ThresholdID)
		}
	}
	return nil
}

// validatePublishRequest enforces the shape contract on the
// `policy.publish` payload.
func validatePublishRequest(req PublishRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("%w: name must be non-empty", ErrInvalidRequest)
	}
	if len(req.RuleRefs) == 0 {
		return fmt.Errorf("%w: rule_refs must be non-empty (a policy must reference at least one rule)", ErrInvalidRequest)
	}
	ruleSeen := make(map[RuleRef]struct{}, len(req.RuleRefs))
	for i, r := range req.RuleRefs {
		if strings.TrimSpace(r.RuleID) == "" {
			return fmt.Errorf("%w: rule_refs[%d].rule_id is empty", ErrInvalidRequest, i)
		}
		if r.Version <= 0 {
			return fmt.Errorf("%w: rule_refs[%d].version=%d must be > 0", ErrInvalidRequest, i, r.Version)
		}
		if _, dup := ruleSeen[r]; dup {
			return fmt.Errorf("%w: rule_refs[%d]={rule_id=%q, version=%d} duplicated within payload", ErrInvalidRequest, i, r.RuleID, r.Version)
		}
		ruleSeen[r] = struct{}{}
	}
	threshSeen := make(map[uuid.UUID]struct{}, len(req.ThresholdRefs))
	for i, t := range req.ThresholdRefs {
		if t.ThresholdID == uuid.Nil {
			return fmt.Errorf("%w: threshold_refs[%d].threshold_id is the zero uuid", ErrInvalidRequest, i)
		}
		if _, dup := threshSeen[t.ThresholdID]; dup {
			return fmt.Errorf("%w: threshold_refs[%d].threshold_id=%s duplicated within payload", ErrInvalidRequest, i, t.ThresholdID)
		}
		threshSeen[t.ThresholdID] = struct{}{}
	}
	if req.RefactorWeights.WindowDays <= 0 {
		return fmt.Errorf("%w: refactor_weights.window_days=%d must be > 0", ErrInvalidRequest, req.RefactorWeights.WindowDays)
	}
	if strings.TrimSpace(req.RefactorWeights.EffortModelVersion) == "" {
		return fmt.Errorf("%w: refactor_weights.effort_model_version must be non-empty", ErrInvalidRequest)
	}
	if req.RefactorWeights.FreshnessWindowSeconds != nil && *req.RefactorWeights.FreshnessWindowSeconds <= 0 {
		return fmt.Errorf("%w: refactor_weights.freshness_window_seconds=%d must be > 0 when present",
			ErrInvalidRequest, *req.RefactorWeights.FreshnessWindowSeconds)
	}
	return nil
}

// validateActivateRequest enforces the shape contract on the
// `policy.activate` payload.
func validateActivateRequest(req ActivateRequest) error {
	if req.PolicyVersionID == uuid.Nil {
		return fmt.Errorf("%w: policy_version_id must be a non-zero uuid", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.ActivatedBy) == "" {
		return fmt.Errorf("%w: activated_by must be non-empty", ErrInvalidRequest)
	}
	return nil
}

// validatePublishRulepackRequest enforces the shape contract
// on the `policy.publish_rulepack` payload. Also pins the
// architecture Sec 5.3.1 logical FK by requiring each rule's
// `pack_id` (inherited from the parent in [Steward.PublishRulepack])
// to be non-empty.
//
// Duplicate-key check: rejects two entries that share the same
// `(rule_id, version)` tuple at validation time, mirroring the
// `ruleSeen` pattern in [validatePublishRequest]. Without this
// guard, a caller submitting duplicates would otherwise see
// either a [Store.ErrDuplicateRule] from the InMemoryStore or
// a less specific PK-violation rollback from the SQL store --
// both correct but neither pins the offending index. The
// validation-time rejection produces a precise 400 with the
// offending `rules[i]` index.
func validatePublishRulepackRequest(req PublishRulepackRequest) error {
	if strings.TrimSpace(req.PackID) == "" {
		return fmt.Errorf("%w: pack_id must be non-empty", ErrInvalidRequest)
	}
	if req.Version <= 0 {
		return fmt.Errorf("%w: version=%d must be > 0", ErrInvalidRequest, req.Version)
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		return fmt.Errorf("%w: display_name must be non-empty", ErrInvalidRequest)
	}
	if len(req.Rules) == 0 {
		return fmt.Errorf("%w: rules must be non-empty (a rulepack with no rules is meaningless)", ErrInvalidRequest)
	}
	ruleSeen := make(map[RuleRef]struct{}, len(req.Rules))
	for i, r := range req.Rules {
		if strings.TrimSpace(r.RuleID) == "" {
			return fmt.Errorf("%w: rules[%d].rule_id is empty", ErrInvalidRequest, i)
		}
		if r.Version <= 0 {
			return fmt.Errorf("%w: rules[%d].version=%d must be > 0", ErrInvalidRequest, i, r.Version)
		}
		key := RuleRef{RuleID: r.RuleID, Version: r.Version}
		if _, dup := ruleSeen[key]; dup {
			return fmt.Errorf("%w: rules[%d]={rule_id=%q, version=%d} duplicated within payload", ErrInvalidRequest, i, r.RuleID, r.Version)
		}
		ruleSeen[key] = struct{}{}
		if !r.SeverityDefault.IsValid() {
			return fmt.Errorf("%w: rules[%d].severity_default=%q not in {info,warn,block}", ErrInvalidRequest, i, r.SeverityDefault)
		}
		if strings.TrimSpace(r.PredicateDSL) == "" {
			return fmt.Errorf("%w: rules[%d].predicate_dsl is empty", ErrInvalidRequest, i)
		}
	}
	return nil
}
