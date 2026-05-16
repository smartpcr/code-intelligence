// Package recallcontext is the only library in services/agent-memory
// allowed to perform DML against the `recall_context_log` table.
// It implements Stage 2.4 of the implementation-plan; the
// architectural intent is set by architecture.md §5.4.1 (the
// `RecallContextLog` schema) and §4.2 step 4 (the recall flow
// appends one log row per `agent.recall` response so a later
// `observe` can refer to exactly the same snapshot).
//
// Why a dedicated package
// -----------------------
// The Stage 2 split keeps each load-bearing append-only table
// behind its own writer library so the role-grant policy is the
// load-bearing G5 enforcer (tech-spec §8.7.4) regardless of
// which library issued the DML:
//
//   - `graphwriter`     -- structural graph (Repo / Commit / Node / Edge).
//   - `retirement`      -- node / edge tombstones.
//   - `recallcontext`   -- this package; recall-context snapshots.
//
// Two public methods
// ------------------
//
//   - `Append(...)` writes one row to `recall_context_log` and
//     returns the assigned `context_id`. It is the §4.2 step 4
//     entry point the `agent.recall` flow calls right before
//     handing the `RecallContext` envelope back to the caller.
//
//   - `Resolve(context_id)` reads the row back AND dereferences
//     every node_id / edge_id / concept_id through `graphreader`
//     so `mgmt.read.context` (architecture.md §6.2) returns the
//     log row plus the entity cards an operator can render. The
//     dereference runs with `ReaderOptions.IncludeRetired = true`
//     so a historical context is still inspectable after its
//     referenced rows are retired (risk §9.13 in the
//     implementation plan).
//
// Ordering invariant (the critical correctness property)
// ------------------------------------------------------
// architecture.md §5.4.1 says `node_ids[]`, `edge_ids[]`, and
// `concept_ids[]` are "Ordered list[s] of ... ids returned" --
// the reranker_model_version pinned alongside makes the order
// reproducible. PostgreSQL preserves insertion order in `uuid[]`
// columns; the writer below passes each list as a `pq.Array`
// over the input `[]string` so order is preserved through the
// wire. The Resolve path walks each array in storage order
// (SELECT returns the array elements in stored order) and
// calls `GetNode` / `GetEdge` / `GetConcept` one at a time so
// the returned slices land in the same order the recall
// produced. Two tests pin this end-to-end:
//
//   * TestAppend_resolveRoundtrip_integration -- live PG path.
//   * TestResolve_preservesNodeIDsOrder_unit_sqlmock -- pure unit
//     path using sqlmock + a fake card resolver.
//
// Typed error contract
// --------------------
// Two error shapes are exposed so callers can pattern-match on
// the failure mode without parsing message strings; both mirror
// the equivalent types in `graphwriter` / `retirement` by
// intent so downstream consumers see uniform shapes.
//
//   - `ErrContextNotFound` -- sentinel returned by `Resolve` when
//     the `recall_context_log` row does not exist (and propagated
//     when a referenced node / edge / concept itself has truly
//     vanished -- a database-corruption case worth surfacing
//     loudly).
//
//   - `*WriteContractViolation` -- SQLSTATE 42501; the application
//     role was denied the INSERT (G5 invariant enforced at the
//     role-grant layer per tech-spec §8.7.4).
package recallcontext

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// pgErrCodeInsufficientPrivilege is the SQLSTATE PostgreSQL
// returns when a role lacks the privilege required by a
// statement (class 42, code 01). Used by the classifier to
// surface `*WriteContractViolation` from `Append`.
const pgErrCodeInsufficientPrivilege = "42501"

// allowedVerbs is the closed set defined by the `verb` ENUM in
// migration 0001 (architecture.md §5.4.1). Validated Go-side
// before the SQL round-trip so the API returns a clear error
// rather than the opaque `invalid input value for enum verb`
// PostgreSQL would otherwise emit.
var allowedVerbs = map[string]struct{}{
	"recall":    {},
	"expand":    {},
	"summarize": {},
}

