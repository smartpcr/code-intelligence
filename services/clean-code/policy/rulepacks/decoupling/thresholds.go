// Threshold catalogue for the Stage 5.6 decoupling rulepacks.
//
// The implementation-plan Stage 5.6 brief (line 534) requires
// the coupling rule to consume `metric_kind IN ('fan_in',
// 'fan_out', 'coupling_between_objects')` "with thresholds
// from `Threshold` rows", and the duplication rule to consume
// `metric_kind='duplication_ratio'` -- per architecture Sec
// 5.3.5 the canonical place to hold a tunable numeric cut-off
// is a [steward.Threshold] row, not a literal in the predicate
// text. The predicates in `coupling.yaml` and `duplication.yaml`
// therefore use `threshold('<uuid>')` atoms; this file owns
// the UUIDs they reference and the seeding helper that the
// Bootstrap step calls to populate `clean_code.threshold`.
//
// # Why deterministic v5 UUIDs
//
// A YAML predicate is a literal text fragment -- the UUID
// strings inside `threshold('...')` MUST be stable across
// deployments so the predicate text in source survives a
// `policy.publish_rulepack` round-trip. We derive each
// threshold_id at init time as
// `uuid.NewV5(Namespace, "<metric_kind>")` and pin the
// resulting strings via [TestThresholdIDs_MatchYAML] -- the
// test fails if either the namespace, the seed names, or the
// YAML text drift, so an accidental edit to any one of those
// three sources is caught at build time.
//
// The same v5-from-name pattern is used by
// `internal/ast/scope/identity.go` for ScopeBinding rows
// (Sec 5.2.3) -- this file follows that established
// convention.
//
// # v1 default values
//
// The numeric `Value` field on each canonical Threshold row
// matches the operator-facing defaults documented inside
// `coupling.yaml` / `duplication.yaml`:
//   - fan_in                   > 20   (class scope)
//   - fan_out                  > 20   (class scope)
//   - coupling_between_objects > 12   (class scope, CBO band)
//   - duplication_ratio        > 0.20 (file scope, 20% LoC)
//
// Operators can re-tune by INSERTing a NEW Threshold row with
// a fresh UUID and re-publishing the rulepack at `version=2`
// that references the new UUID -- Threshold rows are
// append-only (G3 / Sec 5.3.5) so there is no UPDATE path,
// only "publish a new row, point a new rulepack version at it".
package decoupling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// Namespace is the v5 UUID namespace every canonical
// decoupling Threshold UUID is derived from. Pinned by
// [TestNamespace_Pinned] so a future drift on the seed string
// is caught at test time.
//
// The seed `"clean-code/policy/rulepacks/decoupling/v1"`
// encodes (a) the service name, (b) the package path, (c)
// the rulepack family, and (d) the canonical-set version.
// Bumping the trailing `/v1` to `/v2` is how operators rotate
// the entire canonical threshold set without colliding with
// the v1 IDs.
var Namespace = uuid.NewV5(uuid.NamespaceURL, "clean-code/policy/rulepacks/decoupling/v1")

// Canonical decoupling Threshold UUIDs, derived from
// [Namespace] via `uuid.NewV5(Namespace, "<metric_kind>")`.
// The string forms are also embedded literally in the YAML
// rulepacks under `threshold('...')` atoms; the
// [TestCanonicalThresholdIDs_MatchYAML] test pins each pair.
var (
	// FanInThresholdID -> Threshold(metric_kind=fan_in,
	// scope_kind=class, op=gt, value=20).
	FanInThresholdID = uuid.NewV5(Namespace, "fan_in")

	// FanOutThresholdID -> Threshold(metric_kind=fan_out,
	// scope_kind=class, op=gt, value=20).
	FanOutThresholdID = uuid.NewV5(Namespace, "fan_out")

	// CBOThresholdID -> Threshold(metric_kind=
	// coupling_between_objects, scope_kind=class, op=gt,
	// value=12).
	CBOThresholdID = uuid.NewV5(Namespace, "coupling_between_objects")

	// DuplicationRatioThresholdID -> Threshold(
	// metric_kind=duplication_ratio, scope_kind=file,
	// op=gt, value=0.20).
	DuplicationRatioThresholdID = uuid.NewV5(Namespace, "duplication_ratio")
)

// canonicalThresholds is the in-process source of truth for
// the four Threshold rows the decoupling rulepacks reference.
//
// Held private so external callers do not mutate the slice;
// [ListCanonicalThresholds] hands out a fresh copy and
// [Resolver] hands out a fresh [dsl.MapResolver].
var canonicalThresholds = []steward.Threshold{
	{
		ThresholdID: FanInThresholdID,
		MetricKind:  "fan_in",
		ScopeKind:   "class",
		Op:          string(dsl.OpGT),
		Value:       20,
	},
	{
		ThresholdID: FanOutThresholdID,
		MetricKind:  "fan_out",
		ScopeKind:   "class",
		Op:          string(dsl.OpGT),
		Value:       20,
	},
	{
		ThresholdID: CBOThresholdID,
		MetricKind:  "coupling_between_objects",
		ScopeKind:   "class",
		Op:          string(dsl.OpGT),
		Value:       12,
	},
	{
		ThresholdID: DuplicationRatioThresholdID,
		MetricKind:  "duplication_ratio",
		ScopeKind:   "file",
		Op:          string(dsl.OpGT),
		Value:       0.20,
	},
}

