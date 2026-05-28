package refactor

// Stage 8.2 of the Refactor Planner: assemble a
// `clean_code.refactor_plan` row covering the top-N hot_spots
// at one (repo_id, sha) and emit one or more
// `clean_code.refactor_task` rows per covered hotspot. Per
// architecture Sec 5.5.2 + Sec 5.5.3 the canonical schemas
// pinned here are:
//
//	refactor_plan(plan_id, repo_id, sha, hotspot_ids JSONB,
//	              summary_md, created_at)            -- NO
//	                                                   policy_version_id
//
//	refactor_task(task_id, plan_id, scope_id, kind,
//	              effort_hours DOUBLE, rule_id,
//	              description_md, created_at)        -- NO
//	                                                   status,
//	                                                   NO
//	                                                   expected_metric_delta
//
// The canonical `task.kind` enum is exactly the five-value
// closed set `split_class | extract_method | invert_dependency
// | break_cycle | consolidate_duplication`. The iter-3 alias
// set `extract_function | introduce_interface |
// reduce_inheritance | reduce_coupling | reduce_lcom |
// reduce_duplication` is REJECTED here; [ValidateTaskKind]
// surfaces the rejection via [ErrRejectedTaskKindAlias].
//
// Effort attribution: `effort_hours` is the column the Stage
// 8.3 ML model will populate. Stage 8.2 emits rows with
// `EffortHours == 0.0` as an explicit "unestimated"
// placeholder; the v0 NOT NULL constraint is satisfied. The
// Stage 8.3 follow-up replaces the planner-side default
// effort estimator with the ML model output (see
// implementation-plan Stage 8.3 + the rubber-duck Stage 8.2
// design review finding #8).
//
// Effort-model version inheritance per implementation-plan
// Stage 8.3 line 751 traverses
// `refactor_task -> refactor_plan -> hot_spot.policy_version_id
// -> policy_version.refactor_weights.effort_model_version`
// (no `effort_model_version` column is duplicated on
// `refactor_task` or `refactor_plan`).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Canonical task.kind enum (architecture Sec 5.5.3 line 1274)
// -----------------------------------------------------------------------------

// TaskKind is the canonical five-value closed set of
// `refactor_task.kind` values per architecture Sec 5.5.3
// line 1274 and migration 0003 line 140-146. Adding a sixth
// value requires (a) a new const here, (b) appending it to
// [CanonicalTaskKinds], (c) the catalogue-bump migration
// extending `clean_code.refactor_task_kind`, AND
// (d) updating the architecture row. The migration's CHECK is
// enforced by the ENUM type itself; the application
// validator below is a belt-and-braces guard so a writer that
// SKIPS the SQL path (in-memory composition root, future
// gRPC trampoline) still refuses unknown kinds.
type TaskKind string

const (
	// TaskKindSplitClass is the canonical task for a Single
	// Responsibility violation: split the class along its
	// responsibility boundaries. The default rule_id
	// mapping in [DefaultTaskKindForRule] pins
	// `solid.srp.*` and `solid.isp.*` (interface
	// segregation = split fat interface) to this kind.
	TaskKindSplitClass TaskKind = "split_class"

	// TaskKindExtractMethod is the canonical task for an
	// Open/Closed or Liskov violation: extract the
	// distinguishing behaviour into its own method so the
	// type can be extended without modification. Default
	// fallback when the rule_id mapping has no specific
	// answer.
	TaskKindExtractMethod TaskKind = "extract_method"

	// TaskKindInvertDependency is the canonical task for a
	// Dependency Inversion violation OR a high fan-out /
	// coupling-between-objects hot_spot: invert the
	// dependency through an interface. Default for
	// `solid.dip.*` and the `decoupling.coupling.*` /
	// `decoupling.cbo*` / `decoupling.fan_*` rule families.
	TaskKindInvertDependency TaskKind = "invert_dependency"

	// TaskKindBreakCycle is the canonical task for a
	// dependency cycle member: break the cycle by inverting
	// one edge or extracting a shared abstraction. Default
	// for the `decoupling.cycles*` rule family
	// (architecture Sec 1.5 / decoupling rule pack
	// `decoupling.cycle_member`).
	TaskKindBreakCycle TaskKind = "break_cycle"

	// TaskKindConsolidateDuplication is the canonical task
	// for a high `duplication_ratio` hot_spot: extract the
	// duplicated text into a single canonical
	// implementation and depend on it from each former
	// duplicate site. Default for the
	// `decoupling.duplication*` rule family per the
	// `decoupling/duplication.yaml` brief.
	TaskKindConsolidateDuplication TaskKind = "consolidate_duplication"
)

// CanonicalTaskKinds is the closed five-value set per
// architecture Sec 5.5.3 line 1274. The conformance test in
// [TestCanonicalTaskKinds_AreExactlyTheFiveCanonicalValues]
// asserts this slice matches the migration's ENUM labels in
// order. Callers iterating MUST treat the order as stable.
var CanonicalTaskKinds = []TaskKind{
	TaskKindSplitClass,
	TaskKindExtractMethod,
	TaskKindInvertDependency,
	TaskKindBreakCycle,
	TaskKindConsolidateDuplication,
}

// RejectedTaskKindAliases is the iter-3 alias set the planner
// MUST reject when a caller (rule pack author, future
// composition root, gRPC payload) tries to emit one. Per the
// workstream brief: "the iter 3 alias set `extract_function |
// introduce_interface | reduce_inheritance | reduce_coupling |
// reduce_lcom | reduce_duplication` is REJECTED".
//
// Implemented as a map rather than a slice so the lookup is
// O(1) and so a future addition is a one-line edit.
var RejectedTaskKindAliases = map[TaskKind]struct{}{
	"extract_function":    {},
	"introduce_interface": {},
	"reduce_inheritance":  {},
	"reduce_coupling":     {},
	"reduce_lcom":         {},
	"reduce_duplication":  {},
}

// IsCanonicalTaskKind reports whether k is one of the five
// canonical values. The membership check is order-independent
// (a linear scan of [CanonicalTaskKinds]); the slice is
// fixed-size so the scan is O(5) regardless of input.
func IsCanonicalTaskKind(k TaskKind) bool {
	for _, c := range CanonicalTaskKinds {
		if c == k {
			return true
		}
	}
	return false
}

// IsRejectedTaskKindAlias reports whether k is in the
// known-rejected alias set. Used by [ValidateTaskKind] to
// distinguish a typo from a deliberate iter-3 alias attempt
// in the error message.
func IsRejectedTaskKindAlias(k TaskKind) bool {
	_, ok := RejectedTaskKindAliases[k]
	return ok
}

// ValidateTaskKind returns nil when k is one of the five
// canonical values, [ErrRejectedTaskKindAlias] when k is in
// [RejectedTaskKindAliases] (a deliberate iter-3 drift
// signal), and [ErrUnknownTaskKind] otherwise (a typo or a
// future spec change that has not landed in this build).
// The wrapping error message names the offending kind verbatim
// so the operator gets actionable feedback without grepping
// the source.
func ValidateTaskKind(k TaskKind) error {
	if IsCanonicalTaskKind(k) {
		return nil
	}
	if IsRejectedTaskKindAlias(k) {
		return fmt.Errorf("%w: %q (canonical set: %v)",
			ErrRejectedTaskKindAlias, k, CanonicalTaskKinds)
	}
	return fmt.Errorf("%w: %q (canonical set: %v)",
		ErrUnknownTaskKind, k, CanonicalTaskKinds)
}

// -----------------------------------------------------------------------------
// Default rule_id -> TaskKind mapping
// -----------------------------------------------------------------------------

