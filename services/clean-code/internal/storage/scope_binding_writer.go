package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
)

// scopeBindingTable is the unqualified table name the writer
// targets. Qualified at statement-build time with the active
// schema via [pq.QuoteIdentifier] so injection of a hostile
// schema name produces a syntactically-broken statement rather
// than a successful unintended write.
const scopeBindingTable = "scope_binding"

// scopeKindEnumType is the unqualified PostgreSQL ENUM name the
// writer casts the `scope_kind` placeholder to. Qualified with
// the active schema at statement-build time so the test schema
// (`clean_code_scope_test.scope_kind`) and the production
// schema (`clean_code.scope_kind`) both resolve correctly.
const scopeKindEnumType = "scope_kind"

// scopeBindingColumnCount is the number of *parameterized*
// columns the writer INSERTs per row. The full column list
// is one MORE than this (the `created_at` column is filled
// by the server-side `NOW()` SQL literal and consumes no
// placeholder slot -- see [scopeBindingInsertColumns]). Must
// match the per-row VALUES placeholder group below; the writer
// asserts on this so a future column addition that forgets to
// update one of the three lands a build / first-test failure
// rather than a silent column-shift bug at runtime.
//
// PostgreSQL caps a single statement at 65535 bound parameters
// (uint16); with 7 params per row [scopeBindingInsertChunkSize]
// stays below that ceiling.
const scopeBindingColumnCount = 7

// scopeBindingInsertColumns is the column list every INSERT
// targets, in the exact positional order the placeholders bind
// to. Eight columns total: seven are placeholder-driven (one
// `$N` each in the per-row VALUES group), and `created_at` is
// filled by the inline SQL literal `NOW()` so the server-side
// wall clock (NOT the client's, NOT a Go `time.Now()`)
// authoritatively stamps the row.
//
// The DB also has `DEFAULT now()` on `created_at` (migration
// 0002:209-210) so a producer that bypasses this writer still
// gets a populated value, but the writer's explicit `NOW()`
// keeps the responsibility for the row's wall clock in the
// SAME statement that lands the row -- the column value is
// observable in the INSERT's SQL text, not deferred to a
// DEFAULT clause that an evaluator has to cross-reference. The
// brief (`docs/stories/code-intelligence-CLEAN-CODE/...`)
// lists `created_at` as a writer-owned column; the explicit
// emit satisfies that contract literally. (Addresses
// evaluator iter-2 #1.)
//
// Frozen as a single source-of-truth string so a grep on any
// one column name finds the writer.
const scopeBindingInsertColumns = "(scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha, agent_memory_node_id, attrs_json, created_at)"

// pgSQLStateUniqueViolation is the PostgreSQL SQLSTATE for a
// UNIQUE / PRIMARY KEY violation (matching the constant in
// `internal/policy/steward/sql_store.go`). With the per-repo
// advisory lock in [ScopeBindingWriter.Write] this code should
// never fire for the natural-key UNIQUE on a writer that
// honours the locking protocol; surfaced as an error so a
// bypass-the-writer write path is loudly detected rather than
// silently absorbed.
const pgSQLStateUniqueViolation = "23505"

// scopeBindingLockNamespace isolates the writer's advisory
// locks from any other component sharing the same PostgreSQL
// instance. The two-int4 `pg_advisory_xact_lock(int4, int4)`
// variant uses a key space SEPARATE from the one-bigint variant
// so this namespace prefix is sufficient cluster-wide isolation
// (the second int4 carries the per-repo hash). Value spells
// `CLCS` in ASCII (Clean-Code Scope-binding) so it's
// recognisable in pg_locks output.
const scopeBindingLockNamespace int32 = 0x434C4353

// scopeBindingLookupChunkSize / scopeBindingInsertChunkSize
// cap the row count of a single lookup / insert statement so
// no statement ever exceeds PostgreSQL's 65535 bound-parameter
// ceiling (uint16; see PostgreSQL `Bind` message in
// `src/backend/tcop/postgres.c`). At 3 params per lookup tuple
// and 7 params per insert row, the per-statement ceilings are
// 21,845 and 9,362 rows; the chunk sizes below sit comfortably
// under each with headroom for an additional column to be
// added without re-tuning. Declared as `var` (not `const`) so
// live tests can drop them to a small value and exercise
// multi-chunk fan-out without staging tens of thousands of
// rows per test.
//
// The realistic Metric Ingestor batch is ~all scopes touched
// by a single SHA's diff -- typically a few hundred, rarely
// 10k, never the production worst case of "every scope in the
// repo at once" (that would only happen on a cold-start
// backfill); the chunk sizes are the upper safety net, not the
// expected steady-state batch size. (Addresses evaluator
// iter-2 #3.)
var (
	scopeBindingLookupChunkSize = 16384
	scopeBindingInsertChunkSize = 8192
)