// uuidPattern matches the textual UUID shape PostgreSQL accepts
// in a `uuid[]` literal. The validator is intentionally strict:
// a malformed id would otherwise reach the driver and surface
// as a SQLSTATE 22P02 cast error against the array literal,
// which gives no hint about which element of which array failed.
// Pre-validating per id lets the API name the offending slice
// index in the returned error.
var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// ErrContextNotFound is returned by `Resolve` when the
// requested `recall_context_log` row does not exist. Pattern-
// match with `errors.Is(err, recallcontext.ErrContextNotFound)`.
//
// Returned in two cases:
//
//   - The supplied `context_id` does not name any row (operator
//     typo, expired retention, or a request for a context that
//     was never written).
//
//   - A referenced node / edge / concept id has truly vanished
//     from the graph -- a G3/G4/G5 invariant violation worth
//     surfacing loudly so the caller sees data corruption
//     rather than a silently truncated card list. In that case
//     the returned error wraps `graphreader.ErrNotFound` and
//     identifies the offending id via the chain accessible
//     through `errors.Unwrap`.
var ErrContextNotFound = errors.New("recallcontext: context_id not found")

// WriteContractViolation indicates the writer attempted DML the
// application role does not have privileges for -- almost
// always an INSERT on a table the role lacks `INSERT` on (G5
// invariant enforced at the role-grant layer per tech-spec
// §8.7.4). Mirrors the equivalent types in `graphwriter` and
// `retirement` by intent so downstream consumers see a uniform
// shape across the three writer libraries.
type WriteContractViolation struct {
	// Op identifies the writer entry point that triggered the
	// violation (always "Append" today; reserved for future
	// writer methods).
	Op string
	// SQLState is the PostgreSQL SQLSTATE returned by the
	// driver. For this error type it is always "42501".
	SQLState string
	// Err is the wrapped *pq.Error returned by lib/pq.
	Err error
}

func (e *WriteContractViolation) Error() string {
	return fmt.Sprintf(
		"recallcontext: %s denied by role-grant policy (SQLSTATE %s): %v",
		e.Op, e.SQLState, e.Err,
	)
}

// Unwrap exposes the wrapped driver error for errors.As / errors.Is.
func (e *WriteContractViolation) Unwrap() error { return e.Err }

// classifyErr maps a raw SQL error into one of the typed errors
// recallcontext exposes. Currently the only typed wrapping is
// WriteContractViolation (SQLSTATE 42501); other errors pass
// through unchanged so callers see the original driver context.
func classifyErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && string(pqErr.Code) == pgErrCodeInsufficientPrivilege {
		return &WriteContractViolation{
			Op:       op,
			SQLState: pgErrCodeInsufficientPrivilege,
			Err:      err,
		}
	}
	return err
}

// cardResolver is the subset of `*graphreader.Reader` the
// Resolve path consumes. Declared at the consumer side
// (recallcontext, not graphreader) so unit tests can inject a
// fake without taking on the full pgxpool dependency surface
// and without adding test-only types to the production reader
// package.
type cardResolver interface {
	GetNode(ctx context.Context, nodeID string, opts graphreader.ReaderOptions) (graphreader.Node, error)
	GetEdge(ctx context.Context, edgeID string, opts graphreader.ReaderOptions) (graphreader.Edge, error)
	GetConcept(ctx context.Context, conceptID string) (graphreader.Concept, error)
}

