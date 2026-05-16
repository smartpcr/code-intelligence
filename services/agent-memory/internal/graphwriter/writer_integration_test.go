package graphwriter

// Integration tests exercising the GraphWriter library against a
// live PostgreSQL 16 instance. Skips cleanly when
// AGENT_MEMORY_PG_URL is unset, mirroring the convention in
// migrations/test_migrate_test.go.
//
// Implementation-plan.md Stage 2.1 acceptance scenarios covered:
//
//   * "fingerprint determinism" -- pure unit test in
//     pkg/fingerprint (TestNodeFingerprint_determinism).
//   * "idempotent Node insert"
//       -> TestInsertNode_idempotentSecondInsertNoOp
//       -> TestInsertEdge_idempotentSecondInsertNoOp
//       -> TestInsertObservedCallsEdge_idempotentReturnsExisting
//   * "writer denied UPDATE"
//       -> TestWriter_forceUpdateNode_surfacesWriteContractViolation
//
// Plus correctness coverage the brief calls for explicitly:
//   * EnsureRepo / EnsureCommit idempotence
//   * Same-repo guard on parent_node_id and edge endpoints
//   * Structured-logging middleware emits {repo_id, kind,
//     fingerprint_hex, sha} on every insert.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/testpglock"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 30 * time.Second
)

// dbFixture is the per-test PostgreSQL substrate. `owner` is the
// connection that created the schema and ran migrations (it has
// CREATEROLE / superuser privileges). `app` is a SECOND
// connection authenticated as `agent_memory_app` so the writer's
// SQL surface exercises the real role-grant path the production
// service hits.
type dbFixture struct {
	owner   *sql.DB
	app     *sql.DB
	schema  string
	cleanup func()
}

