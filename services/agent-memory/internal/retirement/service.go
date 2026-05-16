// Package retirement is the only library in services/agent-memory
// allowed to perform DML against the tombstone tables
// (`node_retirement` and `edge_retirement`). It implements Stage 2.3
// of the implementation-plan; the architectural intent is set by
// architecture.md §5.2.4 ("retirement is a separate tombstone row,
// not a column rewrite") and tech-spec §8.7.2 (exactly one
// tombstone per retired entity, enforced by UNIQUE indices on
// `(node_id)` / `(edge_id)`).
//
// Why a dedicated package
// -----------------------
// The Stage 2 split keeps "writes that mint new graph rows"
// (graphwriter) separate from "writes that mark graph rows as
// retired" (retirement). Both are forbidden from issuing UPDATE
// by the role-grant policy in migration 0016 (G5 invariant); the
// schema's append-only invariant is enforced at the database
// layer. The two libraries share the same wire-protocol shape
// (single PostgreSQL transaction per public call) and the same
// SQLSTATE 42501 typed wrapper so callers see role-grant denials
// the same way regardless of which writer surfaced them.
//
// Typed errors
// ------------
// Three error shapes are exposed so callers can pattern-match on
// the failure mode without parsing message strings:
//
//   - *AlreadyRetired -- SQLSTATE 23505 (unique_violation) on
//     the per-table UNIQUE `(node_id)` / `(edge_id)` index. This
//     is the canonical "double-retirement rejected" scenario from
//     the Stage 2.3 brief.
//
//   - *NotFound -- the target node / edge id (or, for RetireNode,
//     the supplied `superseded_by_node_id`) does not exist in the
//     graph. RetireNode / RetireEdge surface this via an explicit
//     pre-check inside the same transaction so the error names
//     the specific id that was missing; RetireMany lets the
//     foreign-key constraint do the work and surfaces a single
//     batch-wide NotFound carrying the underlying pq error.
//
//   - *WriteContractViolation -- SQLSTATE 42501
//     (insufficient_privilege). The application role does not
//     have UPDATE / DELETE on the tombstone tables (migration
//     0016 lists them in the append-only set), so any forbidden
//     DML surfaces as this typed value. Mirrors the equivalent
//     error in the graphwriter package by intent.
//
// Batch semantics (RetireMany / RetireManyEdges)
// ----------------------------------------------
// Two batch entry points are exposed -- one per tombstone table
// because the underlying tables, UNIQUE indices, and FK targets
// differ:
//
//   - RetireMany(ctx, nodeIDs, sha) -- node tombstones; the
//     bulk-rename hot path the tech-spec §9.7 risk calls out.
//
//   - RetireManyEdges(ctx, edgeIDs, sha) -- edge tombstones; the
//     companion path for bulk file/module removals where every
//     dangling Edge whose endpoint is being retired must also be
//     tombstoned. The Repo Indexer's removal-detection step
//     issues both batches inside the same logical commit.
//
// Both methods run a single multi-row INSERT keyed off
// `unnest($1::uuid[])`. The whole batch is atomic: any
// UNIQUE / FK violation rolls the transaction back, so zero
// rows land. Callers must pre-deduplicate / pre-filter retired
// ids if they want partial-progress semantics; the bulk-rename
// hot path (tech-spec §9.7) builds its input from a fresh delta
// against the retirement tables and so does not need
// partial-progress.
package retirement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lib/pq"
)

// PostgreSQL SQLSTATE codes the classifier maps to typed errors.
// Documented in the canonical error-code catalogue (class 23 is
// "integrity constraint violation", class 42 is "syntax error or
// access rule violation"). The message text is locale-sensitive
// and not relied on.
const (
	pgErrCodeUniqueViolation       = "23505"
	pgErrCodeForeignKeyViolation   = "23503"
	pgErrCodeInsufficientPrivilege = "42501"
)

// Kind constants identify which tombstone table is involved. They
// are the only public sentinel that callers may switch on to
// distinguish a node-side failure from an edge-side failure
// without depending on internal SQL details.
const (
	KindNode = "node"
	KindEdge = "edge"
)