// Log is the only object that performs DML against the
// `recall_context_log` table AND the only object that hydrates
// dereferenced cards for `mgmt.read.context`. Construct one
// with `New`.
//
// Log is safe for concurrent use: it does not retain state
// across method calls. The underlying *sql.DB pools its own
// connections and the underlying *graphreader.Reader uses a
// pgxpool that does the same.
//
// Wiring note: `db` MUST be authenticated as a role with INSERT
// + SELECT on `recall_context_log` (typically `agent_memory_app`
// per migration 0016). `reader` MUST be authenticated as a role
// with SELECT on the structural graph tables (typically
// `agent_memory_ro` per migration 0017) -- the two halves can
// use different connection pools because `Resolve` reads the
// log row through `db` and only dereferences the listed entity
// ids through `reader`.
type Log struct {
	db     *sql.DB
	reader cardResolver
	logger *slog.Logger
}

// New constructs a Log over the supplied write handle (`db`)
// and read handle (`reader`). The reader is required because
// `Resolve` dereferences each node/edge/concept id through it
// per the implementation-plan brief ("joins the dereferenced
// Node / Edge / Concept cards through GraphReader").
//
// A nil logger is replaced with slog.Default(). A nil `db` or
// nil `reader` panics -- both are unambiguously programmer bugs
// that would otherwise surface as a NPE on the first call.
func New(db *sql.DB, reader *graphreader.Reader, logger *slog.Logger) *Log {
	if db == nil {
		panic("recallcontext: nil *sql.DB")
	}
	if reader == nil {
		panic("recallcontext: nil *graphreader.Reader")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Log{
		db:     db,
		reader: reader,
		logger: logger,
	}
}

// newWithResolver is a test-only constructor that accepts an
// arbitrary cardResolver. Used by the unit tests to inject a
// fake resolver alongside a sqlmock-backed *sql.DB without
// requiring a live pgxpool. The production constructor
// (`New`) stays pinned to `*graphreader.Reader` so the
// production wiring cannot accidentally use a fake.
//
// Not exported: keeping this unexported means the only
// in-package callers are *_test.go files; production code
// across the module cannot reach it.
func newWithResolver(db *sql.DB, r cardResolver, logger *slog.Logger) *Log {
	if db == nil {
		panic("recallcontext: nil *sql.DB")
	}
	if r == nil {
		panic("recallcontext: nil cardResolver")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Log{db: db, reader: r, logger: logger}
}

// ----- Append ------------------------------------------------------

// AppendInput is the argument shape for Append. It mirrors the
// architecture.md §5.4.1 RecallContextLog row schema exactly so
// a caller can derive the values for each field from the
// matching field on the in-memory recall response envelope.
type AppendInput struct {
	// Verb is the originating verb name and MUST be one of the
	// `verb` ENUM members defined in migration 0001
	// (recall / expand / summarize per architecture.md §5.4.1).
	Verb string
	// RepoID is the repo whose recall this snapshot pertains
	// to. Zero values are rejected.
	RepoID fingerprint.RepoID
	// QueryJSON is the originating verb's input payload, stored
	// verbatim in the `query_json jsonb` column. MUST be a
	// non-empty syntactically valid JSON value.
	QueryJSON json.RawMessage
	// NodeIDs is the ordered list of node ids the recall
	// returned, in rank order. Empty / nil is accepted and
	// stored as an empty `uuid[]` (matching the column default).
	NodeIDs []string
	// EdgeIDs is the ordered list of edge ids the recall
	// returned, in rank order. Same nil/empty semantics as
	// NodeIDs.
	EdgeIDs []string
	// ConceptIDs is the ordered list of concept ids the recall
	// returned, in rank order. Same nil/empty semantics as
	// NodeIDs.
	ConceptIDs []string
	// RerankerModelVersion is the version string pinned for
	// reproducibility per architecture.md §5.4.1. Empty values
	// are rejected.
	RerankerModelVersion string
	// ServedUnderDegraded is true iff the recall was served
	// from a cached snapshot during a graph outage (architecture.md
	// §7.5). The Resolve path surfaces this verbatim so the
	// `mgmt.read.context` caller can render a "served while
	// degraded" badge.
	ServedUnderDegraded bool
}

// AppendRecord is the post-insert state of a `recall_context_log`
// row. Both fields come from the RETURNING clause so callers
// know the assigned id AND the wall-clock timestamp the row
// received without a follow-up SELECT.
type AppendRecord struct {
	// ContextID is the textual UUID of the new row.
	ContextID string
	// CreatedAt is the server-side timestamp PostgreSQL stamped
	// at INSERT time (`DEFAULT now()`). Returned so callers
	// that index into the partition layout (e.g. for a future
	// (context_id, created_at) seek) do not need a second
	// round-trip.
	CreatedAt time.Time
}

// Append writes one row to `recall_context_log` inside its own
// transaction and returns the assigned `context_id`. The id is
// minted by the table's `DEFAULT gen_random_uuid()` (migration
// 0010) so callers do not need to pre-generate uuids.
//
// Ordering: the three uuid[] columns receive the supplied
// slices in input order via `pq.Array`; PostgreSQL preserves
// insertion order in `uuid[]` storage so Resolve will return
// them in the same order. This is the load-bearing invariant
// the "ordering preserved" acceptance scenario asserts.
//
// On failure the returned error is one of:
//
//   - *WriteContractViolation -- role-grant policy rejected the
//     INSERT (SQLSTATE 42501).
//   - validation error -- non-pq error returned unwrapped when
//     the input fails the pre-flight checks (invalid verb,
//     zero RepoID, malformed JSON, malformed UUID in any id
//     slice, empty reranker version).
//   - any other database / context error, returned unchanged.
//
// Emits one structured log record per call (`recallcontext.Append`
// on success, `recallcontext.Append.failed` on error).
func (l *Log) Append(ctx context.Context, in AppendInput) (AppendRecord, error) {
	if err := validateAppendInput(in); err != nil {
		l.emitFailure("Append", "", err)
		return AppendRecord{}, err
	}

	const q = `
		INSERT INTO recall_context_log (
			repo_id, verb, query_json,
			node_ids, edge_ids, concept_ids,
			reranker_model_version, served_under_degraded
		)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8)
		RETURNING context_id::text, created_at
	`
	var rec AppendRecord
	err := l.runInTx(ctx, "Append", func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, q,
			in.RepoID.String(),
			in.Verb,
			string(in.QueryJSON),
			pq.Array(nonNil(in.NodeIDs)),
			pq.Array(nonNil(in.EdgeIDs)),
			pq.Array(nonNil(in.ConceptIDs)),
			in.RerankerModelVersion,
			in.ServedUnderDegraded,
		).Scan(&rec.ContextID, &rec.CreatedAt)
	})
	if err != nil {
		l.emitFailure("Append", "", err)
		return AppendRecord{}, err
	}
	l.emitSuccess("Append", rec.ContextID, in)
	return rec, nil
}