// openFixture provisions a per-test schema, applies all
// migrations, and returns both an owner-role and an
// agent_memory_app-role *sql.DB pinned to that schema.
//
// The pattern mirrors migrations/test_stage14_role_grants_test.go.
// The integration brief calls for the writer to be exercised
// "as agent_memory_app", not via SET ROLE -- so we follow the
// established LOGIN flip.
func openFixture(t *testing.T) *dbFixture {
	t.Helper()
	base := os.Getenv(envPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", envPGURL)
	}

	owner, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("sql.Open owner: %v", err)
	}
	owner.SetMaxOpenConns(1)
	owner.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", envPGURL, err)
	}
	schema := newSchemaName(t)
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	if _, err := owner.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public, partman`, quoteIdent(schema),
	)); err != nil {
		_ = owner.Close()
		t.Fatalf("set search_path: %v", err)
	}

	// Apply all migrations (0001..0016) so role grants are in place.
	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	// Open a SECOND connection as agent_memory_app.
	app, revertRole := openAppRoleDB(t, owner, base, schema)

	cleanup := func() {
		_ = app.Close()
		revertRole()
		// Drop partman.part_config rows for tables in this schema
		// to keep cluster state clean (see migrations test for
		// the reasoning), then drop the schema.
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &dbFixture{owner: owner, app: app, schema: schema, cleanup: cleanup}
}

// openAppRoleDB enables LOGIN on agent_memory_app with a per-test
// random password and returns a *sql.DB authenticated as that
// role with search_path pinned to the per-test schema. The
// returned cleanup func reverts the role to NOLOGIN. Pattern
// adapted from migrations/test_stage14_role_grants_test.go so
// the two test packages share intent.
//
// Cross-package safety: the LOGIN ↔ NOLOGIN window is a
// cluster-wide mutation (the `agent_memory_app` role is a
// single global object). When `go test ./...` runs the
// `migrations` and `internal/graphwriter` packages
// concurrently against the same database — which is the
// default behaviour when AGENT_MEMORY_PG_URL is set — two
// sibling tests can clobber each other's password, producing
// spurious auth failures. To prevent that, this helper takes
// `testpglock.AcquireAppRoleLogin` (a session-level
// pg_advisory_lock) BEFORE the first `ALTER ROLE ... WITH
// LOGIN` and releases it AFTER the corresponding NOLOGIN
// revert, so the LOGIN window is mutually exclusive across
// every test binary.
func openAppRoleDB(
	t *testing.T, owner *sql.DB, baseURL, schema string,
) (*sql.DB, func()) {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse PG URL: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("AGENT_MEMORY_PG_URL must be postgres:// (got %q)", u.Scheme)
	}
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	password := "amwriter_" + hex.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Acquire the cross-package advisory lock BEFORE flipping
	// the role to LOGIN. The lock owns a dedicated *sql.DB on
	// its own session-pinned connection — it does NOT borrow
	// from the owner pool (which is intentionally capped at 1
	// connection here to keep search_path stable), so the
	// subsequent ALTER ROLE calls below cannot deadlock on it.
	releaseLock, err := testpglock.AcquireAppRoleLogin(ctx, baseURL)
	if err != nil {
		t.Fatalf("acquire app-role login lock: %v", err)
	}
	// Failure-path safety: if any of the steps below abort
	// the test via t.Fatalf BEFORE we reach the success
	// sentinel, runtime.Goexit unwinds deferreds — so the
	// lock is released even though the caller never sees the
	// returned cleanup closure. On the success path,
	// `success = true` disables this guard and the returned
	// cleanup func is responsible for the release.
	success := false
	defer func() {
		if !success {
			releaseLock()
		}
	}()

	if _, err := owner.ExecContext(ctx,
		`ALTER ROLE agent_memory_app WITH LOGIN PASSWORD `+pq.QuoteLiteral(password),
	); err != nil {
		t.Fatalf("ALTER ROLE LOGIN: %v", err)
	}
	// revert: NOLOGIN only. The advisory-lock release is
	// orchestrated separately (deferred guard on failure
	// paths; returned cleanup on the success path) so the
	// release happens exactly once.
	revert := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		_, _ = owner.ExecContext(ctx2, `ALTER ROLE agent_memory_app WITH NOLOGIN`)
	}

	u2 := *u
	u2.User = url.UserPassword("agent_memory_app", password)
	app, err := sql.Open("postgres", u2.String())
	if err != nil {
		revert()
		t.Fatalf("sql.Open app: %v", err)
	}
	app.SetMaxOpenConns(1)
	app.SetMaxIdleConns(1)
	if err := app.PingContext(ctx); err != nil {
		_ = app.Close()
		revert()
		t.Fatalf("ping app DB: %v", err)
	}
	if _, err := app.ExecContext(ctx,
		`SET search_path TO `+quoteIdent(schema)+`, public`,
	); err != nil {
		_ = app.Close()
		revert()
		t.Fatalf("SET search_path on app DB: %v", err)
	}
	success = true
	return app, func() {
		revert()
		releaseLock()
	}
}

func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amwriter_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// captureLogger returns a slog.Logger that writes JSON records
// into the supplied buffer. Used to assert the structured-logging
// middleware emits the {repo_id, kind, fingerprint_hex, sha}
// audit tuple from the brief.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seedRepo is a small helper that inserts a Repo row with a
// random URL (so tests are independent) and returns the
// EnsureRepo result. Uses the supplied Writer so it exercises
// the same role-grant + slog plumbing the rest of the test does.
func seedRepo(t *testing.T, ctx context.Context, w *Writer) RepoRecord {
	t.Helper()
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	rec, err := w.EnsureRepo(ctx, RepoInput{
		URL:            "https://example.test/" + hex.EncodeToString(buf[:]),
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go"},
	})
	if err != nil {
		t.Fatalf("seedRepo EnsureRepo: %v", err)
	}
	return rec
}

// ----- Tests --------------------------------------------------------

