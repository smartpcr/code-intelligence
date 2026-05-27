// Package management exposes the clean-code service's read-side
// verbs. Stage 5.1 ships the first verb -- `policy.keys.list_active`
// -- which returns the active signing-key inventory the Policy
// Steward maintains. Later stages append additional read verbs
// to the same [Reader] surface and additional HTTP handlers to
// the same [Handler] mux.
//
// Read verbs in this service are deliberately side-effect free.
// Every method on [Reader] returns canonical state derived from
// authoritative subsystems (the signing key cache, the
// PostgreSQL store, ...) but never mutates them. Mutations land
// in the writer-side packages (`internal/aggregator`,
// `internal/rule_engine`, the Policy Steward's rotation path,
// etc.) and surface in the audit log; this package is the
// boundary that a dashboard / operator-CLI / SRE-runbook hits
// when it wants to know "what is the current state of X".
//
// # Canonical transport (v1): HTTP/JSON
//
// Stage 5.1 ships HTTP/JSON as the canonical and only transport
// for the management verbs. The wire shape is:
//
//	GET /v1/policy/keys/list_active
//	-> 200 application/json
//	   [{"key_id":"<uuid>","fingerprint":"<hex64>",
//	     "valid_from":"<rfc3339>","valid_until":"<rfc3339>"}]
//
// The body is a BARE JSON array (NOT enveloped) -- pinned this
// way by the workstream brief verbatim and locked by
// `TestHandler_ListActiveBareJSONArray`. The `/v1/` path prefix
// reserves room for a future shape change to ship under `/v2/`
// without breaking the live dashboards.
//
// # gRPC: out-of-scope for Stage 5.1 (future workstream)
//
// The original tech-spec narrative referenced a gRPC/proto layer
// for management verbs. Stage 5.1 deliberately PINS HTTP/JSON v1
// as the SOLE transport for `policy.keys.list_active`: no
// `*.proto` file or gRPC server exists in this service, and the
// dashboards / operator-CLI tooling that consume the verb are
// already HTTP-based. Shipping gRPC speculatively alongside HTTP
// would create wire-shape drift between two transports that have
// to be kept in sync.
//
// A gRPC adapter is OUT-OF-SCOPE for Stage 5.1 and would land as
// a separate downstream workstream if and only if a future
// consumer service requires streaming or strong-typed verbs.
// Until that workstream materialises, [Handler] is the canonical
// and ratified transport surface. Refer to
// `services/clean-code/docs/runbook.md` for the operator-facing
// version of this narrative.
package management
