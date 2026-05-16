// Package graphwriter is the only library in services/agent-memory
// allowed to perform DML against the structural graph tables
// (`repo`, `repo_commit`, `node`, `edge`). It encapsulates:
//
//   - Computation of the G2 fingerprint for every Node and Edge
//     it inserts (architecture.md §1.3).
//
//   - Idempotent INSERT semantics so a re-ingest of the same
//     commit does not produce duplicate rows.
//
//   - A typed `WriteContractViolation` error that surfaces
//     SQLSTATE 42501 from PostgreSQL when the writer (acting as
//     the `agent_memory_app` role) accidentally issues DML the
//     role-grant policy forbids — the database layer is the
//     load-bearing enforcer of G5 append-only invariants per
//     tech-spec §8.7.4.
//
//   - A single structured-logging middleware — `emitAudit` —
//     that runs on EVERY public writer call (EnsureRepo,
//     EnsureCommit, InsertNode, InsertEdge,
//     InsertObservedCallsEdge). It emits exactly ONE structured
//     log record per call:
//
//     · success → info level `graphwriter.<op>` with
//     `{op, repo_id, kind, fingerprint_hex, sha, ...}`.
//     · failure → error level `graphwriter.<op>.failed`
//     with the same fields plus `error`, `error_type`,
//     and `contract_violation` (true when the failure was
//     classified to `*WriteContractViolation`).
//     · panic   → error level `graphwriter.<op>.failed`
//     with `{panic, contract_violation:false}`; the
//     original panic is re-raised so the goroutine still
//     crashes.
//
//     `kind` and `fingerprint_hex` are emitted as empty strings
//     on operations that don't have them (EnsureRepo,
//     EnsureCommit) so the audit schema is uniform across
//     methods. Operators consuming structured logs see the same
//     four-key tuple regardless of which writer call produced
//     the record.
//
// Each public writer method opens its own PostgreSQL transaction,
// runs the insert, and commits. Stage 2.1's acceptance scenarios
// (idempotent insert, fingerprint determinism, denied UPDATE) all
// gate on this single-transaction semantics. Future stages
// (Stage 3.1 Repo Indexer) will need batched-tx writes for
// throughput; the internal helpers below are split so a
// `BatchTx`-style entry point can be layered on without changing
// the per-call public API.
package graphwriter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// pgErrCodeInsufficientPrivilege is the SQLSTATE PostgreSQL
// returns when a role lacks the privilege required by a
// statement (per the canonical error-code catalogue, class 42).
// This is the load-bearing classification for
// WriteContractViolation; the message text is locale-sensitive
// and not relied on.
const pgErrCodeInsufficientPrivilege = "42501"

// Writer is the only object that performs DML against the graph
// tables. Every method opens its own transaction and commits
// before returning. Construct one with New().
//
// Writer is safe for concurrent use: it does not retain state
// across method calls and the underlying *sql.DB pools its own
// connections.
type Writer struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

