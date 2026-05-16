package graphreader

// Concept reader path for the Concept layer (architecture.md
// §5.5.1). The Concept table is non-partitioned and append-only
// per the migration in 0011; this reader is the read-side
// dereference path used by the RecallContextLog Resolve helper
// (implementation-plan.md Stage 2.4) so a `mgmt.read.context`
// caller can rehydrate the Concept cards a recall context
// referenced.
//
// Concept has no retirement table — concepts are never retired,
// only re-versioned (a new `ConceptVersion` row supersedes the
// old). That is why `GetConcept` accepts no `ReaderOptions`:
// there is no `IncludeRetired` toggle to expose. The optional
// ConceptVersion join lives behind `mgmt.read.concepts`
// (architecture.md §6.2); `GetConcept` returns only the base row
// so the recall-context dereference is a single SELECT per id.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Concept is the read-shape of one row from the `concept` table
// (architecture.md §5.5.1). The fingerprint is exposed as the
// raw bytes the database stores; concept fingerprints are
// cross-repo per **G6** so there is no `repo_id` field.
type Concept struct {
	// ConceptID is the textual UUID of the concept row.
	ConceptID string
	// Fingerprint is the 32-byte canonical hash. Exposed as a
	// raw byte slice (not a typed Sum) because the concept
	// fingerprint domain is distinct from the structural-graph
	// fingerprint domain — concept fingerprints derive from the
	// canonical concept name + observed-feature signature
	// (architecture.md §5.5.1), not from the (repo, kind,
	// signature, sha) tuple `fingerprint.Sum` represents.
	Fingerprint []byte
	// Name is the human-readable label set at insert time.
	Name string
	// DescriptionMD is the markdown description set at insert
	// time.
	DescriptionMD string
	// CreatedAt is the server-side timestamp PostgreSQL stamped
	// at INSERT (`DEFAULT now()` on the column).
	CreatedAt time.Time
}

// GetConcept fetches a single Concept by id. Returns
// `ErrNotFound` when the row does not exist. Concepts have no
// retirement semantics (see package doc above) so this method
// takes no ReaderOptions — there is no opt-in toggle.
//
// Used by the recallcontext.Log Resolve helper to dereference
// the `concept_ids[]` column of a `recall_context_log` row into
// the Concept cards `mgmt.read.context` returns.
func (r *Reader) GetConcept(ctx context.Context, conceptID string) (Concept, error) {
	if conceptID == "" {
		return Concept{}, errors.New("graphreader: GetConcept: empty concept_id")
	}
	const q = `
		SELECT
			concept_id::text,
			fingerprint,
			name,
			description_md,
			created_at
		FROM concept
		WHERE concept_id = $1
	`
	var c Concept
	err := r.pool.QueryRow(ctx, q, conceptID).Scan(
		&c.ConceptID, &c.Fingerprint, &c.Name,
		&c.DescriptionMD, &c.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Concept{}, ErrNotFound
		}
		return Concept{}, fmt.Errorf("graphreader: GetConcept: %w", err)
	}
	return c, nil
}
