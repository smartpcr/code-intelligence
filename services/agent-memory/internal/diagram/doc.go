// Package diagram defines the JSON envelope and Go types that the
// REPO-SCANNER diagram projector emits and that the React +
// neo4j-nvl UI consumes.
//
// One envelope, two diagram families. The module diagram
// (`Diagram.Diagram == "module"`) returns the repo's containment
// tree rolled up to a chosen granularity plus the aggregated
// `imports` edges between components. The call-chain diagram
// (`Diagram.Diagram == "callchain"`) returns a bounded BFS over
// `static_calls`/`observed_calls` rooted at a seed symbol, walked
// upstream and/or downstream. Both families MUST serialise into the
// identical key order so the UI ships exactly one parser; the order
// is asserted by the golden test in envelope_marshal_test.go and is:
//
//	diagram, repo, generatedAt, layoutHint, nodes, edges, truncated, stats
//
// Authoritative specs:
//
//   - REPO-SCANNER architecture S4.4 (`docs/stories/code-intelligence-REPO-SCANNER/architecture.md`)
//     pins the envelope shape, the layoutHint enum, the "synthetic ids"
//     rules for the memory + JSON backend and module-diagram roll-up
//     nodes (`pkg:<canonical_signature>`), and the requirement that
//     `truncated` + `stats.cappedAt` are populated whenever any
//     `graphreader.ListNodes` / `ListEdgesFrom` / `ListEdgesTo` call
//     hits `MaxListLimit = 10_000`.
//
//   - REPO-SCANNER tech-spec S6.2 (forward anchor in
//     `docs/stories/code-intelligence-REPO-SCANNER/tech-spec.md` S8)
//     owns the exhaustive per-field JSON schema, optionality rules,
//     and the error envelope shape that wraps this Diagram on serve
//     endpoints.
//
//   - REPO-SCANNER architecture S4.3 / S9.1 pin the `Repo.SHA`
//     identity for non-git local scans: a 32-char lowercase hex
//     `fingerprint.MTimeTreeSHA` computed before any
//     `Materializer.Materialize` call. The literal "local" sentinel
//     was considered and REJECTED -- every non-git scan would
//     collide under the same `(repo_id, sha)` key and break re-scan
//     dedupe. Git scans continue to use the 40-char commit SHA.
//
//   - REPO-SCANNER architecture S6 (Truncation MUST be visible) and
//     S7.3 explain why `truncated` and `stats.skipped` are mandatory
//     fields rather than omit-empty: the UI uses their presence to
//     decide whether to show the truncation badge and the
//     coverage-degradation banner.
//
// The Stats.Skipped sub-map keys are the dispatcher's well-known
// skip-reason strings (`no_parser`, `pwsh_not_available`). New
// reasons added in the dispatcher MUST also be mirrored here so
// the UI can render a degradation banner with full context.
package diagram
