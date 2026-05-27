package aggregator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// ErrPGSystemTierInputSourceNilDB surfaces a nil *sql.DB at
// composition-root wiring time.
var ErrPGSystemTierInputSourceNilDB = errors.New("aggregator: NewPGSystemTierInputSource: *sql.DB is nil")

// ErrPGSystemTierInputSourceEmptySchema surfaces an empty
// schema name at wiring time.
var ErrPGSystemTierInputSourceEmptySchema = errors.New("aggregator: NewPGSystemTierInputSourceWithSchema: schema is empty")

// PGSystemTierInputSource is the production
// [SystemTierInputSource]. Per [ReadSystemTierInputs] it
// performs one tick's-worth of SELECTs against the Measurement
// + Catalog sub-stores and yields one [SystemTierInput] per
// `(repo_id, sha)` pair that the active set covers.
//
// # Embedded-mode v1
//
// Stage 7.2 ships an embedded-mode-only PG source: every
// returned input carries `Mode=SystemTierModeEmbedded`,
// `XRepoEdgesAvailable=false`, `CallEdgesAvailable=false`,
// and empty `XRepoEdges` / `CallEdges` slices. The composer
// degrades the cross-repo-edge-dependent kinds
// (`xrepo_dep_depth`, `blast_radius`) with
// `xrepo_edges_unavailable` accordingly per the architecture
// Sec 3.10 step 4 lines 637-657 fail-safe contract. A future
// Stage 8.x linked-mode adapter will replace this source with
// an implementation that fetches edges from the agent-memory
// service AND flips the availability flags true.
//
// # Read shape (per tick)
//
//  1. ONE SELECT against `metric_sample_active` (JOIN through
//     to `metric_sample` + LEFT JOIN
//     `metric_retraction` WHERE `mr.sample_id IS NULL` --
//     the canonical retracted-row anti-join used by
//     [PGSampleSource]) yields the DISTINCT
//     `(repo_id, sha)` pairs to compose for. Pairs with zero
//     non-retracted active rows do not appear (the
//     anti-join eliminates rows whose only active pointer
//     references a retracted sample).
//
//  2. ONE SELECT against `scan_run` resolves the
//     `producer_run_id` per pair: the most recent
//     `succeeded` run whose `to_sha` matches the pair's SHA.
//     When no such run exists for a pair (e.g. a row whose
//     producing scan was retracted before the active pointer
//     was rebuilt) the pair is SKIPPED for this tick -- the
//     composer's `producer_run_id` is a required NOT NULL FK
//     on the system-tier write side.
//
//  3. ONE SELECT against `scope_binding` per pair yields the
//     DISTINCT (scope_id, scope_kind) scopes the composer
//     needs to iterate over (per arch Sec 5.2.3, scope
//     identity is stable across SHAs -- scope_id rows are
//     append-only). Same retraction anti-join applies so
//     scopes referenced ONLY by retracted samples drop out.
//
//  4. ONE SELECT against the active foundation samples per
//     pair yields the `FoundationSample` slice -- the
//     foundation+ingested rows the composer reads
//     (`pack IN ('base', 'solid', 'ingested')`). System-pack
//     rows are deliberately EXCLUDED -- feeding system rows
//     back into the system-tier composer would create a
//     definitional cycle (per
//     `internal/aggregator/types.go` Observation doc). Same
//     retraction anti-join applies.
//
// # Transactional read consistency (G6)
//
// All four queries above run inside ONE read-only PG
// transaction at REPEATABLE READ isolation so the tick sees a
// single consistent snapshot of `metric_sample_active`,
// `metric_retraction`, `metric_sample`, `scope_binding`, and
// `scan_run`. Without the wrapping transaction a concurrent
// Metric Ingestor write between the repo+sha enumeration
// query and the per-pair scope/foundation queries could tear
// the read -- the composer would see a pair in step 1 whose
// scopes / foundation rows / scan_run anchor have advanced
// underneath it, breaking the deterministic-per-tick
// invariant. REPEATABLE READ is sufficient (we do not need
// SERIALIZABLE's predicate locking because the reads are
// idempotent); the tx is explicitly marked read-only so PG
// can optimise.
//
// # Determinism (G6)
//
// The source sorts each output slice by stable keys (repo_id
// / sha for the top-level slice; scope_id then metric_kind
// for the per-pair scope/sample slices) so two consecutive
// ReadSystemTierInputs calls against the same DB state
// return identical bytes. The composer's downstream
// determinism contract depends on this.
//
// # Concurrency
//
// Safe for concurrent invocation; each call opens its own
// `database/sql` connection from the pool. The aggregator
// calls the source once per tick in production, but the
// concurrent-safety property lets tests drive parallel ticks
// against the same source.
type PGSystemTierInputSource struct {
	db     *sql.DB
	schema string
}