// validateAppendInput runs every Go-side pre-flight check the
// Append path requires. Fails fast with descriptive errors that
// name the offending field / index so an upstream caller can
// fix the input without parsing a generic SQL cast error.
func validateAppendInput(in AppendInput) error {
	if _, ok := allowedVerbs[in.Verb]; !ok {
		return fmt.Errorf(
			"recallcontext: Append: invalid verb %q (allowed: recall/expand/summarize)",
			in.Verb,
		)
	}
	if in.RepoID.IsZero() {
		return errors.New("recallcontext: Append: zero repo_id")
	}
	if len(in.QueryJSON) == 0 {
		return errors.New("recallcontext: Append: empty query_json")
	}
	if !json.Valid(in.QueryJSON) {
		return errors.New("recallcontext: Append: query_json is not valid JSON")
	}
	if in.RerankerModelVersion == "" {
		return errors.New("recallcontext: Append: empty reranker_model_version")
	}
	if err := validateUUIDs("node_ids", in.NodeIDs); err != nil {
		return err
	}
	if err := validateUUIDs("edge_ids", in.EdgeIDs); err != nil {
		return err
	}
	if err := validateUUIDs("concept_ids", in.ConceptIDs); err != nil {
		return err
	}
	return nil
}

// validateUUIDs rejects any entry in `ids` that does not match
// the canonical 8-4-4-4-12 hex form PostgreSQL accepts in a
// `uuid[]` literal. The error names the offending field name
// and slice index so the caller can pinpoint the bad value.
func validateUUIDs(field string, ids []string) error {
	for i, id := range ids {
		if id == "" {
			return fmt.Errorf(
				"recallcontext: Append: %s[%d] is empty",
				field, i,
			)
		}
		if !uuidPattern.MatchString(id) {
			return fmt.Errorf(
				"recallcontext: Append: %s[%d]=%q is not a valid UUID",
				field, i, id,
			)
		}
	}
	return nil
}

