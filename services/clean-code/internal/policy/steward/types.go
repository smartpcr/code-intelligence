// -----------------------------------------------------------------------
// <copyright file="types.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package steward

import (
	"errors"
	"strings"
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

	// TopN is the Stage 8.2 refactor-plan truncation knob: the
	// `RefactorPlanner` reads ALL `hot_spot` rows it has just
	// scored at this SHA, then truncates the per-plan
	// `hotspot_ids` JSONB array (and the per-hotspot task
	// emission) to the top-`TopN` rows by composite score
	// (architecture Sec 5.5.2 + implementation-plan Stage 8.2
	// line 732 "reading the top-N hotspots per repo
	// (N from `policy_version.refactor_weights.top_n`)").
	//
	// Semantics:
	//
	//   - `TopN > 0` -- truncate to that many highest-scoring
	//     hot_spots when assembling the plan. Hot_spot rows
	//     beyond the cut are still PERSISTED (the planner is
	//     the sole writer of `hot_spot`; truncating storage
	//     would lose audit signal) but are NOT referenced from
	//     `refactor_plan.hotspot_ids` and DO NOT produce
	//     `refactor_task` rows.
	//
	//   - `TopN == 0` -- "operator did not configure a
	//     truncation knob"; the planner emits a plan covering
	//     ALL scored hot_spots and tasks for each one (no
	//     truncation). Default for legacy `refactor_weights`
	//     blobs that were authored before this field existed.
	//
	//   - `TopN < 0` -- rejected by `validatePublishRequest`
	//     in `steward.go` so a misconfigured policy fails at
	//     publish time rather than silently behaving like
	//     "no truncation".
	//
	// The field carries the `omitempty` tag so the canonical
	// JSON for pre-Stage-8.2 policies (which all default to
	// `TopN == 0`) matches the existing signed bytes and the
	// signature verification stays stable on rollout.
	TopN int `json:"top_n,omitempty"`
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

// ScopeKind is the closed set of scope kinds an Override's
// `scope_filter.scope_kind` may carry. Matches the DB ENUM
// `clean_code.scope_kind` declared in migration 0002 line 142
// verbatim (`repo`, `package`, `file`, `class`, `interface`,
// `method`, `block`).
type ScopeKind string

// Canonical scope kinds. Pinned to the seven values in
// architecture Sec 5.2.1 line 1046 + migration 0002. New
// values MUST land via a coordinated migration + this constant
// list -- a free-form text scope_kind would defeat the
// evaluator's switch-on-kind dispatch.
const (
	ScopeKindRepo      ScopeKind = "repo"
	ScopeKindPackage   ScopeKind = "package"
	ScopeKindFile      ScopeKind = "file"
	ScopeKindClass     ScopeKind = "class"
	ScopeKindInterface ScopeKind = "interface"
	ScopeKindMethod    ScopeKind = "method"
	ScopeKindBlock     ScopeKind = "block"
)

// IsValid reports whether k is a member of the closed scope
// kind set declared in architecture Sec 5.2.1 line 1046.
func (k ScopeKind) IsValid() bool {
	switch k {
	case ScopeKindRepo, ScopeKindPackage, ScopeKindFile,
		ScopeKindClass, ScopeKindInterface, ScopeKindMethod, ScopeKindBlock:
		return true
	default:
		return false
	}
}

// ScopeFilter is the in-memory mirror of an `override.scope_filter`
// JSONB row's `{repo_id, scope_kind, scope_signature_glob}`
// shape per architecture Sec 5.3.6 line 1167. The Stage 5.3
// implementation pins all three fields as REQUIRED for v1 (no
// "global mute" semantics; the rubber-duck critique calls out
// that an ambiguous `{}` filter would let an operator silently
// mute every rule everywhere -- the canonical wildcard for
// "every scope of this kind in this repo" is the glob `"*"`).
//
// The JSON tags match the architecture-mandated field names
// verbatim so the serialised form survives a grep against the
// spec.
type ScopeFilter struct {
	RepoID             string    `json:"repo_id"`
	ScopeKind          ScopeKind `json:"scope_kind"`
	ScopeSignatureGlob string    `json:"scope_signature_glob"`
}

// IsZero reports whether all three fields are empty. Used by
// the validator to surface a precise error rather than three
// separate "field X is empty" messages.
func (f ScopeFilter) IsZero() bool {
	return f.RepoID == "" && f.ScopeKind == "" && f.ScopeSignatureGlob == ""
}

// CandidateScope is the CONCRETE scope tuple the evaluator
// surface passes to the Policy Steward to look up the active
// mute state per architecture Sec 5.3.6 line 1171:
//
//	MAX(created_at) WHERE rule_id=$1 AND scope_filter matches the candidate scope
//
// Unlike [ScopeFilter] (which carries a GLOB the operator
// registered), CandidateScope carries the LITERAL signature of
// the scope under evaluation -- e.g. the fully-qualified class
// name `com.example.legacy.Foo`, the file path
// `src/internal/foo.go`, or the package coordinate
// `com.example.legacy`. The reader translates "matches" as:
//
//   - scope_filter.repo_id     == candidate.RepoID,
//   - scope_filter.scope_kind  == candidate.ScopeKind,
//   - scopeGlobMatches(scope_filter.scope_signature_glob,
//     candidate.Signature) == true.
//
// CandidateScope is purely an internal parameter type; it has
// no wire form. The `mgmt.override` write verb carries
// `ScopeFilter`, never `CandidateScope`. The evaluator surface
// is the only caller; we omit JSON tags to keep the gate-time
// hot path free of serialisation cost.
type CandidateScope struct {
	RepoID    string
	ScopeKind ScopeKind
	// Signature is the LITERAL scope signature being
	// evaluated -- NOT a glob. The reader compares this
	// string against the stored `scope_signature_glob`
	// via [scopeGlobMatches]. Operators MUST pass the
	// concrete identifier (`com.example.Foo`), not a
	// pattern (`com.example.*`). An empty Signature is
	// invalid -- the gate would otherwise match every
	// stored override whose glob is `*`, masking upstream
	// bugs that forget to compute the candidate's
	// signature.
	Signature string
}

// IsValid reports whether all three CandidateScope fields are
// populated and the ScopeKind is in the canonical seven-value
// set. Used by [Steward.LatestMatchingOverride] to refuse a
// nonsensical read before scanning the store.
func (c CandidateScope) IsValid() bool {
	return strings.TrimSpace(c.RepoID) != "" &&
		c.ScopeKind.IsValid() &&
		strings.TrimSpace(c.Signature) != ""
}

// Override mirrors a row in `clean_code.override` per
// architecture Sec 5.3.6 lines 1160-1170. The row is
// append-only: the latest row by `created_at` for a given
// `(rule_id, scope_filter)` pair defines the current mute
// state (architecture Sec 5.3.6 line 1171, tech-spec Sec 10A
// pin "mute lifecycle"). There is NO `expires_at` column
// (tech-spec Sec 10A pins v1 to latest-row-wins without a
// TTL) and NO `policy_version_id` column (overrides bind to
// rules, not policy versions).
type Override struct {
	OverrideID  uuid.UUID   `json:"override_id"`
	RuleID      string      `json:"rule_id"`
	ScopeFilter ScopeFilter `json:"scope_filter"`
	Mute        bool        `json:"mute"`
	// Reason carries the operator's justification for the
	// mute. The architecture requires it when `mute=true`
	// (Sec 5.3.6 line 1169); the SQL CHECK constraint
	// `override_reason_required_when_muted` enforces it at
	// the database level. The Steward validator rejects
	// whitespace-only reasons up front so a partial-init
	// SQL write never produces a "logically empty" audit
	// reason that nonetheless passes the NULL check.
	Reason string `json:"reason,omitempty"`
	// ActorID is the OIDC subject of the caller (architecture
	// Sec 5.3.6 line 1170). The column name is `actor_id` --
	// NOT `created_by`. The Stage 5.3 brief explicitly pins
	// "NO `created_by` column".
	ActorID   string    `json:"actor_id"`
	CreatedAt time.Time `json:"created_at"`
}

// OverrideRequest is the input shape of the `mgmt.override`
// verb. The Stage 5.3 brief pins the request shape to exactly
// `{scope_filter, rule_id, mute, reason}` plus an
// implicit `actor_id` carried from the OIDC-authenticated
// caller (NOT from the request body, which is operator-
// spoofable).
//
// The verb refuses any caller-supplied `expires_at` field
// (tech-spec Sec 10A "mute lifecycle" pin: v1 has no TTL).
// Rejection happens at the HTTP layer via `DisallowUnknownFields`
// rather than at this struct -- the Steward verb has no
// `expires_at` to bind against, so a future caller that bypasses
// the HTTP wire shape cannot smuggle it in either.
type OverrideRequest struct {
	RuleID      string      `json:"rule_id"`
	ScopeFilter ScopeFilter `json:"scope_filter"`
	Mute        bool        `json:"mute"`
	Reason      string      `json:"reason"`
	// ActorID is filled by the HTTP layer from the
	// `X-OIDC-Subject` header (the trust boundary is the
	// authenticating gateway, not the JSON body). The
	// Steward verb rejects an empty ActorID with
	// [ErrInvalidOverride].
	ActorID string `json:"-"`
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

	// ErrInvalidOverride is returned by [Steward.Override]
	// when the inbound payload fails shape validation
	// (empty rule_id, malformed scope_filter, empty
	// reason when `mute=true`, empty actor_id, etc.). The
	// concrete validation reason is included in the wrapped
	// error so the HTTP layer can echo a precise 400 body
	// without the caller having to grep the source.
	ErrInvalidOverride = errors.New("steward: invalid override request")

	// ErrUnknownRule is returned by [Steward.Override] when
	// the inbound `rule_id` does not reference any persisted
	// rule_id lineage in `clean_code.rule`. Distinct from
	// [ErrUnknownRuleRef] (which is the `policy.publish`
	// rule_refs[i] FK miss); the Override row's logical FK
	// is on `rule_id` ALONE (no `version` column on the
	// row -- overrides bind to the rule lineage, not to a
	// specific rule version).
	ErrUnknownRule = errors.New("steward: rule_id does not reference any persisted rule (Override.rule_id FK)")

	// ErrInvalidCandidateScope is returned by
	// [Steward.LatestMatchingOverride] when the caller's
	// [CandidateScope] is missing a required field (repo_id,
	// scope_kind, or signature) or carries an unknown
	// scope_kind. The gate-time hot path refuses such reads
	// rather than silently returning ok=false, which would
	// mask upstream bugs in the evaluator that fail to
	// compute the candidate's concrete signature before
	// asking "is this scope muted?".
	ErrInvalidCandidateScope = errors.New("steward: invalid candidate scope (repo_id, scope_kind, and signature are all required)")
)
