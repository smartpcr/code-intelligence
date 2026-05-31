//go:build cgo

// Package sqlite is the SQLite backend for `graphsink.Sink`. It
// is the default storage for the `codeintel scan` CLI when the
// operator does not bring their own Postgres -- one SQLite file
// per repo (or one file holding many repos, keyed by `repo_id`),
// with no external services required.
//
// CGO is mandatory. `mattn/go-sqlite3` is a CGO driver; the
// `//go:build cgo` tag on this file (and on `reader.go` once
// Stage 3.6 lands it) makes a CGO=0 build of the `sqlite`
// package fail at compile time -- it has no Go files under
// CGO=0 -- so the codeintel binary cannot silently produce a
// build that links but cannot open a SQLite file. This matches
// tech-spec C7 / §4.3 which already mandate CGO=1 for the
// tree-sitter parsers (`internal/repoindexer/ast/parsers_cgo.go`),
// so the SQLite backend adds no new toolchain requirement on top
// of what the scanner already needs.
//
// SCHEMA BOOTSTRAP. `Open` applies `schema.sql` (embedded via
// `//go:embed`) on every call so a fresh database file gets the
// `repo`, `repo_commit`, `node`, `edge` tables and indexes
// without needing a separate migration step. Every DDL statement
// uses `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT
// EXISTS`, so applying the schema to an already-bootstrapped file
// is a no-op.
//
// IDENTITY PARITY (S3.4). The fingerprint helpers in
// `pkg/fingerprint` are the SINGLE source of truth for node /
// edge identity. This file calls `fingerprint.NodeFingerprint`
// and `fingerprint.EdgeFingerprint` with byte-identical inputs
// to `*graphwriter.Writer.InsertNode` / `InsertEdge`. As a
// result a repo scanned to SQLite and later re-scanned to
// Postgres produces the same `(repo_id, fingerprint)` pairs --
// the dedupe key both backends use -- so node identities match
// across stores. The synthesised UUIDs (`repo_id`, `node_id`,
// `edge_id`) are surrogate PKs only; the natural identity is
// the fingerprint.
//
// CONCURRENCY. SQLite serialises writers at the database level
// (one writer at a time). The Sink opens a single
// `*sql.DB` and lets the stdlib pool serialise transactions;
// readers (Stage 3.6) will share the same handle. WAL journal
// mode is enabled in `Open` so a long read does not block a
// concurrent write (and vice versa) for the local CLI use case.
//
// FOREIGN KEYS. SQLite's `PRAGMA foreign_keys = ON` is OFF by
// default. The Sink issues that pragma per-connection through a
// `ConnectHook`-equivalent (a one-time `Exec` against every
// fresh connection produced by the pool) so the `ON DELETE
// RESTRICT` references the schema declares are enforced.
//
// FLUSH / CLOSE. SQLite commits each transaction inline, so
// `Flush` is a no-op (and returns nil) -- there is no buffered
// state to drain. `Close` releases the underlying `*sql.DB`
// handle and is idempotent: the second and subsequent calls
// return nil per the `graphsink.Sink` contract.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3" // CGO sqlite3 driver

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

//go:embed schema.sql
var schemaSQL string

// driverName is the registered sqlite3 driver name. Kept as a
// const so a future switch to a CGO-free driver (e.g. modernc.org/sqlite)
// only edits this line plus the blank-import above.
const driverName = "sqlite3"

// ErrSinkClosed is the sentinel returned by every Sink method
// after Close has been called. It wraps `sql.ErrConnDone` so
// existing callers using `errors.Is(err, sql.ErrConnDone)` keep
// working unchanged (the Sink contract calls this out in
// `graphsink/sink.go`).
var ErrSinkClosed = fmt.Errorf("graphsink/sqlite: sink closed: %w", sql.ErrConnDone)