// Service is the only object that issues DML against the
// tombstone tables. Every public method opens its own
// PostgreSQL transaction, runs the SQL, and commits before
// returning. Construct one with New().
//
// Service is safe for concurrent use: it does not retain state
// across method calls and the underlying *sql.DB pools its own
// connections.
type Service struct {
	db     *sql.DB
	logger *slog.Logger
}

// New constructs a Service over the supplied *sql.DB. The DB
// must be authenticated as a role that satisfies the GRANTs in
// migration 0016 (typically `agent_memory_app`). A nil logger
// is replaced with slog.Default().
//
// The `retired_at` timestamp is stamped server-side via the
// table's `DEFAULT now()` column, so the Service deliberately
// does not carry a clock dependency.
func New(db *sql.DB, logger *slog.Logger) *Service {
	if db == nil {
		panic("retirement: nil *sql.DB")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:     db,
		logger: logger,
	}
}

// ----- Typed errors ------------------------------------------------

// AlreadyRetired indicates the target id already has a tombstone
// row. PostgreSQL returns SQLSTATE 23505 (unique_violation) on
// the `node_retirement_node_id_uidx` / `edge_retirement_edge_id_uidx`
// indices defined in migration 0004. The error is the
// load-bearing surface for the Stage 2.3 acceptance scenario
// "double-retirement rejected".
type AlreadyRetired struct {
	// Kind is "node" or "edge"; see the Kind* constants.
	Kind string
	// TargetID is the textual UUID of the entity that was already
	// retired. Empty when surfaced from the RetireMany batch path
	// (PostgreSQL only reports one violating row in its Detail
	// field and we do not parse it).
	TargetID string
	// SQLState is the PostgreSQL SQLSTATE returned by the driver;
	// always "23505" for this error type.
	SQLState string
	// Err is the wrapped *pq.Error returned by lib/pq.
	Err error
}

func (e *AlreadyRetired) Error() string {
	if e.TargetID == "" {
		return fmt.Sprintf(
			"retirement: %s already retired (SQLSTATE %s): %v",
			e.Kind, e.SQLState, e.Err,
		)
	}
	return fmt.Sprintf(
		"retirement: %s %s already retired (SQLSTATE %s): %v",
		e.Kind, e.TargetID, e.SQLState, e.Err,
	)
}

// Unwrap exposes the wrapped driver error for errors.As / errors.Is.
func (e *AlreadyRetired) Unwrap() error { return e.Err }

// NotFound indicates the target node / edge (or, for RetireNode,
// the supplied `superseded_by_node_id`) does not exist in the
// graph. RetireNode / RetireEdge surface this via an explicit
// pre-check inside the same transaction so the error names the
// specific id; RetireMany surfaces it through the foreign-key
// constraint with TargetID left empty.
type NotFound struct {
	// Kind is "node" or "edge"; see the Kind* constants. The
	// supersede pre-check sets Kind = "node" since the column
	// references the `node` table.
	Kind string
	// TargetID is the textual UUID of the missing entity, or
	// empty when surfaced from RetireMany (the pq error's Detail
	// field carries the specific id and is reachable via
	// errors.As(*pq.Error)).
	TargetID string
	// SQLState is empty for the pre-check path (no SQL error) and
	// "23503" (foreign_key_violation) for the batch path.
	SQLState string
	// Err is the underlying error (nil for the pre-check path or
	// the wrapped *pq.Error for the batch path).
	Err error
}

func (e *NotFound) Error() string {
	if e.TargetID == "" {
		return fmt.Sprintf("retirement: %s not found", e.Kind)
	}
	return fmt.Sprintf("retirement: %s %s not found", e.Kind, e.TargetID)
}

// Unwrap exposes the wrapped driver error (if any) for
// errors.As / errors.Is.
func (e *NotFound) Unwrap() error { return e.Err }

// WriteContractViolation indicates the service attempted DML the
// application role does not have privileges for -- almost always
// an UPDATE / DELETE on a tombstone table (G5 invariant enforced
// at the role-grant layer per tech-spec §8.7.4). Mirrors the
// equivalent type in the graphwriter package by intent so
// downstream consumers see a uniform shape.
type WriteContractViolation struct {
	// Op identifies the service entry point that triggered the
	// violation (e.g. "RetireNode", "RetireEdge", "RetireMany").
	Op string
	// SQLState is the PostgreSQL SQLSTATE returned by the driver;
	// always "42501" for this error type.
	SQLState string
	// Err is the wrapped *pq.Error returned by lib/pq.
	Err error
}