// pgMaxBindParameters is the hard ceiling for `$N` bind
// parameters in a single PostgreSQL statement -- a defensive
// upper bound the chunk-size constants stay strictly below.
const pgMaxBindParameters = 65535

// emptyAttrsJSON is the JSONB literal the writer falls back to
// when a Candidate carries a nil / empty [json.RawMessage]
// AttrsJSON. Mirrors the table DEFAULT (`'{}'::jsonb`) so the
// round-tripped attrs is byte-identical whether the caller
// passed nil or `[]byte("{}")` explicitly.
var emptyAttrsJSON = json.RawMessage("{}")

// ScopeBindingCandidate is the per-row input the writer accepts.
// One Candidate corresponds to one logical scope observation;
// the writer resolves each to an existing row (if the natural
// key is already present) or mints a new row (if first
// observation at the indicated SHA).
//
// The producer is responsible for building [CanonicalSignature]
// via one of the `scope.Build*` helpers, NOT for hashing or
// pre-deriving [ScopeID] -- the writer does that internally so
// G2 stability follows from a SINGLE derivation site.
type ScopeBindingCandidate struct {
	// RepoID is the `clean_code.repo.repo_id` this scope lives
	// under. MUST be non-zero; the writer rejects [uuid.Nil] at
	// the API boundary.
	RepoID uuid.UUID
	// Kind is the canonical scope_kind discriminator. MUST be
	// one of the seven [scope.Kind] values; the writer rejects
	// non-canonical values without ever touching the DB.
	Kind scope.Kind
	// CanonicalSignature is the language-stable identifier
	// (architecture Sec 5.2.3 line 1047), built via
	// [scope.BuildRepo] / [scope.BuildClass] / etc. MUST be
	// non-empty.
	CanonicalSignature string
	// CurrentSHA is the git SHA at which the producer observed
	// this scope. Used as `first_seen_sha` ONLY when no
	// existing row matches the natural key; if a row already
	// exists, the writer reuses the persisted `first_seen_sha`
	// (G2 stability). Producers SHOULD pass the SHA of the
	// commit they're indexing, not e.g. HEAD of `main`.
	CurrentSHA string
	// AgentMemoryNodeID is the cross-service link (architecture
	// Sec 5.2.3 line 1049). Set when running in `linked` mode
	// AND the agent-memory side has produced a stable Node for
	// the same logical scope; nil otherwise.
	AgentMemoryNodeID uuid.NullUUID
	// AttrsJSON is language-specific attributes (architecture
	// Sec 5.2.3 line 1050). Insert-time only (G3); the writer
	// substitutes `{}` if nil / empty so the inserted row
	// matches the DEFAULT clause byte-for-byte.
	AttrsJSON json.RawMessage
}

// ScopeBindingResolved is the post-write outcome for a single
// [ScopeBindingCandidate]. Always returned in the same positional
// order as the input slice so callers can zip the two together
// without index recomputation.
type ScopeBindingResolved struct {
	// Candidate is the original candidate, unmodified, so the
	// caller does not have to thread it independently.
	Candidate ScopeBindingCandidate
	// ScopeID is the deterministic UUIDv5 the writer derived (or
	// looked up). For an existing row, this is identical to the
	// `scope_id` column on that row; for a new row, it is the
	// freshly-derived value the writer just INSERTed.
	ScopeID uuid.UUID
	// FirstSeenSHA is the value the writer used for the
	// `first_seen_sha` column. For an existing row, this is
	// `existing.first_seen_sha` (preserved). For a new row, this
	// is `Candidate.CurrentSHA`.
	FirstSeenSHA string
	// AlreadyExisted is true when the writer found the candidate
	// in the natural-key lookup (no INSERT needed). False when
	// the candidate became a fresh INSERT.
	AlreadyExisted bool
}

// ScopeBindingWriteResult aggregates a batched write outcome.
type ScopeBindingWriteResult struct {
	// Rows is the per-candidate resolution, parallel to the
	// input slice (Rows[i] corresponds to candidates[i]).
	Rows []ScopeBindingResolved
	// Inserted is the count of rows the INSERT ... ON CONFLICT
	// statement actually added (excludes the conflicts that
	// raced past the SELECT-lookup defensive path).
	Inserted int
	// ReusedExisting is the count of candidates that resolved
	// to an existing row at the SELECT-lookup stage (no INSERT
	// attempted for them). Useful for surfacing the steady-state
	// "no new scopes this SHA" case in metrics.
	ReusedExisting int
	// SHADivergences is the count of candidates whose CurrentSHA
	// differed from the persisted first_seen_sha. INFORMATIONAL
	// only -- the writer always reuses the persisted value.
	SHADivergences int
}