// DefaultTaskKindForRule maps a `finding.rule_id` to the
// canonical [TaskKind] the planner emits for the motivated
// task. Returns `(kind, true)` when the rule_id matches a
// known family, `(zero, false)` when no mapping exists; the
// caller (typically [TaskPlanner.Plan]) falls back to the
// configured default kind for the unknown case.
//
// The family-level mapping is intentionally prefix-based so
// every concrete rule under a SOLID pack (e.g.
// `solid.srp.lcom4_high`, `solid.srp.interface_width_high`)
// inherits the family's canonical kind without each new rule
// requiring a one-line edit here. The rule-pack briefs that
// drive each mapping:
//
//   - `solid.srp.*` -> `split_class` (Stage 5.5 srp.yaml:
//     "split the class along its responsibility boundaries").
//   - `solid.isp.*` -> `split_class` (Stage 5.5 isp.yaml:
//     "segregate the fat interface" -- splitting the
//     interface is the structural equivalent of splitting a
//     class). `split_class` is the closest fit in the
//     canonical enum; the description_md text on the emitted
//     row preserves the ISP framing.
//   - `solid.ocp.*` -> `extract_method` (Stage 5.5 ocp.yaml:
//     "extract the distinguishing branch into its own
//     extension point").
//   - `solid.lsp.*` -> `extract_method` (Stage 5.5 lsp.yaml:
//     LSP violations typically resolve by extracting an
//     overridden behaviour into a shared base method).
//   - `solid.dip.*` -> `invert_dependency` (Stage 5.5
//     dip.yaml: "invert the dependency through an
//     interface").
//   - `decoupling.cycles` / `decoupling.cycle_member` ->
//     `break_cycle` (Stage 6.3 cycles.yaml).
//   - `decoupling.duplication_*` -> `consolidate_duplication`
//     (Stage 6.4 duplication.yaml).
//   - `decoupling.coupling.*` / `decoupling.cbo_*` /
//     `decoupling.fan_in_*` / `decoupling.fan_out_*` ->
//     `invert_dependency` (Stage 6.3 coupling.yaml: high
//     coupling is resolved by inverting the dependency
//     through a port).
//
// A rule_id that does not match any of the above returns
// `("", false)`. The caller decides whether to skip the
// hotspot, fall back to a configured default, or surface an
// error.
func DefaultTaskKindForRule(ruleID string) (TaskKind, bool) {
	switch {
	case strings.HasPrefix(ruleID, "solid.srp."), ruleID == "solid.srp":
		return TaskKindSplitClass, true
	case strings.HasPrefix(ruleID, "solid.isp."), ruleID == "solid.isp":
		return TaskKindSplitClass, true
	case strings.HasPrefix(ruleID, "solid.ocp."), ruleID == "solid.ocp":
		return TaskKindExtractMethod, true
	case strings.HasPrefix(ruleID, "solid.lsp."), ruleID == "solid.lsp":
		return TaskKindExtractMethod, true
	case strings.HasPrefix(ruleID, "solid.dip."), ruleID == "solid.dip":
		return TaskKindInvertDependency, true
	case ruleID == "decoupling.cycle_member",
		strings.HasPrefix(ruleID, "decoupling.cycles."),
		ruleID == "decoupling.cycles":
		return TaskKindBreakCycle, true
	case strings.HasPrefix(ruleID, "decoupling.duplication"):
		return TaskKindConsolidateDuplication, true
	case strings.HasPrefix(ruleID, "decoupling.coupling."),
		ruleID == "decoupling.coupling",
		strings.HasPrefix(ruleID, "decoupling.cbo"),
		strings.HasPrefix(ruleID, "decoupling.fan_in"),
		strings.HasPrefix(ruleID, "decoupling.fan_out"):
		return TaskKindInvertDependency, true
	}
	return "", false
}

// -----------------------------------------------------------------------------
// Row shapes -- refactor_plan + refactor_task
// -----------------------------------------------------------------------------

// RefactorPlan mirrors the canonical `clean_code.refactor_plan`
// row per architecture Sec 5.5.2 lines 1256-1265 and migration
// 0003 lines 802-819. There is NO `policy_version_id` column
// (rubber-duck pinning: "policy attribution lives on each
// `hot_spot` row referenced by this plan's `hotspot_ids`
// JSON array"); recovery of the policy version traverses
// `refactor_plan.hotspot_ids[0] -> hot_spot.policy_version_id`.
type RefactorPlan struct {
	// PlanID is the row PK. Minted by [TaskPlanner.Plan] via
	// the configured ID factory.
	PlanID uuid.UUID

	// RepoID FK -> `clean_code.repo.repo_id`.
	RepoID uuid.UUID

	// SHA is the commit SHA the plan was scored against.
	// Equal to the SHA of every HotSpot row referenced by
	// [HotspotIDs].
	SHA string

	// HotspotIDs is the JSONB array of
	// `HotSpot.hotspot_id` values covered by this plan
	// (architecture Sec 5.5.2 line 1263). Populated from
	// the top-`Weights.TopN` hot_spots by composite score.
	// The slice order matches the score-DESC ordering so a
	// future consumer that iterates the array sees the
	// highest-priority hot_spot first.
	HotspotIDs []uuid.UUID

	// SummaryMD is the per-plan markdown summary
	// (architecture Sec 5.5.2 line 1264). Stage 8.2 emits a
	// minimal default produced by [defaultSummaryMD]; a
	// future LLM-explainer can overwrite via the
	// [WithSummaryFunc] option.
	SummaryMD string

	// CreatedAt is the row's `created_at timestamptz`.
	// Shared with every emitted [RefactorTask.CreatedAt] in
	// the same plan (single clock snapshot per
	// [TaskPlanner.Plan] call).
	CreatedAt time.Time
}

// RefactorTask mirrors the canonical `clean_code.refactor_task`
// row per architecture Sec 5.5.3 lines 1267-1278 and migration
// 0003 lines 840-864. Carries NO `status` column (life-cycle
// state is in the agent-memory `Node` graph, not on the
// immutable refactor row) and NO `expected_metric_delta`
// column (rejected by iter-3 evaluator drift item 7).
type RefactorTask struct {
	// TaskID is the row PK. Minted by [TaskPlanner.Plan].
	TaskID uuid.UUID

	// PlanID FK -> `clean_code.refactor_plan.plan_id`.
	// Equal to the PK of the [RefactorPlan] this task
	// belongs to.
	PlanID uuid.UUID

	// ScopeID is the logical FK to
	// `scope_binding.scope_id` -- the scope this task
	// targets. Inherited from the [HotSpot] row's
	// `scope_id` so the task references the same code
	// location the score was driven by.
	ScopeID uuid.UUID

	// Kind is the canonical task category. MUST be in
	// [CanonicalTaskKinds]; emission is refused (the whole
	// batch is rejected, no row lands) when any task in the
	// batch carries a non-canonical or rejected-alias kind
	// per architecture Sec 5.5.3 + the workstream brief.
	Kind TaskKind

	// EffortHours is the estimated effort produced by the
	// Stage 8.3 ML model pinned at
	// `PolicyVersion.refactor_weights.effort_model_version`.
	// Stage 8.2 emits an explicit `0.0` placeholder for
	// every task; the v0 NOT NULL constraint is satisfied
	// and Stage 8.3 will replace the placeholder with real
	// estimates. Consumers MUST treat `EffortHours == 0`
	// as "unestimated" rather than "free".
	EffortHours float64

	// RuleID is the `finding.rule_id` that motivated this
	// task -- the logical FK to `rule.rule_id`
	// (architecture Sec 5.5.3 line 1276 "The rule that
	// motivated the task."). Required (NOT NULL); a hot_spot
	// with no qualifying findings yields ZERO tasks rather
	// than fabricating a synthetic rule_id (rubber-duck
	// Stage 8.2 design review finding #2).
	RuleID string

	// DescriptionMD is the human-readable description slot
	// (architecture Sec 5.5.3 line 1277). Stage 8.2 emits a
	// minimal default produced by
	// [defaultTaskDescriptionMD]; a future LLM-explainer
	// can overwrite via the [WithTaskDescriptionFunc]
	// option.
	DescriptionMD string

	// CreatedAt mirrors the plan's `created_at` so all rows
	// in one TaskPlanner.Plan invocation share a single
	// clock reading.
	CreatedAt time.Time
}

// PlanAndTasksResult bundles the Stage 8.2 emission for the
// caller. Returned by [TaskPlanner.Plan].
type PlanAndTasksResult struct {
	// PolicyVersionID mirrors the [PlanResult.PolicyVersionID]
	// stamped on every emitted hot_spot.
	PolicyVersionID uuid.UUID

	// HotSpots is the FULL batch the planner scored at this
	// (repo_id, sha) -- ALL rows are persisted to
	// `hot_spot` even when [Snapshot.Weights.TopN] truncates
	// the plan to a subset (architecture Sec 5.5.1
	// append-only: truncating the hot_spot write would lose
	// audit signal). In canonical sort order (Score DESC,
	// ScopeID ASC).
	HotSpots []HotSpot

	// Plan is the persisted [RefactorPlan] row OR a zero
	// value when no plan was written (empty input, no
	// hot_spots survived top-N). Callers check
	// `Plan.PlanID == uuid.Nil` to distinguish.
	Plan RefactorPlan

	// Tasks is the persisted [RefactorTask] rows for the
	// plan, in deterministic order: by hot_spot rank
	// (score-DESC) THEN by rule_id ASC for ties. Empty when
	// [Plan.PlanID] is zero OR when every covered hot_spot
	// had no qualifying findings.
	Tasks []RefactorTask
}