// nonNil returns an empty (non-nil) slice when the input is
// nil. The `uuid[]` column is NOT NULL with DEFAULT
// ARRAY[]::uuid[]; passing a nil Go slice through `pq.Array`
// also encodes as NULL on the wire, which the column would
// reject. Materialising a zero-length slice keeps the API
// flexible (callers may pass nil to mean "no ids") without
// tripping the constraint.
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// ----- Resolve -----------------------------------------------------

// ResolvedContext is the structured shape `Resolve` returns:
// the raw `recall_context_log` row plus the dereferenced Node /
// Edge / Concept rows the log referenced, in the order the log
// stored them. The struct is consumed by `mgmt.read.context`
// (architecture.md §6.2) which serialises it onto its wire
// envelope verbatim.
type ResolvedContext struct {
	// ContextID echoes the input id so callers piping
	// ResolvedContext through a transform pipeline do not have
	// to re-thread it.
	ContextID string
	// RepoID is the textual UUID of the repo whose recall this
	// snapshot pertains to.
	RepoID string
	// Verb is the originating verb (recall / expand / summarize)
	// from the stored row.
	Verb string
	// QueryJSON is the stored verb input payload, returned as
	// a raw JSON bytes view (the column is `jsonb` so the
	// driver returns a canonicalised JSON byte stream).
	QueryJSON json.RawMessage
	// RerankerModelVersion is the version string pinned to this
	// recall for reproducibility.
	RerankerModelVersion string
	// ServedUnderDegraded is the stored flag (architecture.md
	// §7.5). The `mgmt.*` reads envelope per architecture.md
	// §6.3 exposes this as the top-level `degraded` field; this
	// layer surfaces the raw column name so the mgmt-api layer
	// has full freedom over the wire shape.
	ServedUnderDegraded bool
	// CreatedAt is the server-side append timestamp.
	CreatedAt time.Time
	// Nodes are the dereferenced node cards, in the same order
	// the log row stored their ids in `node_ids[]`. Empty when
	// the column was empty.
	Nodes []graphreader.Node
	// Edges are the dereferenced edge cards, in the same order
	// the log row stored their ids in `edge_ids[]`.
	Edges []graphreader.Edge
	// Concepts are the dereferenced concept cards, in the same
	// order the log row stored their ids in `concept_ids[]`.
	Concepts []graphreader.Concept
}