// NewPGSystemTierInputSource wraps `db` using the canonical
// `clean_code` schema.
func NewPGSystemTierInputSource(db *sql.DB) (*PGSystemTierInputSource, error) {
	return NewPGSystemTierInputSourceWithSchema(db, pgDefaultSchema)
}

// NewPGSystemTierInputSourceWithSchema is the test-friendly
// schema-isolated constructor. Tests inject a non-default
// schema (e.g. `clean_code_aggregator_test`) to keep their
// sqlmock assertions visibly diff-able from production.
func NewPGSystemTierInputSourceWithSchema(db *sql.DB, schema string) (*PGSystemTierInputSource, error) {
	if db == nil {
		return nil, ErrPGSystemTierInputSourceNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGSystemTierInputSourceEmptySchema
	}
	return &PGSystemTierInputSource{db: db, schema: schema}, nil
}

// qual returns `"<schema>"."<table>"` with both halves
// individually quoted via [pq.QuoteIdentifier].
func (s *PGSystemTierInputSource) qual(table string) string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(table)
}

// repoShaPairsQuery enumerates the DISTINCT `(repo_id, sha)`
// pairs covered by the active sample set this tick. The JOIN
// against `metric_sample` materialises `sha` (which lives on
// the underlying sample row, not on the active pointer's
// shape). The LEFT JOIN against `metric_retraction` +
// `WHERE mr.sample_id IS NULL` is the canonical retracted-row
// anti-join (same pattern as [PGSampleSource.readActiveQuery]):
// a pair whose only active pointer references a retracted
// sample drops out so the composer never re-attempts to
// compose at a tombstoned SHA.
func (s *PGSystemTierInputSource) repoShaPairsQuery() string {
	return fmt.Sprintf(
		`SELECT DISTINCT ms.repo_id, ms.sha
		   FROM %s msa
		   JOIN %s ms ON ms.sample_id = msa.sample_id
		   LEFT JOIN %s mr ON mr.sample_id = msa.sample_id
		  WHERE mr.sample_id IS NULL
		  ORDER BY ms.repo_id, ms.sha`,
		s.qual("metric_sample_active"),
		s.qual("metric_sample"),
		s.qual("metric_retraction"),
	)
}

// producerRunQuery resolves the latest `succeeded` scan_run
// at `to_sha = $sha` for the pair. We deliberately constrain
// `status='succeeded'` so a half-finished or rolled-back scan
// run cannot anchor a system-tier composition.
func (s *PGSystemTierInputSource) producerRunQuery() string {
	return fmt.Sprintf(
		`SELECT scan_run_id
		   FROM %s
		  WHERE repo_id = $1
		    AND to_sha  = $2
		    AND status  = 'succeeded'
		  ORDER BY started_at DESC
		  LIMIT 1`,
		s.qual("scan_run"),
	)
}

// scopesQuery yields the DISTINCT (scope_id, scope_kind) for
// scopes referenced by the active set for the given pair.
// Sorted by scope_id for G6 determinism. Same retraction
// anti-join shape as [repoShaPairsQuery] so scopes referenced
// ONLY by retracted samples drop out of the composer's input
// set.
func (s *PGSystemTierInputSource) scopesQuery() string {
	return fmt.Sprintf(
		`SELECT DISTINCT sb.scope_id, sb.scope_kind::text
		   FROM %s msa
		   JOIN %s ms ON ms.sample_id = msa.sample_id
		   JOIN %s sb ON sb.scope_id  = ms.scope_id
		   LEFT JOIN %s mr ON mr.sample_id = msa.sample_id
		  WHERE ms.repo_id = $1 AND ms.sha = $2
		    AND mr.sample_id IS NULL
		  ORDER BY sb.scope_id`,
		s.qual("metric_sample_active"),
		s.qual("metric_sample"),
		s.qual("scope_binding"),
		s.qual("metric_retraction"),
	)
}

// foundationSamplesQuery yields the non-degraded, non-NULL
// foundation + ingested samples for the given pair. The pack
// filter EXCLUDES `system` rows (per the source's doc comment
// -- system-tier rows must never be re-fed to the system-tier
// composer; that would create a definitional cycle). The
// `value IS NOT NULL AND degraded = false` filter mirrors the
// G3 invariant on foundation rows. The LEFT JOIN +
// `mr.sample_id IS NULL` anti-join drops samples whose
// active pointer references a retracted row.
func (s *PGSystemTierInputSource) foundationSamplesQuery() string {
	return fmt.Sprintf(
		`SELECT sb.scope_id, sb.scope_kind::text, ms.metric_kind, ms.value, ms.attrs_json
		   FROM %s msa
		   JOIN %s ms ON ms.sample_id = msa.sample_id
		   JOIN %s sb ON sb.scope_id  = ms.scope_id
		   LEFT JOIN %s mr ON mr.sample_id = msa.sample_id
		  WHERE ms.repo_id = $1 AND ms.sha = $2
		    AND mr.sample_id IS NULL
		    AND ms.pack IN ('base', 'solid', 'ingested')
		    AND ms.value IS NOT NULL
		    AND ms.degraded = false
		  ORDER BY sb.scope_id, ms.metric_kind`,
		s.qual("metric_sample_active"),
		s.qual("metric_sample"),
		s.qual("scope_binding"),
		s.qual("metric_retraction"),
	)
}