// Sink is the `graphsink.Sink` implementation backed by SQLite.
//
// The zero value is NOT usable; call `Open` to construct one.
// All exported methods are safe for concurrent use by multiple
// goroutines -- they delegate to `*sql.DB`, which serialises
// transactions internally.
type Sink struct {
	db *sql.DB

	closeOnce sync.Once
	closeErr  error
	// closed is set atomically inside closeOnce.Do BEFORE the
	// underlying db.Close is invoked, so checkOpen can read it
	// from any goroutine without holding a mutex. A successful
	// Close (the common case) leaves closeErr == nil, so the
	// flag is needed as a distinct signal from "closeErr is
	// non-nil".
	closed atomic.Bool
}

// compile-time assertion: *Sink satisfies the graphsink.Sink
// interface. If a future change to the Sink contract widens the
// surface, this assertion fails at build time inside the
// `sqlite` package so the gap is caught before the CLI wires
// the backend in.
var _ graphsink.Sink = (*Sink)(nil)

// Open opens (creating if necessary) the SQLite database at `dsn`
// and applies `schema.sql` so the `repo`, `repo_commit`, `node`,
// and `edge` tables exist. `dsn` is passed through to
// `mattn/go-sqlite3` verbatim; pass a filesystem path
// (e.g. `./graph.db`) for a normal database, `:memory:` for an
// ephemeral in-process store (used by the test suite), or any
// other DSN form the driver accepts.
//
// PRAGMA POLICY (iter-3 tightening):
//
//   - `_foreign_keys=on` is ALWAYS forced. The schema declares
//     `ON DELETE RESTRICT` references between `repo` ->
//     `repo_commit`, `node`, `edge` and within `node`
//     (parent_node_id). SQLite's foreign-key enforcement is OFF
//     by default; if the caller could disable it via a DSN
//     query (e.g. `graph.db?_foreign_keys=off`), the
//     hierarchy and cross-repo guards documented on
//     `InsertNode`/`InsertEdge` would silently weaken. Open
//     therefore STRIPS any caller-supplied `_foreign_keys=...`
//     binding and re-appends `_foreign_keys=on` so the
//     documented FK invariant is unambiguous and tamper-proof.
//   - `_journal_mode=WAL` and `_busy_timeout=5000` are DEFAULTS
//     the caller may override (a read-only snapshot scan might
//     reasonably want `_journal_mode=DELETE`, and a CI run with
//     no concurrent writers can lower the busy timeout).
//   - All other DSN parameters (e.g. `?cache=shared`,
//     `?mode=ro`) pass through unchanged.
//
// Returns a usable `*Sink` or a non-nil error. On error the
// underlying `*sql.DB` (if it was opened) is closed before the
// function returns so no leak occurs.
func Open(ctx context.Context, dsn string) (*Sink, error) {
	openDSN := buildDSN(dsn)

	db, err := sql.Open(driverName, openDSN)
	if err != nil {
		return nil, fmt.Errorf("graphsink/sqlite: open %q: %w", dsn, err)
	}
	// SQLite serialises writers; bound the pool small so we don't
	// fan out and immediately collide on SQLITE_BUSY. Reads will
	// be served from the same handle in Stage 3.6.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("graphsink/sqlite: ping %q: %w", dsn, err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("graphsink/sqlite: bootstrap schema: %w", err)
	}
	return &Sink{db: db}, nil
}

// buildDSN applies the iter-3 PRAGMA policy: caller's DSN is
// preserved verbatim except that any `_foreign_keys=...` binding
// is dropped (FK enforcement is non-negotiable), and the
// soft-default pragmas (`_journal_mode=WAL`, `_busy_timeout=5000`)
// are appended only when the caller has NOT supplied them. The
// mandatory `_foreign_keys=on` is always appended last so it
// wins regardless of upstream parsing order.
func buildDSN(dsn string) string {
	// Step 1: strip any caller-supplied _foreign_keys binding.
	stripped := stripQueryKey(dsn, "_foreign_keys")

	// Step 2: merge soft defaults (caller may override these).
	merged := mergeDefaultPragmas(stripped, [][2]string{
		{"_journal_mode", "WAL"},
		{"_busy_timeout", "5000"},
	})

	// Step 3: forcibly append _foreign_keys=on.
	if strings.ContainsRune(merged, '?') {
		return merged + "&_foreign_keys=on"
	}
	return merged + "?_foreign_keys=on"
}

