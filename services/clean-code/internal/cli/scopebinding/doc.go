// -----------------------------------------------------------------------
// <copyright file="doc.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package scopebinding -- canonical anchor index.
//
// This `doc.go` complements `scopebinding.go` (which carries the
// concrete [ScopeBinding] row, [MintScopeID] / [TryMintScopeID],
// and both the concurrent [Table] and the simple [Store] keyed
// containers) by giving godoc a stable, table-of-contents-style
// entry point that lists every spec anchor the package observes
// and every invariant it guards.
//
// # Spec anchors
//
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 1.4 row G2 ("stable identifiers") -- `scope_id` is a
//     deterministic UUID-v5 over the
//     `(repo_id, scope_kind, canonical_signature, first_seen_sha)`
//     tuple so re-runs on the same checkout mint identical scope
//     ids and the rule engine's delta path stays meaningful.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 4.2 ("ScopeBinding") -- the seven-field row shape
//     mirrored by [ScopeBinding] (ScopeID, ScopeKind, FilePath,
//     StartLine, EndLine, Signature, Language).
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 4.11 ("stable identity") and constraint C3 -- pin the
//     UUID-v5 derivation algorithm and the rule that the
//     scope-id pre-image MUST be canonical across hosts.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md`
//     Stage 1.2 -- the workstream that introduces this package.
//
// # Invariants guarded
//
//   - [MintScopeID] panics on `uuid.Nil` repo id (LOUD failure;
//     the orchestrator must mint the repo id BEFORE any scope id
//     so a nil here is a wiring bug). [TryMintScopeID] is the
//     error-returning variant used when the caller wants to
//     translate the same condition into an operator-facing
//     diagnostic instead of crashing.
//   - The `scopeNamespace` UUID is a fixed constant
//     (`6ba7b810-9dad-11d1-80b4-00c04fd430c8`, the canonical
//     URL namespace) so the derivation can never silently
//     change. A future renaming or namespacing change would be
//     a hard cross-version migration and is intentionally loud
//     to make.
//   - [Table.Insert] returns [ErrZeroScopeID] for a zero
//     ScopeID and does NOT mutate the table; this surfaces
//     the orchestrator wiring bug (forgot to mint the scope
//     id before insert) as a caller-visible diagnostic
//     rather than a silent no-op. Callers can
//     `errors.Is(err, ErrZeroScopeID)` to discriminate the
//     wiring-bug failure from any future I/O-shaped error a
//     Stage 1.2 backing store may add behind the same
//     signature. [Table.Len] is accurate even under
//     concurrent Insert calls.
//
// # Sibling packages
//
//   - `internal/cli/repocontext` -- the upstream producer of the
//     `repo_id` value consumed by [MintScopeID].
//   - `cmd/cleanc` -- the dispatcher; later stages of the
//     pipeline (Stage 2.x orchestrator) populate a [Table]
//     during the parse pass and consult it during the
//     refactor-task pass to map findings back to source
//     locations.
package scopebinding