func (e *WriteContractViolation) Error() string {
	return fmt.Sprintf(
		"retirement: %s denied by role-grant policy (SQLSTATE %s): %v",
		e.Op, e.SQLState, e.Err,
	)
}

// Unwrap exposes the wrapped driver error for errors.As / errors.Is.
func (e *WriteContractViolation) Unwrap() error { return e.Err }

// classifyErr maps a raw SQL error into one of the typed errors
// the service exposes. Callers can errors.As(err, &target) on
// the typed shapes without re-running the classifier.
//
// classifyErr is idempotent on already-typed errors: a body that
// classified its own *pq.Error before returning passes through
// runInTx's outer classifyErr unchanged. Without this guard the
// outer call would walk the typed wrapper's Unwrap chain, find
// the embedded *pq.Error, and double-wrap with TargetID = ""
// (clobbering the body's id-specific attribution).
func classifyErr(op, kind, targetID string, err error) error {
	if err == nil {
		return nil
	}
	// Idempotency guard. Order matters only in that we short-
	// circuit before the errors.As(*pq.Error) walk below would
	// otherwise re-wrap the typed value.
	var (
		alreadyRetired *AlreadyRetired
		notFound       *NotFound
		contractV      *WriteContractViolation
	)
	if errors.As(err, &alreadyRetired) ||
		errors.As(err, &notFound) ||
		errors.As(err, &contractV) {
		return err
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return err
	}
	switch string(pqErr.Code) {
	case pgErrCodeUniqueViolation:
		return &AlreadyRetired{
			Kind:     kind,
			TargetID: targetID,
			SQLState: pgErrCodeUniqueViolation,
			Err:      err,
		}
	case pgErrCodeForeignKeyViolation:
		// The supersede FK and the target FK both live on the
		// same table; default the surfaced Kind to "node" since
		// both columns reference `node (node_id)` -- the caller
		// passes in a more specific kind when applicable
		// (e.g. KindEdge for the edge_retirement FK).
		fkKind := kind
		if fkKind == "" {
			fkKind = KindNode
		}
		return &NotFound{
			Kind:     fkKind,
			TargetID: targetID,
			SQLState: pgErrCodeForeignKeyViolation,
			Err:      err,
		}
	case pgErrCodeInsufficientPrivilege:
		return &WriteContractViolation{
			Op:       op,
			SQLState: pgErrCodeInsufficientPrivilege,
			Err:      err,
		}
	}
	return err
}

// ----- RetireNode --------------------------------------------------

// NodeRetirementInput describes a single node-side retirement.
// SupersededByNodeID is empty for non-rename retirements (e.g.
// a method deleted outright); when set it MUST refer to an
// existing node in the graph (typically the post-rename Node
// the Repo Indexer has just inserted via GraphWriter.InsertNode).
type NodeRetirementInput struct {
	// NodeID is the textual UUID of the node row to tombstone.
	NodeID string
	// RetiredAtSHA is the commit SHA at which the retirement
	// took effect. The Repo Indexer passes `parent(to_sha)` for
	// removed entities and the new commit SHA for renames
	// (implementation-plan.md Stage 3.3 step 4).
	RetiredAtSHA string
	// SupersededByNodeID is the textual UUID of the replacement
	// node when this retirement is part of a rename. Empty for
	// outright deletions. When set, the value is FK-checked
	// inside the same transaction.
	SupersededByNodeID string
}

// NodeRetirementRecord is the post-insert state of a
// `node_retirement` row.
type NodeRetirementRecord struct {
	// RetirementID is the surrogate UUID primary key as a
	// textual UUID.
	RetirementID string
	// NodeID echoes the input so callers can pipe the record to
	// downstream consumers without re-passing the id.
	NodeID string
	// RetiredAtSHA echoes the input.
	RetiredAtSHA string
	// RetiredAt is the server-side timestamp PostgreSQL stamped
	// at INSERT time (`DEFAULT now()`).
	RetiredAt time.Time
	// SupersededByNodeID echoes the input; empty for non-rename
	// retirements.
	SupersededByNodeID string
}