// stripQueryKey returns `dsn` with every `key=<value>` binding
// removed from its query string. Used to enforce the
// non-negotiable `_foreign_keys=on` pragma: the caller's value
// (if any) is discarded so a misconfigured DSN cannot silently
// disable FK enforcement and weaken the documented hierarchy
// and cross-repo guards.
//
// Returns `dsn` unchanged when the query string is empty or
// contains no binding for `key`. When stripping the last binding
// also empties the query string, the trailing '?' is removed so
// the returned DSN is canonical.
func stripQueryKey(dsn, key string) string {
	idx := strings.IndexByte(dsn, '?')
	if idx < 0 {
		return dsn
	}
	prefix := dsn[:idx]
	q := dsn[idx+1:]
	needle := key + "="
	var kept []string
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, needle) {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return prefix
	}
	return prefix + "?" + strings.Join(kept, "&")
}

// mergeDefaultPragmas appends each (key, value) pair from
// `defaults` to `dsn` only when `dsn`'s existing query string
// does NOT already contain that key. Caller-supplied keys win;
// absent keys get our defaults. Used for SOFT defaults only --
// pragmas the caller is allowed to override (currently
// `_journal_mode` and `_busy_timeout`). The mandatory
// `_foreign_keys=on` enforcement lives in `buildDSN` and goes
// through `stripQueryKey` instead.
//
// Examples:
//
//	mergeDefaultPragmas(":memory:", defaults)
//	    -> ":memory:?_journal_mode=WAL&_busy_timeout=5000"
//
//	mergeDefaultPragmas("graph.db?cache=shared", defaults)
//	    -> "graph.db?cache=shared&_journal_mode=WAL&_busy_timeout=5000"
//
//	mergeDefaultPragmas("graph.db?_journal_mode=DELETE", defaults)
//	    -> "graph.db?_journal_mode=DELETE&_busy_timeout=5000"
func mergeDefaultPragmas(dsn string, defaults [][2]string) string {
	q := ""
	idx := strings.IndexByte(dsn, '?')
	if idx >= 0 {
		q = dsn[idx+1:]
	}
	var b strings.Builder
	b.WriteString(dsn)
	first := idx < 0
	for _, kv := range defaults {
		if containsKey(q, kv[0]) {
			continue
		}
		if first {
			b.WriteByte('?')
			first = false
		} else {
			b.WriteByte('&')
		}
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(kv[1])
	}
	return b.String()
}

// containsKey reports whether the URL query string `q` already
// binds `key` (i.e. contains a `key=` token at the start of `q`
// or immediately after a '&').
func containsKey(q, key string) bool {
	if q == "" {
		return false
	}
	needle := key + "="
	if strings.HasPrefix(q, needle) {
		return true
	}
	return strings.Contains(q, "&"+needle)
}

// Close releases the underlying database handle. Idempotent: the
// second and subsequent calls return nil per the `graphsink.Sink`
// contract. After Close returns, every other method on the Sink
// yields `ErrSinkClosed` (which wraps `sql.ErrConnDone`).
func (s *Sink) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Flush is a no-op for the SQLite backend: every Sink method
// commits inline through `runInTx`, so there is never any
// buffered state to drain. Returns ErrSinkClosed if the Sink
// has been closed.
func (s *Sink) Flush(ctx context.Context) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	return nil
}