func TestEnsureRepo_idempotentByURL(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	in := RepoInput{
		URL:            "https://example.test/idempotent-repo",
		DefaultBranch:  "main",
		CurrentHeadSHA: "0000aaaa",
		LanguageHints:  []string{"go", "python"},
	}
	a, err := w.EnsureRepo(ctx, in)
	if err != nil {
		t.Fatalf("EnsureRepo first: %v", err)
	}
	if !a.Inserted {
		t.Error("first EnsureRepo: Inserted = false, want true")
	}
	// Second call with bumped head SHA must hit the UPDATE
	// branch and return Inserted=false but with the same RepoID.
	in.CurrentHeadSHA = "ffff1111"
	b, err := w.EnsureRepo(ctx, in)
	if err != nil {
		t.Fatalf("EnsureRepo second: %v", err)
	}
	if b.Inserted {
		t.Error("second EnsureRepo: Inserted = true, want false")
	}
	if a.RepoID != b.RepoID {
		t.Errorf("RepoID changed across calls: %s -> %s", a.RepoID, b.RepoID)
	}
	// Verify mutable fields actually moved.
	var head string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT current_head_sha FROM repo WHERE repo_id::text = $1`,
		a.RepoID,
	).Scan(&head); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if head != "ffff1111" {
		t.Errorf("current_head_sha after upsert = %q, want ffff1111", head)
	}
}

func TestEnsureCommit_idempotentByRepoSha(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)

	in := CommitInput{
		RepoID:      repo.ID,
		SHA:         "deadbeef00",
		ParentSHA:   "",
		CommittedAt: time.Now().UTC(),
	}
	a, err := w.EnsureCommit(ctx, in)
	if err != nil {
		t.Fatalf("EnsureCommit first: %v", err)
	}
	if !a.Inserted {
		t.Error("first EnsureCommit: Inserted = false, want true")
	}
	b, err := w.EnsureCommit(ctx, in)
	if err != nil {
		t.Fatalf("EnsureCommit second: %v", err)
	}
	if b.Inserted {
		t.Error("second EnsureCommit: Inserted = true, want false")
	}
	// Confirm there is exactly one row.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM repo_commit WHERE repo_id::text = $1 AND sha = $2`,
		a.RepoID, a.SHA,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("repo_commit count = %d, want 1 (idempotent insert)", n)
	}
}

// TestInsertNode_idempotentSecondInsertNoOp is the Stage 2.1
// "idempotent Node insert" scenario.
func TestInsertNode_idempotentSecondInsertNoOp(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	node := NodeInput{
		RepoID:             repo.ID,
		Kind:               "method",
		CanonicalSignature: "pkg.IdempotentTest#run()",
		FromSHA:            "deadbeef",
	}
	a, err := w.InsertNode(ctx, node)
	if err != nil {
		t.Fatalf("InsertNode first: %v", err)
	}
	if !a.Inserted {
		t.Error("first InsertNode: Inserted = false, want true")
	}
	b, err := w.InsertNode(ctx, node)
	if err != nil {
		t.Fatalf("InsertNode second: %v", err)
	}
	if b.Inserted {
		t.Error("second InsertNode: Inserted = true, want false (idempotent)")
	}
	if a.NodeID != b.NodeID {
		t.Errorf("NodeID changed across idempotent inserts: %s -> %s", a.NodeID, b.NodeID)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("Fingerprint changed across calls: %s -> %s", a.Fingerprint, b.Fingerprint)
	}
	// Exactly one row must exist with this fingerprint.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node WHERE repo_id::text = $1 AND fingerprint = $2`,
		repo.RepoID, a.Fingerprint.Bytes(),
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("node row count = %d, want 1 (G2 dedupe)", n)
	}
}