// RetireNode inserts one `node_retirement` row inside its own
// transaction. The target node must exist; the supplied
// SupersededByNodeID (if non-empty) must also exist. Both checks
// run inside the same transaction so a concurrent retire-then-
// reinsert race cannot slip past them.
//
// On failure the returned error is one of:
//
//   - *NotFound   -- target or supersede id does not exist.
//   - *AlreadyRetired -- a tombstone row already exists for
//     NodeID (SQLSTATE 23505 on the UNIQUE index).
//   - *WriteContractViolation -- role-grant policy rejected the
//     DML (SQLSTATE 42501).
//   - any other database / context error, returned unchanged.
func (s *Service) RetireNode(
	ctx context.Context, in NodeRetirementInput,
) (NodeRetirementRecord, error) {
	if in.NodeID == "" {
		return NodeRetirementRecord{}, errors.New(
			"retirement: RetireNode: empty node_id")
	}
	if in.RetiredAtSHA == "" {
		return NodeRetirementRecord{}, errors.New(
			"retirement: RetireNode: empty retired_at_sha")
	}

	rec := NodeRetirementRecord{
		NodeID:             in.NodeID,
		RetiredAtSHA:       in.RetiredAtSHA,
		SupersededByNodeID: in.SupersededByNodeID,
	}
	err := s.runInTx(ctx, "RetireNode", func(tx *sql.Tx) error {
		// Pre-check the target so the NotFound error names the
		// missing id. The same-tx read is safe because the
		// `node` table is append-only at the role layer; no
		// concurrent DELETE can race with this read.
		if err := assertNodeExists(ctx, tx, in.NodeID); err != nil {
			return err
		}
		// Pre-check the supersede id (when set) for the same
		// reason. A NotFound here is the rename scenario where
		// the Repo Indexer forgot to insert the new node before
		// retiring the old one -- an actionable caller bug.
		if in.SupersededByNodeID != "" {
			if err := assertNodeExists(
				ctx, tx, in.SupersededByNodeID,
			); err != nil {
				return err
			}
		}

		const insertQ = `
			INSERT INTO node_retirement
			    (node_id, retired_at_sha, superseded_by_node_id)
			VALUES ($1, $2, $3)
			RETURNING retirement_id::text, retired_at
		`
		var supersede sql.NullString
		if in.SupersededByNodeID != "" {
			supersede = sql.NullString{
				String: in.SupersededByNodeID, Valid: true,
			}
		}
		err := tx.QueryRowContext(ctx, insertQ,
			in.NodeID, in.RetiredAtSHA, supersede,
		).Scan(&rec.RetirementID, &rec.RetiredAt)
		// Classify here so the typed error names the specific
		// node_id we tried to retire. classifyErr passes through
		// non-pq errors unchanged.
		return classifyErr("RetireNode", KindNode, in.NodeID, err)
	})
	if err != nil {
		s.emitFailure("RetireNode", in.NodeID, err)
		return NodeRetirementRecord{}, err
	}
	s.emitSuccess("RetireNode", in.NodeID, in.RetiredAtSHA)
	return rec, nil
}

// ----- RetireEdge --------------------------------------------------

// EdgeRetirementInput describes a single edge-side retirement.
// Edge tombstones do not carry a supersede pointer because edges
// are identified by their endpoint fingerprints; a "renamed"
// edge is modelled as a NEW edge row pointing at the new node
// fingerprints, not a supersede column.
type EdgeRetirementInput struct {
	// EdgeID is the textual UUID of the edge row to tombstone.
	EdgeID string
	// RetiredAtSHA is the commit SHA at which the retirement
	// took effect.
	RetiredAtSHA string
}

// EdgeRetirementRecord is the post-insert state of an
// `edge_retirement` row.
type EdgeRetirementRecord struct {
	RetirementID string
	EdgeID       string
	RetiredAtSHA string
	RetiredAt    time.Time
}

