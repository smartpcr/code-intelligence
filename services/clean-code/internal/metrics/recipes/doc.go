// Package recipes is the foundation-tier Compute Engine
// (architecture Sec 3.4) -- the surface that turns a canonical
// `*parser.AstFile` (Sec 4.5 / Stage 2.1) into the
// `MetricSample` draft rows the Metric Ingestor writes
// (Sec 5.2.1).
//
// # Contract
//
// Every recipe implements [Recipe]:
//
//	type Recipe interface {
//	    MetricKind() string
//	    Version() int
//	    AppliesTo(ast *parser.AstFile) bool
//	    Compute(ast *parser.AstFile) []MetricSampleDraft
//	}
//
// A `Compute` call is pure: same `*AstFile` in, same drafts out
// (G2 + G3). Recipes do NOT read source bytes, mutate the AST,
// or touch the database -- the Metric Ingestor owns every
// I/O-bearing step (G1).
//
// # Stage 2.3 base pack recipes
//
// The three foundation-tier base-pack recipes are:
//
//   - [CycloRecipe] -- `metric_kind='cyclo'` at
//     `scope_kinds={method, file}` (Sec 1.4.1 row 1).
//   - [CognitiveComplexityRecipe] -- `metric_kind='cognitive_complexity'`
//     at `scope_kinds={method, file}` (Sec 1.4.1 row 2).
//   - [LocRecipe] -- `metric_kind='loc'` at applicability
//     set `{file, package, repo}` (Sec 1.4.1 row 3). The
//     recipe directly emits at `file` only per Compute call;
//     `package` / `repo` rows are produced by the
//     materialiser (Stage 2.6) and Cross-Repo Aggregator
//     (Sec 3.10 step 4) which SUM file rows. See [LocRecipe]
//     "Two scope-kind sets, NOT one" for the rationale.
//
// The registered set is asserted by
// [TestDefaultRegistry_BaseRecipesOnlyCanonicalKinds] against
// [DefaultRegistry]: any drift -- a new `metric_kind`, a
// typo'd alias such as `cyclomatic_complexity` /
// `lines_of_code`, or a missing recipe -- fails the test.
//
// # Startup snapshot (impl-plan Stage 2.3 line 201)
//
// The composition root (`cmd/clean-coded/main.go`) calls
// [DefaultRegistryWithLog] at startup; the call emits one
// deterministic structured INFO line via
// [Registry.LogRegistered] listing every registered
// metric_kind + version. The line is the operator-side
// evidence the binary booted with the expected base-pack
// recipe set. [DefaultRegistry] itself is side-effect-free
// so tests composing alternate registries do not emit a
// stray startup line.
//
// # Two scope-kind sets, NOT one
//
// For every recipe, the architecture pins TWO related
// scope_kind sets that this package keeps LEXICALLY
// DISTINCT to prevent iter-2/3 evaluator drift:
//
//   - The METRIC_KIND APPLICABILITY SET (Sec 1.4.1 row 2):
//     the closed set of scope_kinds the schema accepts for
//     a persisted `MetricSample(metric_kind='<kind>')` row.
//     Wired as the `allowedKinds` slice passed to [newDraft].
//     For loc this is `{file, package, repo}`; for cyclo /
//     cognitive_complexity it is `{method, file}`.
//   - The SUBSET THE RECIPE DIRECTLY EMITS AT, per Compute
//     call. For loc this is `{file}` (named
//     `locDirectlyEmittedKinds`). The per-file Compute
//     interface forbids the recipe from authoritatively
//     emitting at upper-tier scopes (`package`, `repo`) it
//     cannot fully observe; partial-fact rows would break
//     the writer's `(repo_id, sha, scope_id, metric_kind,
//     metric_version)` uniqueness invariant (Sec 5.2.1 line
//     905). The materialiser / aggregator emits the
//     upper-tier rows via the SAME [newDraft] helper +
//     SAME `allowedKinds` slice, so the panic guard
//     forward-accepts those future emission paths without
//     rewriting the shared helper.
//
// # Canonical scope kinds (NOT `function`, NOT `module`)
//
// Drafts emit at scopes from the canonical seven-value enum
// (architecture Sec 5.2.3 lines 1039-1050):
// `repo | package | file | class | interface | method | block`.
// In particular `function` and `module` are NOT canonical
// scope_kind values; the [newDraft] helper panics on any
// out-of-set value (its panic strings are exercised directly
// in `recipe_internal_test.go`, where the unexported helper
// is reachable). Each recipe ALSO passes a per-recipe
// `allowedKinds` slice -- the architecture Sec 1.4.1 row's
// pinned applicability set -- so the helper rejects a drift
// even within the canonical 7-enum (e.g. cyclo emitting at
// `class`, or loc emitting at `method`).
//
// # Pack and source
//
// Every draft this package emits is tagged with
// `pack = PackBase` and `source = SourceComputed` (the
// foundation-tier defaults of Sec 1.4.1 row 1-3 / Sec 5.2.1
// `source` enum).
//
// # Computed rows are NEVER degraded
//
// [MetricSampleDraft] does NOT carry a `degraded_reason`
// field. Architecture Sec 3.4 lines 490-494 pins the rule:
// "Computed rows are never `degraded=true`: if an input is
// missing the row is not written, not stamped degraded".
// Recipes honour this by SKIPPING emission on a degraded
// AST -- both [Recipe.AppliesTo] returns false AND
// [Recipe.Compute] short-circuits to nil when
// `ast.DegradedReason != ""`. Only the system-tier
// derivation path (Cross-Repo Aggregator, Sec 8.2) is
// allowed to stamp `degraded=true` on a `MetricSample` row.
// The absence of the field is enforced at runtime by
// [TestMetricSampleDraft_HasNoDegradedReasonField] which
// reflects over the struct's field set and fails on any
// degradation-shaped field name.
//
// # Capability gating (parser staging)
//
// The Stage 2.1 parser fleet emits file / package / class /
// interface / method scopes but does NOT yet decompose method
// bodies into `ScopeKindBlock` decision-point children. Until
// a future parser stage adds that walk, recipes that depend on
// decision blocks (cyclo, cognitive_complexity) MUST silently
// produce zero drafts on real parser output -- emitting an
// authoritative `cyclo=1` for every method just because the
// AST is currently shallow would put a misleading
// `source='computed'` row into the Metrics Store. The
// capability gate is the file-level attribute
// `decision_blocks="true"` on [parser.AstFile.Attrs]
// ([AttrDecisionBlocks]); producers that emit block-decision
// children MUST stamp this attr, and [Recipe.AppliesTo]
// checks it before [Recipe.Compute] is allowed to run.
//
// [LocRecipe] does NOT need this capability -- the file
// scope's `Range` is populated by every parser in the fleet
// from Stage 2.1 onward.
package recipes