// ReadSystemTierInputs implements [SystemTierInputSource].
// Performs the four canonical SELECTs documented on
// [PGSystemTierInputSource] inside ONE read-only REPEATABLE
// READ transaction so the per-tick reads cannot be torn by a
// concurrent Metric Ingestor write landing between the
// repo+sha enumeration and the per-pair scope / foundation /
// scan_run lookups. Skips repo+SHA pairs that have no
// `succeeded` scan_run anchor.
//
// Connection ownership: the *sql.DB is owned by the caller;
// this method does NOT call Close. The tx is rolled back on
// any error path AND on the happy path (read-only -- nothing
// to commit). Rows.Close is deferred on every cursor so a
// partial iteration on error releases the underlying
// connection back to the pool.
func (s *PGSystemTierInputSource) ReadSystemTierInputs(ctx context.Context) ([]SystemTierInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		ReadOnly:  true,
		Isolation: sql.LevelRepeatableRead,
	})
	if err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: BEGIN read-only repeatable-read: %w", err)
	}
	// Read-only tx -- nothing to commit. Rollback releases
	// the snapshot and returns the connection to the pool;
	// PG treats a Rollback on a read-only tx as a no-op
	// modulo snapshot release. Safe to call on the happy
	// path AND any error path.
	defer func() { _ = tx.Rollback() }()

	pairs, err := s.readRepoShaPairs(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	out := make([]SystemTierInput, 0, len(pairs))
	for _, p := range pairs {
		runID, ok, err := s.readProducerRunID(ctx, tx, p.repoID, p.sha)
		if err != nil {
			return nil, err
		}
		if !ok {
			// No succeeded scan_run anchors this (repo_id,
			// sha) -- e.g. the producing scan was retracted
			// before the active pointer was rebuilt, or the
			// active pointer is mid-rebuild and the
			// scan_run row hasn't transitioned to
			// `succeeded` yet. Per the
			// PGSystemTierInputSource doc contract we SKIP
			// this pair this tick; the next tick will pick
			// it up if the run completes.
			continue
		}
		scopes, err := s.readScopes(ctx, tx, p.repoID, p.sha)
		if err != nil {
			return nil, err
		}
		foundation, err := s.readFoundationSamples(ctx, tx, p.repoID, p.sha)
		if err != nil {
			return nil, err
		}
		out = append(out, SystemTierInput{
			Mode:                SystemTierModeEmbedded,
			RepoID:              p.repoID,
			SHA:                 p.sha,
			ProducerRunID:       runID,
			Scopes:              scopes,
			Foundation:          foundation,
			XRepoEdgesAvailable: false,
			CallEdgesAvailable:  false,
		})
	}
	// `pairs` is already sorted by (repo_id, sha) from the
	// repoShaPairsQuery ORDER BY; the loop above preserves
	// that order. Belt-and-braces sort here so a future
	// refactor that pulls pairs from a non-deterministic
	// source still produces a deterministic [out].
	sort.SliceStable(out, func(i, j int) bool {
		if c := bytes16Compare(out[i].RepoID, out[j].RepoID); c != 0 {
			return c < 0
		}
		return out[i].SHA < out[j].SHA
	})
	return out, nil
}

// repoShaPair is the local tuple used to thread `(repo_id,
// sha)` through the per-pair read fan-out.
type repoShaPair struct {
	repoID uuid.UUID
	sha    string
}

func (s *PGSystemTierInputSource) readRepoShaPairs(ctx context.Context, tx *sql.Tx) ([]repoShaPair, error) {
	rows, err := tx.QueryContext(ctx, s.repoShaPairsQuery())
	if err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: query repo+sha pairs: %w", err)
	}
	defer rows.Close()
	out := make([]repoShaPair, 0)
	for rows.Next() {
		var (
			ridStr string
			sha    string
		)
		if err := rows.Scan(&ridStr, &sha); err != nil {
			return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: scan repo+sha pair: %w", err)
		}
		rid, err := uuid.FromString(ridStr)
		if err != nil {
			return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: parse repo_id=%q: %w", ridStr, err)
		}
		out = append(out, repoShaPair{repoID: rid, sha: sha})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: rows.Err repo+sha pairs: %w", err)
	}
	return out, nil
}