// checkOpen returns ErrSinkClosed if Close has been called.
// Otherwise returns nil. Cheaper than re-pinging the DB on
// every call.
func (s *Sink) checkOpen() error {
	// atomic.Bool.Load synchronises with the Store inside
	// closeOnce.Do, so a concurrent Close-then-Insert race sees
	// either "open" (Store not yet observed; Insert proceeds and
	// either succeeds against the still-live handle or fails
	// with sql.ErrConnDone propagated from the pool) or "closed"
	// (ErrSinkClosed returned cleanly). No data race either way.
	if s.closed.Load() {
		return ErrSinkClosed
	}
	return nil
}

// runInTx executes fn inside a single transaction, committing on
// success and rolling back on error. Mirrors the
// `*graphwriter.Writer.runInTx` helper so the SQLite backend
// preserves the same all-or-nothing semantics on every write
// path.
func (s *Sink) runInTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// Rollback is a no-op after a successful Commit.
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// ----- EnsureRepo --------------------------------------------------

// EnsureRepo upserts a Repo row keyed by URL.
//
// IDENTITY POLICY (cross-backend parity, S3.4):
//
//   - When `in.RepoID` is the zero value, a fresh insert
//     synthesises a new UUID via `uuid.NewString()`. This
//     matches `*graphwriter.Writer.EnsureRepo`'s
//     `gen_random_uuid()` default and preserves the pre-Stage-3.4
//     CLI behaviour.
//   - When `in.RepoID` is non-zero (the deterministic
//     `fingerprint.RepoIDFromURL(URL)` path), the SQLite Sink
//     PERSISTS that exact UUID as the row's `repo_id` on a fresh
//     insert. This matches the Postgres `EnsureRepoWithID`
//     contract and is the path the `codeintel scan` CLI uses so
//     a repo scanned to SQLite carries the SAME `repo_id` as the
//     same repo scanned to Postgres -- a prerequisite for node /
//     edge fingerprint parity, because fingerprints embed
//     `repo_id` (see `fingerprint.NodeFingerprint`).
//   - On a URL conflict the mutable columns are overwritten and
//     the PRE-EXISTING `repo_id` is returned with
//     `Inserted = false`; the `repo_id` PK is NEVER re-keyed,
//     matching the Postgres adapter's documented
//     "legacy-collision" caveat.
//
// Returns `ErrSinkClosed` (wrapping `sql.ErrConnDone`) if Close
// has been called.
func (s *Sink) EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.RepoRecord{}, err
	}
	if in.URL == "" {
		return graphwriter.RepoRecord{}, errors.New("graphsink/sqlite: EnsureRepo: empty url")
	}

	hints := in.LanguageHints
	if hints == nil {
		hints = []string{}
	}
	hintsJSON, err := json.Marshal(hints)
	if err != nil {
		return graphwriter.RepoRecord{}, fmt.Errorf("graphsink/sqlite: EnsureRepo language_hints: %w", err)
	}

	var rec graphwriter.RepoRecord
	err = s.runInTx(ctx, func(tx *sql.Tx) error {
		// SELECT-then-INSERT-or-UPDATE inside a single tx. SQLite
		// supports ON CONFLICT (RFC 3997 / SQLite 3.24+) but
		// returning whether the row was newly inserted is
		// easiest with the explicit pre-check; the tx pins the
		// row so a concurrent writer can't sneak in between
		// SELECT and INSERT (SQLite serialises writers anyway).
		const selQ = `SELECT repo_id FROM repo WHERE url = ?`
		var existingID string
		switch err := tx.QueryRowContext(ctx, selQ, in.URL).Scan(&existingID); {
		case errors.Is(err, sql.ErrNoRows):
			// Fresh insert. Honour caller-supplied deterministic
			// RepoID when non-zero so SQLite / Postgres backends
			// agree on the row's PK -- the cross-backend identity
			// invariant the story documents under S3.4.
			var newID string
			if in.RepoID.IsZero() {
				newID = uuid.NewString()
			} else {
				newID = in.RepoID.String()
			}
			const insQ = `
				INSERT INTO repo (repo_id, url, default_branch, current_head_sha, language_hints, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`
			if _, err := tx.ExecContext(ctx, insQ,
				newID, in.URL, in.DefaultBranch, in.CurrentHeadSHA,
				string(hintsJSON), time.Now().UTC().UnixMilli(),
			); err != nil {
				return fmt.Errorf("insert repo: %w", err)
			}
			rec.RepoID = newID
			rec.Inserted = true
		case err != nil:
			return fmt.Errorf("select repo: %w", err)
		default:
			// Update mutable fields, keep existing PK.
			const updQ = `
				UPDATE repo
				   SET default_branch   = ?,
				       current_head_sha = ?,
				       language_hints   = ?
				 WHERE repo_id = ?
			`
			if _, err := tx.ExecContext(ctx, updQ,
				in.DefaultBranch, in.CurrentHeadSHA, string(hintsJSON), existingID,
			); err != nil {
				return fmt.Errorf("update repo: %w", err)
			}
			rec.RepoID = existingID
			rec.Inserted = false
		}
		return nil
	})
	if err != nil {
		return graphwriter.RepoRecord{}, fmt.Errorf("graphsink/sqlite: EnsureRepo: %w", err)
	}

	id, err := fingerprint.ParseRepoID(rec.RepoID)
	if err != nil {
		return graphwriter.RepoRecord{}, fmt.Errorf("graphsink/sqlite: EnsureRepo parse repo_id: %w", err)
	}
	rec.ID = id
	return rec, nil
}