// -----------------------------------------------------------------------------
// New reader interface: FindingDetailReader
// -----------------------------------------------------------------------------

// FindingDetail is one qualifying finding row reduced to the
// two columns the task planner consumes: `(scope_id, rule_id)`.
// The detail intentionally drops `policy_version_id` (already
// the filter), `severity`, `delta` (also filtered), and the
// metric_sample_ids JSONB (irrelevant to task emission). A
// scope with multiple qualifying findings at the SAME
// `(scope_id, rule_id)` appears ONCE in the detail set; the
// [TaskPlanner] dedupes per the rubber-duck finding #7 so a
// repeated rule firing does not produce duplicate
// refactor_task rows.
type FindingDetail struct {
	ScopeID uuid.UUID
	RuleID  string
}

// FindingDetailReader returns the qualifying `(scope_id,
// rule_id)` pairs at one (repo_id, sha, policy_version_id)
// restricted to a closed `scopeIDs` set (the top-N hot_spot
// scopes). The filter mirrors [FindingReader]:
//
//   - `delta IN ('new', 'newly_failing')` (architecture
//     Sec 5.4.1) -- same qualifying-delta closed set as
//     [HotSpotQualifyingDeltas].
//   - `policy_version_id = $4` (the architecture Sec 5.5.1
//     reproducibility invariant -- counts only findings
//     produced by the policy version stamped on the
//     hot_spot).
//
// `scopeIDs` is the closed scope filter; an empty slice
// returns an empty slice (no work to do). The reader MUST
// dedupe by `(scope_id, rule_id)` so multiple firings of the
// same rule at the same scope do not produce duplicate task
// rows in the plan.
//
// Implementations:
//
//   - [InMemoryFindingDetailReader] -- a process-local slice.
//   - [SQLFindingDetailReader] -- the production reader
//     against `clean_code.finding`.
type FindingDetailReader interface {
	FindingDetails(
		ctx context.Context,
		repoID uuid.UUID,
		sha string,
		policyVersionID uuid.UUID,
		scopeIDs []uuid.UUID,
	) ([]FindingDetail, error)
}

// HotSpotReader returns the LATEST batch of `hot_spot` rows
// at one (repo_id, sha, policy_version_id) restricted to the
// policy's `top_n` truncation. "Latest" means: the batch
// whose `created_at` equals `MAX(created_at)` for the same
// (repo_id, sha, policy_version_id) tuple, which under the
// Stage 8.1 single-writer / single-replica invariant
// (architecture Sec 3.9, rollout.md Stage 8.2) corresponds
// to the most recently written [Planner.Plan] batch.
//
// `topN`:
//
//   - `> 0` truncates the returned slice to that many rows
//     ordered by `score DESC, scope_id ASC` (the
//     deterministic Computer ranking).
//   - `0` returns the FULL latest batch (no truncation) --
//     matches the [RefactorWeights.TopN] zero-default
//     semantics.
//   - `< 0` is undefined; the [TaskPlanner] caller rejects
//     negative TopN before invoking the reader.
//
// Returns an empty slice (not an error) when no hot_spot
// rows exist at the tuple -- Stage 8.1 has not run yet, or
// every scope was filtered out by Computer eligibility.
//
// Implementations:
//
//   - [InMemoryHotSpotReader] -- a process-local slice.
//   - [SQLHotSpotReader] -- the production reader against
//     `clean_code.hot_spot`.
//
// Single-writer assumption: the production refactor-planner
// binary runs SINGLE-REPLICA (see rollout.md Stage 8.2). A
// future migration MAY add an explicit `batch_id` /
// `planner_run_id` column to `hot_spot` to disambiguate
// parallel writers; until then the `MAX(created_at)` rule
// is sufficient.
type HotSpotReader interface {
	LatestHotSpotsByScore(
		ctx context.Context,
		repoID uuid.UUID,
		sha string,
		policyVersionID uuid.UUID,
		topN int,
	) ([]HotSpot, error)
}

// -----------------------------------------------------------------------------
// Writers
// -----------------------------------------------------------------------------

// RefactorPlanTaskWriter persists ONE [RefactorPlan] row + N
// [RefactorTask] rows ATOMICALLY -- the rubber-duck Stage 8.2
// design review finding #1 caught the partial-failure shape
// of a split writer (plan inserted, tasks failed -> orphan
// append-only plan). The SQL implementation wraps both
// inserts in a single transaction; the in-memory
// implementation just appends to two slices guarded by the
// same mutex.
//
// A nil `tasks` slice is permitted (a plan covering hot_spots
// that had no qualifying findings) but the writer MUST still
// insert the plan row.
//
// Implementations:
//
//   - [InMemoryRefactorPlanTaskWriter] -- collects rows in
//     two slices.
//   - [SQLRefactorPlanTaskWriter] -- INSERTs into
//     `clean_code.refactor_plan` + `clean_code.refactor_task`
//     in one transaction.
type RefactorPlanTaskWriter interface {
	WriteRefactorPlanAndTasks(ctx context.Context, plan RefactorPlan, tasks []RefactorTask) error
}

// -----------------------------------------------------------------------------
// Sentinel errors specific to Stage 8.2
// -----------------------------------------------------------------------------

var (
	// ErrUnknownTaskKind is returned by [ValidateTaskKind]
	// when the supplied kind is NOT in [CanonicalTaskKinds]
	// AND NOT in [RejectedTaskKindAliases] -- a typo or a
	// future-spec value that has not landed in this build.
	ErrUnknownTaskKind = errors.New(
		"refactor: unknown task.kind (not in canonical enum)")

	// ErrRejectedTaskKindAlias is returned when the supplied
	// kind is one of the iter-3 alias set the workstream
	// brief calls out: `extract_function |
	// introduce_interface | reduce_inheritance |
	// reduce_coupling | reduce_lcom | reduce_duplication`.
	// Distinct from [ErrUnknownTaskKind] so the error
	// message can call out the rejection rather than a typo.
	ErrRejectedTaskKindAlias = errors.New(
		"refactor: task.kind is a REJECTED iter-3 alias " +
			"(use the canonical five-value enum)")

	// ErrInvalidTopN is returned by [TaskPlanner.Plan] when
	// the active policy's `refactor_weights.top_n` is
	// negative. (Zero is allowed; it means "no truncation".)
	// The steward's `validatePublishRequest` rejects
	// negative top_n at policy-publish time, so this branch
	// is mostly defensive; it covers in-memory
	// composition-root wiring that bypasses publish
	// validation.
	ErrInvalidTopN = errors.New(
		"refactor: refactor_weights.top_n must be >= 0")

	// ErrNilHotSpotReader / ErrNilFindingDetailReader /
	// ErrNilPlanTaskWriter signal a composition-root wiring
	// bug at [NewTaskPlanner] time. Mirror the [Planner]'s
	// nil-dependency sentinels.
	ErrNilHotSpotReader = errors.New(
		"refactor: HotSpotReader is nil")
	ErrNilFindingDetailReader = errors.New(
		"refactor: FindingDetailReader is nil")
	ErrNilPlanTaskWriter = errors.New(
		"refactor: RefactorPlanTaskWriter is nil")

	// ErrNilSummaryFunc / ErrNilTaskDescriptionFunc /
	// ErrNilRuleKindMapper signal that a caller passed
	// `nil` through [WithSummaryFunc] / [WithTaskDescriptionFunc]
	// / [WithRuleKindMapper]. Caught at construction
	// (rubber-duck iter-2 finding #5) so the operator sees a
	// clear error rather than a `nil pointer dereference`
	// panic at the first [TaskPlanner.Plan] call. Each
	// option also has a non-nil default; calling
	// `Option(nil)` is therefore always a bug.
	ErrNilSummaryFunc = errors.New(
		"refactor: WithSummaryFunc was passed nil")
	ErrNilTaskDescriptionFunc = errors.New(
		"refactor: WithTaskDescriptionFunc was passed nil")
	ErrNilRuleKindMapper = errors.New(
		"refactor: WithRuleKindMapper was passed nil")

	// ErrNilEffortEstimator signals that [WithEffortEstimator]
	// was called with a nil receiver. The Stage 8.3 estimator
	// is OPTIONAL (default behaviour emits `effort_hours = 0.0`
	// per Stage 8.2); passing `nil` is therefore unambiguously
	// a wiring bug -- the caller intended to wire an estimator
	// but the underlying value was nil. Mirrors the other
	// [TaskOption] nil sentinels.
	ErrNilEffortEstimator = errors.New(
		"refactor: WithEffortEstimator was passed nil")

	// ErrNilIDFactoryOption / ErrNilClockOption signal that a
	// caller passed `nil` through [WithTaskIDFactory] /
	// [WithTaskClock]. Distinct from the [ErrNilIDFactory] /
	// [ErrNilClock] sentinels in `hotspot.go` (which signal
	// a missing default at Plan() time, not a nil-passed
	// option at construction time).
	ErrNilIDFactoryOption = errors.New(
		"refactor: WithTaskIDFactory was passed nil")
	ErrNilClockOption = errors.New(
		"refactor: WithTaskClock was passed nil")
)