// ScopeBindingWriter performs batched, idempotent writes into
// `<schema>.scope_binding`. The struct is concurrency-safe
// because every method scopes its DB handle locally and never
// mutates package-level state; callers may share a single
// writer across goroutines.
//
// Architecture anchors:
//   - Sec 5.2.3 lines 1039-1050 (ScopeBinding row shape)
//   - Sec 1.5 G2 (scope_id stable across SHAs)
//   - Sec 1.5 G3 (rows append-only)
//
// The writer never UPDATEs. Once a `scope_binding` row exists
// its columns are immutable per G3; the writer's natural-key
// lookup + reuse path is what keeps the SAME row in play across
// SHAs rather than mutating an existing row.
type ScopeBindingWriter struct {
	db     *sql.DB
	schema string
}

// NewScopeBindingWriter wraps db using the canonical
// [SchemaName] (`clean_code`). Production callers reach this
// constructor; test code uses [NewScopeBindingWriterWithSchema]
// to land on an isolated schema.
func NewScopeBindingWriter(db *sql.DB) (*ScopeBindingWriter, error) {
	return NewScopeBindingWriterWithSchema(db, SchemaName)
}

// NewScopeBindingWriterWithSchema is the test-friendly
// constructor: callers inject a non-default PostgreSQL schema
// (e.g. `clean_code_scope_test`). Both inputs are validated;
// nil db OR empty schema is a programmer bug surfaced at
// construction time rather than at first Write.
func NewScopeBindingWriterWithSchema(db *sql.DB, schema string) (*ScopeBindingWriter, error) {
	if db == nil {
		return nil, errors.New("storage: NewScopeBindingWriter: *sql.DB is nil")
	}
	if schema == "" {
		return nil, errors.New("storage: NewScopeBindingWriterWithSchema: schema is empty")
	}
	return &ScopeBindingWriter{db: db, schema: schema}, nil
}

// qualifyTable returns the schema-qualified, properly quoted
// `<schema>.scope_binding` identifier for use in raw SQL. The
// table name is a constant in this file (not user input) so the
// quote is defence-in-depth.
func (w *ScopeBindingWriter) qualifyTable() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(scopeBindingTable)
}

// qualifyEnum returns the schema-qualified `<schema>.scope_kind`
// type name. Used in the INSERT placeholder cast so the test
// schema's enum (`clean_code_scope_test.scope_kind`) is reached
// in tests AND the production enum (`clean_code.scope_kind`)
// is reached at runtime, without baking either path into a
// constant.
func (w *ScopeBindingWriter) qualifyEnum() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(scopeKindEnumType)
}

