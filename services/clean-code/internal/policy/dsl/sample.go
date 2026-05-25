package dsl

import "github.com/gofrs/uuid"

// Sample is the denormalised [MetricSample] view the DSL
// evaluator consumes. The DSL grammar references both
// `metric_kind` (which lives on the `metric_sample` row
// directly) and `scope_kind` (which lives on the
// `scope_binding` row referenced by `metric_sample.scope_id`,
// architecture Sec 5.2.3 line 1046). To keep [Predicate.Eval]
// a pure function with no IO, callers MUST denormalise the
// scope kind into [Sample.ScopeKind] before evaluation.
//
// The Rule Engine (Stage 5.7) does this join at batch-load
// time using a single SQL JOIN of `metric_sample` against
// `scope_binding`; the evaluator never reaches back into the
// store.
//
// Field shape mirrors architecture Sec 5.2.1 line 894-907.
// Nullable fields are represented as Has<Field> companions
// so callers can distinguish "missing" from "zero" without
// pointer indirection on the hot path:
//
//   - `Value` is paired with `HasValue` because the
//     `metric_sample.value` column is nullable when
//     `Degraded=true` (migration 0002 line 367 `CHECK (value
//     IS NOT NULL OR degraded = true)`).
//
//   - `DegradedReason` is the empty string when `Degraded`
//     is false.
type Sample struct {
	SampleID       uuid.UUID
	RepoID         uuid.UUID
	SHA            string
	ScopeID        uuid.UUID
	ScopeKind      string
	MetricKind     string
	MetricVersion  int
	Value          float64
	HasValue       bool
	Pack           string
	Source         string
	Degraded       bool
	DegradedReason string
}

// CanonicalMetricKinds is the closed set of `metric_kind`
// values architecture Sec 1.4 (lines 81-125) pins as the
// v1 catalogue. The DSL parser refuses any string literal
// in a `metric_kind == '...'` comparison that is not in this
// set -- this is the `dsl-rejects-unknown-metric-kind` test
// scenario in the Stage 5.4 implementation plan (canon-guard
// against non-canonical aliases like `lines_of_code` instead
// of the canonical `loc`).
//
// Adding a new metric_kind requires (1) an entry here, (2) a
// Catalog row in `clean_code.metric_kind` (migration 0001),
// and (3) a Compute Engine recipe or Cross-Repo Aggregator
// composer. The set is curated by architecture Sec 1.4 and
// any extension here MUST be cross-referenced to that section.
//
// DO NOT MUTATE: this map is treated as an immutable closed
// set by the parser's canon-guard (parser.go validateStringLit
// and the `dsl-rejects-unknown-metric-kind` test scenario).
// Inserting or deleting entries at runtime silently bypasses
// that guard and lets non-canonical metric_kinds slip into
// compiled predicates. Callers MUST treat this as read-only:
// only the literal at-rest declaration below may add entries.
// The variable is exported for in-package iteration (e.g.
// [TestThreshold_Validate_AcceptsAllCanonicalMetricKinds] and
// parser.go's error-message helper) and for godoc visibility;
// a follow-up refactor will unexport it to `canonicalMetricKinds`
// once those call sites migrate to the [ListCanonicalMetricKinds]
// helper added below.
var CanonicalMetricKinds = map[string]struct{}{
	// Foundation tier / pack=base (architecture Sec 1.4.1).
	"cyclo":                        {},
	"cognitive_complexity":         {},
	"loc":                          {},
	"cycle_member":                 {},
	"duplication_ratio":            {},
	"modification_count_in_window": {},
	// Foundation tier / pack=solid (architecture Sec 1.4.1).
	"lcom4":                    {},
	"fan_in":                   {},
	"fan_out":                  {},
	"depth_of_inheritance":     {},
	"interface_width":          {},
	"coupling_between_objects": {},
	// LSP override-signature signal (architecture Sec 1.4.1
	// row 13 and Sec 3.5.1.c lines 514-525). `lsp_violation`
	// is a first-class canonical metric_kind at `scope_kind=
	// 'method'`, `pack='solid'`, `source='computed'` with
	// `value=1` when an overriding method strengthens its
	// parent's precondition or weakens its postcondition (an
	// LSP-violating override) and `value=0` otherwise. The
	// producer is the Stage 2.4 `recipes/lsp_violation.go`
	// recipe (implementation-plan Stage 2.4) -- it ALSO
	// stamps the same boolean on `MetricSample.attrs_json.
	// lsp_violation` for forensics, exactly mirroring the
	// `cycle_member` dual-encoding precedent (above, row 10:
	// 0/1 metric_kind + `attrs_json.cycle_id` detail). The
	// DSL only exposes the columnar fields
	// `{metric_kind, scope_kind, pack, source, value,
	// degraded}` -- not `attrs_json` -- so the projected
	// `metric_kind` row is the path the Stage 5.5 LSP
	// override rule (`solid.lsp.override_violation`)
	// consumes. The architecture Sec 1.4.1 row 13 entry is
	// the normative declaration; this map mirrors it.
	"lsp_violation": {},
	// System tier / pack=system (architecture Sec 1.4.2).
	"xrepo_dep_depth":           {},
	"arch_debt_ratio":           {},
	"velocity_trend":            {},
	"arch_fitness":              {},
	"blast_radius":              {},
	"xservice_test_reliability": {},
	"knowledge_index":           {},
	// Ingested tier / pack=ingested (implementation-plan line 31
	// "Canonical 3 ingested metric_kinds", tech-spec Sec 4.1.X
	// table at lines 302-304). The v1 set is exactly three:
	// `coverage_line_ratio`, `coverage_branch_ratio` (both
	// written by Metric Ingestor on `ingest.coverage`), and
	// `pass_first_try_ratio` (written on `ingest.test_balance`).
	// The legacy aliases `coverage_line` and `coverage_branch`
	// are NEVER written (negative clause in implementation-plan
	// line 31).
	"coverage_line_ratio":   {},
	"coverage_branch_ratio": {},
	"pass_first_try_ratio":  {},
}