// -----------------------------------------------------------------------------
// TaskPlanner options
// -----------------------------------------------------------------------------

// TaskOption configures the [TaskPlanner]. Mirrors the
// Stage 8.1 [Option] shape: each option is a function that
// mutates the planner during [NewTaskPlanner]. Options are
// applied in slice order so a later option overrides an
// earlier one of the same kind.
//
// Rubber-duck iter-2 finding #5: every option that accepts a
// callback rejects `nil` by stashing a sentinel error on the
// planner; [NewTaskPlanner] returns that error after applying
// all options. This catches a misconfigured composition root
// at construction time instead of at the first [TaskPlanner.Plan]
// call (where the nil callback would surface as a panic).
type TaskOption func(*TaskPlanner)

// WithTaskIDFactory overrides the uuid factory the
// [TaskPlanner] uses for the `plan_id` and per-task
// `task_id`. The factory MUST return a fresh, non-zero uuid
// per call; collisions break the PK contract on both rows.
// Tests inject a counter-backed factory for byte-identical
// output across runs. A nil factory yields
// [ErrNilIDFactoryOption] from [NewTaskPlanner].
func WithTaskIDFactory(f func() (uuid.UUID, error)) TaskOption {
	return func(tp *TaskPlanner) {
		if f == nil {
			tp.optErr = errors.Join(tp.optErr, ErrNilIDFactoryOption)
			return
		}
		tp.newID = f
	}
}

// WithTaskClock overrides the clock the [TaskPlanner] reads
// once per [TaskPlanner.Plan] call. All emitted rows share
// the single reading so the plan + every task carry one
// `created_at`. A nil clock yields [ErrNilClockOption] from
// [NewTaskPlanner].
func WithTaskClock(f func() time.Time) TaskOption {
	return func(tp *TaskPlanner) {
		if f == nil {
			tp.optErr = errors.Join(tp.optErr, ErrNilClockOption)
			return
		}
		tp.now = f
	}
}

// WithRuleKindMapper overrides the `rule_id -> TaskKind`
// mapping. The default is [DefaultTaskKindForRule]; tests or
// future operators inject a custom mapper to surface
// non-canonical rule families. The mapper returns
// `(kind, true)` when the rule_id maps and `(zero, false)`
// when no mapping exists; the [TaskPlanner] then consults
// the configured [WithDefaultKind] fallback.
// A nil mapper yields [ErrNilRuleKindMapper] from
// [NewTaskPlanner].
func WithRuleKindMapper(m func(ruleID string) (TaskKind, bool)) TaskOption {
	return func(tp *TaskPlanner) {
		if m == nil {
			tp.optErr = errors.Join(tp.optErr, ErrNilRuleKindMapper)
			return
		}
		tp.ruleKindMapper = m
	}
}

// WithDefaultKind overrides the fallback kind used when a
// finding's rule_id does NOT map to a canonical kind. The
// default fallback is [TaskKindExtractMethod] (the most
// generic refactor in the canonical enum). The fallback MUST
// itself be canonical; [NewTaskPlanner] rejects a non-
// canonical default at construction time per rubber-duck
// finding #11.
func WithDefaultKind(k TaskKind) TaskOption {
	return func(tp *TaskPlanner) { tp.defaultKind = k }
}

// WithSummaryFunc overrides the per-plan summary_md generator.
// The default is [defaultSummaryMD]; a future LLM-explainer
// wires a richer generator without changing the writer
// contract. A nil fn yields [ErrNilSummaryFunc] from
// [NewTaskPlanner].
func WithSummaryFunc(fn func(plan RefactorPlan, tasks []RefactorTask, snap PolicySnapshot) string) TaskOption {
	return func(tp *TaskPlanner) {
		if fn == nil {
			tp.optErr = errors.Join(tp.optErr, ErrNilSummaryFunc)
			return
		}
		tp.summaryFn = fn
	}
}

// WithTaskDescriptionFunc overrides the per-task description_md
// generator. The default is [defaultTaskDescriptionMD]; a
// future LLM-explainer wires a richer generator without
// changing the writer contract. A nil fn yields
// [ErrNilTaskDescriptionFunc] from [NewTaskPlanner].
func WithTaskDescriptionFunc(fn func(task RefactorTask, hs HotSpot, snap PolicySnapshot) string) TaskOption {
	return func(tp *TaskPlanner) {
		if fn == nil {
			tp.optErr = errors.Join(tp.optErr, ErrNilTaskDescriptionFunc)
			return
		}
		tp.descriptionFn = fn
	}
}

// WithEffortEstimator wires the Stage 8.3 [EffortEstimator]
// into the [TaskPlanner]. When set, the planner calls
// `e.Estimate(task, hs, snap)` for each emitted task and
// stamps the returned float on `task.EffortHours` (replacing
// the Stage 8.2 `0.0` placeholder).
//
// Estimator errors abort the WHOLE batch (no plan row, no
// task row lands) -- silent fallback to `0.0` would hide a
// production model/schema misconfiguration (rubber-duck
// Stage 8.3 design review #2). The
// [ErrEffortModelVersionMismatch] sentinel is the canonical
// "the loaded model and the active policy disagree on the
// pinned version" signal; the operator MUST reconcile the
// two before re-running the planner.
//
// When the option is NOT supplied (the default), the planner
// preserves the Stage 8.2 byte-identical placeholder
// behaviour (`effort_hours = 0.0` per task). A nil estimator
// yields [ErrNilEffortEstimator] from [NewTaskPlanner].
func WithEffortEstimator(e EffortEstimator) TaskOption {
	return func(tp *TaskPlanner) {
		if e == nil {
			tp.optErr = errors.Join(tp.optErr, ErrNilEffortEstimator)
			return
		}
		tp.effortEstimator = e
	}
}

// -----------------------------------------------------------------------------
// TaskPlanner -- the Stage 8.2 orchestrator
// -----------------------------------------------------------------------------

// TaskPlanner is the Stage 8.2 orchestrator. It is a thin
// adapter that READS the existing top-N hot_spot rows that
// the Stage 8.1 [Planner] has already persisted for the
// target (repo_id, sha), reads the per-scope finding
// details, and writes ONE [RefactorPlan] row + N
// [RefactorTask] rows atomically.
//
//	Stage 8.1 Planner.Plan(ctx, repoID, sha)
//	    reads policy + metric_sample + finding ->
//	    writes hot_spot (append-only batch).
//	Stage 8.2 TaskPlanner.Plan(ctx, repoID, sha)
//	    reads policy (for TopN) + LATEST hot_spot batch +
//	    finding details -> writes refactor_plan +
//	    refactor_task (atomic).
//
// The split keeps the planner's responsibilities crisp: the
// hot_spot table stays the single source of truth for "what
// did we score, when?" and the Stage 8.2 adapter never
// duplicates a hot_spot row (rubber-duck iter-2 finding #3
// fix). The append-only `hot_spot` table is read by latest
// batch via `WHERE created_at = (SELECT MAX(created_at) ...)`
// so a fresh Stage 8.1 run supersedes the prior batch
// without the Stage 8.2 reader having to know which one is
// "live".
//
// Race-safe sequential wiring: when the composition root
// calls Stage 8.1 [Planner.Plan] then Stage 8.2
// [TaskPlanner.Plan] in sequence, a concurrent
// `policy.activate` between the two calls would produce a
// torn plan whose hot_spot batch was scored against policy
// version A and whose top-N truncation was driven by policy
// version B. The cmd binary AVOIDS this race by calling
// [TaskPlanner.PlanFromSnapshot] with the
// [PlanResult.Snapshot] returned by Stage 8.1; the same
// snapshot is reused for the TopN read so the two passes
// pin the same policy_version (rubber-duck iter-2 finding
// #1).
//
// [TaskPlanner.Plan] remains the standalone entry that reads
// the active snapshot itself; it is used by tests and by
// callers that do not also drive Stage 8.1.
//
// The planner is stateless across calls.
type TaskPlanner struct {
	// Inputs needed by both Plan() and PlanFromSnapshot().
	policy         PolicyReader
	hotSpotReader  HotSpotReader
	findingDetails FindingDetailReader
	planTaskWriter RefactorPlanTaskWriter

	// Determinism controls.
	newID func() (uuid.UUID, error)
	now   func() time.Time

	// Behaviour knobs.
	ruleKindMapper func(ruleID string) (TaskKind, bool)
	defaultKind    TaskKind
	summaryFn      func(plan RefactorPlan, tasks []RefactorTask, snap PolicySnapshot) string
	descriptionFn  func(task RefactorTask, hs HotSpot, snap PolicySnapshot) string

	// effortEstimator is the optional Stage 8.3 estimator.
	// Nil = Stage 8.2 byte-identical behaviour (every emitted
	// task carries `EffortHours = 0.0`). Non-nil = the
	// estimator's value lands on `task.EffortHours`; an
	// estimator error aborts the whole batch (no plan or
	// task row lands). Wired via [WithEffortEstimator].
	effortEstimator EffortEstimator

	// optErr accumulates errors stashed by the [TaskOption]
	// setters (nil callbacks) so [NewTaskPlanner] can
	// surface them after applying every option. Rubber-duck
	// iter-2 finding #5: report ALL bad-wire options at
	// once rather than failing on the first one.
	optErr error
}