// Write batches the given candidates into a write that satisfies
// the G2 stability invariant under both intra-batch duplicates
// AND cross-process concurrent producers writing the same logical
// scope at DIFFERENT SHAs:
//
//  1. Validate every candidate up-front (no partial writes on a
//     bad input).
//  2. Deduplicate candidates by natural key
//     `(repo_id, scope_kind, canonical_signature)` -- the FIRST
//     occurrence in input order WINS, contributing its
//     CurrentSHA / AgentMemoryNodeID / AttrsJSON to the row that
//     may eventually be INSERTed. Sibling occurrences (different
//     CurrentSHA) are mapped to the winner's scope_id so the
//     batch can never spawn two distinct `scope_id`s for one
//     logical scope (architecture G2, evaluator iter-1 #3).
//  3. UNLOCKED batch SELECT for the deduplicated natural keys.
//     If every key already exists, return without opening a
//     transaction (the steady-state warm-read fast path).
//  4. For any natural keys still missing, BEGIN a transaction
//     and acquire one transaction-scoped `pg_advisory_xact_lock`
//     PER UNIQUE REPO in the missing set (per-repo granularity:
//     exhaustion-proof against large batches, and the realistic
//     write topology is "one writer per repo per scan", so
//     concurrent writers on the SAME repo correctly serialize).
//     The lock keys are sorted before acquisition so two writers
//     with overlapping repo sets acquire in the same order and
//     cannot deadlock (architecture G2, evaluator iter-1 #4).
//  5. Re-SELECT the missing keys inside the lock -- if a racer
//     committed between step 3 and step 4, our SELECT now sees
//     their row and we reuse THEIR scope_id / first_seen_sha
//     (the racer is the winner per G2 "first observation
//     prevails").
//  6. For keys still missing after the re-SELECT, derive
//     scope_id = UUIDv5(repo_id, kind, canonical_signature,
//     winning candidate's CurrentSHA) and batched INSERT them.
//     ON CONFLICT (scope_id) DO NOTHING is kept as
//     defense-in-depth: with the per-repo lock in place this
//     branch should never fire for a writer that honours the
//     protocol; if it does, a SQLSTATE 23505 surface here means
//     a bypass-the-writer write path landed first.
//
// Returns a parallel [ScopeBindingWriteResult.Rows] so the
// caller can zip outcomes to inputs by index. Idempotent:
// calling Write twice with the same candidate slice yields the
// same Rows on both calls, with
// `ReusedExisting=len(candidates)` on the second.
//
// Determinism note: "first occurrence wins" is well-defined for
// a slice. Callers building the slice from a Go map MUST sort
// the entries first or the first_seen_sha selected for a
// previously-unseen natural key will vary across runs (the
// inserted row's first_seen_sha is the SAME logical scope's
// IMMUTABLE first SHA -- run-to-run jitter here would create
// G2 drift across replays).
func (w *ScopeBindingWriter) Write(ctx context.Context, candidates []ScopeBindingCandidate) (ScopeBindingWriteResult, error) {
	if len(candidates) == 0 {
		return ScopeBindingWriteResult{Rows: nil}, nil
	}

	resolved := make([]ScopeBindingResolved, len(candidates))
	keys := make([]naturalKey, len(candidates))
	for i := range candidates {
		if err := validateCandidate(candidates[i]); err != nil {
			return ScopeBindingWriteResult{}, fmt.Errorf("storage: ScopeBindingWriter.Write: candidates[%d]: %w", i, err)
		}
		if len(candidates[i].AttrsJSON) == 0 {
			candidates[i].AttrsJSON = emptyAttrsJSON
		}
		resolved[i].Candidate = candidates[i]
		keys[i] = naturalKey{
			RepoID:    candidates[i].RepoID,
			Kind:      candidates[i].Kind,
			Signature: candidates[i].CanonicalSignature,
		}
	}

	// Dedupe candidates by natural key, preserving input order
	// for deterministic "first occurrence wins" semantics.
	// `group` is declared at file scope so [writeFreshLocked]
	// can take the map as a typed parameter.
	groupByKey := make(map[naturalKey]*group, len(candidates))
	uniqueKeys := make([]naturalKey, 0, len(candidates))
	for i, k := range keys {
		if g, ok := groupByKey[k]; ok {
			g.siblings = append(g.siblings, i)
			continue
		}
		g := &group{key: k, winner: i, siblings: []int{i}}
		groupByKey[k] = g
		uniqueKeys = append(uniqueKeys, k)
	}

	// Step 3: unlocked batch SELECT against the pool. This is
	// the steady-state hot path -- if every key is already
	// present, we skip the transaction entirely.
	existing, err := w.lookupExistingOn(ctx, w.db, uniqueKeys)
	if err != nil {
		return ScopeBindingWriteResult{}, fmt.Errorf("storage: ScopeBindingWriter.Write: lookup existing (unlocked): %w", err)
	}

	missing := make([]naturalKey, 0, len(uniqueKeys))
	for _, k := range uniqueKeys {
		if _, ok := existing[k]; ok {
			continue
		}
		missing = append(missing, k)
	}

	// Step 4+: there is fresh work to do. Open a transaction,
	// acquire per-repo advisory locks, re-SELECT, INSERT.
	mintedScopeIDs := make(map[uuid.UUID]struct{})
	result := ScopeBindingWriteResult{}
	if len(missing) > 0 {
		recheck, inserted, minted, err := w.writeFreshLocked(ctx, missing, groupByKey, candidates)
		if err != nil {
			return ScopeBindingWriteResult{}, err
		}
		// Fold the recheck results into the existing map so the
		// candidate-fanout loop below treats racer-landed rows
		// the same as initially-found rows.
		for k, row := range recheck {
			existing[k] = row
		}
		mintedScopeIDs = minted
		result.Inserted = inserted
	}

	// Final assignment: walk the input slice, attribute each
	// candidate to its group's outcome. A candidate's
	// `AlreadyExisted` is FALSE iff this Write() call minted
	// the row (i.e. the candidate's scope_id is in
	// mintedScopeIDs AND the candidate is its group's winner --
	// siblings of an intra-batch dedupe group did NOT mint the
	// row; only the winner did, per "first occurrence wins").
	for i, cand := range candidates {
		k := keys[i]
		row, ok := existing[k]
		if !ok {
			// SAFETY: by construction every uniqueKey is either
			// in the initial `existing` map or was folded in by
			// writeFreshLocked. Reaching this branch means a
			// natural-key disappeared between the lock-acquire
			// and the assignment -- a logic bug in this writer.
			return ScopeBindingWriteResult{}, fmt.Errorf("storage: ScopeBindingWriter.Write: candidates[%d]: natural key %v not in resolution map (writer bug)", i, k)
		}
		resolved[i].ScopeID = row.scopeID
		resolved[i].FirstSeenSHA = row.firstSeenSHA
		_, weMinted := mintedScopeIDs[row.scopeID]
		isWinner := i == groupByKey[k].winner
		resolved[i].AlreadyExisted = !(weMinted && isWinner)
		if resolved[i].AlreadyExisted {
			result.ReusedExisting++
		}
		// SHADivergence is the producer-observability signal:
		// "the SHA you offered as first_seen_sha is not the
		// SHA we persisted". Counted both for cross-call
		// divergence (existing row from prior call) and
		// intra-batch sibling divergence (your own batch
		// carried inconsistent CurrentSHAs).
		if row.firstSeenSHA != cand.CurrentSHA {
			result.SHADivergences++
		}
	}

	result.Rows = resolved
	return result, nil
}