// Resolve reads the `recall_context_log` row identified by
// `contextID` and returns it plus the dereferenced Node / Edge
// / Concept cards in storage order. The dereferences run with
// `ReaderOptions.IncludeRetired = true` so historical contexts
// remain inspectable after their referenced rows are retired
// (per the implementation-plan brief, risk §9.13).
//
// LIMIT 2 + corruption guard
// --------------------------
// The table is partitioned by `created_at` with a composite PK
// `(context_id, created_at)` so PostgreSQL does NOT enforce
// global uniqueness on `context_id` -- two partitions could in
// principle hold the same uuid. `gen_random_uuid()` makes that
// collision vanishingly unlikely (122-bit random), but the
// SELECT is issued with `LIMIT 2` and Resolve refuses to
// return when two or more rows match, so a real collision is
// surfaced as a hard error rather than silently returning the
// first hit.
//
// On failure the returned error is one of:
//
//   - ErrContextNotFound -- the row does not exist OR a
//     referenced graph entity has truly vanished (the latter
//     wraps graphreader.ErrNotFound so callers can drill in).
//   - any other database / context error, returned unchanged.
func (l *Log) Resolve(ctx context.Context, contextID string) (ResolvedContext, error) {
	if contextID == "" {
		return ResolvedContext{}, errors.New("recallcontext: Resolve: empty context_id")
	}
	if !uuidPattern.MatchString(contextID) {
		return ResolvedContext{}, fmt.Errorf(
			"recallcontext: Resolve: %q is not a valid UUID",
			contextID,
		)
	}

	const q = `
		SELECT
			context_id::text,
			repo_id::text,
			verb::text,
			query_json::text,
			node_ids::text[],
			edge_ids::text[],
			concept_ids::text[],
			reranker_model_version,
			served_under_degraded,
			created_at
		FROM recall_context_log
		WHERE context_id = $1
		LIMIT 2
	`
	rows, err := l.db.QueryContext(ctx, q, contextID)
	if err != nil {
		return ResolvedContext{}, fmt.Errorf("recallcontext: Resolve query: %w", err)
	}
	defer rows.Close()

	var (
		out      ResolvedContext
		queryStr string
		nodeIDs  []string
		edgeIDs  []string
		concIDs  []string
		seen     int
	)
	for rows.Next() {
		seen++
		if seen > 1 {
			return ResolvedContext{}, fmt.Errorf(
				"recallcontext: Resolve: context_id %s matched >1 partition row (corruption)",
				contextID,
			)
		}
		if err := rows.Scan(
			&out.ContextID, &out.RepoID, &out.Verb, &queryStr,
			pq.Array(&nodeIDs), pq.Array(&edgeIDs), pq.Array(&concIDs),
			&out.RerankerModelVersion, &out.ServedUnderDegraded,
			&out.CreatedAt,
		); err != nil {
			return ResolvedContext{}, fmt.Errorf("recallcontext: Resolve scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return ResolvedContext{}, fmt.Errorf("recallcontext: Resolve rows: %w", err)
	}
	if seen == 0 {
		return ResolvedContext{}, ErrContextNotFound
	}
	out.QueryJSON = json.RawMessage(queryStr)

	// Dereference in storage order. IncludeRetired=true per the
	// implementation-plan brief so historical contexts surface
	// even when a referenced node / edge has since been
	// tombstoned. Concept has no retirement table (see the
	// graphreader concept.go package doc), so GetConcept takes
	// no opts.
	//
	// Missing-reference wrapping
	// --------------------------
	// A referenced node/edge/concept that genuinely vanished is
	// a database-corruption case (an append-only table referenced
	// rows that were retired without the application-level
	// retention coordination expected by G3/G4). The returned
	// error therefore wraps BOTH the recallcontext sentinel
	// (ErrContextNotFound -- so generic mgmt-api "context
	// missing" branches still match) AND the underlying
	// graphreader.ErrNotFound (so callers that want to drill
	// into "which entity" can errors.Is on the graphreader
	// sentinel and route to a different alert pipeline). Go
	// 1.20+ multi-%w preserves both in the error chain, so both
	// `errors.Is(err, ErrContextNotFound)` and
	// `errors.Is(err, graphreader.ErrNotFound)` return true on
	// the same error value.
	const opOnMissing = "recallcontext: Resolve: %s id %s not found in graph: %w: %w"
	opts := graphreader.ReaderOptions{IncludeRetired: true}
	out.Nodes = make([]graphreader.Node, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		n, err := l.reader.GetNode(ctx, id, opts)
		if err != nil {
			if errors.Is(err, graphreader.ErrNotFound) {
				return ResolvedContext{}, fmt.Errorf(
					opOnMissing, "node", id,
					ErrContextNotFound, graphreader.ErrNotFound,
				)
			}
			return ResolvedContext{}, fmt.Errorf(
				"recallcontext: Resolve: GetNode %s: %w", id, err,
			)
		}
		out.Nodes = append(out.Nodes, n)
	}
	out.Edges = make([]graphreader.Edge, 0, len(edgeIDs))
	for _, id := range edgeIDs {
		e, err := l.reader.GetEdge(ctx, id, opts)
		if err != nil {
			if errors.Is(err, graphreader.ErrNotFound) {
				return ResolvedContext{}, fmt.Errorf(
					opOnMissing, "edge", id,
					ErrContextNotFound, graphreader.ErrNotFound,
				)
			}
			return ResolvedContext{}, fmt.Errorf(
				"recallcontext: Resolve: GetEdge %s: %w", id, err,
			)
		}
		out.Edges = append(out.Edges, e)
	}
	out.Concepts = make([]graphreader.Concept, 0, len(concIDs))
	for _, id := range concIDs {
		c, err := l.reader.GetConcept(ctx, id)
		if err != nil {
			if errors.Is(err, graphreader.ErrNotFound) {
				return ResolvedContext{}, fmt.Errorf(
					opOnMissing, "concept", id,
					ErrContextNotFound, graphreader.ErrNotFound,
				)
			}
			return ResolvedContext{}, fmt.Errorf(
				"recallcontext: Resolve: GetConcept %s: %w", id, err,
			)
		}
		out.Concepts = append(out.Concepts, c)
	}
	return out, nil
}

// ----- Plumbing ----------------------------------------------------

// runInTx wraps a body in a single PostgreSQL transaction,
// classifies any returned error through classifyErr (so
// SQLSTATE 42501 surfaces as WriteContractViolation), and
// commits on success. Mirrors the equivalent helper in
// graphwriter / retirement by intent.
func (l *Log) runInTx(
	ctx context.Context, op string, body func(tx *sql.Tx) error,
) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyErr(op, fmt.Errorf("recallcontext: %s begin: %w", op, err))
	}
	if err := body(tx); err != nil {
		_ = tx.Rollback()
		return classifyErr(op, err)
	}
	if err := tx.Commit(); err != nil {
		return classifyErr(op, fmt.Errorf("recallcontext: %s commit: %w", op, err))
	}
	return nil
}