// NewTaskPlanner wires a [TaskPlanner] with the four required
// dependencies + optional [TaskOption] arguments. Returns an
// error when any dependency is nil, when any callback
// option was passed nil, OR when the configured default kind
// is non-canonical.
//
// Production composition root wiring (see
// `cmd/clean-code-refactor-planner/main.go`):
//
//	st := steward.New(steward.Config{Store: store, Signer: signer})
//	tp, _ := refactor.NewTaskPlanner(
//	    &refactor.StewardPolicyReader{Steward: st},
//	    refactor.NewSQLHotSpotReader(db),
//	    refactor.NewSQLFindingDetailReader(db),
//	    refactor.NewSQLRefactorPlanTaskWriter(db),
//	)
func NewTaskPlanner(
	policy PolicyReader,
	hotSpotReader HotSpotReader,
	findingDetails FindingDetailReader,
	planTaskWriter RefactorPlanTaskWriter,
	opts ...TaskOption,
) (*TaskPlanner, error) {
	if policy == nil {
		return nil, ErrNilPolicyReader
	}
	if hotSpotReader == nil {
		return nil, ErrNilHotSpotReader
	}
	if findingDetails == nil {
		return nil, ErrNilFindingDetailReader
	}
	if planTaskWriter == nil {
		return nil, ErrNilPlanTaskWriter
	}
	tp := &TaskPlanner{
		policy:         policy,
		hotSpotReader:  hotSpotReader,
		findingDetails: findingDetails,
		planTaskWriter: planTaskWriter,
		newID:          uuid.NewV4,
		now:            time.Now,
		ruleKindMapper: DefaultTaskKindForRule,
		defaultKind:    TaskKindExtractMethod,
		summaryFn:      defaultSummaryMD,
		descriptionFn:  defaultTaskDescriptionMD,
	}
	for _, opt := range opts {
		opt(tp)
	}
	if tp.optErr != nil {
		return nil, fmt.Errorf("NewTaskPlanner: %w", tp.optErr)
	}
	// Belt-and-braces: a misbehaving custom option could
	// reset a field to nil; reject those too. The default
	// constructor body above guarantees every field is
	// non-nil before opts are applied, so a nil here means
	// an option overrode the default with nil despite the
	// per-option nil rejection.
	if tp.newID == nil {
		return nil, ErrNilIDFactoryOption
	}
	if tp.now == nil {
		return nil, ErrNilClockOption
	}
	if tp.ruleKindMapper == nil {
		return nil, ErrNilRuleKindMapper
	}
	if tp.summaryFn == nil {
		return nil, ErrNilSummaryFunc
	}
	if tp.descriptionFn == nil {
		return nil, ErrNilTaskDescriptionFunc
	}
	// Validate default kind eagerly per rubber-duck finding
	// #11 (catch bad wiring at construction, not at first
	// Plan() call).
	if err := ValidateTaskKind(tp.defaultKind); err != nil {
		return nil, fmt.Errorf("NewTaskPlanner: default kind: %w", err)
	}
	return tp, nil
}

// Plan executes the full Stage 8.2 read → emit → write
// cycle against the currently-active policy. The active
// snapshot is fetched at the head of the method so a
// standalone caller (test, ad-hoc CLI) does not need to
// drive Stage 8.1 first.
//
// Production composition wiring SHOULD prefer
// [TaskPlanner.PlanFromSnapshot] with the
// [PlanResult.Snapshot] returned by Stage 8.1 so the two
// passes pin the same policy_version (rubber-duck iter-2
// finding #1).
//
// Returns [ErrNoActivePolicy] when no policy is active,
// [ErrInvalidTopN] when the active policy carries a
// negative top_n, [ErrRejectedTaskKindAlias] or
// [ErrUnknownTaskKind] when an emitted task carries a
// non-canonical kind, or a wrapped error from any
// dependency.
//
// On empty input (no hot_spot rows at this repo/sha for the
// active policy_version_id) returns a [PlanAndTasksResult]
// with a zero-valued [Plan] (PlanID == uuid.Nil) and an
// empty [Tasks] slice -- the plan / task writer is NOT
// called.
//
// Concurrency: stateless across calls; safe to invoke from
// multiple goroutines on distinct (repo_id, sha) tuples.
func (tp *TaskPlanner) Plan(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
) (PlanAndTasksResult, error) {
	snap, ok, err := tp.policy.ActivePolicyVersion(ctx)
	if err != nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: read active policy: %w", err)
	}
	if !ok {
		return PlanAndTasksResult{}, ErrNoActivePolicy
	}
	return tp.PlanFromSnapshot(ctx, repoID, sha, snap)
}

