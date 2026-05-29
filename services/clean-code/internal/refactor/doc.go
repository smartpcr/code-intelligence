// Package refactor implements the Refactor Planner (architecture
// Sec 3.9 of the CLEAN-CODE story).
//
// The Refactor Planner is a post-ingest worker that turns
// `Finding` rows plus `MetricSample` rows into
// `HotSpot`, `RefactorPlan`, and `RefactorTask` rows. It reads
// `PolicyVersion.refactor_weights` for the composite-score
// weights and the effort-model pin. The Planner is a SOLE
// writer of the Refactor sub-store per architecture G1 (Sec
// 1.5) -- the database role `clean_code_refactor_planner` is
// granted exactly `INSERT, SELECT` on `hot_spot`,
// `refactor_plan`, and `refactor_task` and `REVOKE UPDATE,
// DELETE` on the same (see migration `0004_roles.up.sql:482-509`).
//
// # Wen Zhong principle (architecture Sec 3.9 lines 622-624)
//
// The Refactor Planner NEVER mutates source code. Every output
// is a row, never a patch. The package exposes pure compute
// helpers ([Score], [RobustZ]), a [Computer] that returns
// in-memory [Computation] values, AND a [Planner] that
// orchestrates the canonical read → compute → write cycle
// against the steward (for the active policy) and the
// measurement / audit sub-stores (for metric_sample / finding
// rows).
//
// # Stage scope split
//
// Per `docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md`
// Phase 8, this package is built up across three stages:
//
//   - **Stage 8.1 (this file set)** -- composite hotspot scoring
//     END-TO-END. Ships (a) [Computer] / [Computation] /
//     [HotSpot] / [Score] / [RobustZ] plus the canonical input
//     metric_kind constants, AND (b) [Planner] with the
//     orchestrator boundaries ([PolicyReader],
//     [MetricSampleReader], [FindingReader], [HotSpotWriter])
//     and concrete implementations: [StewardPolicyReader] for
//     the active-policy READ, [SQLMetricSampleReader] /
//     [SQLFindingReader] for metric+finding ingestion against
//     `clean_code.metric_sample` / `clean_code.finding`, and
//     [SQLHotSpotWriter] for the canonical
//     `hot_spot(hotspot_id, repo_id, sha, scope_id, score,
//     policy_version_id, created_at)` row emission. In-memory
//     variants of all three boundaries are also exposed for
//     test / scaffold-mode wiring.
//
//     The SQL readers enforce two architecture invariants the
//     application code cannot drop without breaking
//     reproducibility:
//
//   - [SQLMetricSampleReader] drives from
//     `metric_sample_active` and joins through `sample_id`
//     into `metric_sample`, so retracted samples are
//     filtered by the join itself (architecture G2 /
//     Sec 5.2.1 lines 991-1003 / tech-spec Sec 7.1.b
//     lines 1103-1119). The pointer table's primary key
//     is over the FULL `(repo_id, sha, scope_id,
//     metric_kind, metric_version)` quintuple, so multiple
//     co-active versions per `(scope, metric_kind)` are
//     allowed by the schema; the reader collapses them in
//     SQL via `DISTINCT ON (scope_id, metric_kind) ...
//     ORDER BY ... metric_version DESC` so the largest
//     active version wins deterministically (the
//     [MetricSampleReader] contract).
//
//   - [SQLFindingReader] adds a `policy_version_id` filter
//     scoped to the ACTIVE policy snapshot, so findings
//     produced by parallel evaluations against an
//     experimental policy at the same SHA do NOT inflate
//     the active-policy hot_spot's `finding_count` (the
//     architecture Sec 5.5.1 reproducibility invariant).
//
//   - **Stage 8.2 (this file set adds)** -- refactor-plan and
//     refactor-task generation. Ships
//     [TaskPlanner] / [PlanAndTasksResult] /
//     [RefactorPlan] / [RefactorTask] / [TaskKind] +
//     [CanonicalTaskKinds] / [RejectedTaskKindAliases] /
//     [ValidateTaskKind] / [DefaultTaskKindForRule] AND the
//     orchestration boundaries [HotSpotReader] (latest-batch
//     read for `clean_code.hot_spot`), [FindingDetailReader],
//     and [RefactorPlanTaskWriter] (the latter writes ONE
//     [RefactorPlan] row + N [RefactorTask] rows in a single
//     ATOMIC transaction per the rubber-duck Stage 8.2 design
//     review finding #1).
//
//     The [TaskPlanner.Plan] / [TaskPlanner.PlanFromSnapshot]
//     orchestrator READS the LATEST top-N [HotSpot] batch
//     written by Stage 8.1 [Planner.Plan] (via
//     [HotSpotReader.LatestHotSpotsByScore], pinned to the
//     same `policy_version_id` the Stage 8.1 pass wrote);
//     it never recomputes hot_spots and never writes
//     `clean_code.hot_spot`. Stage 8.1 [Planner.Plan]
//     remains the SOLE writer of the hot_spot table.
//     For each top-N hot_spot, the planner reads the
//     qualifying finding details, dedupes by `(scope_id,
//     rule_id)`, maps each unique rule_id to a canonical
//     [TaskKind] via [DefaultTaskKindForRule] (with a
//     configured fallback for unmapped rules), validates
//     every emitted kind against the canonical enum, and
//     writes the plan + tasks atomically.
//
//     The race-safe two-pass composition root (production
//     binary `cmd/clean-code-refactor-planner`) calls
//     [Planner.Plan] then
//     [TaskPlanner.PlanFromSnapshot](snap = planRes.Snapshot)
//     so the same `policy_version_id` is pinned across both
//     passes; a concurrent `policy.activate` between the
//     two passes cannot produce a torn plan whose hot_spots
//     were scored by one PV and whose top-N truncation used
//     another PV's `top_n`.
//
//     A hot_spot with NO qualifying findings (metric-only
//     signal) is STILL listed in
//     [RefactorPlan.HotspotIDs] but emits ZERO tasks: the
//     planner refuses to fabricate a synthetic rule_id
//     (rubber-duck Stage 8.2 design review finding #2 -- a
//     synthetic rule_id would violate the logical FK to
//     `rule.rule_id`).
//
//     The canonical `task.kind` enum is exactly the
//     five-value closed set per architecture Sec 5.5.3 line
//     1274:
//     `split_class | extract_method | invert_dependency |
//     break_cycle | consolidate_duplication`.
//     The iter-3 alias set
//     `extract_function | introduce_interface |
//     reduce_inheritance | reduce_coupling | reduce_lcom |
//     reduce_duplication` is REJECTED by
//     [ValidateTaskKind] via [ErrRejectedTaskKindAlias];
//     unknown kinds (typo or future-spec drift) surface
//     [ErrUnknownTaskKind]. The [SQLRefactorPlanTaskWriter]
//     runs the validator before opening its transaction so a
//     buggy custom rule mapper aborts the whole batch rather
//     than landing a partial row set.
//
//   - **Stage 8.3 / Stage 9.3** -- ML effort-model loader and
//     version pinning. [effort_model.go] declares the
//     [EffortModel] interface and three concrete
//     implementations -- [ZeroEffortModel] (preserves the
//     Stage 8.2 "unestimated" placeholder), [HeuristicEffortModel]
//     (deterministic per-kind+score formula, no external
//     artefact), and [MLEffortModel] (v0 hash-based estimator
//     that loads the artefact named by [config.EnvMLModelURI]
//     and pins its version against [config.EnvMLModelVersion]).
//     [NewEffortModelFromConfig] selects the implementation
//     from the operator pin `refactor-effort-source`
//     (architecture Sec 1.6). The Stage 9.3 [TaskPlanner]
//     invokes the configured model per task to stamp
//     `refactor_task.effort_hours`; a non-nil estimate error
//     aborts the WHOLE atomic plan + tasks write so a row
//     never lands with a bogus value. The model version is
//     pinned in `policy_version.refactor_weights.effort_model_version`
//     and reproducibility is preserved by traversing
//     `refactor_task -> refactor_plan -> hot_spot.policy_version_id ->
//     policy_version.refactor_weights.effort_model_version`;
//     [MLEffortModel.Estimate] returns
//     [ErrMLModelVersionMismatch] when the loaded artefact's
//     version diverges from the policy snapshot's pin.
//
// # Composite hotspot score (architecture Sec 3.9 lines 602-613)
//
// Per scope:
//
//	score = alpha * complexity_z
//	      + beta  * churn_z
//	      + gamma * coupling_z
//	      + delta * finding_count
//
// where `alpha`, `beta`, `gamma`, `delta` are the per-policy
// weights from [steward.PolicyVersion.RefactorWeights]
// (architecture Sec 5.3.3) and the `_z` suffixes are robust
// z-scores over the repo's foundation-tier distribution. The
// `finding_count` term is the RAW count of qualifying
// `finding` rows for the scope -- NOT z-scored. The four-term
// linear combination is normative; the package's [Score]
// helper applies it verbatim.
//
// # Canonical input metric_kinds
//
//   - **complexity** raw value =
//     `cyclo` + `cognitive_complexity` (architecture Sec 1.4.1
//     rows 1, 6; the foundation-tier pack pins both). When only
//     one of the two is present for a scope, the present value
//     is used as the raw complexity; when both are absent the
//     scope contributes z=0 to the composite (see
//     [ScopeInputs.RawComplexity]). Sum aggregation is the
//     canonical operation; pluggability is intentionally NOT
//     exposed so reproducibility is preserved across replays
//     (a `Computer` re-derived against the same `policy_version`
//     and the same `MetricSample` rows MUST produce
//     byte-identical scores -- per architecture G6).
//
//   - **churn** raw value = `modification_count_in_window`
//     (architecture Sec 1.4.1 row 12). The `window_days`
//     parameter is pinned at
//     `PolicyVersion.refactor_weights.window_days` (Section
//     5.3.3) and consumed by the Metric Ingestor's churn
//     materialiser; the planner reads whatever
//     `modification_count_in_window` rows the materialiser has
//     already written.
//
//   - **coupling** raw value =
//     `coupling_between_objects` + `fan_out` (architecture Sec
//     1.4.1 rows 8, 9). Same sum-aggregation rationale as
//     complexity.
//
//   - **finding_count** raw value = COUNT of `finding` rows
//     joined on `(repo_id, sha, scope_id)` where
//     `delta IN ('newly_failing', 'new')` per architecture Sec
//     5.4.1 lines 1186-1190 canonical delta enum. The
//     `unchanged` and `resolved` delta values are NOT counted
//     (the latter would invert the signal: a freshly-resolved
//     finding is a HEALING signal, not a hotspot driver). The
//     [HotSpotQualifyingDeltas] slice + [IsHotSpotQualifyingDelta]
//     helper are the canonical filter; consumers MUST use them
//     rather than open-coding the string set.
//
// # Robust z-score (architecture Sec 3.9 line 613)
//
// The architecture says "robust z-scores over the repo's
// foundation-tier distribution". [RobustZ] applies the
// textbook MAD-based form:
//
//	z = (x - median) / (1.4826 * MAD)
//
// where `MAD = median(|x_i - median|)` and the factor
// `1.4826 = 1 / Phi^{-1}(0.75)` makes MAD asymptotically
// equivalent to the standard deviation for normally
// distributed inputs. The factor is well-known in robust
// statistics; pinning it as a package constant ([madToSigma])
// keeps the dependency explicit.
//
// # MAD = 0 fallback
//
// When MAD evaluates to zero, two cases need different
// handling:
//
//   - **Constant distribution** (all values identical) --
//     there is no signal, every scope receives `z=0`.
//
//   - **Sparse distribution with outliers** (e.g.
//     `[0, 0, 0, 100]`) -- MAD is 0 because the median and most
//     points coincide at 0, but the outlier `100` is clearly a
//     hotspot signal. Returning `z=0` would erase it. In this
//     case [RobustZ] falls back to the standard z-score
//     `(x - mean) / stddev`; when the standard deviation is
//     ALSO zero (the constant case), it finally returns 0.
//
// This dual-mode is critical for `modification_count_in_window`
// distributions in young repos where most scopes have never
// been touched (raw value 0) but a handful of hot files
// dominate. The rubber-duck design review caught this case
// (iter 1 rubber-duck finding #2) and the test
// `TestRobustZ_SparseOutlier` pins the behaviour.
//
// # Missing-vs-zero semantics
//
// [ScopeInputs] carries a `Has<Field>` companion bool for
// every nullable metric input so callers can distinguish
// "metric_sample row absent" from "metric_sample row present
// with value 0". Distributions are built ONLY from scopes with
// `Has<Field>=true`; scopes missing a dimension contribute
// `z=0` for that dimension (they are not silently treated as
// `value=0`, which would inflate the distribution's mass at
// zero and distort medians). This matches the architecture's
// G2 "active row" semantics: a row that does not exist is NOT
// a row with value 0. The test `TestComputer_Compute_MissingMetricsExcludedFromDistribution`
// pins the behaviour.
//
// # Policy-snapshot bundling
//
// [PolicySnapshot] bundles `PolicyVersionID` and `Weights`
// into one value the caller passes to [Computer.Compute]. A
// previous design accepted the two arguments separately; the
// rubber-duck review (iter 1 finding #3) caught the
// possibility that a caller could pass weights from one
// PolicyVersion and the id of another. Carrying the snapshot
// as one struct closes that gap at the type level.
//
// # Determinism (architecture G6)
//
// [Computer.Compute] sorts its output slice by `Score DESC,
// ScopeID ASC` so identical inputs produce a deterministic
// ordering across calls. `HotSpot.CreatedAt` is taken from a
// single `now()` reading at the start of `Compute` so every
// row in a batch carries the same timestamp; the
// rubber-duck review (iter 1 finding #8) pinned this detail.
// Byte-identical row content across calls requires the caller
// to inject a deterministic id factory ([WithIDFactory]) and
// clock ([WithClock]); the default factories are
// `uuid.NewV4` and `time.Now`.
//
// # eval.gate carve-out
//
// The Refactor Planner runs OUT OF BAND of `eval.gate`. The
// `eval.gate` HTTP response is shaped purely from
// `evaluation_run` / `evaluation_verdict` / `finding` rows;
// `hot_spot` rows are never read by the gate. This package
// therefore has NO dependency on the `eval.gate` hot path and
// NO degraded-reason taxonomy of its own (the four-value set
// `samples_pending | policy_signature_invalid |
// xrepo_edges_unavailable | ast_subprocess_unavailable` is the
// gate's concern, NOT the planner's).
package refactor