// ListCanonicalThresholds returns the four canonical
// Threshold rows for v1 of the decoupling rulepack family.
// The returned slice is a fresh copy on every call so a
// caller cannot mutate the in-package source-of-truth.
//
// Returned in deterministic order: fan_in, fan_out, cbo,
// duplication_ratio. Order matches the predicate-ordering
// the YAMLs declare (coupling rules first, duplication last).
func ListCanonicalThresholds() []steward.Threshold {
	out := make([]steward.Threshold, len(canonicalThresholds))
	copy(out, canonicalThresholds)
	return out
}

// Resolver returns a fresh [dsl.ThresholdResolver] populated
// with every canonical decoupling Threshold row. Callers who
// `dsl.Compile` a coupling-or-duplication predicate MUST pass
// this resolver (or a superset of it) so the `threshold(...)`
// atoms bind successfully.
//
// The Rule Engine (Stage 5.7) typically builds a superset
// resolver covering EVERY active policy's threshold_refs;
// this helper is the canonical decoupling-only resolver used
// by tests, the bootstrap step, and any single-pack
// dry-run / preview surface.
func Resolver() dsl.ThresholdResolver {
	r := make(dsl.MapResolver, len(canonicalThresholds))
	for _, t := range canonicalThresholds {
		r[t.ThresholdID] = dsl.Threshold{
			ThresholdID: t.ThresholdID,
			MetricKind:  t.MetricKind,
			ScopeKind:   t.ScopeKind,
			Op:          dsl.ThresholdOp(t.Op),
			Value:       t.Value,
		}
	}
	return r
}

// SeedThresholds inserts every canonical decoupling
// [steward.Threshold] row into store. Idempotent: an
// "already exists" error from [steward.InMemoryStore] or a
// PG unique-violation from [steward.SQLStore] is swallowed
// so the helper is safe to call on every clean-coded
// service boot.
//
// The Steward [steward.Steward] does NOT expose a
// `policy.publish_threshold` write verb on the canonical
// surface (architecture Sec 6.5 + tech-spec Sec 8.5: the
// only `policy.*` write verbs are `policy.publish`,
// `policy.activate`, `policy.publish_rulepack`). Per
// `internal/policy/steward/types.go` line 130 "Threshold
// rows are seeded either by migration / operator tooling
// or by tests via the [Store.InsertThreshold] primitive";
// this seeder uses that primitive directly so the threshold
// catalogue is populated before [Bootstrap] calls
// `policy.publish_rulepack` (which would otherwise have its
// own threshold_refs FK enforcement reject the publish --
// though in v1 publish_rulepack does NOT take threshold_refs;
// it is `policy.publish` that consumes them. We still seed
// at this stage so a downstream `policy.publish` that
// composes the canonical Threshold UUIDs into a PolicyVersion
// passes the FK check).
//
// Returns the actual list of newly-inserted Threshold rows
// (excluding skipped duplicates) so the caller can audit
// the bootstrap step.
//
// # CreatedAt stamping
//
// Each newly-inserted Threshold row is stamped with `now() UTC`
// at the seeding moment (the canonical "append-time" timestamp
// per G3 / Sec 5.3.5). Without this stamp [SQLStore.InsertThreshold]
// would persist the zero time (since it writes `t.CreatedAt.UTC()`
// verbatim -- see sql_store.go line 371), producing a
// `0001-01-01` audit timestamp on every seeded row and breaking
// any "what is the oldest seeded canonical threshold?" query the
// runbook later relies on. The `now()` value is identical for
// every threshold seeded by a single SeedThresholds call (a
// single `time.Now()` capture at function entry), so an operator
// reading the catalogue sees the four canonical rows as one
// atomic seeding event rather than four sub-millisecond-apart
// rows.
func SeedThresholds(ctx context.Context, store steward.Store) ([]steward.Threshold, error) {
	if store == nil {
		return nil, errors.New("decoupling.SeedThresholds: store is required")
	}
	now := time.Now().UTC()
	inserted := make([]steward.Threshold, 0, len(canonicalThresholds))
	for _, t := range canonicalThresholds {
		exists, err := store.ThresholdExists(ctx, t.ThresholdID)
		if err != nil {
			return nil, fmt.Errorf("decoupling.SeedThresholds: ThresholdExists(%s): %w", t.ThresholdID, err)
		}
		if exists {
			continue
		}
		stamped := t
		stamped.CreatedAt = now
		if err := store.InsertThreshold(ctx, stamped); err != nil {
			// Race with another bootstrap: a sibling
			// process inserted between our Exists check
			// and Insert. The duplicate-id error path is
			// not exported as a sentinel; we infer by
			// re-checking Exists() and treating "now
			// exists" as a benign race winner.
			if exists2, exErr := store.ThresholdExists(ctx, t.ThresholdID); exErr == nil && exists2 {
				continue
			}
			return nil, fmt.Errorf("decoupling.SeedThresholds: InsertThreshold(%s): %w", t.ThresholdID, err)
		}
		inserted = append(inserted, stamped)
	}
	return inserted, nil
}