// IsCanonicalMetricKind reports whether s is a member of the
// closed [CanonicalMetricKinds] set.
func IsCanonicalMetricKind(s string) bool {
	_, ok := CanonicalMetricKinds[s]
	return ok
}

// ListCanonicalMetricKinds returns the canonical metric_kind
// set as a sorted slice of fresh strings. The returned slice
// is a new allocation each call, so mutation by the caller
// cannot leak back into the underlying [CanonicalMetricKinds]
// canon-guard set. Prefer this helper over ranging over the
// exported map when constructing error messages or surfacing
// the catalogue to callers outside this file.
func ListCanonicalMetricKinds() []string {
	return sortedKeys(CanonicalMetricKinds)
}

// CanonicalScopeKinds mirrors the `clean_code.scope_kind`
// ENUM declared in migration 0002 line 142. Architecture Sec
// 5.2.3 line 1046 lists the same closed set.
//
// DO NOT MUTATE: read-only canon-guard set; see the
// [CanonicalMetricKinds] doc for the rationale. Prefer
// [IsCanonicalScopeKind] / [ListCanonicalScopeKinds] over
// direct map access.
var CanonicalScopeKinds = map[string]struct{}{
	"repo":      {},
	"package":   {},
	"file":      {},
	"class":     {},
	"interface": {},
	"method":    {},
	"block":     {},
}

// IsCanonicalScopeKind reports whether s is a member of the
// closed [CanonicalScopeKinds] set.
func IsCanonicalScopeKind(s string) bool {
	_, ok := CanonicalScopeKinds[s]
	return ok
}

// ListCanonicalScopeKinds returns the canonical scope_kind
// set as a sorted slice of fresh strings; see
// [ListCanonicalMetricKinds] for the no-leak guarantee.
func ListCanonicalScopeKinds() []string {
	return sortedKeys(CanonicalScopeKinds)
}

// CanonicalPacks mirrors the `clean_code.metric_sample_pack`
// ENUM declared in migration 0002 line 103. Architecture Sec
// 5.2.1 line 901 names the same closed set.
//
// DO NOT MUTATE: read-only canon-guard set; see the
// [CanonicalMetricKinds] doc for the rationale. Prefer
// [IsCanonicalPack] / [ListCanonicalPacks] over direct map
// access.
var CanonicalPacks = map[string]struct{}{
	"base":     {},
	"solid":    {},
	"system":   {},
	"ingested": {},
}

// IsCanonicalPack reports whether s is a member of the closed
// [CanonicalPacks] set.
func IsCanonicalPack(s string) bool {
	_, ok := CanonicalPacks[s]
	return ok
}

// ListCanonicalPacks returns the canonical pack set as a
// sorted slice of fresh strings; see [ListCanonicalMetricKinds]
// for the no-leak guarantee.
func ListCanonicalPacks() []string {
	return sortedKeys(CanonicalPacks)
}

// CanonicalSources mirrors the `clean_code.metric_sample_source`
// ENUM declared in migration 0002 line 115. Architecture Sec
// 5.2.1 line 902 names the same closed set.
//
// DO NOT MUTATE: read-only canon-guard set; see the
// [CanonicalMetricKinds] doc for the rationale. Prefer
// [IsCanonicalSource] / [ListCanonicalSources] over direct
// map access.
var CanonicalSources = map[string]struct{}{
	"computed": {},
	"derived":  {},
	"ingested": {},
}

// IsCanonicalSource reports whether s is a member of the
// closed [CanonicalSources] set.
func IsCanonicalSource(s string) bool {
	_, ok := CanonicalSources[s]
	return ok
}

// ListCanonicalSources returns the canonical source set as a
// sorted slice of fresh strings; see [ListCanonicalMetricKinds]
// for the no-leak guarantee.
func ListCanonicalSources() []string {
	return sortedKeys(CanonicalSources)
}

// sortedKeys returns the keys of set as a sorted slice. The
// returned slice is a fresh allocation; mutation by the caller
// does not affect set. Closed canon-guard sets here are tiny
// (<= 30 entries) so the tiny in-place selection sort matches
// parser.go's listCanonical -- algorithmic choice is irrelevant
// next to the determinism we need for stable error messages.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		mn := i
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[mn] {
				mn = j
			}
		}
		out[i], out[mn] = out[mn], out[i]
	}
	return out
}