// New constructs a Writer over the supplied *sql.DB. The DB must
// be authenticated as a role that satisfies the GRANTs in
// migration 0016 (typically `agent_memory_app`). A nil logger is
// replaced with slog.Default(); a nil now-func is replaced with
// time.Now.
func New(db *sql.DB, logger *slog.Logger) *Writer {
	if db == nil {
		panic("graphwriter: nil *sql.DB")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Writer{
		db:     db,
		logger: logger,
		now:    time.Now,
	}
}

// WriteContractViolation indicates the writer attempted DML the
// `agent_memory_app` role does not have privileges for — almost
// always an UPDATE on an append-only table (G5 invariant
// enforced at the role-grant layer per tech-spec §8.7.4).
//
// Wrapping the raw driver error in a typed value lets callers
// distinguish "the database refused this on principle" from "the
// database was unreachable" — the former is a code bug to fix
// upstream, the latter is a transient outage to retry.
type WriteContractViolation struct {
	// Op identifies the writer entry point that triggered the
	// violation (e.g. "InsertNode", "force_update_for_testing").
	Op string
	// SQLState is the PostgreSQL SQLSTATE string returned by the
	// driver. For this error type it is always "42501".
	SQLState string
	// Err is the wrapped *pq.Error returned by lib/pq.
	Err error
}

func (e *WriteContractViolation) Error() string {
	return fmt.Sprintf(
		"graphwriter: %s denied by role-grant policy (SQLSTATE %s): %v",
		e.Op, e.SQLState, e.Err,
	)
}

// Unwrap exposes the wrapped driver error for errors.As / errors.Is.
func (e *WriteContractViolation) Unwrap() error { return e.Err }

// classifyErr maps a raw SQL error into one of the typed errors
// graphwriter exposes. Currently the only typed wrapping is
// WriteContractViolation (SQLSTATE 42501); other errors pass
// through unchanged so callers see the original driver context.
//
// Classification happens at the runInTx boundary so the
// emitAudit middleware can simply test `errors.As(err,
// *WriteContractViolation)` without re-running the classifier.
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

// auditFields is the uniform shape every writer call emits
// through `emitAudit`. Optional fields (Kind, FingerprintHex)
// are empty strings for operations that don't have them
// (EnsureRepo, EnsureCommit) so structured-log consumers always
// see the same four-key tuple.
//
// `Extras` is the per-op tail — node_id, edge_id, inserted, url,
// etc. — appended verbatim to the record.
type auditFields struct {
	RepoID         string
	Kind           string
	FingerprintHex string
	SHA            string
	Extras         []slog.Attr
}

// emitAudit is THE structured-logging middleware. Every public
// writer method routes through it via a deferred closure so that
// success, error, and panic each produce exactly one structured
// log record with the same `{op, repo_id, kind, fingerprint_hex,
// sha}` tuple. On failure the record also carries `error`,
// `error_type`, and `contract_violation`.
//
// Behaviour pinned by tests (writer_integration_test.go):
//   - success path: msg = "graphwriter.<op>" at Info level.
//   - failure path: msg = "graphwriter.<op>.failed" at Error
//     level, `contract_violation` is true iff the underlying
//     error satisfies `errors.As(err, *WriteContractViolation)`.
//
// emitAudit MUST NOT panic; it intentionally only reads `err`
// and never reclassifies. Classification happens once in
// `classifyErr` at the runInTx boundary.
func (w *Writer) emitAudit(op string, f auditFields, err error) {
	attrs := make([]any, 0, 6+len(f.Extras))
	attrs = append(attrs,
		slog.String("op", op),
		slog.String("repo_id", f.RepoID),
		slog.String("kind", f.Kind),
		slog.String("fingerprint_hex", f.FingerprintHex),
		slog.String("sha", f.SHA),
	)
	for _, x := range f.Extras {
		attrs = append(attrs, x)
	}
	if err == nil {
		w.logger.Info("graphwriter."+op, attrs...)
		return
	}
	var cv *WriteContractViolation
	contractViolation := errors.As(err, &cv)
	attrs = append(attrs,
		slog.String("error", err.Error()),
		slog.String("error_type", fmt.Sprintf("%T", err)),
		slog.Bool("contract_violation", contractViolation),
	)
	w.logger.Error("graphwriter."+op+".failed", attrs...)
}

// auditDefer is the single boilerplate every public writer
// method uses to wire `emitAudit`. It returns a function that
// recovers from a panic (logs failure, then re-panics) and
// otherwise emits the success-or-error record based on the
// caller's final err.
//
// Usage (named-return required for the err pointer to capture
// the final value):
//
//	func (w *Writer) Foo(...) (rec FooRecord, err error) {
//	    fields := auditFields{ ... }
//	    defer w.auditDefer("foo", &fields, &err)()
//	    ...
//	}
//
// The trailing `()` is intentional: auditDefer returns the
// closure that runs at function exit.
func (w *Writer) auditDefer(op string, fields *auditFields, errp *error) func() {
	return func() {
		if r := recover(); r != nil {
			fields.Extras = append(fields.Extras, slog.Any("panic", r))
			w.emitAudit(op, *fields, fmt.Errorf("graphwriter: %s panic: %v", op, r))
			panic(r)
		}
		w.emitAudit(op, *fields, *errp)
	}
}

// ----- EnsureRepo --------------------------------------------------

// RepoInput describes the Repo row to upsert. URL is the natural
// key (UNIQUE in the schema, per migration 0002); the remaining
// fields are mutable and the upsert overwrites them on conflict.
type RepoInput struct {
	URL            string
	DefaultBranch  string
	CurrentHeadSHA string
	LanguageHints  []string
}

// RepoRecord is the post-upsert state of a Repo row.
type RepoRecord struct {
	// RepoID is the surrogate UUID primary key as a textual UUID.
	RepoID string
	// ID is the same value as RepoID but in the canonical 16-byte
	// form used by the fingerprint domain.
	ID fingerprint.RepoID
	// Inserted is true when the row was newly created and false
	// when an existing row was updated. Stage 2.1's idempotent-
	// ingest scenario asserts on this flag.
	Inserted bool
}

// EnsureRepo upserts a Repo by URL. The application role has
// UPDATE on `repo` (tech-spec §8.7.4) so we can use a single
// `INSERT ... ON CONFLICT (url) DO UPDATE` and disambiguate the
// insert-vs-update branch with the `(xmax = 0)` trick that the
// PostgreSQL community has standardised on.
//
// Routes through `emitAudit` so every call produces a structured
// log record (success or failure) carrying the uniform
// `{op, repo_id, kind, fingerprint_hex, sha, ...}` tuple. `kind`
// and `fingerprint_hex` are emitted empty because they don't
// apply to repo rows.
func (w *Writer) EnsureRepo(ctx context.Context, in RepoInput) (rec RepoRecord, err error) {
	fields := auditFields{
		SHA: in.CurrentHeadSHA,
		Extras: []slog.Attr{
			slog.String("url", in.URL),
		},
	}
	defer w.auditDefer("ensure_repo", &fields, &err)()

	if in.URL == "" {
		return RepoRecord{}, errors.New("graphwriter: EnsureRepo: empty url")
	}
	// language_hints is NOT NULL DEFAULT '{}' in the schema, so a
	// nil slice from Go must become an empty array, not NULL.
	hints := in.LanguageHints
	if hints == nil {
		hints = []string{}
	}

	const q = `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (url) DO UPDATE SET
		    default_branch   = EXCLUDED.default_branch,
		    current_head_sha = EXCLUDED.current_head_sha,
		    language_hints   = EXCLUDED.language_hints
		RETURNING repo_id::text, (xmax = 0) AS inserted
	`
	var idStr string
	err = w.runInTx(ctx, "EnsureRepo", func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, q,
			in.URL, in.DefaultBranch, in.CurrentHeadSHA, pq.Array(hints),
		).Scan(&idStr, &rec.Inserted)
	})
	if err != nil {
		return RepoRecord{}, err
	}
	repoID, err := fingerprint.ParseRepoID(idStr)
	if err != nil {
		return RepoRecord{}, fmt.Errorf("graphwriter: EnsureRepo parse repo_id: %w", err)
	}
	rec.RepoID = idStr
	rec.ID = repoID

	fields.RepoID = idStr
	fields.Extras = append(fields.Extras, slog.Bool("inserted", rec.Inserted))
	return rec, nil
}