// TestInsertNode_parentMustBeSameRepo verifies the same-repo
// parent guard rubber-duck flagged. A parent in another repo is
// rejected before any INSERT lands.
func TestInsertNode_parentMustBeSameRepo(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoA, err := w.EnsureRepo(ctx, RepoInput{
		URL: "https://example.test/parent-repo-A", DefaultBranch: "main",
		CurrentHeadSHA: "aaaa", LanguageHints: []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo A: %v", err)
	}
	repoB, err := w.EnsureRepo(ctx, RepoInput{
		URL: "https://example.test/parent-repo-B", DefaultBranch: "main",
		CurrentHeadSHA: "bbbb", LanguageHints: []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo B: %v", err)
	}
	parentInB, err := w.InsertNode(ctx, NodeInput{
		RepoID: repoB.ID, Kind: "class",
		CanonicalSignature: "pkg.B.Parent", FromSHA: "bbbb",
	})
	if err != nil {
		t.Fatalf("seed parent in B: %v", err)
	}
	// Try to insert a child in A whose parent_node_id is in B.
	_, err = w.InsertNode(ctx, NodeInput{
		RepoID: repoA.ID, Kind: "method",
		CanonicalSignature: "pkg.A.kid#m()",
		ParentNodeID:       parentInB.NodeID,
		FromSHA:            "aaaa",
	})
	if err == nil {
		t.Fatal("expected cross-repo parent rejection; got nil")
	}
	if !strings.Contains(err.Error(), "belongs to repo") {
		t.Errorf("error did not mention cross-repo parent: %v", err)
	}
}

// TestInsertEdge_idempotentSecondInsertNoOp covers the Edge side
// of the idempotency contract.
func TestInsertEdge_idempotentSecondInsertNoOp(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	src, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Src#m()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dst, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Dst#m()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	in := EdgeInput{
		RepoID: repo.ID, Kind: "static_calls",
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID,
		FromSHA: "deadbeef",
	}
	a, err := w.InsertEdge(ctx, in)
	if err != nil {
		t.Fatalf("InsertEdge first: %v", err)
	}
	if !a.Inserted {
		t.Error("first InsertEdge: Inserted = false, want true")
	}
	// EdgeFingerprint must be computed from the stored src/dst FPs.
	wantFP, err := fingerprint.EdgeFingerprint(repo.ID, "static_calls",
		src.Fingerprint, dst.Fingerprint, "deadbeef")
	if err != nil {
		t.Fatalf("recompute edge fp: %v", err)
	}
	if a.Fingerprint != wantFP {
		t.Errorf("edge fp = %s, want %s", a.Fingerprint, wantFP)
	}
	// Second call is a no-op.
	b, err := w.InsertEdge(ctx, in)
	if err != nil {
		t.Fatalf("InsertEdge second: %v", err)
	}
	if b.Inserted {
		t.Error("second InsertEdge: Inserted = true, want false (idempotent)")
	}
	if a.EdgeID != b.EdgeID {
		t.Errorf("EdgeID changed across calls: %s -> %s", a.EdgeID, b.EdgeID)
	}
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM edge WHERE repo_id::text = $1 AND fingerprint = $2`,
		repo.RepoID, a.Fingerprint.Bytes(),
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("edge row count = %d, want 1", n)
	}
}

// TestInsertObservedCallsEdge_idempotentReturnsExisting is the
// §3.3 step 3 contract from the brief: a call to
// InsertObservedCallsEdge on an existing fingerprint returns the
// existing edge rather than erroring.
func TestInsertObservedCallsEdge_idempotentReturnsExisting(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	src, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Caller#hit()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dst, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Callee#run()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	// Caller must NOT pre-pin Kind; the helper does.
	in := EdgeInput{
		RepoID:    repo.ID, // Kind intentionally left zero
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID,
		FromSHA: "deadbeef",
	}
	a, err := w.InsertObservedCallsEdge(ctx, in)
	if err != nil {
		t.Fatalf("InsertObservedCallsEdge first: %v", err)
	}
	if !a.Inserted {
		t.Error("first InsertObservedCallsEdge: Inserted = false, want true")
	}
	b, err := w.InsertObservedCallsEdge(ctx, in)
	if err != nil {
		t.Fatalf("InsertObservedCallsEdge second: %v", err)
	}
	if b.Inserted {
		t.Error("second InsertObservedCallsEdge: Inserted = true, want false")
	}
	if a.EdgeID != b.EdgeID {
		t.Errorf("EdgeID changed: %s -> %s", a.EdgeID, b.EdgeID)
	}
	// Read back the row and confirm kind is `observed_calls`.
	var kind string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT kind FROM edge WHERE edge_id::text = $1`, a.EdgeID,
	).Scan(&kind); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if kind != "observed_calls" {
		t.Errorf("edge kind = %q, want observed_calls", kind)
	}
}

