// Package scopebinding holds the process-local mirror of the
// production `clean_code.scope_binding` sub-store. The CLI
// composition root populates the [Table] during parse + recipe
// fan-out (Stage 2.2); downstream consumers (the prompt
// emitter, the markdown / JSON renderers) resolve a
// `RefactorTask.ScopeID` back to its `(file_path, start_line,
// end_line, signature, language)` tuple through [Table.Get].
//
// Anchors: REFACTOR-GUIDE `architecture.md` Sec 4.3 (in-memory
// ScopeBinding shape); Sec 3.7.3 (prompt emitter resolves
// ScopeID via this table); `tech-spec.md` constraint C3 (G2
// identity).
package scopebinding

import (
	"fmt"
	"sync"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// ScopeBinding is the in-memory mirror of one row in
// `clean_code.scope_binding`. The fields match
// REFACTOR-GUIDE `architecture.md` Sec 4.3:
//
//   - `ScopeID` is the G2 identity minted via [MintScopeID].
//   - `ScopeKind` is one of the closed CLEAN-CODE arch G2
//     kinds: `method`, `class`, `interface`, `file`,
//     `package`, `repo`.
//   - `Signature` is the human-readable canonical signature
//     (e.g. `services/clean-code/internal/foo/bar.go::Bar.Compute`).
//   - `FilePath` is repo-relative.
//   - `StartLine` / `EndLine` are 1-indexed.
//   - `Language` echoes the parent file's language token.
type ScopeBinding struct {
	ScopeID   uuid.UUID
	ScopeKind string
	Signature string
	FilePath  string
	StartLine int
	EndLine   int
	Language  string
}

// Table is the process-local map from `ScopeID` to
// [ScopeBinding]. Built on top of [sync.Map] so the parse +
// recipe fan-out workers (sized to `GOMAXPROCS` per
// `architecture.md` Sec 10) can [Insert] concurrently while
// the orchestrator [Get]s on the main goroutine without
// guarding the map itself.
//
// The zero value is ready to use; callers SHOULD construct
// one [Table] per CLI invocation and pass it through the
// pipeline.
type Table struct {
	m sync.Map
}

// NewTable returns a freshly-allocated [Table]. Callers MAY
// also use the zero value (`var t scopebinding.Table`); the
// constructor exists so call-sites that want a pointer can
// read more idiomatically.
func NewTable() *Table { return &Table{} }

// Insert stores `binding` in the table keyed by its
// [ScopeBinding.ScopeID]. The last-writer wins when two
// bindings share a `ScopeID`; this is intentional and safe
// because [MintScopeID]'s pre-image collapses any
// "logically the same scope" tuple onto a single id, so a
// second insert can only carry equal data (or differ in a
// non-load-bearing field like `EndLine` for a partially
// indexed file).
//
// Insert is a no-op when `binding.ScopeID` is the zero UUID
// to avoid masking a programmer bug upstream (an unminted
// scope id) with a silently overwritten entry; the
// orchestrator validates the id BEFORE calling Insert.
func (t *Table) Insert(binding ScopeBinding) {
	if binding.ScopeID == uuid.Nil {
		return
	}
	t.m.Store(binding.ScopeID, binding)
}

// Get returns the [ScopeBinding] previously [Insert]ed for
// `id` and `(zero-value, false)` if no binding is present.
//
// The lookup is the prompt emitter's hot path (one call per
// `RefactorTask` in the planner output); it MUST stay
// allocation-free under the [sync.Map] read path.
func (t *Table) Get(id uuid.UUID) (ScopeBinding, bool) {
	v, ok := t.m.Load(id)
	if !ok {
		return ScopeBinding{}, false
	}
	binding, ok := v.(ScopeBinding)
	if !ok {
		return ScopeBinding{}, false
	}
	return binding, true
}

// Len returns the number of bindings currently stored. Useful
// for tests and for the `--diagnostics` JSON sink that wants
// to surface "N scopes bound" alongside the dark-metric
// diagnostics; not part of the hot path.
func (t *Table) Len() int {
	var n int
	t.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// MintScopeID derives a deterministic UUID-v5 `scope_id` for
// the `(repoID, scopeKind, canonicalSignature, firstSeenSHA)`
// tuple per CLEAN-CODE arch G2.
//
// Delegates to [scope.DeriveScopeID] so the CLI shares the
// SINGLE source of truth for the production identity
// algorithm; reproducing the byte-framing here would risk
// silent drift if the production pre-image is ever bumped.
// The four pre-conditions [scope.DeriveScopeID] enforces
// (non-zero repoID, valid kind, non-empty / NUL-free
// signature + sha) are programmer-supplied in this code
// path; a violation indicates the orchestrator handed in
// corrupt data, so the helper panics with the underlying
// error. Callers that want soft-fail semantics should use
// [TryMintScopeID].
//
// Anchor: `architecture.md` Sec 4.3, Sec 1.4 G2;
// `tech-spec.md` constraint C3 ("Scope IDs MUST follow the
// CLEAN-CODE G2 hash").
func MintScopeID(repoID uuid.UUID, scopeKind, canonicalSignature, firstSeenSHA string) uuid.UUID {
	id, err := TryMintScopeID(repoID, scopeKind, canonicalSignature, firstSeenSHA)
	if err != nil {
		panic(fmt.Sprintf("scopebinding: MintScopeID(repoID=%s, kind=%q): %v", repoID, scopeKind, err))
	}
	return id
}

// TryMintScopeID is the error-returning variant of
// [MintScopeID]; orchestrator code paths that want to surface
// a `WalkSkip{Reason: "scope_id_mint_failed"}` rather than
// abort the run can call this directly.
func TryMintScopeID(repoID uuid.UUID, scopeKind, canonicalSignature, firstSeenSHA string) (uuid.UUID, error) {
	return scope.DeriveScopeID(repoID, scope.Kind(scopeKind), canonicalSignature, firstSeenSHA)
}