// ----- EnsureCommit ------------------------------------------------

// CommitInput describes the repo_commit row to insert. The
// natural key (repo_id, sha) is UNIQUE per migration 0002; the
// remaining fields are immutable once written (G5).
type CommitInput struct {
	RepoID      fingerprint.RepoID
	SHA         string
	ParentSHA   string // empty for the root commit
	CommittedAt time.Time
}

// CommitRecord is the post-insert state of a repo_commit row.
type CommitRecord struct {
	RepoID   string
	SHA      string
	Inserted bool
}

// EnsureCommit idempotently writes a repo_commit row. The table
// is append-only (`agent_memory_app` has only INSERT + SELECT) so
// the pattern is a two-step `INSERT ... ON CONFLICT DO NOTHING`
// followed by a fallback `SELECT` if the conflict path was taken.
// READ COMMITTED guarantees the second statement sees rows
// committed by a concurrent transaction that won the race.
//
// Routes through `emitAudit` so every call emits a structured
// log line. `kind` and `fingerprint_hex` are empty because they
// don't apply to commit rows.
func (w *Writer) EnsureCommit(ctx context.Context, in CommitInput) (rec CommitRecord, err error) {
	repoIDStr := in.RepoID.String()
	fields := auditFields{
		RepoID: repoIDStr,
		SHA:    in.SHA,
	}
	defer w.auditDefer("ensure_commit", &fields, &err)()

	if in.RepoID.IsZero() {
		return CommitRecord{}, errors.New("graphwriter: EnsureCommit: zero repo_id")
	}
	if in.SHA == "" {
		return CommitRecord{}, errors.New("graphwriter: EnsureCommit: empty sha")
	}
	if in.CommittedAt.IsZero() {
		in.CommittedAt = w.now()
	}

	// `parent_sha` column is nullable; map empty Go string -> SQL NULL.
	var parent sql.NullString
	if in.ParentSHA != "" {
		parent = sql.NullString{String: in.ParentSHA, Valid: true}
	}

	rec = CommitRecord{RepoID: repoIDStr, SHA: in.SHA}
	err = w.runInTx(ctx, "EnsureCommit", func(tx *sql.Tx) error {
		const insertQ = `
			INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (repo_id, sha) DO NOTHING
			RETURNING repo_id::text, sha
		`
		var gotRepo, gotSHA string
		err := tx.QueryRowContext(ctx, insertQ,
			repoIDStr, in.SHA, parent, in.CommittedAt.UTC(),
		).Scan(&gotRepo, &gotSHA)
		switch {
		case err == nil:
			rec.Inserted = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			// Conflict path: another caller already inserted this
			// row. Verify the row is actually present (defence
			// against the rare snapshot-isolation edge case where
			// the conflicting tx is still in flight).
			const selectQ = `
				SELECT 1 FROM repo_commit
				WHERE repo_id = $1 AND sha = $2
			`
			var seen int
			if err := tx.QueryRowContext(ctx, selectQ, repoIDStr, in.SHA).Scan(&seen); err != nil {
				return fmt.Errorf("graphwriter: EnsureCommit verify: %w", err)
			}
			rec.Inserted = false
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return CommitRecord{}, err
	}
	fields.Extras = append(fields.Extras, slog.Bool("inserted", rec.Inserted))
	return rec, nil
}

// ----- InsertNode --------------------------------------------------

// NodeInput describes a Node row to insert. The G2 fingerprint
// is computed from (RepoID, Kind, CanonicalSignature, FromSHA);
// passing the same tuple twice yields the same row.
type NodeInput struct {
	RepoID             fingerprint.RepoID
	Kind               string
	CanonicalSignature string
	// ParentNodeID is the textual UUID of the parent Node in the
	// repo→package→file→class→method→block hierarchy. Empty for
	// the repo Node (the only one without a parent).
	ParentNodeID string
	FromSHA      string
	// AttrsJSON is the language-specific attribute bag stored in
	// `attrs_json`. Empty / nil is materialised as `{}`. Must be
	// a JSON object (tech-spec §5.2.1) — arrays / scalars are
	// rejected before the SQL round-trip.
	AttrsJSON json.RawMessage
}

// NodeRecord is the post-insert state of a node row.
type NodeRecord struct {
	NodeID      string
	Fingerprint fingerprint.Sum
	Inserted    bool
}

// InsertNode idempotently writes a Node by (repo_id, fingerprint).
// The fingerprint is computed inside the function from the
// caller's (RepoID, Kind, CanonicalSignature, FromSHA); callers
// who already hold the fingerprint (e.g. from a prior call) can
// rely on the returned NodeRecord.Fingerprint to confirm.
//
// When a `parent_node_id` is supplied the function verifies the
// parent belongs to the same `repo_id` inside the same
// transaction; cross-repo parents would silently corrupt the
// hierarchy walks the GraphReader does (architecture.md §4.5).
//
// Routes through `emitAudit` so every call emits exactly one
// structured log line carrying the uniform `{op, repo_id, kind,
// fingerprint_hex, sha}` tuple required by the brief.
func (w *Writer) InsertNode(ctx context.Context, in NodeInput) (rec NodeRecord, err error) {
	repoIDStr := in.RepoID.String()
	fields := auditFields{
		RepoID: repoIDStr,
		Kind:   in.Kind,
		SHA:    in.FromSHA,
	}
	defer w.auditDefer("insert_node", &fields, &err)()

	fp, err := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	if err != nil {
		return NodeRecord{}, fmt.Errorf("graphwriter: InsertNode fingerprint: %w", err)
	}
	fields.FingerprintHex = fp.Hex()

	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return NodeRecord{}, fmt.Errorf("graphwriter: InsertNode attrs_json: %w", err)
	}

	rec = NodeRecord{Fingerprint: fp}
	err = w.runInTx(ctx, "InsertNode", func(tx *sql.Tx) error {
		// Same-repo parent guard. Resolves the parent inside the
		// tx so a concurrent retire-then-reinsert race cannot
		// slip a cross-repo parent past this check.
		if in.ParentNodeID != "" {
			var parentRepo string
			err := tx.QueryRowContext(ctx,
				`SELECT repo_id::text FROM node WHERE node_id = $1`,
				in.ParentNodeID,
			).Scan(&parentRepo)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("graphwriter: InsertNode: parent_node_id %s not found",
					in.ParentNodeID)
			}
			if err != nil {
				return fmt.Errorf("graphwriter: InsertNode: lookup parent: %w", err)
			}
			if parentRepo != repoIDStr {
				return fmt.Errorf(
					"graphwriter: InsertNode: parent_node_id %s belongs to repo %s, not %s",
					in.ParentNodeID, parentRepo, repoIDStr,
				)
			}
		}

		const insertQ = `
			INSERT INTO node
			    (fingerprint, repo_id, kind, canonical_signature, parent_node_id, from_sha, attrs_json)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
			ON CONFLICT (repo_id, fingerprint) DO NOTHING
			RETURNING node_id::text
		`
		var parent sql.NullString
		if in.ParentNodeID != "" {
			parent = sql.NullString{String: in.ParentNodeID, Valid: true}
		}
		err := tx.QueryRowContext(ctx, insertQ,
			fp.Bytes(), repoIDStr, in.Kind, in.CanonicalSignature,
			parent, in.FromSHA, string(attrs),
		).Scan(&rec.NodeID)
		switch {
		case err == nil:
			rec.Inserted = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			const selectQ = `
				SELECT node_id::text FROM node
				WHERE repo_id = $1 AND fingerprint = $2
			`
			if err := tx.QueryRowContext(ctx, selectQ, repoIDStr, fp.Bytes()).
				Scan(&rec.NodeID); err != nil {
				return fmt.Errorf("graphwriter: InsertNode fallback select: %w", err)
			}
			rec.Inserted = false
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return NodeRecord{}, err
	}
	fields.Extras = append(fields.Extras,
		slog.String("node_id", rec.NodeID),
		slog.Bool("inserted", rec.Inserted),
	)
	return rec, nil
}

// ----- InsertEdge / InsertObservedCallsEdge -----------------------

// EdgeInput describes the Edge row to insert. The Edge fingerprint
// is computed inside the writer from the *stored* fingerprints of
// the src/dst Nodes (looked up by id inside the same transaction)
// — passing the fingerprints directly would let a buggy caller
// mint an edge whose identity disagrees with what the schema
// stores, defeating G2.
type EdgeInput struct {
	RepoID    fingerprint.RepoID
	Kind      string // edge_kind enum value
	SrcNodeID string
	DstNodeID string
	FromSHA   string
	AttrsJSON json.RawMessage
}

// EdgeRecord is the post-insert state of an edge row.
type EdgeRecord struct {
	EdgeID      string
	Fingerprint fingerprint.Sum
	SrcFP       fingerprint.Sum
	DstFP       fingerprint.Sum
	Inserted    bool
}

// InsertEdge idempotently writes an Edge by (repo_id, fingerprint).
// The src/dst node fingerprints are read from the database inside
// the same transaction so the edge's identity is byte-identical
// to what an outside re-computation would produce.
//
// Both endpoints must belong to in.RepoID; cross-repo edges are
// rejected with a typed error before any INSERT is attempted.
//
// Routes through `emitAudit` so every call emits exactly one
// structured log line. The op name is "insert_edge".
func (w *Writer) InsertEdge(ctx context.Context, in EdgeInput) (rec EdgeRecord, err error) {
	repoIDStr := in.RepoID.String()
	fields := auditFields{
		RepoID: repoIDStr,
		Kind:   in.Kind,
		SHA:    in.FromSHA,
	}
	defer w.auditDefer("insert_edge", &fields, &err)()
	rec, err = w.insertEdgeImpl(ctx, in, &fields)
	return rec, err
}

// InsertObservedCallsEdge is the §3.3-step-3 entry point used by
// the Span Ingestor. It is a thin specialisation of InsertEdge
// that pins kind = "observed_calls" so call-site code reads
// declaratively. The function returns the existing edge record
// when the same (repo_id, src, dst, sha) tuple has been seen
// before — Stage 2.1 calls this out explicitly: the edge writer
// returns the existing edge if the fingerprint is already present.
//
// Emits its OWN audit record under op="insert_observed_calls_edge"
// — it does NOT delegate to InsertEdge, which would produce two
// log lines per single public call. The underlying SQL work runs
// through the shared `insertEdgeImpl` helper.
func (w *Writer) InsertObservedCallsEdge(ctx context.Context, in EdgeInput) (rec EdgeRecord, err error) {
	in.Kind = "observed_calls"
	repoIDStr := in.RepoID.String()
	fields := auditFields{
		RepoID: repoIDStr,
		Kind:   in.Kind,
		SHA:    in.FromSHA,
	}
	defer w.auditDefer("insert_observed_calls_edge", &fields, &err)()
	rec, err = w.insertEdgeImpl(ctx, in, &fields)
	return rec, err
}

// insertEdgeImpl is the shared body of InsertEdge and
// InsertObservedCallsEdge. It performs no audit logging itself;
// the caller installs the deferred `emitAudit` and supplies the
// `auditFields` pointer so this helper can write
// fingerprint_hex / edge_id / inserted back as they're computed.
//
// `auditFields.RepoID`, `.Kind`, `.SHA` MUST be pre-populated by
// the caller — this helper only fills the fields that depend on
// SQL results.
func (w *Writer) insertEdgeImpl(
	ctx context.Context, in EdgeInput, fields *auditFields,
) (EdgeRecord, error) {
	if in.Kind == "" {
		return EdgeRecord{}, errors.New("graphwriter: InsertEdge: empty kind")
	}
	if in.SrcNodeID == "" || in.DstNodeID == "" {
		return EdgeRecord{}, errors.New("graphwriter: InsertEdge: empty src/dst node_id")
	}
	if in.FromSHA == "" {
		return EdgeRecord{}, errors.New("graphwriter: InsertEdge: empty from_sha")
	}
	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return EdgeRecord{}, fmt.Errorf("graphwriter: InsertEdge attrs_json: %w", err)
	}
	repoIDStr := in.RepoID.String()

	var rec EdgeRecord
	err = w.runInTx(ctx, "InsertEdge", func(tx *sql.Tx) error {
		// Resolve src/dst inside the tx. A single SELECT with
		// ARRAY[$1,$2] would be cuter but it's harder to map back
		// to the right endpoint when the rows come back.
		srcRepo, srcFP, err := lookupNodeFingerprint(ctx, tx, in.SrcNodeID)
		if err != nil {
			return fmt.Errorf("graphwriter: InsertEdge src: %w", err)
		}
		dstRepo, dstFP, err := lookupNodeFingerprint(ctx, tx, in.DstNodeID)
		if err != nil {
			return fmt.Errorf("graphwriter: InsertEdge dst: %w", err)
		}
		if srcRepo != repoIDStr {
			return fmt.Errorf(
				"graphwriter: InsertEdge: src_node_id %s belongs to repo %s, not %s",
				in.SrcNodeID, srcRepo, repoIDStr,
			)
		}
		if dstRepo != repoIDStr {
			return fmt.Errorf(
				"graphwriter: InsertEdge: dst_node_id %s belongs to repo %s, not %s",
				in.DstNodeID, dstRepo, repoIDStr,
			)
		}

		fp, err := fingerprint.EdgeFingerprint(in.RepoID, in.Kind, srcFP, dstFP, in.FromSHA)
		if err != nil {
			return fmt.Errorf("graphwriter: InsertEdge fingerprint: %w", err)
		}
		rec.Fingerprint = fp
		rec.SrcFP = srcFP
		rec.DstFP = dstFP
		fields.FingerprintHex = fp.Hex()

		const insertQ = `
			INSERT INTO edge
			    (fingerprint, repo_id, kind, src_node_id, dst_node_id, from_sha, attrs_json)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
			ON CONFLICT (repo_id, fingerprint) DO NOTHING
			RETURNING edge_id::text
		`
		err = tx.QueryRowContext(ctx, insertQ,
			fp.Bytes(), repoIDStr, in.Kind,
			in.SrcNodeID, in.DstNodeID, in.FromSHA, string(attrs),
		).Scan(&rec.EdgeID)
		switch {
		case err == nil:
			rec.Inserted = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			const selectQ = `
				SELECT edge_id::text FROM edge
				WHERE repo_id = $1 AND fingerprint = $2
			`
			if err := tx.QueryRowContext(ctx, selectQ, repoIDStr, fp.Bytes()).
				Scan(&rec.EdgeID); err != nil {
				return fmt.Errorf("graphwriter: InsertEdge fallback select: %w", err)
			}
			rec.Inserted = false
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return EdgeRecord{}, err
	}
	fields.Extras = append(fields.Extras,
		slog.String("edge_id", rec.EdgeID),
		slog.Bool("inserted", rec.Inserted),
	)
	return rec, nil
}

// lookupNodeFingerprint reads a node's repo_id and fingerprint
// inside the supplied transaction so the (repo_id, fingerprint)
// the writer hashes against the database matches the (repo_id,
// fingerprint) the database actually stores. Returns sql.ErrNoRows
// wrapped if the node is missing.
func lookupNodeFingerprint(
	ctx context.Context, tx *sql.Tx, nodeID string,
) (repoID string, fp fingerprint.Sum, err error) {
	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT repo_id::text, fingerprint FROM node WHERE node_id = $1`,
		nodeID,
	).Scan(&repoID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fingerprint.Sum{}, fmt.Errorf("node_id %s not found", nodeID)
	}
	if err != nil {
		return "", fingerprint.Sum{}, err
	}
	fp, err = fingerprint.SumFromBytes(raw)
	if err != nil {
		return "", fingerprint.Sum{}, fmt.Errorf("decode fingerprint for %s: %w", nodeID, err)
	}
	return repoID, fp, nil
}

// runInTx wraps a body in a single PostgreSQL transaction,
// classifies any returned error through classifyErr (so
// SQLSTATE 42501 surfaces as WriteContractViolation), and emits
// a debug log on commit. The transaction uses the database's
// default isolation level (READ COMMITTED on PostgreSQL 16) which
// is what the role-grant scenario tests assume.
func (w *Writer) runInTx(
	ctx context.Context,
	op string,
	body func(tx *sql.Tx) error,
) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyErr(op, fmt.Errorf("graphwriter: %s begin: %w", op, err))
	}
	if err := body(tx); err != nil {
		_ = tx.Rollback()
		return classifyErr(op, err)
	}
	if err := tx.Commit(); err != nil {
		return classifyErr(op, fmt.Errorf("graphwriter: %s commit: %w", op, err))
	}
	return nil
}

// normaliseAttrs returns a JSON object byte slice that satisfies
// the `attrs_json jsonb NOT NULL DEFAULT '{}'` column contract:
//
//   - nil or empty input becomes the literal "{}".
//   - a valid JSON object passes through unchanged.
//   - any other JSON value (array, scalar, null) is rejected,
//     because the architecture treats attrs_json as a property
//     bag and never as a top-level array or scalar.
func normaliseAttrs(in json.RawMessage) (json.RawMessage, error) {
	if len(in) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(in) {
		return nil, errors.New("attrs_json is not valid JSON")
	}
	// Cheapest object-shape check that does not allocate a full
	// decode tree: skip leading whitespace, look for '{'.
	for _, b := range in {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return in, nil
		default:
			return nil, fmt.Errorf("attrs_json must be a JSON object, got %q", string([]byte{b}))
		}
	}
	return nil, errors.New("attrs_json is empty whitespace")
}