// PlanFromSnapshot is the race-safe entrypoint the
// composition root uses when Stage 8.1 [Planner.Plan] has
// already produced a [PolicySnapshot]. It SKIPS the
// active-policy lookup so the hot_spot rows that Stage 8.1
// just wrote are guaranteed to be filtered against the SAME
// policy_version_id the Stage 8.2 reader looks for
// (rubber-duck iter-2 finding #1).
//
// `snap.PolicyVersionID` MUST be non-zero; pass the
// snapshot returned by [Planner.Plan].
//
// All other behaviour matches [TaskPlanner.Plan].
func (tp *TaskPlanner) PlanFromSnapshot(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	snap PolicySnapshot,
) (PlanAndTasksResult, error) {
	if tp.newID == nil {
		return PlanAndTasksResult{}, fmt.Errorf("refactor.TaskPlanner.Plan: %w", ErrNilIDFactory)
	}
	if tp.now == nil {
		return PlanAndTasksResult{}, fmt.Errorf("refactor.TaskPlanner.Plan: %w", ErrNilClock)
	}
	if snap.PolicyVersionID == uuid.Nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.PlanFromSnapshot: snapshot.PolicyVersionID is zero -- " +
				"pass a [PolicySnapshot] returned by [Planner.Plan]")
	}

	// Defensive top_n validation. The steward's
	// `validatePublishRequest` rejects negative top_n at
	// publish time, but a composition root that bypasses
	// the steward (in-memory / test wiring) could still pass
	// a negative value through.
	if snap.Weights.TopN < 0 {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: %w (got %d)",
			ErrInvalidTopN, snap.Weights.TopN)
	}

	// Step 1: read the LATEST hot_spot batch for the
	// (repo_id, sha, policy_version_id) tuple, truncated to
	// the policy's top_n. The reader returns rows in
	// score-DESC, scope_id-ASC order. When TopN == 0 the
	// reader returns the FULL latest batch (no truncation).
	topHotSpots, err := tp.hotSpotReader.LatestHotSpotsByScore(
		ctx, repoID, sha, snap.PolicyVersionID, snap.Weights.TopN)
	if err != nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: read hot_spot batch: %w", err)
	}

	// Empty hot_spot set is well-defined: Stage 8.1 has not
	// run yet OR there were no scopes with the required
	// inputs. Skip the writer entirely -- emitting an empty
	// plan would be semantically meaningless and would clutter
	// the append-only refactor_plan table.
	if len(topHotSpots) == 0 {
		return PlanAndTasksResult{
			PolicyVersionID: snap.PolicyVersionID,
			HotSpots:        nil,
		}, nil
	}

	// Step 2: collect the scope_id set for the top-N
	// hot_spots and read finding details in ONE batch
	// (rubber-duck finding #6 N+1 fix).
	scopeIDs := make([]uuid.UUID, 0, len(topHotSpots))
	for _, hs := range topHotSpots {
		scopeIDs = append(scopeIDs, hs.ScopeID)
	}
	details, err := tp.findingDetails.FindingDetails(
		ctx, repoID, sha, snap.PolicyVersionID, scopeIDs)
	if err != nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: read finding_details: %w", err)
	}

	// Index details by scope_id -> dedupe by rule_id within
	// each scope (rubber-duck finding #7).
	detailsByScope := make(map[uuid.UUID]map[string]struct{}, len(topHotSpots))
	for _, d := range details {
		ruleSet, ok := detailsByScope[d.ScopeID]
		if !ok {
			ruleSet = make(map[string]struct{})
			detailsByScope[d.ScopeID] = ruleSet
		}
		ruleSet[d.RuleID] = struct{}{}
	}

	// Step 3 prep: snapshot the clock ONCE so plan +
	// every task share `created_at`.
	createdAt := tp.now()

	planID, err := tp.newID()
	if err != nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: mint plan_id: %w", err)
	}
	if planID == uuid.Nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: %w (plan_id)",
			ErrIDFactoryReturnedNil)
	}

	// Build hotspot_ids in score-DESC order (matches the
	// hot_spot reader's ORDER BY).
	hotspotIDs := make([]uuid.UUID, 0, len(topHotSpots))
	for _, hs := range topHotSpots {
		hotspotIDs = append(hotspotIDs, hs.HotspotID)
	}

	// Step 4: emit tasks. For each top-N hot_spot, for each
	// unique rule_id in its dedup'd finding-detail set, emit
	// one task. Sort rule_ids ASC within each scope so the
	// output is deterministic. Tasks across hot_spots are
	// emitted in score-DESC hot_spot order.
	tasks := make([]RefactorTask, 0)
	for _, hs := range topHotSpots {
		ruleSet, ok := detailsByScope[hs.ScopeID]
		if !ok || len(ruleSet) == 0 {
			// Hot_spot with NO qualifying findings emits
			// ZERO tasks per rubber-duck finding #2 (no
			// synthetic rule_id fabrication). The hot_spot
			// itself IS still referenced via
			// `RefactorPlan.HotspotIDs`; consumers that
			// want to surface metric-only hot_spots can
			// traverse the JSONB array.
			continue
		}

		// Deterministic rule_id ordering for this scope.
		ruleIDs := make([]string, 0, len(ruleSet))
		for rid := range ruleSet {
			ruleIDs = append(ruleIDs, rid)
		}
		sort.Strings(ruleIDs)

		for _, ruleID := range ruleIDs {
			kind, mapped := tp.ruleKindMapper(ruleID)
			if !mapped {
				kind = tp.defaultKind
			}
			// Validate every emitted kind. Rejection
			// aborts the whole batch (no plan row, no
			// task row) -- partial emission would
			// violate the atomic write contract.
			if err := ValidateTaskKind(kind); err != nil {
				return PlanAndTasksResult{}, fmt.Errorf(
					"refactor.TaskPlanner.Plan: kind for rule_id=%q: %w",
					ruleID, err)
			}
			taskID, err := tp.newID()
			if err != nil {
				return PlanAndTasksResult{}, fmt.Errorf(
					"refactor.TaskPlanner.Plan: mint task_id: %w", err)
			}
			if taskID == uuid.Nil {
				return PlanAndTasksResult{}, fmt.Errorf(
					"refactor.TaskPlanner.Plan: %w (task_id, rule_id=%q)",
					ErrIDFactoryReturnedNil, ruleID)
			}
			task := RefactorTask{
				TaskID:      taskID,
				PlanID:      planID,
				ScopeID:     hs.ScopeID,
				Kind:        kind,
				EffortHours: 0.0, // Stage 8.3 ML model populates below when wired
				RuleID:      ruleID,
				CreatedAt:   createdAt,
			}
			// Stage 8.3: when an [EffortEstimator] is wired
			// via [WithEffortEstimator], swap the Stage 8.2
			// `0.0` placeholder for the model's estimate.
			// Estimator errors abort the WHOLE batch -- no
			// plan row, no task row lands (rubber-duck Stage
			// 8.3 design review #2: silent fallback to 0
			// would hide a model/policy version drift).
			if tp.effortEstimator != nil {
				hours, eErr := tp.effortEstimator.Estimate(task, hs, snap)
				if eErr != nil {
					return PlanAndTasksResult{}, fmt.Errorf(
						"refactor.TaskPlanner.Plan: effort estimator for rule_id=%q kind=%q: %w",
						ruleID, kind, eErr)
				}
				task.EffortHours = hours
			}
			task.DescriptionMD = tp.descriptionFn(task, hs, snap)
			tasks = append(tasks, task)
		}
	}

	plan := RefactorPlan{
		PlanID:     planID,
		RepoID:     repoID,
		SHA:        sha,
		HotspotIDs: hotspotIDs,
		CreatedAt:  createdAt,
	}
	plan.SummaryMD = tp.summaryFn(plan, tasks, snap)

	// Step 5: ATOMIC plan + tasks write.
	if err := tp.planTaskWriter.WriteRefactorPlanAndTasks(ctx, plan, tasks); err != nil {
		return PlanAndTasksResult{}, fmt.Errorf(
			"refactor.TaskPlanner.Plan: write plan+tasks: %w", err)
	}

	return PlanAndTasksResult{
		PolicyVersionID: snap.PolicyVersionID,
		HotSpots:        topHotSpots,
		Plan:            plan,
		Tasks:           tasks,
	}, nil
}

// -----------------------------------------------------------------------------
// Default summary / description generators
// -----------------------------------------------------------------------------

// defaultSummaryMD produces a minimal markdown summary for a
// plan. The output names the (repo_id, sha) coordinate, the
// hot_spot count, and the task count. A future
// LLM-explainer can wire a richer generator via
// [WithSummaryFunc] without changing the writer contract.
func defaultSummaryMD(plan RefactorPlan, tasks []RefactorTask, snap PolicySnapshot) string {
	return fmt.Sprintf(
		"Refactor plan for repo %s at sha %s.\n\n"+
			"Covered hot_spots: %d\nGenerated tasks: %d\n"+
			"Policy version: %s\n",
		plan.RepoID, plan.SHA,
		len(plan.HotspotIDs), len(tasks),
		snap.PolicyVersionID,
	)
}

// defaultTaskDescriptionMD produces a minimal markdown
// description for a single task. Names the scope_id, rule_id,
// and canonical kind. Stage 8.3's LLM-explainer can wire a
// richer generator via [WithTaskDescriptionFunc].
func defaultTaskDescriptionMD(task RefactorTask, hs HotSpot, snap PolicySnapshot) string {
	return fmt.Sprintf(
		"Refactor task: kind=%s for scope %s motivated by rule %s.\n"+
			"Hot_spot id: %s (score=%g, policy_version=%s).\n",
		task.Kind, task.ScopeID, task.RuleID,
		hs.HotspotID, hs.Score, snap.PolicyVersionID,
	)
}

// -----------------------------------------------------------------------------
// In-memory implementations -- tests + scaffold mode
// -----------------------------------------------------------------------------

// InMemoryFindingDetailReader is a process-local
// [FindingDetailReader] backed by the same kind of slice as
// [InMemoryFindingReader] (an in-memory `finding` table).
// The reader filters by `delta IN HotSpotQualifyingDeltas`,
// `policy_version_id`, `repo_id`, `sha`, AND the
// `scopeIDs` membership set; dedup is by `(scope_id, rule_id)`.
type InMemoryFindingDetailReader struct {
	mu sync.Mutex
	// findings reuses the [InMemoryFinding] shape; we add
	// a `RuleID` field via a sibling type so the existing
	// [InMemoryFindingReader] keeps its smaller surface.
	rows []InMemoryFindingWithRule
}

// InMemoryFindingWithRule extends [InMemoryFinding] with a
// `RuleID` column the Stage 8.2 detail reader needs. Stage
// 8.1's [InMemoryFinding] intentionally omitted rule_id
// because the count reader did not consume it; the Stage 8.2
// reader does. A separate type keeps the Stage 8.1 surface
// unchanged.
type InMemoryFindingWithRule struct {
	InMemoryFinding
	RuleID string
}

// NewInMemoryFindingDetailReader returns a fresh reader.
func NewInMemoryFindingDetailReader() *InMemoryFindingDetailReader {
	return &InMemoryFindingDetailReader{}
}

// Insert appends a finding-with-rule row. Concurrent-safe.
func (r *InMemoryFindingDetailReader) Insert(f InMemoryFindingWithRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, f)
}