// emitSuccess emits the structured-logging audit record for a
// successful call. Mirrors the retirement package's shape
// (`<package>.<op>` at info level) so operators grepping the
// JSON logs see consistent message names across the three
// writer libraries.
func (l *Log) emitSuccess(op, contextID string, in AppendInput) {
	l.logger.Info("recallcontext."+op,
		slog.String("op", op),
		slog.String("context_id", contextID),
		slog.String("repo_id", in.RepoID.String()),
		slog.String("verb", in.Verb),
		slog.Int("node_count", len(in.NodeIDs)),
		slog.Int("edge_count", len(in.EdgeIDs)),
		slog.Int("concept_count", len(in.ConceptIDs)),
		slog.String("reranker_model_version", in.RerankerModelVersion),
		slog.Bool("served_under_degraded", in.ServedUnderDegraded),
	)
}

// emitFailure emits the structured-logging audit record for a
// failed call. Records the typed-error classification booleans
// so operator dashboards can split "role-grant denial" from
// "everything else" without parsing message strings.
func (l *Log) emitFailure(op, contextID string, err error) {
	var contractV *WriteContractViolation
	l.logger.Error("recallcontext."+op+".failed",
		slog.String("op", op),
		slog.String("context_id", contextID),
		slog.String("error", err.Error()),
		slog.String("error_type", fmt.Sprintf("%T", err)),
		slog.Bool("contract_violation", errors.As(err, &contractV)),
	)
}