// RetireEdge inserts one `edge_retirement` row inside its own
// transaction. See RetireNode for the error contract; the only
// difference is that EdgeRetirement has no supersede column, so
// only one FK pre-check runs.
func (s *Service) RetireEdge(
	ctx context.Context, in EdgeRetirementInput,
) (EdgeRetirementRecord, error) {
	if in.EdgeID == "" {
		return EdgeRetirementRecord{}, errors.New(
			"retirement: RetireEdge: empty edge_id")
	}
	if in.RetiredAtSHA == "" {
		return EdgeRetirementRecord{}, errors.New(
			"retirement: RetireEdge: empty retired_at_sha")
	}

	rec := EdgeRetirementRecord{
		EdgeID:       in.EdgeID,
		RetiredAtSHA: in.RetiredAtSHA,
	}
	err := s.runInTx(ctx, "RetireEdge", func(tx *sql.Tx) error {
		if err := assertEdgeExists(ctx, tx, in.EdgeID); err != nil {
			return err
		}
		const insertQ = `
			INSERT INTO edge_retirement (edge_id, retired_at_sha)
			VALUES ($1, $2)
			RETURNING retirement_id::text, retired_at
		`
		err := tx.QueryRowContext(ctx, insertQ,
			in.EdgeID, in.RetiredAtSHA,
		).Scan(&rec.RetirementID, &rec.RetiredAt)
		return classifyErr("RetireEdge", KindEdge, in.EdgeID, err)
	})
	if err != nil {
		s.emitFailure("RetireEdge", in.EdgeID, err)
		return EdgeRetirementRecord{}, err
	}
	s.emitSuccess("RetireEdge", in.EdgeID, in.RetiredAtSHA)
	return rec, nil
}

// ----- RetireMany --------------------------------------------------

// BatchResult is the post-insert summary RetireMany returns.
type BatchResult struct {
	// InsertedCount is len(Records); kept as a separate field so
	// callers that only care about the count can skip walking
	// the slice in their own code.
	InsertedCount int
	// Records is one NodeRetirementRecord per input id in the
	// order PostgreSQL returned them from the RETURNING clause.
	// The order is NOT guaranteed to match the input order --
	// callers that depend on a specific ordering must key by
	// NodeID.
	Records []NodeRetirementRecord
}

