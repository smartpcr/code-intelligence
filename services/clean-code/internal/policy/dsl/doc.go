// Package dsl is the parser + evaluator for the Stage 5.4
// predicate DSL described in architecture Sec 5.3.1 line 1101
// (the `predicate_dsl` text column on `clean_code.rule`).
//
// Each [Rule] row in the Policy / rules sub-store carries a
// `predicate_dsl` text column (architecture Sec 5.3.1 line
// 1101); the Rule Engine compiles that text into a boolean
// function over [Sample] rows and evaluates it against every
// active [MetricSample] for a SHA. The DSL is intentionally
// tiny: the v1 surface is "name a metric kind, filter by
// scope, compare a numeric value, compose with AND/OR/NOT".
//
// # Grammar
//
//	predicate      ::= or_expr
//	or_expr        ::= and_expr ( "OR" and_expr )*
//	and_expr       ::= not_expr ( "AND" not_expr )*
//	not_expr       ::= "NOT" not_expr | atom
//	atom           ::= "(" predicate ")"
//	                |  threshold_call
//	                |  comparison
//	                |  bool_literal
//	threshold_call ::= "threshold" "(" string_literal ")"
//	comparison     ::= operand cmp_op operand
//	cmp_op         ::= "==" | "!=" | ">" | ">=" | "<" | "<="
//	operand        ::= field | string_literal | number_literal | bool_literal
//	field          ::= "metric_kind" | "scope_kind" | "value"
//	                |  "pack" | "source" | "degraded"
//
// Precedence: `NOT` binds tightest, then `AND`, then `OR`.
// Parentheses override.
//
// # Purity (Stage 5.4 brief)
//
// "Predicates are pure functions over MetricSample rows -- no
// side effects, no IO." Concretely:
//
//   - The [Parser] does parse-time validation of closed-set
//     literals (`metric_kind`, `scope_kind`, `pack`, `source`)
//     against the canonical sets surfaced by
//     [IsCanonicalMetricKind] / [ListCanonicalMetricKinds],
//     [IsCanonicalScopeKind] / [ListCanonicalScopeKinds],
//     [IsCanonicalPack] / [ListCanonicalPacks], and
//     [IsCanonicalSource] / [ListCanonicalSources]. The
//     backing maps are unexported so the closed sets cannot
//     be mutated at runtime. This is the canon-guard called
//     out by the `dsl-rejects-unknown-metric-kind` test
//     scenario in the Stage 5.4 implementation plan.
//
//   - The [Bind] step resolves `threshold('<uuid>')` calls
//     against the policy's [Threshold] set ONCE per
//     compilation. The resolved [Threshold] is then captured
//     in the bound predicate; evaluation never re-reads the
//     resolver.
//
//   - [Predicate.Eval] is a pure function of its [Sample]
//     argument. No locking, no maps, no IO. The
//     `dsl-deterministic` test scenario relies on this.
//
// # Threshold reference shape
//
// `threshold('<uuid>')` references a [Threshold] row by its
// `threshold_id` UUID. The UUID MUST be present in the
// [PolicyVersion.ThresholdRefs] of the policy the predicate
// belongs to -- this is the application-layer FK contract
// from migration 0003 line 462 ("the FK target is enforced
// by the writer, not by SQL"). [Bind] enforces this at
// compile time so the hot path never errors on unresolved
// refs.
//
// A bound `threshold(t)` atom evaluates true iff ALL of:
//
//  1. `sample.metric_kind == t.MetricKind`;
//
//  2. `sample.scope_kind == t.ScopeKind`;
//
//  3. `sample.value <op> t.Value` per `t.Op`.
//
// Mismatch on (1) or (2) returns false (not an error) -- a
// rule that fires on lcom4 will simply not match a sample
// for fan_in.
//
// # Caching
//
// [Cache] keeps compiled [Predicate] instances per
// `(policy_version_id, source string)` pair. `PolicyVersion`
// rows are immutable (architecture G5) so the cache is
// monotone -- entries are never invalidated except when an
// entire policy version is dropped from memory. The hot
// path is a `RWMutex.RLock` + two map lookups + a
// closed-channel receive (single-digit nanoseconds).
//
// The miss path installs a per-entry placeholder under the
// cache mutex and then RELEASES the mutex before calling
// [Compile]. This gives the cache two desirable properties:
//
//   - concurrent compilations on DIFFERENT keys never block
//     each other (a slow [ThresholdResolver.Lookup] on one
//     `(policy, source)` doesn't stall unrelated hits or
//     compiles);
//
//   - concurrent callers racing for the SAME
//     `(policy, source)` de-duplicate via the placeholder's
//     `ready` channel, so [Compile] runs at most once per
//     key (singleflight).
//
// # Threshold-ID identity check
//
// [Bind] verifies that the [Threshold.ThresholdID] returned
// by [ThresholdResolver.Lookup] matches the UUID requested
// by the `threshold('<uuid>')` atom. A mismatched ID is an
// upstream bug (rows are immutable and keyed by their own
// `threshold_id`) and is rejected with `ErrBind` rather
// than silently binding the wrong row.
package dsl