// FindingDetails implements [FindingDetailReader].
func (r *InMemoryFindingDetailReader) FindingDetails(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	policyVersionID uuid.UUID,
	scopeIDs []uuid.UUID,
) ([]FindingDetail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(scopeIDs) == 0 {
		return nil, nil
	}
	scopeSet := make(map[uuid.UUID]struct{}, len(scopeIDs))
	for _, s := range scopeIDs {
		scopeSet[s] = struct{}{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Dedupe by (scope_id, rule_id). Use a stable seen set.
	type key struct {
		scopeID uuid.UUID
		ruleID  string
	}
	seen := make(map[key]struct{})
	out := make([]FindingDetail, 0)
	for _, f := range r.rows {
		if f.RepoID != repoID || f.SHA != sha {
			continue
		}
		if f.PolicyVersionID != policyVersionID {
			continue
		}
		if !IsHotSpotQualifyingDelta(f.Delta) {
			continue
		}
		if _, ok := scopeSet[f.ScopeID]; !ok {
			continue
		}
		k := key{scopeID: f.ScopeID, ruleID: f.RuleID}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, FindingDetail{ScopeID: f.ScopeID, RuleID: f.RuleID})
	}
	// Deterministic sort: by scope_id (byte-lex) then
	// rule_id ASC so tests / callers see a stable order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScopeID != out[j].ScopeID {
			return uuidLess(out[i].ScopeID, out[j].ScopeID)
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out, nil
}

// InMemoryRefactorPlanTaskWriter is a process-local
// [RefactorPlanTaskWriter] that records every plan + task
// write in insertion order. Tests inspect [Plans] and [Tasks]
// to assert the planner emitted the expected rows.
//
// The "atomic" contract is satisfied by guarding the two
// append operations with one mutex: a concurrent reader
// observing one of them necessarily observes the other.
type InMemoryRefactorPlanTaskWriter struct {
	mu    sync.Mutex
	plans []RefactorPlan
	tasks []RefactorTask
}

// NewInMemoryRefactorPlanTaskWriter returns a fresh writer.
func NewInMemoryRefactorPlanTaskWriter() *InMemoryRefactorPlanTaskWriter {
	return &InMemoryRefactorPlanTaskWriter{}
}

// WriteRefactorPlanAndTasks implements [RefactorPlanTaskWriter].
func (w *InMemoryRefactorPlanTaskWriter) WriteRefactorPlanAndTasks(
	ctx context.Context, plan RefactorPlan, tasks []RefactorTask,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.plans = append(w.plans, plan)
	if len(tasks) > 0 {
		w.tasks = append(w.tasks, tasks...)
	}
	return nil
}

// Plans returns a snapshot of every plan written so far in
// insert order. Returns a fresh slice so callers can mutate
// without affecting subsequent reads.
func (w *InMemoryRefactorPlanTaskWriter) Plans() []RefactorPlan {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]RefactorPlan, len(w.plans))
	copy(out, w.plans)
	return out
}

// Tasks returns a snapshot of every task written so far in
// insert order. Returns a fresh slice.
func (w *InMemoryRefactorPlanTaskWriter) Tasks() []RefactorTask {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]RefactorTask, len(w.tasks))
	copy(out, w.tasks)
	return out
}

// Reset clears state. Used by tests that exercise multiple
// Plan() calls.
func (w *InMemoryRefactorPlanTaskWriter) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.plans = nil
	w.tasks = nil
}

// InMemoryHotSpotReader is a process-local [HotSpotReader]
// backed by a slice of [HotSpot] rows. Tests Insert one or
// more batches (each row tagged with the batch's
// `CreatedAt`); LatestHotSpotsByScore returns the rows whose
// `CreatedAt == max` for the matching (repo_id, sha,
// policy_version_id) tuple, ordered by score-DESC,
// scope_id-ASC, then truncated to topN.
type InMemoryHotSpotReader struct {
	mu   sync.Mutex
	rows []HotSpot
}

// NewInMemoryHotSpotReader returns a fresh reader.
func NewInMemoryHotSpotReader() *InMemoryHotSpotReader {
	return &InMemoryHotSpotReader{}
}

// Insert appends a hot_spot row. Multiple calls with rows
// sharing one `CreatedAt` model a single Stage 8.1 Planner
// batch.
func (r *InMemoryHotSpotReader) Insert(hs HotSpot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, hs)
}

// InsertBatch appends a batch of hot_spot rows in one call.
func (r *InMemoryHotSpotReader) InsertBatch(rows []HotSpot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rows...)
}

// Reset clears state. Used by tests that exercise multiple
// Plan() calls.
func (r *InMemoryHotSpotReader) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = nil
}

// LatestHotSpotsByScore implements [HotSpotReader] by:
//
//  1. Filtering to rows whose (RepoID, SHA,
//     PolicyVersionID) match.
//  2. Picking `max(CreatedAt)` over that subset (the latest
//     batch's stamp).
//  3. Returning only rows whose CreatedAt equals the max.
//  4. Sorting score-DESC, scope_id-ASC.
//  5. Truncating to topN (when topN > 0).
func (r *InMemoryHotSpotReader) LatestHotSpotsByScore(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	policyVersionID uuid.UUID,
	topN int,
) ([]HotSpot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Pass 1: find max(CreatedAt) for the (repo, sha,
	// policy_version_id) tuple.
	var (
		maxCreatedAt time.Time
		anyMatch     bool
	)
	for _, hs := range r.rows {
		if hs.RepoID != repoID || hs.SHA != sha {
			continue
		}
		if hs.PolicyVersionID != policyVersionID {
			continue
		}
		if !anyMatch || hs.CreatedAt.After(maxCreatedAt) {
			maxCreatedAt = hs.CreatedAt
			anyMatch = true
		}
	}
	if !anyMatch {
		return nil, nil
	}

	// Pass 2: collect rows whose CreatedAt == max.
	out := make([]HotSpot, 0)
	for _, hs := range r.rows {
		if hs.RepoID != repoID || hs.SHA != sha {
			continue
		}
		if hs.PolicyVersionID != policyVersionID {
			continue
		}
		if !hs.CreatedAt.Equal(maxCreatedAt) {
			continue
		}
		out = append(out, hs)
	}

	// Sort score-DESC, scope_id-ASC for determinism (the
	// SQL reader's ORDER BY pin).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return uuidLess(out[i].ScopeID, out[j].ScopeID)
	})

	if topN > 0 && topN < len(out) {
		out = out[:topN]
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// SQL-backed implementations
// -----------------------------------------------------------------------------

// SQLHotSpotReader is the production [HotSpotReader] against
// `clean_code.hot_spot`. The reader pins the latest batch via
// `created_at = (SELECT MAX(created_at) ...)` so a fresh
// Stage 8.1 [Planner.Plan] run supersedes the prior batch
// without the reader having to know which one is "live"
// (rubber-duck iter-2 finding #2).
//
// Two query shapes:
//
//   - `topN > 0` -- appends `LIMIT $4` so the database
//     returns AT MOST topN rows.
//   - `topN == 0` -- omits LIMIT entirely so the full
//     latest batch returns.
//
// The two-query shape avoids the obscure
// `LIMIT NULLIF($n, 0)` trick (rubber-duck feedback) and
// keeps the sqlmock assertions explicit.
type SQLHotSpotReader struct {
	db *sql.DB
}

// NewSQLHotSpotReader wraps db. The caller owns the
// `*sql.DB` lifecycle.
func NewSQLHotSpotReader(db *sql.DB) *SQLHotSpotReader {
	return &SQLHotSpotReader{db: db}
}

// LatestHotSpotsByScore implements [HotSpotReader].
//
// SQL shape (topN > 0):
//
//	SELECT hotspot_id, repo_id, sha, scope_id, score,
//	       policy_version_id, created_at
//	  FROM clean_code.hot_spot
//	 WHERE repo_id = $1
//	   AND sha = $2
//	   AND policy_version_id = $3
//	   AND created_at = (
//	         SELECT MAX(created_at)
//	           FROM clean_code.hot_spot
//	          WHERE repo_id = $1
//	            AND sha = $2
//	            AND policy_version_id = $3
//	   )
//	 ORDER BY score DESC, scope_id ASC
//	 LIMIT $4
//
// SQL shape (topN == 0): identical to above without the
// `LIMIT $4` tail.
//
// Negative topN is rejected as invalid input; the
// [TaskPlanner] also rejects it upstream via [ErrInvalidTopN].
func (r *SQLHotSpotReader) LatestHotSpotsByScore(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	policyVersionID uuid.UUID,
	topN int,
) ([]HotSpot, error) {
	if topN < 0 {
		return nil, fmt.Errorf(
			"refactor.SQLHotSpotReader.LatestHotSpotsByScore: %w (got %d)",
			ErrInvalidTopN, topN)
	}
	base := fmt.Sprintf(`
		SELECT hotspot_id, repo_id, sha, scope_id, score, policy_version_id, created_at
		  FROM %s.hot_spot
		 WHERE repo_id = $1
		   AND sha = $2
		   AND policy_version_id = $3
		   AND created_at = (
		         SELECT MAX(created_at)
		           FROM %s.hot_spot
		          WHERE repo_id = $1
		            AND sha = $2
		            AND policy_version_id = $3
		   )
		 ORDER BY score DESC, scope_id ASC
	`, schemaName, schemaName)
	var (
		rows *sql.Rows
		err  error
	)
	if topN > 0 {
		q := base + " LIMIT $4"
		rows, err = r.db.QueryContext(ctx, q, repoID, sha, policyVersionID, topN)
	} else {
		rows, err = r.db.QueryContext(ctx, base, repoID, sha, policyVersionID)
	}
	if err != nil {
		return nil, fmt.Errorf(
			"refactor.SQLHotSpotReader.LatestHotSpotsByScore: query: %w", err)
	}
	defer rows.Close()
	out := make([]HotSpot, 0)
	for rows.Next() {
		var hs HotSpot
		if err := rows.Scan(
			&hs.HotspotID,
			&hs.RepoID,
			&hs.SHA,
			&hs.ScopeID,
			&hs.Score,
			&hs.PolicyVersionID,
			&hs.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"refactor.SQLHotSpotReader.LatestHotSpotsByScore: scan: %w", err)
		}
		out = append(out, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"refactor.SQLHotSpotReader.LatestHotSpotsByScore: iterate: %w", err)
	}
	return out, nil
}