// RetireMany inserts tombstones for a batch of node ids in a
// single multi-row INSERT keyed off `unnest($1::uuid[])` -- the
// bulk-rename hot path the tech-spec §9.7 risk calls out.
//
// Atomicity: the whole batch runs inside ONE transaction with
// ONE INSERT statement. PostgreSQL aborts the statement on the
// first UNIQUE / FK violation, so either every id in the batch
// is tombstoned or none of them are. Callers that want
// partial-progress semantics must pre-deduplicate / pre-filter
// already-retired ids.
//
// On failure the returned error follows the same typed
// classification as RetireNode (AlreadyRetired / NotFound /
// WriteContractViolation) but with TargetID left empty because
// PostgreSQL only reports one violating row per failed INSERT
// statement and we do not parse the Detail string. The wrapped
// *pq.Error reachable via errors.As carries the offending
// `Key (node_id)=(...) ...` Detail for diagnostics.
//
// SupersededByNodeID is intentionally not exposed on the batch
// path -- bulk renames track replacements through the parallel
// `renamed_to` Edge writes that the Repo Indexer issues via
// GraphWriter, not through per-row supersede columns.
func (s *Service) RetireMany(
	ctx context.Context, nodeIDs []string, retiredAtSHA string,
) (BatchResult, error) {
	if retiredAtSHA == "" {
		return BatchResult{}, errors.New(
			"retirement: RetireMany: empty retired_at_sha")
	}
	if len(nodeIDs) == 0 {
		// Defensible no-op: zero-length batch is a successful
		// "nothing to do" so the caller does not have to guard
		// the call site.
		return BatchResult{}, nil
	}
	// Empty-id guard. A single empty string in the batch would
	// otherwise reach the database as an empty UUID literal and
	// surface as an opaque cast error. Reject upfront with a
	// clear message that names the offending index.
	for i, id := range nodeIDs {
		if id == "" {
			return BatchResult{}, fmt.Errorf(
				"retirement: RetireMany: nodeIDs[%d] is empty", i,
			)
		}
	}

	out := BatchResult{
		Records: make([]NodeRetirementRecord, 0, len(nodeIDs)),
	}
	err := s.runInTx(ctx, "RetireMany", func(tx *sql.Tx) error {
		// Single multi-row INSERT per the brief. `unnest` is
		// preferable to VALUES ($1,$2), ($3,$4), ... at scale
		// because the planner sees a single in-memory array
		// rather than N parameter pairs.
		const insertQ = `
			INSERT INTO node_retirement (node_id, retired_at_sha)
			SELECT n_id, $2
			FROM unnest($1::uuid[]) AS n_id
			RETURNING retirement_id::text, node_id::text, retired_at
		`
		rows, err := tx.QueryContext(ctx, insertQ,
			pq.Array(nodeIDs), retiredAtSHA,
		)
		if err != nil {
			// TargetID is left empty -- see the doc comment for
			// the rationale. The wrapped *pq.Error carries the
			// per-row Detail for diagnostics.
			return classifyErr("RetireMany", KindNode, "", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r NodeRetirementRecord
			r.RetiredAtSHA = retiredAtSHA
			if err := rows.Scan(
				&r.RetirementID, &r.NodeID, &r.RetiredAt,
			); err != nil {
				return fmt.Errorf(
					"retirement: RetireMany scan: %w", err)
			}
			out.Records = append(out.Records, r)
		}
		if err := rows.Err(); err != nil {
			return classifyErr("RetireMany", KindNode, "", err)
		}
		return nil
	})
	if err != nil {
		s.emitFailure("RetireMany",
			fmt.Sprintf("count=%d", len(nodeIDs)), err)
		return BatchResult{}, err
	}
	out.InsertedCount = len(out.Records)
	s.emitSuccess("RetireMany",
		fmt.Sprintf("count=%d", out.InsertedCount), retiredAtSHA)
	return out, nil
}

// ----- RetireManyEdges ---------------------------------------------

// EdgeBatchResult is the post-insert summary RetireManyEdges
// returns. Shape mirrors BatchResult to keep batch consumers
// symmetric across the two tombstone tables.
type EdgeBatchResult struct {
	// InsertedCount is len(Records); see BatchResult.
	InsertedCount int
	// Records is one EdgeRetirementRecord per input id in the
	// order PostgreSQL returned them from the RETURNING clause.
	// The order is NOT guaranteed to match the input order --
	// callers that depend on a specific ordering must key by
	// EdgeID.
	Records []EdgeRetirementRecord
}

// RetireManyEdges inserts tombstones for a batch of edge ids in a
// single multi-row INSERT keyed off `unnest($1::uuid[])`. It is
// the edge-side companion to RetireMany; see that method's doc
// comment for the atomicity contract (whole-batch rollback on the
// first UNIQUE / FK violation) and the typed-error mapping
// (AlreadyRetired / NotFound / WriteContractViolation, with
// TargetID intentionally empty on the batch path).
//
// The split into two methods (rather than a single generic
// `RetireMany(kind, ids)`) is deliberate: the underlying tables,
// UNIQUE indices, and FK targets differ, so a stringly-typed
// dispatcher would force every caller to handle the type-erasure
// at the call site. Two strongly-typed methods keep callers
// honest about which tombstone table they are writing to.
func (s *Service) RetireManyEdges(
	ctx context.Context, edgeIDs []string, retiredAtSHA string,
) (EdgeBatchResult, error) {
	if retiredAtSHA == "" {
		return EdgeBatchResult{}, errors.New(
			"retirement: RetireManyEdges: empty retired_at_sha")
	}
	if len(edgeIDs) == 0 {
		// Defensible no-op; see RetireMany.
		return EdgeBatchResult{}, nil
	}
	for i, id := range edgeIDs {
		if id == "" {
			return EdgeBatchResult{}, fmt.Errorf(
				"retirement: RetireManyEdges: edgeIDs[%d] is empty", i,
			)
		}
	}

	out := EdgeBatchResult{
		Records: make([]EdgeRetirementRecord, 0, len(edgeIDs)),
	}
	err := s.runInTx(ctx, "RetireManyEdges", func(tx *sql.Tx) error {
		const insertQ = `
			INSERT INTO edge_retirement (edge_id, retired_at_sha)
			SELECT e_id, $2
			FROM unnest($1::uuid[]) AS e_id
			RETURNING retirement_id::text, edge_id::text, retired_at
		`
		rows, err := tx.QueryContext(ctx, insertQ,
			pq.Array(edgeIDs), retiredAtSHA,
		)
		if err != nil {
			return classifyErr("RetireManyEdges", KindEdge, "", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r EdgeRetirementRecord
			r.RetiredAtSHA = retiredAtSHA
			if err := rows.Scan(
				&r.RetirementID, &r.EdgeID, &r.RetiredAt,
			); err != nil {
				return fmt.Errorf(
					"retirement: RetireManyEdges scan: %w", err)
			}
			out.Records = append(out.Records, r)
		}
		if err := rows.Err(); err != nil {
			return classifyErr("RetireManyEdges", KindEdge, "", err)
		}
		return nil
	})
	if err != nil {
		s.emitFailure("RetireManyEdges",
			fmt.Sprintf("count=%d", len(edgeIDs)), err)
		return EdgeBatchResult{}, err
	}
	out.InsertedCount = len(out.Records)
	s.emitSuccess("RetireManyEdges",
		fmt.Sprintf("count=%d", out.InsertedCount), retiredAtSHA)
	return out, nil
}

// ----- helpers -----------------------------------------------------

// assertNodeExists returns a typed *NotFound when the supplied
// node_id is missing. The lookup runs inside the supplied
// transaction so it shares the snapshot with the subsequent
// INSERT; the `node` table is append-only at the role layer so
// no concurrent DELETE can race with this read.
func assertNodeExists(
	ctx context.Context, tx *sql.Tx, nodeID string,
) error {
	var seen int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM node WHERE node_id = $1`, nodeID,
	).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return &NotFound{Kind: KindNode, TargetID: nodeID}
	}
	if err != nil {
		return fmt.Errorf("retirement: lookup node %s: %w", nodeID, err)
	}
	return nil
}

// assertEdgeExists is the edge-side mirror of assertNodeExists.
func assertEdgeExists(
	ctx context.Context, tx *sql.Tx, edgeID string,
) error {
	var seen int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM edge WHERE edge_id = $1`, edgeID,
	).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return &NotFound{Kind: KindEdge, TargetID: edgeID}
	}
	if err != nil {
		return fmt.Errorf("retirement: lookup edge %s: %w", edgeID, err)
	}
	return nil
}

