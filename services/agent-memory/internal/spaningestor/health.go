package spaningestor

// Cross-process health-state READ surface. The agent-api recall
// handler consults this to populate `RecallResponse.degraded`;
// the Span Ingestor binary writes via
// `graphwriter.UpsertRepoHealth`. The two binaries do not share
// memory — Postgres is the rendezvous.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HealthState is the per-repo degraded view the recall handler
// surfaces on the response envelope. Mirrors the schema row
// shape minus the timestamps the protocol does not care about.
type HealthState struct {
	Degraded bool
	// Reason is the `degraded_reason` ENUM literal; empty
	// when !Degraded.
	Reason string
	// Source identifies which worker class set the flag.
	// Free-form text (see migration 0019 column-rationale
	// header). Populated even for !Degraded.
	Source string
}

// HealthSource is the read-side abstraction the agent-api
// recall handler consumes. The production implementation is
// PGHealthSource (below); tests pass an in-memory fake.
type HealthSource interface {
	// HealthForRepo returns the current degraded state for
	// the supplied repo_id. Returns (false-default, nil) when
	// the row does not exist (no signal yet) — this is the
	// "healthy by default" semantics that
	// recall.go.RecallResponse depends on.
	//
	// Backend errors (network, SQL) MUST be returned to the
	// caller; recall.go logs and proceeds without surfacing
	// the flag (the rubber-duck pass on the recall path
	// agreed: a backend outage of the health table should
	// NOT also fail the recall response).
	HealthForRepo(ctx context.Context, repoID string) (HealthState, error)
}

// PGHealthSource is the production HealthSource. Reads via the
// `agent_memory_ro` role. Construct via NewPGHealthSource.
type PGHealthSource struct {
	db *sql.DB
}

// NewPGHealthSource constructs a PG-backed HealthSource.
// The `*sql.DB` MUST be authenticated as a role with SELECT on
// `repo_health`; per migration 0017 the default-privileges rule
// automatically grants this to `agent_memory_ro`.
func NewPGHealthSource(db *sql.DB) *PGHealthSource {
	if db == nil {
		panic("spaningestor: NewPGHealthSource: nil *sql.DB")
	}
	return &PGHealthSource{db: db}
}

// HealthForRepo implements HealthSource.
func (s *PGHealthSource) HealthForRepo(
	ctx context.Context, repoID string,
) (HealthState, error) {
	const q = `
		SELECT degraded, COALESCE(degraded_reason::text, ''), source
		FROM repo_health
		WHERE repo_id = $1
	`
	var st HealthState
	err := s.db.QueryRowContext(ctx, q, repoID).
		Scan(&st.Degraded, &st.Reason, &st.Source)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return HealthState{}, nil
	case err != nil:
		return HealthState{}, fmt.Errorf("spaningestor: HealthForRepo: %w", err)
	}
	return st, nil
}