// SQLFindingDetailReader is the production [FindingDetailReader]
// against `clean_code.finding`. Selects DISTINCT (scope_id,
// rule_id) so dedup happens in SQL rather than in
// application code (the rubber-duck Stage 8.2 design
// review finding #7 fix).
type SQLFindingDetailReader struct {
	db *sql.DB
}

// NewSQLFindingDetailReader wraps db. The caller owns the
// `*sql.DB` lifecycle.
func NewSQLFindingDetailReader(db *sql.DB) *SQLFindingDetailReader {
	return &SQLFindingDetailReader{db: db}
}

// FindingDetails issues the canonical qualifying-findings
// detail query: SELECT DISTINCT scope_id, rule_id FROM
// clean_code.finding WHERE delta IN ('new','newly_failing')
// AND policy_version_id = $4 AND repo_id = $1 AND sha = $2
// AND scope_id = ANY($3). DISTINCT collapses multiple firings
// of the same rule at the same scope into ONE detail row.
func (r *SQLFindingDetailReader) FindingDetails(
	ctx context.Context,
	repoID uuid.UUID,
	sha string,
	policyVersionID uuid.UUID,
	scopeIDs []uuid.UUID,
) ([]FindingDetail, error) {
	if len(scopeIDs) == 0 {
		return nil, nil
	}
	qualifying := make([]string, len(HotSpotQualifyingDeltas))
	for i, d := range HotSpotQualifyingDeltas {
		qualifying[i] = string(d)
	}
	q := fmt.Sprintf(`
		SELECT DISTINCT scope_id, rule_id
		  FROM %s.finding
		 WHERE repo_id = $1
		   AND sha = $2
		   AND scope_id = ANY($3)
		   AND delta::text = ANY($4)
		   AND policy_version_id = $5
		 ORDER BY scope_id, rule_id
	`, schemaName)
	rows, err := r.db.QueryContext(
		ctx, q,
		repoID,
		sha,
		pq.Array(uuidsToStrings(scopeIDs)),
		pq.Array(qualifying),
		policyVersionID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"refactor.SQLFindingDetailReader.FindingDetails: query: %w", err)
	}
	defer rows.Close()
	out := make([]FindingDetail, 0)
	for rows.Next() {
		var (
			scopeID uuid.UUID
			ruleID  string
		)
		if err := rows.Scan(&scopeID, &ruleID); err != nil {
			return nil, fmt.Errorf(
				"refactor.SQLFindingDetailReader.FindingDetails: scan: %w", err)
		}
		out = append(out, FindingDetail{ScopeID: scopeID, RuleID: ruleID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"refactor.SQLFindingDetailReader.FindingDetails: iterate: %w", err)
	}
	return out, nil
}

// SQLRefactorPlanTaskWriter is the production
// [RefactorPlanTaskWriter] against `clean_code.refactor_plan`
// + `clean_code.refactor_task`. ATOMIC: both inserts happen
// in one transaction; partial state never lands per the
// rubber-duck Stage 8.2 design review finding #1.
type SQLRefactorPlanTaskWriter struct {
	db *sql.DB
}

// NewSQLRefactorPlanTaskWriter wraps db.
func NewSQLRefactorPlanTaskWriter(db *sql.DB) *SQLRefactorPlanTaskWriter {
	return &SQLRefactorPlanTaskWriter{db: db}
}

// WriteRefactorPlanAndTasks INSERTs the plan + every task row
// in a single transaction. Uses explicit `created_at`
// columns rather than the table's `DEFAULT now()` so the
// rows share the [TaskPlanner]'s single clock snapshot.
func (w *SQLRefactorPlanTaskWriter) WriteRefactorPlanAndTasks(
	ctx context.Context, plan RefactorPlan, tasks []RefactorTask,
) error {
	if plan.PlanID == uuid.Nil {
		return fmt.Errorf(
			"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: " +
				"plan.PlanID is zero -- the planner MUST mint a fresh plan_id before write")
	}
	// Pre-flight kind validation so a misconfigured planner
	// surfaces the error BEFORE opening a transaction.
	for i, t := range tasks {
		if err := ValidateTaskKind(t.Kind); err != nil {
			return fmt.Errorf(
				"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: "+
					"task[%d] kind=%q: %w",
				i, t.Kind, err)
		}
	}

	hotspotJSON, err := json.Marshal(plan.HotspotIDs)
	if err != nil {
		return fmt.Errorf(
			"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: "+
				"marshal hotspot_ids: %w", err)
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf(
			"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op when Commit succeeded

	planQ := fmt.Sprintf(`
		INSERT INTO %s.refactor_plan (
		    plan_id,
		    repo_id,
		    sha,
		    hotspot_ids,
		    summary_md,
		    created_at
		) VALUES (
		    $1, $2, $3, $4::jsonb, $5, $6
		)
	`, schemaName)
	if _, err := tx.ExecContext(
		ctx, planQ,
		plan.PlanID,
		plan.RepoID,
		plan.SHA,
		string(hotspotJSON),
		plan.SummaryMD,
		plan.CreatedAt.UTC(),
	); err != nil {
		return fmt.Errorf(
			"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: "+
				"insert plan (plan_id=%s): %w", plan.PlanID, err)
	}

	if len(tasks) > 0 {
		taskQ := fmt.Sprintf(`
			INSERT INTO %s.refactor_task (
			    task_id,
			    plan_id,
			    scope_id,
			    kind,
			    effort_hours,
			    rule_id,
			    description_md,
			    created_at
			) VALUES (
			    $1, $2, $3, $4::%s.refactor_task_kind, $5, $6, $7, $8
			)
		`, schemaName, schemaName)
		stmt, err := tx.PrepareContext(ctx, taskQ)
		if err != nil {
			return fmt.Errorf(
				"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: "+
					"prepare task: %w", err)
		}
		defer stmt.Close()

		for i, t := range tasks {
			// Belt-and-braces: the plan_id on every task
			// MUST match the plan we just inserted.
			if t.PlanID != plan.PlanID {
				return fmt.Errorf(
					"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: "+
						"task[%d] plan_id=%s != plan.PlanID=%s",
					i, t.PlanID, plan.PlanID)
			}
			if _, err := stmt.ExecContext(
				ctx,
				t.TaskID,
				t.PlanID,
				t.ScopeID,
				string(t.Kind),
				t.EffortHours,
				t.RuleID,
				t.DescriptionMD,
				t.CreatedAt.UTC(),
			); err != nil {
				return fmt.Errorf(
					"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: "+
						"insert task[%d] (task_id=%s): %w",
					i, t.TaskID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf(
			"refactor.SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks: commit: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------------

// uuidsToStrings converts a UUID slice to its canonical
// string form so [pq.Array] can bind it to a Postgres
// `uuid[]` parameter. PostgreSQL accepts the string form
// when the column type is uuid.
func uuidsToStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
