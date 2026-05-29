// Package linked is the optional adapter that wraps the
// `services/agent-memory/` cross-repo edge surface for the
// Clean Code Cross-Repo Aggregator (Stage 7.2).
//
// # Architecture pin
//
// Architecture Sec 1.6 normative operator pin
// `ast-mode-default=embedded`: in default v1 deployments the
// Clean Code service runs WITHOUT an agent-memory dependency
// and the system-tier composer's cross-repo-edge-dependent
// metric_kinds (`xrepo_dep_depth`, `blast_radius`) are
// emitted with `degraded=true,
// degraded_reason='xrepo_edges_unavailable'` per Sec 8.2 / Sec
// 3.10 step 4. Architecture Sec 8.7 ("Optional agent-memory
// composition (`linked` mode)") names the opt-in mode in which
// the AST adapter joins to `agent-memory.GraphReader` for
// cross-repo edges; THIS package is the Clean Code-side client
// that consumes that surface during cross-repo aggregation.
//
// # Two-axis gating
//
// Linked-mode usage is gated on TWO INDEPENDENT axes; the
// adapter MUST observe both before issuing any HTTP call:
//
//  1. Global config flag -- `config.EnableLinkedModeAdapter`
//     (env: `CLEAN_CODE_ENABLE_LINKED_MODE_ADAPTER`). Default
//     false. When false the [AggregatorAdapter] short-circuits
//     to `Applicable=false` for every repo so the aggregator
//     never reads per-repo mode, never dials agent-memory, and
//     the composer's embedded-mode degradation path fires
//     uniformly. The composition root MUST refuse to set this
//     flag true without also supplying
//     `config.LinkedAgentMemoryEndpoint` (interlock enforced
//     in `internal/config`).
//
//  2. Per-repo `clean_code.repo.mode = 'linked'` -- flipped
//     via `mgmt.set_mode(repo_id, 'linked')`
//     (`internal/management/set_mode_verb.go`). Repos still
//     at `mode='embedded'` (the default for newly registered
//     repos per architecture Sec 1.6) NEVER hit the linked
//     code path even when the global flag is true. The
//     adapter reads the mode via the narrow
//     [RepoModeReader] interface (`ReadRepoMode`), which is
//     duck-typed onto `management.RepoModeReader`.
//
// Both gates default to closed: a fresh deployment runs in
// embedded mode end-to-end with zero agent-memory dial
// attempts. Flipping a single axis is insufficient -- the
// operator must explicitly enable the adapter AND mark a repo
// `linked` before any cross-repo edge flows through.
//
// # Fail-safe contract (architecture Sec 3.10 step 4)
//
// When the adapter is engaged for a repo (both gates open) and
// the agent-memory surface fails (network error, 5xx, 404
// "edges not indexed for this SHA", malformed JSON), the
// adapter wraps the underlying error and returns it to the
// aggregator WITHOUT marking the result Applicable. The
// aggregator's [aggregator.Aggregator.Tick] distinguishes
// CONTEXT-CANCEL / DEADLINE-EXCEEDED errors (which abort the
// tick) from REMOTE errors (which leave the affected input in
// embedded shape so the composer naturally degrades the row
// with `xrepo_edges_unavailable`). This honours the Sec 3.10
// step 4 lines 637-657 "fail-safe" contract: the composer
// NEVER silently drops a system-tier row -- it emits a
// degraded one and downstream eval.gate + Insights surface the
// freshness state.
//
// Mode-store errors (the per-repo `ReadRepoMode` call into
// `internal/management`) are deliberately handled
// DIFFERENTLY: they are propagated as fatal because a broken
// catalog is not a fail-safe-to-degraded condition -- the
// operator wants the tick to fail loudly rather than silently
// force every linked-marked repo into embedded behaviour.
//
// # Wire contract (Sec 8.7 "linked-mode endpoint")
//
// One HTTP verb per `(repo_id, sha)`:
//
//	GET <endpoint>/v1/cross-repo/edges?repo_id={uuid}&sha={sha}
//
//	200 OK
//	  Content-Type: application/json
//	  {
//	    "xrepo_edges":          [{"from_repo": "<uuid>", "to_repo": "<uuid>"}, ...],
//	    "xrepo_edges_available": true,
//	    "call_edges":           [{"from_scope": "<uuid>", "to_scope": "<uuid>"}, ...],
//	    "call_edges_available":  true
//	  }
//
//	404 -> [ErrEdgesUnavailable] (NOT "this repo has no edges";
//	       this means "agent-memory has not indexed this
//	       repo+sha pair OR the endpoint route is wrong"). The
//	       aggregator degrades the affected rows. To express
//	       "valid pair with zero edges", the endpoint MUST
//	       return 200 with empty arrays and the availability
//	       flags set TRUE.
//
//	other non-2xx -> [ErrUnexpectedStatus]
//	malformed JSON -> [ErrMalformedResponse]
//
// The two availability flags are emitted PER EDGE FAMILY so
// the aggregator can degrade `xrepo_dep_depth` and
// `blast_radius` independently when agent-memory partially
// indexes a repo (e.g. cross-repo dependencies indexed but
// call graph still building). An omitted flag in the JSON
// response defaults to FALSE so the aggregator degrades
// rather than silently emitting non-degraded zeros (which
// would be a correctness bug -- "missing" must never be
// interpreted as "available empty").
//
// # No agent-memory dependency in embedded mode
//
// `services/clean-code/` does NOT import
// `services/agent-memory/` Go packages. The two services
// communicate over HTTP through this adapter; nothing in the
// embedded-mode hot path takes a build-time agent-memory
// dependency. This honours the architecture Sec 1.6
// "standalone by default" pin: a `go build ./...` of the
// clean-code service must succeed without the agent-memory
// module present.
package linked
