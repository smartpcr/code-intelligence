package steward

import (
	"errors"
	"time"

	"github.com/gofrs/uuid"
)

// Severity is the closed set of severities a [Rule] may carry
// (architecture Sec 5.3.1 line 1102). Matches the DB ENUM
// `clean_code.rule_severity` declared in migration 0003.
type Severity string

// Canonical severity labels. These string constants MUST match
// the DB ENUM labels declared in 0003_policy_audit_refactor.up.sql
// verbatim; the SQLStore relies on string equality at INSERT.
const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityBlock Severity = "block"
)

// IsValid reports whether s is a member of the closed severity
// set.
func (s Severity) IsValid() bool {
	switch s {
	case SeverityInfo, SeverityWarn, SeverityBlock:
		return true
	default:
		return false
	}
}

// RuleRef is one entry of [PolicyVersion.RuleRefs]: the
// `{rule_id, version}` reference that pins a specific [Rule]
// row this policy composes. Architecture Sec 5.3.3 line 1128.
type RuleRef struct {
	RuleID  string `json:"rule_id"`
	Version int    `json:"version"`
}

// ThresholdRef is one entry of [PolicyVersion.ThresholdRefs]:
// the `{threshold_id}` reference pinning a [Threshold] row's
// numeric value. Architecture Sec 5.3.3 line 1129.
type ThresholdRef struct {
	ThresholdID uuid.UUID `json:"threshold_id"`
}

// RefactorWeights bundles the per-policy refactor-planner
// inputs (architecture Sec 5.3.3 line 1130 + Sec 3.9). The
// shape is exactly the keys the architecture mandates --
// `Alpha`/`Beta`/`Gamma`/`Delta` are the composite-score
// weights, `EffortModelVersion` pins the ML model version,
// `WindowDays` is the commit-window the Metric Ingestor uses
// to materialise `modification_count_in_window`, and
// `FreshnessWindowSeconds` is the Insights stale-percentile
// threshold (optional; defaults to 3600 elsewhere).
//
// The JSON tags match the architecture's exact field names so
// the serialised form survives a verbatim grep against the
// spec.
type RefactorWeights struct {
	Alpha                  float64 `json:"alpha"`
	Beta                   float64 `json:"beta"`
	Gamma                  float64 `json:"gamma"`
	Delta                  float64 `json:"delta"`
	EffortModelVersion     string  `json:"effort_model_version"`
	WindowDays             int     `json:"window_days"`
	FreshnessWindowSeconds *int    `json:"freshness_window_seconds,omitempty"`
}

// PolicyVersion mirrors a row in `clean_code.policy_version`
// per architecture Sec 5.3.3.
type PolicyVersion struct {
	PolicyVersionID uuid.UUID       `json:"policy_version_id"`
	Name            string          `json:"name"`
	RuleRefs        []RuleRef       `json:"rule_refs"`
	ThresholdRefs   []ThresholdRef  `json:"threshold_refs"`
	RefactorWeights RefactorWeights `json:"refactor_weights"`
	Signature       []byte          `json:"signature"`
	CreatedAt       time.Time       `json:"created_at"`
}

// PolicyActivation mirrors a row in
// `clean_code.policy_activation` per architecture Sec 5.3.4.
type PolicyActivation struct {
	ActivationID    uuid.UUID `json:"activation_id"`
	PolicyVersionID uuid.UUID `json:"policy_version_id"`
	ActivatedBy     string    `json:"activated_by"`
	CreatedAt       time.Time `json:"created_at"`
}

// RulePack mirrors a row in `clean_code.rule_pack` per
// architecture Sec 5.3.2. The composite PK is `(pack_id,
// version)`.
type RulePack struct {
	PackID        string    `json:"pack_id"`
	Version       int       `json:"version"`
	DisplayName   string    `json:"display_name"`
	DescriptionMD string    `json:"description_md"`
	CreatedAt     time.Time `json:"created_at"`
}

// Rule mirrors a row in `clean_code.rule` per architecture Sec
// 5.3.1. Composite PK `(rule_id, version)`; `PackID` is the
// logical FK to [RulePack.PackID].
type Rule struct {
	RuleID          string    `json:"rule_id"`
	Version         int       `json:"version"`
	PackID          string    `json:"pack_id"`
	PredicateDSL    string    `json:"predicate_dsl"`
	SeverityDefault Severity  `json:"severity_default"`
	DescriptionMD   string    `json:"description_md"`
	CreatedAt       time.Time `json:"created_at"`
}

// Threshold mirrors a row in `clean_code.threshold` per
// architecture Sec 5.3.5 and migration 0003. PolicyVersion.
// ThresholdRefs entries are application-layer FKs into this
// table -- the SQL schema deliberately stores the references
// inside a JSONB document (not as proper FKs), so the FK is
// enforced by the writer per migration 0003 line 462: "the FK
// target is enforced by the writer, not by SQL, since the
// reference lives inside a JSON document".
//
// Stage 5.2 does NOT expose a `policy.publish_threshold` verb
// (the canonical write surface is exactly the three verbs
// implemented here). Threshold rows are seeded either by
// migration / operator tooling or by tests via the
// [Store.InsertThreshold] primitive; the Steward consumes
// them via [Store.ThresholdExists] when enforcing the
// `threshold_refs` FK contract at `policy.publish` time.
type Threshold struct {
	ThresholdID uuid.UUID `json:"threshold_id"`
	MetricKind  string    `json:"metric_kind"`
	ScopeKind   string    `json:"scope_kind"`
	Op          string    `json:"op"`
	Value       float64   `json:"value"`
	CreatedAt   time.Time `json:"created_at"`
}