// runInTx wraps a body in a single PostgreSQL transaction and
// classifies any returned error through classifyErr at the
// boundary so SQLSTATE 42501 (denied UPDATE / DELETE) surfaces
// as *WriteContractViolation regardless of which body produced
// it. Mirrors the equivalent helper in the graphwriter package.
//
// classifyErr is idempotent (see its doc comment): bodies that
// classified their own error pre-attribute the TargetID so the
// outer call here passes them through untouched.
func (s *Service) runInTx(
	ctx context.Context, op string, body func(tx *sql.Tx) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyErr(op, "", "",
			fmt.Errorf("retirement: %s begin: %w", op, err))
	}
	if err := body(tx); err != nil {
		_ = tx.Rollback()
		return classifyErr(op, "", "", err)
	}
	if err := tx.Commit(); err != nil {
		return classifyErr(op, "", "",
			fmt.Errorf("retirement: %s commit: %w", op, err))
	}
	return nil
}

// emitSuccess and emitFailure are thin slog wrappers so every
// public method emits exactly one structured log record per
// call. Kept simple on purpose: the brief does not pin a
// middleware shape and the graphwriter package already proves
// the more elaborate emitAudit pattern; here we mirror just
// enough for operators to grep for `retirement.<op>` records.
func (s *Service) emitSuccess(op, target, sha string) {
	s.logger.Info("retirement."+op,
		slog.String("op", op),
		slog.String("target", target),
		slog.String("retired_at_sha", sha),
	)
}

func (s *Service) emitFailure(op, target string, err error) {
	var (
		alreadyRetired *AlreadyRetired
		notFound       *NotFound
		contractV      *WriteContractViolation
	)
	s.logger.Error("retirement."+op+".failed",
		slog.String("op", op),
		slog.String("target", target),
		slog.String("error", err.Error()),
		slog.String("error_type", fmt.Sprintf("%T", err)),
		slog.Bool("already_retired", errors.As(err, &alreadyRetired)),
		slog.Bool("not_found", errors.As(err, &notFound)),
		slog.Bool("contract_violation", errors.As(err, &contractV)),
	)
}