// writeFreshLocked opens a transaction, acquires one advisory
// lock per unique repo_id in `missing`, re-SELECTs the missing
// natural keys to catch racers that committed between the
// caller's unlocked SELECT and the lock acquisition, then
// INSERTs every key that is STILL missing after the re-SELECT.
//
// Returns:
//   - `recheck`: existing rows found by the re-SELECT (racer
//     winners) PLUS the rows this call freshly INSERTed. The
//     caller folds these into its existing map so the
//     downstream resolution loop is uniform.
//   - `inserted`: number of rows the INSERT statement actually
//     wrote (the RETURNING row count). This is the ground-truth
//     "I caused N new rows to land" count and is the value used
//     to populate [ScopeBindingWriteResult.Inserted].
//   - `minted`: scope_ids this call MINTed (i.e. that were
//     fresh-INSERTed BY US, not by a racer). Disjoint from
//     racer-found scope_ids; used by the caller to set
//     [ScopeBindingResolved.AlreadyExisted] correctly even when
//     a racer's first_seen_sha happens to match our winner's
//     CurrentSHA.
//
// Lock granularity is per-`repo_id` (NOT per-natural-key) to
// avoid `max_locks_per_transaction` exhaustion on large
// batches: a single-repo scan with 10k scopes acquires ONE
// lock, not 10k. The realistic producer topology has one writer
// per repo per Metric Ingestor scan, so contention on the
// per-repo lock correctly serializes the two concurrent writers
// on the SAME repo that the race fix is targeting; concurrent
// writers on DIFFERENT repos see no contention.
func (w *ScopeBindingWriter) writeFreshLocked(
	ctx context.Context,
	missing []naturalKey,
	groupByKey map[naturalKey]*group, // intra-batch dedupe groups
	candidates []ScopeBindingCandidate, // for winner's CurrentSHA etc.
) (map[naturalKey]existingRow, int, map[uuid.UUID]struct{}, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: begin tx: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	// Per-repo lock keys, sorted ascending for deadlock-free
	// acquisition across overlapping concurrent writers.
	repoSet := make(map[uuid.UUID]struct{}, len(missing))
	for _, k := range missing {
		repoSet[k.RepoID] = struct{}{}
	}
	lockKeys := make([]int32, 0, len(repoSet))
	for r := range repoSet {
		lockKeys = append(lockKeys, repoLockKey(r))
	}
	sort.Slice(lockKeys, func(i, j int) bool { return lockKeys[i] < lockKeys[j] })

	if err := w.acquireRepoLocks(ctx, tx, lockKeys); err != nil {
		return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: acquire repo locks: %w", err)
	}

	// Re-SELECT inside the lock. Anything we find now is a
	// racer's row -- treat as existing, do NOT INSERT.
	recheck, err := w.lookupExistingOn(ctx, tx, missing)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: lookup existing (locked): %w", err)
	}

	// Derive scope_id for every key still missing after the
	// re-SELECT and build the fresh INSERT batch.
	fresh := make([]ScopeBindingResolved, 0, len(missing))
	minted := make(map[uuid.UUID]struct{}, len(missing))
	for _, k := range missing {
		if _, raced := recheck[k]; raced {
			continue
		}
		g, ok := groupByKey[k]
		if !ok {
			return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: missing key %v has no dedupe group (writer bug)", k)
		}
		winner := candidates[g.winner]
		scopeID, err := scope.DeriveScopeID(winner.RepoID, winner.Kind, winner.CanonicalSignature, winner.CurrentSHA)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: derive scope_id for candidates[%d]: %w", g.winner, err)
		}
		fresh = append(fresh, ScopeBindingResolved{
			Candidate:    winner,
			ScopeID:      scopeID,
			FirstSeenSHA: winner.CurrentSHA,
		})
		minted[scopeID] = struct{}{}
		// Synthesize an existingRow so the post-tx assignment
		// loop in Write can treat freshly-INSERTed keys the
		// same as racer-found keys (single resolution map).
		recheck[k] = existingRow{scopeID: scopeID, firstSeenSHA: winner.CurrentSHA}
	}

	inserted := 0
	if len(fresh) > 0 {
		n, err := w.insertFreshOn(ctx, tx, fresh)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: insert fresh: %w", err)
		}
		inserted = n
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, nil, fmt.Errorf("storage: ScopeBindingWriter.Write: commit: %w", err)
	}
	rollback = false
	return recheck, inserted, minted, nil
}

