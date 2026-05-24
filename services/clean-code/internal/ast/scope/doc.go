// Package scope owns the deterministic identity and
// canonical-signature derivation for every Measurement
// `scope_binding` row the clean-code service writes
// (architecture Sec 5.2.3 lines 1039-1050).
//
// # Identity (G2 stability)
//
// `scope_id` is a deterministic UUIDv5 of
// `(repo_id, scope_kind, canonical_signature, first_seen_sha)`
// per architecture Sec 5.2.3 line 1044. SHA is NOT part of
// `scope_id`: `first_seen_sha` is the SHA at which this signature
// FIRST appeared (immutable column on `scope_binding`). Subsequent
// SHAs that contain the same `(repo_id, scope_kind,
// canonical_signature)` resolve to the SAME `scope_id` PROVIDED
// the caller passes the cached `first_seen_sha` rather than the
// current SHA. This is G2 -- "stable across SHAs" -- and the
// `storage.ScopeBindingWriter` is the surface that enforces it
// (the writer first looks up the natural-key tuple, reuses the
// existing `first_seen_sha`, and only mints a new value when the
// signature is brand new).
//
// # Canonical signature recipe (linked-mode parity)
//
// `canonical_signature` is built per scope_kind using the SAME
// recipe agent-memory uses for its `Node.canonical_signature`
// (architecture Sec 5.2.3 line 1047). When clean-code is running
// in `linked` mode the produced string is bit-identical to what
// agent-memory would emit for the same logical scope, so the
// cross-service `agent_memory_node_id` link in
// `scope_binding.agent_memory_node_id` is stable. The recipe
// (anchor: services/agent-memory/internal/repoindexer/ast/doc.go
// "Canonical signature scheme"):
//
//	repo:      <repoURL>
//	pkg:       <repoURL>::pkg::<dir>
//	file:      <repoURL>::file::<relPath>
//	class:     <repoURL>::class::<relPath>#<normalisedQN>
//	interface: <repoURL>::class::<relPath>#<normalisedQN>   (same as class -- agent-memory parity)
//	method:    <repoURL>::method::<relPath>#<normalisedQN>(<normalisedParams>)
//	block:     <methodSig>#block_<ordinal>_<kind>           (ordinal is 0-based)
//
// Whitespace and inline comments inside `qualifiedName` and
// `params` are collapsed via [NormalizeSignature] (mirroring
// agent-memory's `NormalizeSignature`) before they enter the
// canonical-signature string, so a formatter-only commit produces
// a byte-identical signature -- the §9.7 / §9.9 stability
// mitigation. Paths (`dir`, `relPath`) are NOT normalised: the
// parser layer is the source of truth for forward-slash repo-
// relative paths, and re-normalising them through the punctuation
// stripper could mangle a legitimate path containing `,` `:` `;`
// or `<>` characters.
//
// Note that `interface` shares the `::class::` discriminator with
// `class` -- agent-memory's `classSignature` mints the canonical
// signature for "a Class / Interface node" without distinction
// (services/agent-memory/internal/repoindexer/ast/dispatcher.go),
// so linked-mode parity requires the SAME discriminator string.
// The clean-code `scope_kind` enum still tracks them separately
// for downstream classification, and the `scope_id` derivation
// includes `scope_kind` in its pre-image, so a class and an
// interface with the same qualifiedName get DIFFERENT `scope_id`s
// even though their `canonical_signature` strings match. Block
// ordinals are 0-based (agent-memory `Block.Ordinal` doc:
// "0-based position of this Block within its enclosing Method's
// Block list"); `BuildBlock` rejects negative ordinals but
// accepts 0.
//
// # ScopeKind discriminator
//
// The canonical seven-value enum `repo | package | file | class |
// interface | method | block` (architecture Sec 5.2.3 line 1046)
// is mirrored as the [Kind] typed string here. The DB ENUM
// `clean_code.scope_kind` carries the same string values so a
// `Kind` value can be sent through `database/sql` parameters as
// plain text and the PostgreSQL enum cast accepts it.
package scope