// TestInsertEdge_endpointsMustBeSameRepo verifies the second
// half of the rubber-duck-flagged guard: cross-repo edges are
// rejected before INSERT.
func TestInsertEdge_endpointsMustBeSameRepo(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoA, err := w.EnsureRepo(ctx, RepoInput{
		URL: "https://example.test/edge-A", DefaultBranch: "main",
		CurrentHeadSHA: "aaaa", LanguageHints: []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo A: %v", err)
	}
	repoB, err := w.EnsureRepo(ctx, RepoInput{
		URL: "https://example.test/edge-B", DefaultBranch: "main",
		CurrentHeadSHA: "bbbb", LanguageHints: []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo B: %v", err)
	}
	srcA, err := w.InsertNode(ctx, NodeInput{
		RepoID: repoA.ID, Kind: "method",
		CanonicalSignature: "pkg.A.src#m()", FromSHA: "aaaa",
	})
	if err != nil {
		t.Fatalf("seed srcA: %v", err)
	}
	dstB, err := w.InsertNode(ctx, NodeInput{
		RepoID: repoB.ID, Kind: "method",
		CanonicalSignature: "pkg.B.dst#m()", FromSHA: "bbbb",
	})
	if err != nil {
		t.Fatalf("seed dstB: %v", err)
	}
	_, err = w.InsertEdge(ctx, EdgeInput{
		RepoID: repoA.ID, Kind: "static_calls",
		SrcNodeID: srcA.NodeID, DstNodeID: dstB.NodeID,
		FromSHA: "aaaa",
	})
	if err == nil {
		t.Fatal("expected cross-repo edge rejection; got nil")
	}
	if !strings.Contains(err.Error(), "belongs to repo") {
		t.Errorf("error did not mention cross-repo endpoint: %v", err)
	}
}

// TestWriter_forceUpdateNode_surfacesWriteContractViolation is
// the Stage 2.1 "writer denied UPDATE" scenario. The forced
// UPDATE on `node` must come back as a typed
// WriteContractViolation, not a raw *pq.Error, so callers can
// pattern-match on the role-grant policy violation.
func TestWriter_forceUpdateNode_surfacesWriteContractViolation(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	node, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Forbidden#update()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}

	err = w.forceUpdateNodeForTesting(ctx, node.NodeID)
	if err == nil {
		t.Fatal("expected WriteContractViolation; got nil")
	}
	var wcv *WriteContractViolation
	if !errors.As(err, &wcv) {
		t.Fatalf("expected *WriteContractViolation, got %T: %v", err, err)
	}
	if wcv.SQLState != pgErrCodeInsufficientPrivilege {
		t.Errorf("SQLState = %q, want %q", wcv.SQLState, pgErrCodeInsufficientPrivilege)
	}
	if wcv.Op != "force_update_for_testing" {
		t.Errorf("Op = %q, want force_update_for_testing", wcv.Op)
	}
	// Underlying *pq.Error must still be reachable.
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Errorf("underlying *pq.Error not reachable via errors.As")
	} else if string(pqErr.Code) != pgErrCodeInsufficientPrivilege {
		t.Errorf("inner SQLSTATE = %q, want %q", pqErr.Code, pgErrCodeInsufficientPrivilege)
	}
}

// TestWriter_emitsStructuredAuditOnInsertNode pins the
// "Wire a structured-logging middleware on every writer call so
// each insert emits {repo_id, kind, fingerprint_hex, sha} for
// audit" requirement from the brief.
func TestWriter_emitsStructuredAuditOnInsertNode(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	var buf bytes.Buffer
	w := New(fix.app, captureLogger(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	node, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Logged#m()", FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("InsertNode: %v", err)
	}
	assertAuditFields(t, buf.Bytes(), "graphwriter.insert_node",
		map[string]any{
			"repo_id":         repo.RepoID,
			"kind":            "method",
			"fingerprint_hex": node.Fingerprint.Hex(),
			"sha":             "feedface",
		})
}

// TestWriter_emitsStructuredAuditOnInsertEdge mirrors the Node
// audit test for edges. The brief requires every insert to emit
// `{repo_id, kind, fingerprint_hex, sha}` so the edge path must
// carry the same uniform tuple as InsertNode.
func TestWriter_emitsStructuredAuditOnInsertEdge(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	var buf bytes.Buffer
	w := New(fix.app, captureLogger(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	src, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.AuditSrc#m()", FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dst, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.AuditDst#m()", FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	buf.Reset() // discard records from the InsertNode seeds
	edge, err := w.InsertEdge(ctx, EdgeInput{
		RepoID: repo.ID, Kind: "static_calls",
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID,
		FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("InsertEdge: %v", err)
	}
	assertAuditFields(t, buf.Bytes(), "graphwriter.insert_edge",
		map[string]any{
			"repo_id":         repo.RepoID,
			"kind":            "static_calls",
			"fingerprint_hex": edge.Fingerprint.Hex(),
			"sha":             "feedface",
		})
}

// TestWriter_emitsStructuredAuditOnInsertObservedCallsEdge pins
// the audit-shape contract for the observed_calls specialisation.
// The op name distinguishes it from the generic InsertEdge path
// — the rubber-duck review explicitly flagged that delegation
// must NOT produce two log lines per public call.
func TestWriter_emitsStructuredAuditOnInsertObservedCallsEdge(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	var buf bytes.Buffer
	w := New(fix.app, captureLogger(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	src, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.ObsAuditSrc#m()", FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("seed src: %v", err)
	}
	dst, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.ObsAuditDst#m()", FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	buf.Reset()
	edge, err := w.InsertObservedCallsEdge(ctx, EdgeInput{
		RepoID:    repo.ID,
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID,
		FromSHA: "feedface",
	})
	if err != nil {
		t.Fatalf("InsertObservedCallsEdge: %v", err)
	}
	assertAuditFields(t, buf.Bytes(), "graphwriter.insert_observed_calls_edge",
		map[string]any{
			"repo_id":         repo.RepoID,
			"kind":            "observed_calls",
			"fingerprint_hex": edge.Fingerprint.Hex(),
			"sha":             "feedface",
		})
	// Defence against the double-emit regression the rubber-duck
	// flagged: InsertObservedCallsEdge must NOT also emit a
	// `graphwriter.insert_edge` line.
	if countLogMessages(buf.Bytes(), "graphwriter.insert_edge") != 0 {
		t.Errorf("InsertObservedCallsEdge emitted an extra graphwriter.insert_edge audit line; buffer:\n%s",
			buf.String())
	}
}

// TestWriter_emitsStructuredAuditOnEnsureRepoAndCommit confirms
// that the audit middleware fires uniformly on EnsureRepo /
// EnsureCommit too. They don't carry kind/fingerprint_hex so we
// only require the keys to be present (empty string is fine).
func TestWriter_emitsStructuredAuditOnEnsureRepoAndCommit(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	var buf bytes.Buffer
	w := New(fix.app, captureLogger(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo, err := w.EnsureRepo(ctx, RepoInput{
		URL: "https://example.test/audit-repo", DefaultBranch: "main",
		CurrentHeadSHA: "shaeq01", LanguageHints: []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	assertAuditFields(t, buf.Bytes(), "graphwriter.ensure_repo",
		map[string]any{
			"repo_id": repo.RepoID,
			"sha":     "shaeq01",
		})

	buf.Reset()
	_, err = w.EnsureCommit(ctx, CommitInput{
		RepoID:      repo.ID,
		SHA:         "shacommit01",
		CommittedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("EnsureCommit: %v", err)
	}
	assertAuditFields(t, buf.Bytes(), "graphwriter.ensure_commit",
		map[string]any{
			"repo_id": repo.RepoID,
			"sha":     "shacommit01",
		})
}

// TestWriter_emitsFailureAuditOnDeniedUpdate is the failure-path
// half of the structured-logging contract. The denied-UPDATE
// scenario must produce a single error-level
// `graphwriter.force_update_for_testing.failed` record carrying
// `contract_violation: true`, the operator-facing
// distinguishing signal for an attempted G5 violation.
func TestWriter_emitsFailureAuditOnDeniedUpdate(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	var buf bytes.Buffer
	w := New(fix.app, captureLogger(&buf))
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	node, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.FailureAudit#m()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	buf.Reset()
	if err := w.forceUpdateNodeForTesting(ctx, node.NodeID); err == nil {
		t.Fatal("expected WriteContractViolation; got nil")
	}
	matched := findLogMessage(t, buf.Bytes(),
		"graphwriter.force_update_for_testing.failed")
	if matched == nil {
		t.Fatalf("no failure audit emitted; buffer:\n%s", buf.String())
	}
	if got, _ := matched["level"].(string); got != "ERROR" {
		t.Errorf("failure audit level = %q, want ERROR", got)
	}
	if cv, _ := matched["contract_violation"].(bool); !cv {
		t.Errorf("contract_violation = %v, want true", matched["contract_violation"])
	}
	if _, ok := matched["error"]; !ok {
		t.Errorf("failure audit missing `error` field; keys: %v", keysOf(matched))
	}
	if _, ok := matched["error_type"]; !ok {
		t.Errorf("failure audit missing `error_type` field; keys: %v", keysOf(matched))
	}
}

// assertAuditFields scans the captured slog buffer for a record
// matching `wantMsg` and asserts (a) all four audit keys are
// present and (b) the supplied values match. Empty / unspecified
// values in `want` are checked for presence only.
func assertAuditFields(t *testing.T, buf []byte, wantMsg string, want map[string]any) {
	t.Helper()
	matched := findLogMessage(t, buf, wantMsg)
	if matched == nil {
		t.Fatalf("no %s log line emitted; buffer:\n%s", wantMsg, string(buf))
	}
	for _, key := range []string{"repo_id", "kind", "fingerprint_hex", "sha"} {
		if _, ok := matched[key]; !ok {
			t.Errorf("%s log missing required audit key %q (keys: %v)",
				wantMsg, key, keysOf(matched))
		}
	}
	for k, v := range want {
		if matched[k] != v {
			t.Errorf("%s log %s = %v, want %v", wantMsg, k, matched[k], v)
		}
	}
}

// findLogMessage scans `buf` (one JSON record per line) for the
// first record whose `msg` field matches `wantMsg` and returns
// the parsed map. Returns nil if no record matches.
func findLogMessage(t *testing.T, buf []byte, wantMsg string) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(buf), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry["msg"] == wantMsg {
			return entry
		}
	}
	return nil
}

// countLogMessages returns how many records in `buf` have the
// supplied `msg`. Used to detect the double-emit regression for
// InsertObservedCallsEdge.
func countLogMessages(buf []byte, wantMsg string) int {
	n := 0
	for _, line := range bytes.Split(bytes.TrimSpace(buf), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry["msg"] == wantMsg {
			n++
		}
	}
	return n
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
