package management

import (
	"context"
	"fmt"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management/insights"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// OverrideReaderFromStore is the production bridge from a
// Policy [steward.Store] to the [insights.OverrideReader]
// contract consumed by the aged-mute projection (Stage 10.2).
//
// Why does this adapter live in `internal/management/` rather
// than in `internal/management/insights/`? The `insights`
// package is held to a STRICT import-isolation invariant:
// zero non-stdlib deps AND zero deps on sibling internal
// packages, so the projection is testable as a pure value
// projection over the [insights.OverrideRecord] value type.
// If the adapter lived inside `insights`, it would need to
// `import .../policy/steward`, which would (a) break the
// isolation invariant, (b) introduce a circular-package risk
// down the line (steward already depends on lower-level
// shared types), and (c) couple the insights projection's
// test surface to the full Steward Store contract.
//
// Instead, this adapter is owned by the management package
// (which is allowed to import both `insights` and `steward`).
// The composition root wires:
//
//	storeAdapter := &OverrideReaderFromStore{Store: stewardStore}
//	agedMutes    := insights.NewAgedMutes(storeAdapter, nil)
//	reader       := management.NewReader(km,
//	                  management.WithAgedMutes(agedMutes))
//
// The adapter is a thin value-type mapper -- it does not cache,
// retry, or filter; the projection drives those concerns.
//
// Nil-Store handling: an adapter with a nil [Store] always
// returns [ErrAgedMuteOverrideStoreUnavailable] from
// [ListAllOverrides], which mirrors the management-surface
// "unwired -> 503" convention by returning an error that the
// insights projection wraps and the Reader maps to
// [ErrBackendUnavailable] for the HTTP layer.
type OverrideReaderFromStore struct {
	// Store is the steward substrate. Nil is permitted at
	// construction (scaffold-mode bring-up); calls then
	// surface [ErrAgedMuteOverrideStoreUnavailable].
	Store steward.Store
}

// ErrAgedMuteOverrideStoreUnavailable is the sentinel returned
// by [OverrideReaderFromStore.ListAllOverrides] when the
// adapter was wired with a nil [steward.Store]. The
// projection wraps it; the HTTP layer maps the wrapped error
// to [ErrBackendUnavailable] (503 Service Unavailable),
// matching the metrics-backend "unwired -> 503" convention.
var ErrAgedMuteOverrideStoreUnavailable = fmt.Errorf(
	"management: aged-mute override store not wired (composition-root bug)")

// ListAllOverrides implements [insights.OverrideReader] by
// delegating to the underlying [steward.Store.ListAllOverrides]
// and mapping each [steward.Override] row to an
// [insights.OverrideRecord] value. The mapping is
// field-for-field; OverrideID is canonicalised from
// `uuid.UUID` to its lowercase 36-char string form so the
// projection's `(CreatedAt, OverrideID)` tie-break is a
// stable lexicographic compare across InMemory + SQL stores
// (the SQL `ORDER BY override_id ASC` is text-ordered;
// `uuid.UUID.String()` produces the same canonical form
// PostgreSQL emits when casting `uuid -> text`).
//
// Returns an empty (non-nil) slice when the steward log is
// empty -- mirrors the projection's "empty result -> []"
// JSON-stability contract.
func (a *OverrideReaderFromStore) ListAllOverrides(ctx context.Context) ([]insights.OverrideRecord, error) {
	if a == nil || a.Store == nil {
		return nil, ErrAgedMuteOverrideStoreUnavailable
	}
	rows, err := a.Store.ListAllOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("management: OverrideReaderFromStore: %w", err)
	}
	out := make([]insights.OverrideRecord, len(rows))
	for i, r := range rows {
		out[i] = insights.OverrideRecord{
			OverrideID: r.OverrideID.String(),
			RuleID:     r.RuleID,
			Scope: insights.OverrideScope{
				RepoID:             r.ScopeFilter.RepoID,
				ScopeKind:          string(r.ScopeFilter.ScopeKind),
				ScopeSignatureGlob: r.ScopeFilter.ScopeSignatureGlob,
			},
			Mute:      r.Mute,
			Reason:    r.Reason,
			ActorID:   r.ActorID,
			CreatedAt: r.CreatedAt,
		}
	}
	return out, nil
}

// Compile-time check that OverrideReaderFromStore satisfies
// the [insights.OverrideReader] contract. If the insights
// interface ever grows a method, this line trips a build
// error at the composition-root layer rather than at runtime
// inside the aged-mute projection.
var _ insights.OverrideReader = (*OverrideReaderFromStore)(nil)