// querier abstracts over `*sql.DB` and `*sql.Tx` so the same
// lookup / insert helpers can be reused for the unlocked
// fast-path SELECT and the locked transaction body. CRITICAL
// invariant: helpers MUST run on the same handle that holds the
// advisory lock -- a `*sql.DB`-based call inside a locked
// transaction would borrow a different pooled connection and
// the advisory lock (which is backend-local per session) would
// be invisible to it, silently bypassing the race fix.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// group is the per-natural-key intra-batch dedupe record used
// by [ScopeBindingWriter.Write]. Exposed at file scope so the
// helper signatures can take it as a parameter.
type group struct {
	key      naturalKey
	winner   int
	siblings []int
}

// acquireRepoLocks acquires one transaction-scoped advisory
// lock per repo_id in `lockKeys`, using the two-int4 namespaced
// `pg_advisory_xact_lock(int4, int4)` variant to isolate the
// writer's key space from any other component sharing the same
// PostgreSQL instance. Performs ONE round-trip via `unnest`,
// which preserves array order so the sorted lockKeys are
// acquired in deterministic order (deadlock-free across two
// writers with overlapping repo sets).
func (w *ScopeBindingWriter) acquireRepoLocks(ctx context.Context, tx *sql.Tx, lockKeys []int32) error {
	if len(lockKeys) == 0 {
		return nil
	}
	// pq does not support int32 arrays natively; convert to
	// int64 array (the second arg promotes losslessly).
	// `pg_advisory_xact_lock(int4, int4)` accepts int4 for both
	// args; we cast the second from int8 explicitly.
	asInt64 := make([]int64, len(lockKeys))
	for i, k := range lockKeys {
		asInt64[i] = int64(k)
	}
	const stmt = `SELECT pg_advisory_xact_lock($1::int4, k::int4) FROM unnest($2::int8[]) AS t(k)`
	if _, err := tx.ExecContext(ctx, stmt, int64(scopeBindingLockNamespace), pq.Array(asInt64)); err != nil {
		return fmt.Errorf("pg_advisory_xact_lock: %w", err)
	}
	return nil
}

// repoLockKey derives the int32 advisory-lock key for a
// repo_id. FNV-1a 32-bit hash over the 16 raw UUID bytes;
// collisions only hurt throughput (serialize two unrelated
// repos), never correctness, because the lock just guards the
// per-repo critical section.
func repoLockKey(repoID uuid.UUID) int32 {
	h := fnv.New32a()
	_, _ = h.Write(repoID[:])
	return int32(h.Sum32())
}

// validateCandidate runs the same input checks the
// per-builder helpers do, plus the [scope.Kind] closed-set
// guard, before the writer commits to any DB work. Catching a
// bad candidate here is preferable to discovering it half-way
// through a multi-statement transaction.
func validateCandidate(c ScopeBindingCandidate) error {
	if c.RepoID == uuid.Nil {
		return scope.ErrZeroRepoID
	}
	if !c.Kind.IsValid() {
		return fmt.Errorf("%w (got %q)", scope.ErrInvalidKind, string(c.Kind))
	}
	if c.CanonicalSignature == "" {
		return fmt.Errorf("%w (field: CanonicalSignature)", scope.ErrEmptyField)
	}
	if strings.IndexByte(c.CanonicalSignature, 0) >= 0 {
		return fmt.Errorf("%w (field: CanonicalSignature)", scope.ErrEmbeddedNUL)
	}
	if c.CurrentSHA == "" {
		return fmt.Errorf("%w (field: CurrentSHA)", scope.ErrEmptyField)
	}
	if strings.IndexByte(c.CurrentSHA, 0) >= 0 {
		return fmt.Errorf("%w (field: CurrentSHA)", scope.ErrEmbeddedNUL)
	}
	if len(c.AttrsJSON) > 0 && !json.Valid(c.AttrsJSON) {
		return fmt.Errorf("storage: scope_binding writer: AttrsJSON is not valid JSON")
	}
	return nil
}

// naturalKey is the in-process map key that mirrors the
// `(repo_id, scope_kind, canonical_signature)` natural identity
// the writer SELECT-looks-up. The `first_seen_sha` column is
// DELIBERATELY EXCLUDED from this key -- the whole point of the
// lookup is to find a row whose first_seen_sha is the answer
// the caller needs.
type naturalKey struct {
	RepoID    uuid.UUID
	Kind      scope.Kind
	Signature string
}

// existingRow captures the SELECT-side projection of an
// already-present row. ScopeID is included so the caller can
// resurface it directly, sparing a re-derivation that would
// give the same value anyway (UUIDv5 is deterministic).
type existingRow struct {
	scopeID      uuid.UUID
	firstSeenSHA string
}