// PublishRequest is the input shape of the `policy.publish`
// verb (architecture Sec 6.5).
//
// The verb's signature in Sec 6.5 reads `policy.publish(name,
// rule_refs, threshold_refs, refactor_weights)`. The
// implementation-plan Stage 5.2 entry uses the synonym
// `rulepack_set`; both refer to the same field. We carry the
// canonical four-field shape so a future operator who copies
// the architecture text into a curl payload lands on a verbatim
// match.
type PublishRequest struct {
	Name            string          `json:"name"`
	RuleRefs        []RuleRef       `json:"rule_refs"`
	ThresholdRefs   []ThresholdRef  `json:"threshold_refs"`
	RefactorWeights RefactorWeights `json:"refactor_weights"`
}

// ActivateRequest is the input shape of the `policy.activate`
// verb. Per architecture Sec 5.3.4 + the implementation-plan
// Stage 5.2 brief there is NO `scope` field -- activation is
// global per deployment in v1. The HTTP handler enforces this
// by rejecting unknown fields on the inbound JSON body.
type ActivateRequest struct {
	PolicyVersionID uuid.UUID `json:"policy_version_id"`
	ActivatedBy     string    `json:"activated_by"`
}

// RuleSpec is the per-rule entry of [PublishRulepackRequest.Rules].
// Mirrors [Rule] minus the `pack_id` (inherited from the
// parent pack) and `created_at` (set at insert time).
type RuleSpec struct {
	RuleID          string   `json:"rule_id"`
	Version         int      `json:"version"`
	PredicateDSL    string   `json:"predicate_dsl"`
	SeverityDefault Severity `json:"severity_default"`
	DescriptionMD   string   `json:"description_md"`
}

// PublishRulepackRequest is the input shape of the
// `policy.publish_rulepack` verb (tech-spec Sec 8.5 lines
// 963-970 canonical verb name).
type PublishRulepackRequest struct {
	PackID        string     `json:"pack_id"`
	Version       int        `json:"version"`
	DisplayName   string     `json:"display_name"`
	DescriptionMD string     `json:"description_md"`
	Rules         []RuleSpec `json:"rules"`
}

// Sentinel errors. Defined as exported sentinels so callers
// can branch via `errors.Is` rather than string-matching the
// message.
var (
	// ErrNoActiveSigningKey is returned by any of the three
	// write verbs when the [keys.Manager] has no active key
	// in its cache. Wraps [keys.ErrNoActiveKey] for
	// composition-root error budgeting.
	ErrNoActiveSigningKey = errors.New("steward: refusing to write -- no active signing key (Stage 5.2 brief: all three verbs require a valid signing key)")

	// ErrInvalidRequest is returned by any verb when the
	// inbound payload fails shape validation (empty
	// required field, unknown enum value, etc.). The
	// concrete validation reason is included in the wrapped
	// error.
	ErrInvalidRequest = errors.New("steward: invalid request")

	// ErrUnknownPolicyVersion is returned by `policy.activate`
	// when the supplied `policy_version_id` does not
	// reference an existing [PolicyVersion] row. Application-
	// layer mirror of the SQL FK on `policy_activation`.
	ErrUnknownPolicyVersion = errors.New("steward: policy_version_id does not reference an existing policy version")

	// ErrUnknownRuleRef is returned by `policy.publish` when
	// any entry of `rule_refs` points at a `(rule_id,
	// version)` pair that has not been registered via
	// `policy.publish_rulepack`. Architecture Sec 5.3.3 and
	// migration 0003 pin the rule lineage contract: a
	// PolicyVersion is meaningless if it cites a rule the
	// Rule Engine cannot load. The Steward enforces this
	// "JSON FK" before signing the canonical bytes -- the
	// signature itself would otherwise commit the service to
	// an unresolvable policy.
	ErrUnknownRuleRef = errors.New("steward: rule_refs entry does not reference an existing rule (rule_id, version)")

	// ErrUnknownThresholdRef is returned by `policy.publish`
	// when any entry of `threshold_refs` points at a
	// `threshold_id` that has not been registered in
	// `clean_code.threshold`. Migration 0003 line 462: "the
	// FK target is enforced by the writer, not by SQL". The
	// Steward enforces this before signing for the same
	// reason as [ErrUnknownRuleRef] -- to avoid committing a
	// signed policy that cannot be resolved at gate time.
	ErrUnknownThresholdRef = errors.New("steward: threshold_refs entry does not reference an existing threshold")

	// ErrDuplicateRulePack is returned by Insert when a
	// `(pack_id, version)` row already exists. Mirrors the
	// composite PK on `clean_code.rule_pack`. Wrap by
	// [Store] implementations.
	ErrDuplicateRulePack = errors.New("steward: duplicate rule_pack (pack_id, version)")

	// ErrDuplicateRule is the analogous sentinel for `rule`.
	ErrDuplicateRule = errors.New("steward: duplicate rule (rule_id, version)")

	// ErrUnimplementedVerb is returned by [Registry.Lookup]
	// for any verb name that is NOT in the canonical
	// closed set. The tech-spec Sec 8.5 + architecture Sec
	// 6.5 pin: the only `policy.*` write verbs are
	// `policy.publish`, `policy.activate`, and
	// `policy.publish_rulepack`. Any other name -- including
	// the historical drafts `policy.rulepack.add` /
	// `policy.rulepack.remove` / `policy.override` --
	// returns this sentinel.
	ErrUnimplementedVerb = errors.New("steward: verb is not in the canonical policy.* surface (returns UNIMPLEMENTED per tech-spec Sec 8.5)")
)