// ----- EnsureCommit ------------------------------------------------

// EnsureCommit idempotently writes a repo_commit row keyed by
// `(RepoID, SHA)`. Semantics mirror
// `*graphwriter.Writer.EnsureCommit`: append-only; on conflict
// the existing row is left untouched and `Inserted = false` is
// returned.
func (s *Sink) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.CommitRecord{}, err
	}
	if in.RepoID.IsZero() {
		return graphwriter.CommitRecord{}, errors.New("graphsink/sqlite: EnsureCommit: zero repo_id")
	}
	if in.SHA == "" {
		return graphwriter.CommitRecord{}, errors.New("graphsink/sqlite: EnsureCommit: empty sha")
	}
	if in.CommittedAt.IsZero() {
		return graphwriter.CommitRecord{}, errors.New("graphsink/sqlite: EnsureCommit: zero committed_at")
	}

	repoIDStr := in.RepoID.String()
	var parent any
	if in.ParentSHA != "" {
		parent = in.ParentSHA
	} else {
		parent = nil
	}

	rec := graphwriter.CommitRecord{RepoID: repoIDStr, SHA: in.SHA}
	err := s.runInTx(ctx, func(tx *sql.Tx) error {
		const insQ = `
			INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (repo_id, sha) DO NOTHING
		`
		res, err := tx.ExecContext(ctx, insQ,
			repoIDStr, in.SHA, parent, in.CommittedAt.UTC().UnixMilli(),
		)
		if err != nil {
			return fmt.Errorf("insert repo_commit: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("repo_commit rows affected: %w", err)
		}
		rec.Inserted = n == 1
		if !rec.Inserted {
			// Defensive: verify the conflicting row is actually
			// present. Matches the Postgres adapter's
			// snapshot-isolation guard.
			const verifyQ = `SELECT 1 FROM repo_commit WHERE repo_id = ? AND sha = ?`
			var seen int
			if err := tx.QueryRowContext(ctx, verifyQ, repoIDStr, in.SHA).Scan(&seen); err != nil {
				return fmt.Errorf("verify repo_commit: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return graphwriter.CommitRecord{}, fmt.Errorf("graphsink/sqlite: EnsureCommit: %w", err)
	}
	return rec, nil
}

// ----- InsertNode --------------------------------------------------

// InsertNode idempotently writes a Node row keyed by
// `(repo_id, fingerprint)`. The fingerprint is computed from
// `(RepoID, Kind, CanonicalSignature, FromSHA)` via
// `fingerprint.NodeFingerprint` -- the SAME helper the Postgres
// writer uses -- so a repo scanned to SQLite and later re-scanned
// to Postgres yields identical node identities.
//
// When a `ParentNodeID` is supplied the function verifies the
// parent belongs to the same `repo_id` inside the same
// transaction; cross-repo parents are rejected, matching the
// Postgres adapter's hierarchy invariant.
func (s *Sink) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.NodeRecord{}, err
	}

	fp, err := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	if err != nil {
		return graphwriter.NodeRecord{}, fmt.Errorf("graphsink/sqlite: InsertNode fingerprint: %w", err)
	}
	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return graphwriter.NodeRecord{}, fmt.Errorf("graphsink/sqlite: InsertNode attrs_json: %w", err)
	}
	repoIDStr := in.RepoID.String()

	rec := graphwriter.NodeRecord{Fingerprint: fp}
	err = s.runInTx(ctx, func(tx *sql.Tx) error {
		if in.ParentNodeID != "" {
			var parentRepo string
			err := tx.QueryRowContext(ctx,
				`SELECT repo_id FROM node WHERE node_id = ?`,
				in.ParentNodeID,
			).Scan(&parentRepo)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("parent_node_id %s not found", in.ParentNodeID)
			}
			if err != nil {
				return fmt.Errorf("lookup parent: %w", err)
			}
			if parentRepo != repoIDStr {
				return fmt.Errorf(
					"parent_node_id %s belongs to repo %s, not %s",
					in.ParentNodeID, parentRepo, repoIDStr,
				)
			}
		}

		// Idempotent insert. On conflict on (repo_id, fingerprint)
		// we fall through to a SELECT to recover the existing
		// node_id, matching graphwriter's two-step pattern.
		var parent any
		if in.ParentNodeID != "" {
			parent = in.ParentNodeID
		}
		newID := uuid.NewString()
		const insQ = `
			INSERT INTO node
			    (node_id, fingerprint, repo_id, kind, canonical_signature,
			     parent_node_id, from_sha, attrs_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (repo_id, fingerprint) DO NOTHING
		`
		res, err := tx.ExecContext(ctx, insQ,
			newID, fp.Bytes(), repoIDStr, in.Kind, in.CanonicalSignature,
			parent, in.FromSHA, string(attrs),
		)
		if err != nil {
			return fmt.Errorf("insert node: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("node rows affected: %w", err)
		}
		if n == 1 {
			rec.NodeID = newID
			rec.Inserted = true
			return nil
		}
		// Conflict: recover the existing node_id.
		const selQ = `SELECT node_id FROM node WHERE repo_id = ? AND fingerprint = ?`
		if err := tx.QueryRowContext(ctx, selQ, repoIDStr, fp.Bytes()).Scan(&rec.NodeID); err != nil {
			return fmt.Errorf("select node fallback: %w", err)
		}
		rec.Inserted = false
		return nil
	})
	if err != nil {
		return graphwriter.NodeRecord{}, fmt.Errorf("graphsink/sqlite: InsertNode: %w", err)
	}
	return rec, nil
}

// ----- InsertEdge --------------------------------------------------

// InsertEdge idempotently writes an Edge row keyed by
// `(repo_id, fingerprint)`. The src/dst node fingerprints are
// read from the database inside the same transaction so the
// edge's identity is byte-identical to what an outside
// re-computation (or the Postgres adapter) would produce.
//
// Both endpoints must belong to `in.RepoID`; cross-repo edges
// are rejected with a typed error before any INSERT runs.
func (s *Sink) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.EdgeRecord{}, err
	}
	if in.Kind == "" {
		return graphwriter.EdgeRecord{}, errors.New("graphsink/sqlite: InsertEdge: empty kind")
	}
	if in.SrcNodeID == "" || in.DstNodeID == "" {
		return graphwriter.EdgeRecord{}, errors.New("graphsink/sqlite: InsertEdge: empty src/dst node_id")
	}
	if in.FromSHA == "" {
		return graphwriter.EdgeRecord{}, errors.New("graphsink/sqlite: InsertEdge: empty from_sha")
	}
	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return graphwriter.EdgeRecord{}, fmt.Errorf("graphsink/sqlite: InsertEdge attrs_json: %w", err)
	}
	repoIDStr := in.RepoID.String()

	var rec graphwriter.EdgeRecord
	err = s.runInTx(ctx, func(tx *sql.Tx) error {
		srcRepo, srcFP, err := lookupNodeFingerprint(ctx, tx, in.SrcNodeID)
		if err != nil {
			return fmt.Errorf("src: %w", err)
		}
		dstRepo, dstFP, err := lookupNodeFingerprint(ctx, tx, in.DstNodeID)
		if err != nil {
			return fmt.Errorf("dst: %w", err)
		}
		if srcRepo != repoIDStr {
			return fmt.Errorf(
				"src_node_id %s belongs to repo %s, not %s",
				in.SrcNodeID, srcRepo, repoIDStr,
			)
		}
		if dstRepo != repoIDStr {
			return fmt.Errorf(
				"dst_node_id %s belongs to repo %s, not %s",
				in.DstNodeID, dstRepo, repoIDStr,
			)
		}

		fp, err := fingerprint.EdgeFingerprint(in.RepoID, in.Kind, srcFP, dstFP, in.FromSHA)
		if err != nil {
			return fmt.Errorf("fingerprint: %w", err)
		}
		rec.Fingerprint = fp
		rec.SrcFP = srcFP
		rec.DstFP = dstFP

		newID := uuid.NewString()
		const insQ = `
			INSERT INTO edge
			    (edge_id, fingerprint, repo_id, kind, src_node_id, dst_node_id,
			     from_sha, attrs_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (repo_id, fingerprint) DO NOTHING
		`
		res, err := tx.ExecContext(ctx, insQ,
			newID, fp.Bytes(), repoIDStr, in.Kind,
			in.SrcNodeID, in.DstNodeID, in.FromSHA, string(attrs),
		)
		if err != nil {
			return fmt.Errorf("insert edge: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("edge rows affected: %w", err)
		}
		if n == 1 {
			rec.EdgeID = newID
			rec.Inserted = true
			return nil
		}
		const selQ = `SELECT edge_id FROM edge WHERE repo_id = ? AND fingerprint = ?`
		if err := tx.QueryRowContext(ctx, selQ, repoIDStr, fp.Bytes()).Scan(&rec.EdgeID); err != nil {
			return fmt.Errorf("select edge fallback: %w", err)
		}
		rec.Inserted = false
		return nil
	})
	if err != nil {
		return graphwriter.EdgeRecord{}, fmt.Errorf("graphsink/sqlite: InsertEdge: %w", err)
	}
	return rec, nil
}

// lookupNodeFingerprint reads a node's repo_id and fingerprint
// inside the supplied transaction so the (repo_id, fingerprint)
// the SQLite Sink hashes against the database matches the
// (repo_id, fingerprint) the database actually stores. Mirrors
// `*graphwriter.Writer.lookupNodeFingerprint`.
func lookupNodeFingerprint(
	ctx context.Context, tx *sql.Tx, nodeID string,
) (repoID string, fp fingerprint.Sum, err error) {
	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT repo_id, fingerprint FROM node WHERE node_id = ?`,
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

// normaliseAttrs is a local copy of the graphwriter helper:
// returns a JSON object byte slice that satisfies the
// `attrs_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(...))`
// column contract.
//
//   - nil or empty input becomes the literal "{}".
//   - a valid JSON object passes through unchanged.
//   - any other JSON value (array, scalar, null) is rejected,
//     matching the Postgres writer so backend-parity tests can
//     pin on identical error shapes.
func normaliseAttrs(in json.RawMessage) (json.RawMessage, error) {
	if len(in) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(in) {
		return nil, errors.New("attrs_json is not valid JSON")
	}
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