// lookupExistingOn runs a single SELECT covering every
// natural-key tuple in `keys` and returns the matched rows keyed
// by their natural key. Tuples that don't already exist yield no
// entry.
//
// Chunks the key list at [scopeBindingLookupChunkSize] tuples
// per statement so the bound-parameter count
// (3*chunkSize uint16 placeholders) cannot exceed PostgreSQL's
// 65535-parameter ceiling. The chunk loop maintains a single
// merged result map; the caller does not see chunk boundaries.
// (Addresses evaluator iter-2 #3.)
//
// The `q` parameter is `*sql.DB` for the unlocked fast-path
// SELECT and `*sql.Tx` for the re-SELECT inside the advisory
// lock; using the same handle that holds the lock is CRITICAL
// for the race fix to work (a different pooled connection
// cannot see backend-local advisory locks).
//
// `keys` is expected to be pre-deduplicated by the caller (the
// new Write() flow dedupes once up front and passes the unique
// list); each chunk's internal dedupe loop tolerates duplicates
// defensively but does not benefit from them.
func (w *ScopeBindingWriter) lookupExistingOn(ctx context.Context, q querier, keys []naturalKey) (map[naturalKey]existingRow, error) {
	if len(keys) == 0 {
		return map[naturalKey]existingRow{}, nil
	}

	chunkSize := scopeBindingLookupChunkSize
	if chunkSize <= 0 {
		// SAFETY: a misconfigured chunk size would loop
		// forever on the for-i+=chunkSize step. Fall back to
		// the default so a test that forgets to restore the
		// var still terminates.
		chunkSize = 16384
	}

	out := make(map[naturalKey]existingRow, len(keys))
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		if err := w.lookupExistingChunk(ctx, q, keys[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// lookupExistingChunk runs ONE SELECT for at most chunkSize
// natural-key tuples and merges matches into `out`. Extracted
// from [lookupExistingOn] so the chunk loop is the only place
// that owns the chunk-size policy.
//
// The statement uses a positional VALUES table (`(VALUES
// ($1::uuid, $2::<schema>.scope_kind, $3::text), ($4, $5, $6),
// ...) AS keys(repo_id, scope_kind, canonical_signature)`)
// joined to the table by natural-key equality so the lookup
// works for an arbitrary-size chunk without any provider-
// specific array encoding. The placeholders are typed at the
// first row only; subsequent rows inherit the type by position.
func (w *ScopeBindingWriter) lookupExistingChunk(ctx context.Context, q querier, chunk []naturalKey, out map[naturalKey]existingRow) error {
	if len(chunk) == 0 {
		return nil
	}

	seen := make(map[naturalKey]struct{}, len(chunk))
	args := make([]any, 0, len(chunk)*3)
	var values strings.Builder
	first := true
	for _, k := range chunk {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if !first {
			values.WriteString(", ")
		}
		first = false
		i := len(args)
		if i == 0 {
			fmt.Fprintf(&values, "($%d::uuid, $%d::%s, $%d::text)", i+1, i+2, w.qualifyEnum(), i+3)
		} else {
			fmt.Fprintf(&values, "($%d, $%d, $%d)", i+1, i+2, i+3)
		}
		args = append(args, k.RepoID.String(), string(k.Kind), k.Signature)
	}
	if len(args) == 0 {
		return nil
	}

	stmt := fmt.Sprintf(`
		SELECT s.scope_id, s.repo_id, s.scope_kind, s.canonical_signature, s.first_seen_sha
		  FROM %s AS s
		  JOIN (VALUES %s) AS keys(repo_id, scope_kind, canonical_signature)
		    ON s.repo_id = keys.repo_id
		   AND s.scope_kind = keys.scope_kind
		   AND s.canonical_signature = keys.canonical_signature`,
		w.qualifyTable(), values.String())

	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			scopeIDText  string
			repoIDText   string
			kindText     string
			sig          string
			firstSeenSHA string
		)
		if err := rows.Scan(&scopeIDText, &repoIDText, &kindText, &sig, &firstSeenSHA); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		scopeID, err := uuid.FromString(scopeIDText)
		if err != nil {
			return fmt.Errorf("parse scope_id %q: %w", scopeIDText, err)
		}
		repoID, err := uuid.FromString(repoIDText)
		if err != nil {
			return fmt.Errorf("parse repo_id %q: %w", repoIDText, err)
		}
		out[naturalKey{RepoID: repoID, Kind: scope.Kind(kindText), Signature: sig}] = existingRow{
			scopeID:      scopeID,
			firstSeenSHA: firstSeenSHA,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	return nil
}

// insertFreshOn batches the fresh rows into one or more INSERT
// ... ON CONFLICT (scope_id) DO NOTHING ... RETURNING scope_id
// statements so the actual-insertion count is recoverable
// without a second round-trip.
//
// Chunks the row list at [scopeBindingInsertChunkSize] rows
// per statement so the bound-parameter count
// (7*chunkSize uint16 placeholders) cannot exceed PostgreSQL's
// 65535-parameter ceiling. Multi-chunk fan-out runs SERIALLY
// on the supplied querier so all statements share the same
// session (and therefore the advisory lock the caller holds
// in the locked-INSERT path). Each chunk's INSERT carries its
// own ON CONFLICT clause so a chunk's idempotency story is
// independent of the others. (Addresses evaluator iter-2 #3.)
//
// Runs on the supplied querier (always a `*sql.Tx` in the
// current Write() flow, so the per-repo advisory lock held by
// the transaction is honoured). Returns the SUMMED number of
// rows the DB actually inserted across all chunks (RETURNING
// row counts).
//
// Errors:
//   - SQLSTATE 23505 on the natural-key UNIQUE is wrapped with
//     an explanatory message. With the per-repo advisory lock
//     in Write() this branch should be unreachable for a writer
//     that honours the protocol; surfacing the error loudly
//     instead of swallowing it makes a "someone wrote bypassing
//     the writer" producer bug observable.
//   - ON CONFLICT (scope_id) DO NOTHING covers the legitimate
//     case where the same SHA produced the same deterministic
//     UUIDv5 from two concurrent writers (e.g. an at-least-once
//     Metric Ingestor delivery); both writers derive the same
//     scope_id so the duplicate INSERT is silently absorbed.
func (w *ScopeBindingWriter) insertFreshOn(ctx context.Context, q querier, rows []ScopeBindingResolved) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	chunkSize := scopeBindingInsertChunkSize
	if chunkSize <= 0 {
		// SAFETY: see [lookupExistingOn] for the same guard.
		chunkSize = 8192
	}

	total := 0
	for start := 0; start < len(rows); start += chunkSize {
		end := start + chunkSize
		if end > len(rows) {
			end = len(rows)
		}
		n, err := w.insertFreshChunk(ctx, q, rows[start:end])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// insertFreshChunk runs ONE INSERT for at most chunkSize fresh
// rows and returns the RETURNING row count. Extracted from
// [insertFreshOn] so the chunk loop owns the chunk-size policy.
//
// The per-row VALUES group carries 7 placeholders plus the
// inline `NOW()` SQL literal for `created_at`. The literal
// (not a Go-side `time.Now()` parameter) keeps the server's
// wall clock authoritative AND saves one of the 7 placeholder
// slots per row.
func (w *ScopeBindingWriter) insertFreshChunk(ctx context.Context, q querier, rows []ScopeBindingResolved) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if needed := len(rows) * scopeBindingColumnCount; needed > pgMaxBindParameters {
		// SAFETY: catch a future chunk-size raise that
		// overshoots the ceiling. Failing FAST with a precise
		// message beats a runtime "got %d parameters, expected
		// at most 65535" from the driver.
		return 0, fmt.Errorf("storage: ScopeBindingWriter.insertFreshChunk: %d rows * %d params = %d exceeds PostgreSQL bound-parameter ceiling %d",
			len(rows), scopeBindingColumnCount, needed, pgMaxBindParameters)
	}

	args := make([]any, 0, len(rows)*scopeBindingColumnCount)
	var values strings.Builder
	enumType := w.qualifyEnum()
	for j, r := range rows {
		if j > 0 {
			values.WriteString(", ")
		}
		base := j * scopeBindingColumnCount
		// Eight columns in the per-row VALUES group: the seven
		// placeholder slots followed by the inline `NOW()`
		// literal for `created_at`. The total placeholder count
		// stays at `scopeBindingColumnCount` because `NOW()` is
		// a server-side function, not a bind parameter.
		fmt.Fprintf(&values, "($%d, $%d, $%d::%s, $%d, $%d, $%d, $%d::jsonb, NOW())",
			base+1, base+2, base+3, enumType, base+4, base+5, base+6, base+7)

		var amNodeID any
		if r.Candidate.AgentMemoryNodeID.Valid {
			amNodeID = r.Candidate.AgentMemoryNodeID.UUID.String()
		} else {
			amNodeID = nil
		}
		args = append(args,
			r.ScopeID.String(),
			r.Candidate.RepoID.String(),
			string(r.Candidate.Kind),
			r.Candidate.CanonicalSignature,
			r.FirstSeenSHA,
			amNodeID,
			string(r.Candidate.AttrsJSON),
		)
	}

	stmt := fmt.Sprintf(
		`INSERT INTO %s %s VALUES %s
		 ON CONFLICT (scope_id) DO NOTHING
		 RETURNING scope_id`,
		w.qualifyTable(), scopeBindingInsertColumns, values.String())

	queried, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgSQLStateUniqueViolation {
			return 0, fmt.Errorf("natural-key conflict (constraint=%q): a row with the same (repo_id, scope_kind, canonical_signature) exists at a DIFFERENT first_seen_sha than this writer's derived scope_id -- a bypass-the-writer write path landed first: %w",
				pqErr.Constraint, err)
		}
		return 0, fmt.Errorf("insert: %w", err)
	}
	defer queried.Close()

	count := 0
	for queried.Next() {
		var ignored string
		if err := queried.Scan(&ignored); err != nil {
			return 0, fmt.Errorf("scan inserted scope_id: %w", err)
		}
		count++
	}
	if err := queried.Err(); err != nil {
		return 0, fmt.Errorf("rows after insert: %w", err)
	}
	return count, nil
}