func (s *PGSystemTierInputSource) readProducerRunID(ctx context.Context, tx *sql.Tx, repoID uuid.UUID, sha string) (uuid.UUID, bool, error) {
	row := tx.QueryRowContext(ctx, s.producerRunQuery(), repoID, sha)
	var runIDStr string
	switch err := row.Scan(&runIDStr); {
	case errors.Is(err, sql.ErrNoRows):
		return uuid.UUID{}, false, nil
	case err != nil:
		return uuid.UUID{}, false, fmt.Errorf("aggregator.PGSystemTierInputSource: scan producer_run (repo_id=%s, sha=%s): %w", repoID, sha, err)
	}
	runID, err := uuid.FromString(runIDStr)
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("aggregator.PGSystemTierInputSource: parse scan_run_id=%q: %w", runIDStr, err)
	}
	return runID, true, nil
}

func (s *PGSystemTierInputSource) readScopes(ctx context.Context, tx *sql.Tx, repoID uuid.UUID, sha string) ([]ScopeRef, error) {
	rows, err := tx.QueryContext(ctx, s.scopesQuery(), repoID, sha)
	if err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: query scopes (repo_id=%s, sha=%s): %w", repoID, sha, err)
	}
	defer rows.Close()
	out := make([]ScopeRef, 0)
	for rows.Next() {
		var (
			sidStr    string
			scopeKind string
		)
		if err := rows.Scan(&sidStr, &scopeKind); err != nil {
			return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: scan scope (repo_id=%s, sha=%s): %w", repoID, sha, err)
		}
		sid, err := uuid.FromString(sidStr)
		if err != nil {
			return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: parse scope_id=%q: %w", sidStr, err)
		}
		out = append(out, ScopeRef{ScopeID: sid, ScopeKind: scopeKind})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: rows.Err scopes (repo_id=%s, sha=%s): %w", repoID, sha, err)
	}
	return out, nil
}

func (s *PGSystemTierInputSource) readFoundationSamples(ctx context.Context, tx *sql.Tx, repoID uuid.UUID, sha string) ([]FoundationSample, error) {
	rows, err := tx.QueryContext(ctx, s.foundationSamplesQuery(), repoID, sha)
	if err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: query foundation samples (repo_id=%s, sha=%s): %w", repoID, sha, err)
	}
	defer rows.Close()
	out := make([]FoundationSample, 0)
	for rows.Next() {
		var (
			sidStr     string
			scopeKind  string
			metricKind string
			value      sql.NullFloat64
			attrsRaw   sql.NullString
		)
		if err := rows.Scan(&sidStr, &scopeKind, &metricKind, &value, &attrsRaw); err != nil {
			return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: scan foundation sample (repo_id=%s, sha=%s): %w", repoID, sha, err)
		}
		if !value.Valid {
			// Defensive: the SQL guard already filters
			// `value IS NOT NULL`. A NULL slipping through
			// is a writer bug; skip rather than crash.
			continue
		}
		if math.IsNaN(value.Float64) || math.IsInf(value.Float64, 0) {
			continue
		}
		sid, err := uuid.FromString(sidStr)
		if err != nil {
			return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: parse scope_id=%q: %w", sidStr, err)
		}
		var attrs map[string]string
		if attrsRaw.Valid && strings.TrimSpace(attrsRaw.String) != "" {
			if err := json.Unmarshal([]byte(attrsRaw.String), &attrs); err != nil {
				// A malformed attrs_json on a foundation
				// row is a writer-side data bug -- the
				// composer's downstream readers (cycle_id,
				// language tag, window) cannot trust a
				// silently-dropped attrs map and would
				// compose against an incomplete shape.
				// Surface the parse failure as a tick
				// error so the operator can see it on
				// /metrics and trace it back to the
				// offending sample. Per iter-4 evaluator
				// finding #4: do NOT silently swallow
				// corrupt input as if it were valid.
				return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: parse attrs_json (repo_id=%s, sha=%s, scope_id=%s, metric_kind=%s): %w", repoID, sha, sid, metricKind, err)
			}
		}
		out = append(out, FoundationSample{
			ScopeID:    sid,
			ScopeKind:  scopeKind,
			MetricKind: metricKind,
			Value:      value.Float64,
			Attrs:      attrs,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregator.PGSystemTierInputSource: rows.Err foundation samples (repo_id=%s, sha=%s): %w", repoID, sha, err)
	}
	return out, nil
}

// bytes16Compare is a stable byte-wise comparator between two
// 16-byte UUIDs. Returns -1 / 0 / +1. Pinned here as a
// package-private helper because the SystemTierInput
// determinism contract sorts on UUID bytes (string-form
// ordering would land the same `out` slice but is more
// expensive; the bytewise sort is the canonical G6 ordering).
func bytes16Compare(a, b uuid.UUID) int {
	for i := 0; i < 16; i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return +1
		}
	}
	return 0
}
