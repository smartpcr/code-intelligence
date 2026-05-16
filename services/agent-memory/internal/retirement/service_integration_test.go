package retirement

// Integration tests for the tombstone retirement service against
// a live PostgreSQL 16 instance. The tests skip cleanly when
// AGENT_MEMORY_PG_URL is unset, matching the convention used by
// the sibling `migrations` and `internal/graphwriter` packages.
//
// Stage 2.3 brief coverage (implementation-plan.md):
//
//   * "double-retirement rejected" scenario
//       -> TestRetireNode_secondRetirementSurfacesAlreadyRetired
//       -> TestRetireEdge_secondRetirementSurfacesAlreadyRetired
//   * "rename retirement links new node" scenario
//       -> TestRetireNode_renameLinksNewNodeViaSupersedeAndEdge
//   * RetireMany single multi-row INSERT (tech-spec §9.7)
//       -> TestRetireMany_insertsAllRowsInOneTx
//       -> TestRetireMany_anyDuplicateAbortsWholeBatch
//   * RetireNode / RetireEdge missing-target surfaces NotFound
//       -> TestRetireNode_missingNodeIDReturnsNotFound
//       -> TestRetireNode_missingSupersedeIDReturnsNotFound
//       -> TestRetireEdge_missingEdgeIDReturnsNotFound
//   * G5 contract enforcement (role-grant denies DELETE on tombstones)
//       -> TestRetireNode_deleteOnTombstoneSurfacesContractViolation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/testpglock"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 30 * time.Second
)

// dbFixture mirrors the per-test substrate used by the graphwriter
// integration tests: `owner` runs migrations and reads back rows
// for assertions; `app` is authenticated as agent_memory_app so
// the retirement service exercises the real role-grant policy.
type dbFixture struct {
	owner   *sql.DB
	app     *sql.DB
	schema  string
	cleanup func()
}

// openFixture provisions a per-test schema, applies all
// migrations, and returns both an owner-role and an
// agent_memory_app-role *sql.DB pinned to that schema. The
// pattern is intentionally identical to graphwriter's openFixture
// so the retirement service is exercised under the same role
// posture the production service hits.
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

	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	app, revertRole := openAppRoleDB(t, owner, base, schema)

	cleanup := func() {
		_ = app.Close()
		revertRole()
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
// role with search_path pinned to the per-test schema. Pattern
// adapted from graphwriter's openAppRoleDB and gated on
// testpglock.AcquireAppRoleLogin so concurrent test packages do
// not clobber each other's password.
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
	password := "retire_" + hex.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	releaseLock, err := testpglock.AcquireAppRoleLogin(ctx, baseURL)
	if err != nil {
		t.Fatalf("acquire app-role login lock: %v", err)
	}
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
	return "retire_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ----- seeding helpers --------------------------------------------

// seedRepoAndNode is the most common test prelude: one Repo,
// one Node, both inserted through the GraphWriter so the role
// posture and audit plumbing are exercised end-to-end.
type seeded struct {
	writer  *graphwriter.Writer
	repoRec graphwriter.RepoRecord
	node    graphwriter.NodeRecord
}

func seedRepoAndNode(t *testing.T, ctx context.Context, app *sql.DB, sig string) seeded {
	t.Helper()
	w := graphwriter.New(app, slog.Default())
	repo, err := w.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            "https://example.test/" + randHex(t, 4),
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go"},
	})
	if err != nil {
		t.Fatalf("seedRepoAndNode EnsureRepo: %v", err)
	}
	node, err := w.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repo.ID,
		Kind:               "method",
		CanonicalSignature: sig,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		t.Fatalf("seedRepoAndNode InsertNode: %v", err)
	}
	return seeded{writer: w, repoRec: repo, node: node}
}

// seedEdge inserts a second method node and a static_calls edge
// between the supplied src node and the new dst.
func seedEdge(t *testing.T, ctx context.Context, s seeded, dstSig string) graphwriter.EdgeRecord {
	t.Helper()
	dst, err := s.writer.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: s.repoRec.ID, Kind: "method",
		CanonicalSignature: dstSig,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		t.Fatalf("seedEdge InsertNode dst: %v", err)
	}
	edge, err := s.writer.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    s.repoRec.ID,
		Kind:      "static_calls",
		SrcNodeID: s.node.NodeID,
		DstNodeID: dst.NodeID,
		FromSHA:   "deadbeef",
	})
	if err != nil {
		t.Fatalf("seedEdge InsertEdge: %v", err)
	}
	return edge
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// ----- RetireNode tests -------------------------------------------

// TestRetireNode_basicHappyPath covers the simplest contract:
// retire one node, get back a record carrying the server-stamped
// retired_at, and confirm the row is visible to the owner-role
// readback.
func TestRetireNode_basicHappyPath(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.Happy#m()")

	rec, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID:       s.node.NodeID,
		RetiredAtSHA: "shaA",
	})
	if err != nil {
		t.Fatalf("RetireNode: %v", err)
	}
	if rec.NodeID != s.node.NodeID {
		t.Errorf("rec.NodeID = %q, want %q", rec.NodeID, s.node.NodeID)
	}
	if rec.RetirementID == "" {
		t.Error("rec.RetirementID empty; expected gen_random_uuid()")
	}
	if rec.RetiredAt.IsZero() {
		t.Error("rec.RetiredAt zero; expected server-side now()")
	}
	if rec.SupersededByNodeID != "" {
		t.Errorf("rec.SupersededByNodeID = %q, want empty", rec.SupersededByNodeID)
	}

	// Owner-role readback confirms the row landed.
	var got string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT retired_at_sha FROM node_retirement WHERE node_id::text = $1`,
		s.node.NodeID,
	).Scan(&got); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if got != "shaA" {
		t.Errorf("retired_at_sha = %q, want shaA", got)
	}
}

// TestRetireNode_secondRetirementSurfacesAlreadyRetired is the
// Stage 2.3 acceptance scenario "double-retirement rejected".
// The brief: "second retirement of the same id fails with the
// UNIQUE-index error from §5.2.4" -> typed *AlreadyRetired.
func TestRetireNode_secondRetirementSurfacesAlreadyRetired(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.DoubleRetire#m()")

	if _, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID: s.node.NodeID, RetiredAtSHA: "sha1",
	}); err != nil {
		t.Fatalf("first RetireNode: %v", err)
	}
	_, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID: s.node.NodeID, RetiredAtSHA: "sha2",
	})
	if err == nil {
		t.Fatal("second RetireNode succeeded; expected AlreadyRetired")
	}
	var ar *AlreadyRetired
	if !errors.As(err, &ar) {
		t.Fatalf("err type = %T (%v); want *AlreadyRetired", err, err)
	}
	if ar.Kind != KindNode {
		t.Errorf("AlreadyRetired.Kind = %q, want %q", ar.Kind, KindNode)
	}
	if ar.TargetID != s.node.NodeID {
		t.Errorf("AlreadyRetired.TargetID = %q, want %q",
			ar.TargetID, s.node.NodeID)
	}
	if ar.SQLState != pgErrCodeUniqueViolation {
		t.Errorf("AlreadyRetired.SQLState = %q, want %q",
			ar.SQLState, pgErrCodeUniqueViolation)
	}
	// The wrapped *pq.Error must remain reachable for operators
	// who want the raw Detail / Constraint name.
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Error("underlying *pq.Error not reachable via errors.As")
	} else if string(pqErr.Code) != pgErrCodeUniqueViolation {
		t.Errorf("inner SQLSTATE = %q, want %q",
			pqErr.Code, pgErrCodeUniqueViolation)
	}

	// Exactly one tombstone row must exist; the UNIQUE index is
	// the load-bearing G5 enforcer.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
		s.node.NodeID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("node_retirement count = %d, want 1", n)
	}
}

// TestRetireNode_renameLinksNewNodeViaSupersedeAndEdge is the
// Stage 2.3 acceptance scenario "rename retirement links new
// node". The brief: "Given a method is renamed in a new commit,
// When the Repo Indexer calls RetireNode(old_id, sha,
// superseded_by=new_id) and writes a `renamed_to` Edge, Then
// NodeRetirement.superseded_by_node_id = new_id and a
// `renamed_to` Edge row exists pointing from old to new
// fingerprint."
func TestRetireNode_renameLinksNewNodeViaSupersedeAndEdge(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.OldName#m()")

	// Insert the renamed Node (the replacement). In production
	// the Repo Indexer would call GraphWriter for this BEFORE
	// retiring the old one, which is what our pre-check assumes.
	newNode, err := s.writer.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             s.repoRec.ID,
		Kind:               "method",
		CanonicalSignature: "pkg.NewName#m()",
		FromSHA:            "shaNew",
	})
	if err != nil {
		t.Fatalf("InsertNode new: %v", err)
	}

	rec, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID:             s.node.NodeID,
		RetiredAtSHA:       "shaNew",
		SupersededByNodeID: newNode.NodeID,
	})
	if err != nil {
		t.Fatalf("RetireNode: %v", err)
	}
	if rec.SupersededByNodeID != newNode.NodeID {
		t.Errorf("rec.SupersededByNodeID = %q, want %q",
			rec.SupersededByNodeID, newNode.NodeID)
	}

	// Write the renamed_to Edge through GraphWriter, mirroring
	// the Repo Indexer's two-step (retire + link) sequence.
	renamed, err := s.writer.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    s.repoRec.ID,
		Kind:      "renamed_to",
		SrcNodeID: s.node.NodeID,
		DstNodeID: newNode.NodeID,
		FromSHA:   "shaNew",
	})
	if err != nil {
		t.Fatalf("InsertEdge renamed_to: %v", err)
	}

	// Cross-table assertion: the tombstone column references the
	// same UUID the renamed_to edge points at.
	var (
		gotSupersede string
		gotEdgeKind  string
		gotEdgeSrcFP []byte
		gotEdgeDstFP []byte
	)
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT nr.superseded_by_node_id::text,
		       e.kind::text,
		       src.fingerprint, dst.fingerprint
		FROM node_retirement nr
		JOIN edge e ON e.edge_id = $2::uuid
		JOIN node src ON src.node_id = e.src_node_id
		JOIN node dst ON dst.node_id = e.dst_node_id
		WHERE nr.node_id = $1::uuid
	`, s.node.NodeID, renamed.EdgeID).Scan(
		&gotSupersede, &gotEdgeKind, &gotEdgeSrcFP, &gotEdgeDstFP,
	); err != nil {
		t.Fatalf("cross-table readback: %v", err)
	}
	if gotSupersede != newNode.NodeID {
		t.Errorf("node_retirement.superseded_by_node_id = %q, want %q",
			gotSupersede, newNode.NodeID)
	}
	if gotEdgeKind != "renamed_to" {
		t.Errorf("edge.kind = %q, want renamed_to", gotEdgeKind)
	}
	// The edge's src/dst fingerprints must equal the original
	// (old, new) node fingerprints. This is what makes the
	// renamed_to edge "point from old to new fingerprint" per
	// the brief.
	if !bytesEqual(gotEdgeSrcFP, s.node.Fingerprint.Bytes()) {
		t.Errorf("renamed_to edge src fingerprint mismatch:\n got %x\nwant %x",
			gotEdgeSrcFP, s.node.Fingerprint.Bytes())
	}
	if !bytesEqual(gotEdgeDstFP, newNode.Fingerprint.Bytes()) {
		t.Errorf("renamed_to edge dst fingerprint mismatch:\n got %x\nwant %x",
			gotEdgeDstFP, newNode.Fingerprint.Bytes())
	}
}

// TestRetireNode_missingNodeIDReturnsNotFound proves the
// pre-check path: a never-inserted node id surfaces as
// *NotFound with the id named, not as a downstream FK error.
func TestRetireNode_missingNodeIDReturnsNotFound(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	const missing = "00000000-0000-0000-0000-000000000099"
	_, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID: missing, RetiredAtSHA: "shaX",
	})
	if err == nil {
		t.Fatal("expected NotFound; got nil")
	}
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err type = %T; want *NotFound", err)
	}
	if nf.Kind != KindNode {
		t.Errorf("NotFound.Kind = %q, want %q", nf.Kind, KindNode)
	}
	if nf.TargetID != missing {
		t.Errorf("NotFound.TargetID = %q, want %q", nf.TargetID, missing)
	}
	// Sanity: no tombstone row leaked.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
		missing,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("tombstone leaked: count = %d", n)
	}
}

// TestRetireNode_missingSupersedeIDReturnsNotFound proves the
// pre-check covers the supersede column too: a Repo Indexer bug
// where the rename target was never inserted surfaces as
// *NotFound with the missing supersede id named, before any
// INSERT lands on `node_retirement`.
func TestRetireNode_missingSupersedeIDReturnsNotFound(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.Old#m()")
	const missing = "00000000-0000-0000-0000-000000000099"
	_, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID:             s.node.NodeID,
		RetiredAtSHA:       "shaX",
		SupersededByNodeID: missing,
	})
	if err == nil {
		t.Fatal("expected NotFound; got nil")
	}
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err type = %T; want *NotFound", err)
	}
	if nf.TargetID != missing {
		t.Errorf("NotFound.TargetID = %q, want %q (supersede id)",
			nf.TargetID, missing)
	}
	// No partial state: the target node must NOT have been
	// retired since the supersede pre-check failed first.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
		s.node.NodeID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("target node was retired despite supersede failure; count = %d", n)
	}
}

// ----- RetireEdge tests -------------------------------------------

// TestRetireEdge_basicHappyPath mirrors the node-side happy path
// for edges.
func TestRetireEdge_basicHappyPath(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.EdgeSrc#m()")
	edge := seedEdge(t, ctx, s, "pkg.EdgeDst#m()")

	rec, err := svc.RetireEdge(ctx, EdgeRetirementInput{
		EdgeID: edge.EdgeID, RetiredAtSHA: "shaE",
	})
	if err != nil {
		t.Fatalf("RetireEdge: %v", err)
	}
	if rec.EdgeID != edge.EdgeID {
		t.Errorf("rec.EdgeID = %q, want %q", rec.EdgeID, edge.EdgeID)
	}
	if rec.RetirementID == "" {
		t.Error("rec.RetirementID empty")
	}
	if rec.RetiredAt.IsZero() {
		t.Error("rec.RetiredAt zero")
	}
	var got string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT retired_at_sha FROM edge_retirement WHERE edge_id::text = $1`,
		edge.EdgeID,
	).Scan(&got); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if got != "shaE" {
		t.Errorf("retired_at_sha = %q, want shaE", got)
	}
}

// TestRetireEdge_secondRetirementSurfacesAlreadyRetired covers
// the edge-side UNIQUE-index path symmetrically.
func TestRetireEdge_secondRetirementSurfacesAlreadyRetired(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.EdgeDoubleSrc#m()")
	edge := seedEdge(t, ctx, s, "pkg.EdgeDoubleDst#m()")
	if _, err := svc.RetireEdge(ctx, EdgeRetirementInput{
		EdgeID: edge.EdgeID, RetiredAtSHA: "shaE1",
	}); err != nil {
		t.Fatalf("first RetireEdge: %v", err)
	}
	_, err := svc.RetireEdge(ctx, EdgeRetirementInput{
		EdgeID: edge.EdgeID, RetiredAtSHA: "shaE2",
	})
	if err == nil {
		t.Fatal("second RetireEdge succeeded; expected AlreadyRetired")
	}
	var ar *AlreadyRetired
	if !errors.As(err, &ar) {
		t.Fatalf("err type = %T; want *AlreadyRetired", err)
	}
	if ar.Kind != KindEdge {
		t.Errorf("AlreadyRetired.Kind = %q, want %q", ar.Kind, KindEdge)
	}
	if ar.TargetID != edge.EdgeID {
		t.Errorf("AlreadyRetired.TargetID = %q, want %q", ar.TargetID, edge.EdgeID)
	}
}

// TestRetireEdge_missingEdgeIDReturnsNotFound covers the pre-check
// path for edges.
func TestRetireEdge_missingEdgeIDReturnsNotFound(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	const missing = "00000000-0000-0000-0000-000000000077"
	_, err := svc.RetireEdge(ctx, EdgeRetirementInput{
		EdgeID: missing, RetiredAtSHA: "shaX",
	})
	if err == nil {
		t.Fatal("expected NotFound; got nil")
	}
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err type = %T; want *NotFound", err)
	}
	if nf.Kind != KindEdge {
		t.Errorf("Kind = %q, want %q", nf.Kind, KindEdge)
	}
}

// ----- RetireMany tests -------------------------------------------

// TestRetireMany_insertsAllRowsInOneTx covers the bulk-rename
// hot path the tech-spec §9.7 risk calls out: a single multi-row
// INSERT lands every input id.
func TestRetireMany_insertsAllRowsInOneTx(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.Many.A#m()")
	// Add 4 more nodes so the batch covers 5 ids; small enough
	// that local laptops run it quickly, large enough to prove
	// "many" is wired.
	ids := []string{s.node.NodeID}
	for i := 1; i < 5; i++ {
		n, err := s.writer.InsertNode(ctx, graphwriter.NodeInput{
			RepoID: s.repoRec.ID, Kind: "method",
			CanonicalSignature: fmt.Sprintf("pkg.Many.%d#m()", i),
			FromSHA:            "deadbeef",
		})
		if err != nil {
			t.Fatalf("InsertNode many[%d]: %v", i, err)
		}
		ids = append(ids, n.NodeID)
	}

	res, err := svc.RetireMany(ctx, ids, "shaBatch")
	if err != nil {
		t.Fatalf("RetireMany: %v", err)
	}
	if res.InsertedCount != len(ids) {
		t.Errorf("InsertedCount = %d, want %d",
			res.InsertedCount, len(ids))
	}
	if len(res.Records) != len(ids) {
		t.Errorf("len(Records) = %d, want %d",
			len(res.Records), len(ids))
	}
	// All returned records carry the shared SHA and a populated
	// RetirementID + RetiredAt timestamp.
	for i, r := range res.Records {
		if r.RetiredAtSHA != "shaBatch" {
			t.Errorf("Records[%d].RetiredAtSHA = %q, want shaBatch",
				i, r.RetiredAtSHA)
		}
		if r.RetirementID == "" {
			t.Errorf("Records[%d].RetirementID empty", i)
		}
		if r.RetiredAt.IsZero() {
			t.Errorf("Records[%d].RetiredAt zero", i)
		}
	}
	// Cross-check: every input id has exactly one tombstone row.
	for _, id := range ids {
		var n int
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
			id,
		).Scan(&n); err != nil {
			t.Fatalf("count for %s: %v", id, err)
		}
		if n != 1 {
			t.Errorf("node_retirement count for %s = %d, want 1", id, n)
		}
	}
}

// TestRetireMany_emptyInputIsNoOp pins the documented zero-length
// behaviour: zero input ids return BatchResult{}-and-nil without
// any database round-trip.
func TestRetireMany_emptyInputIsNoOp(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	res, err := svc.RetireMany(ctx, nil, "shaIgnored")
	if err != nil {
		t.Errorf("RetireMany(nil): err = %v, want nil", err)
	}
	if res.InsertedCount != 0 || len(res.Records) != 0 {
		t.Errorf("RetireMany(nil): got %+v, want zero-value", res)
	}
}

// TestRetireMany_anyDuplicateAbortsWholeBatch proves the atomic
// semantics documented on RetireMany: if any id in the batch is
// already retired, the whole INSERT rolls back -- zero new rows
// land, even for the still-valid ids in the batch.
func TestRetireMany_anyDuplicateAbortsWholeBatch(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.Atomic.A#m()")
	other, err := s.writer.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: s.repoRec.ID, Kind: "method",
		CanonicalSignature: "pkg.Atomic.B#m()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}

	// Pre-retire `other` so the upcoming batch contains a
	// guaranteed duplicate.
	if _, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID: other.NodeID, RetiredAtSHA: "shaPrior",
	}); err != nil {
		t.Fatalf("pre-retire other: %v", err)
	}

	// Batch attempts to retire both ids. The duplicate `other`
	// must abort the INSERT entirely; `s.node` must NOT land.
	_, err = svc.RetireMany(ctx,
		[]string{s.node.NodeID, other.NodeID}, "shaBatch")
	if err == nil {
		t.Fatal("expected AlreadyRetired; got nil")
	}
	var ar *AlreadyRetired
	if !errors.As(err, &ar) {
		t.Fatalf("err type = %T (%v); want *AlreadyRetired", err, err)
	}
	if ar.Kind != KindNode {
		t.Errorf("AlreadyRetired.Kind = %q, want %q", ar.Kind, KindNode)
	}

	// s.node must NOT have been retired -- atomic rollback.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
		s.node.NodeID,
	).Scan(&n); err != nil {
		t.Fatalf("count s.node: %v", err)
	}
	if n != 0 {
		t.Errorf("s.node was retired despite batch abort; count = %d", n)
	}

	// other still has exactly its one prior tombstone.
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
		other.NodeID,
	).Scan(&n); err != nil {
		t.Fatalf("count other: %v", err)
	}
	if n != 1 {
		t.Errorf("other tombstone count = %d, want 1", n)
	}
}

// TestRetireMany_missingTargetAbortsBatch covers the FK side of
// the atomic-rollback contract: a never-inserted id in the batch
// must roll the whole INSERT back and surface as *NotFound (the
// SQLSTATE 23503 path) with the wrapped *pq.Error reachable for
// diagnostics.
func TestRetireMany_missingTargetAbortsBatch(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.Batch.Real#m()")
	const missing = "00000000-0000-0000-0000-000000000099"
	_, err := svc.RetireMany(ctx,
		[]string{s.node.NodeID, missing}, "shaBatch")
	if err == nil {
		t.Fatal("expected NotFound; got nil")
	}
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err type = %T (%v); want *NotFound", err, err)
	}
	if nf.SQLState != pgErrCodeForeignKeyViolation {
		t.Errorf("NotFound.SQLState = %q, want %q",
			nf.SQLState, pgErrCodeForeignKeyViolation)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Error("underlying *pq.Error not reachable")
	}
	// Atomic rollback: s.node must NOT have been retired.
	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node_retirement WHERE node_id::text = $1`,
		s.node.NodeID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("s.node retired despite batch FK abort; count = %d", n)
	}
}

// ----- WriteContractViolation surface ----------------------------

// TestRetireNode_deleteOnTombstoneSurfacesContractViolation proves
// the G5 enforcement at the role-grant layer: agent_memory_app
// has INSERT + SELECT on node_retirement but NOT DELETE, so any
// attempted DELETE returns SQLSTATE 42501 which the service
// surfaces as *WriteContractViolation. Mirrors graphwriter's
// equivalent "writer denied UPDATE" test by intent.
func TestRetireNode_deleteOnTombstoneSurfacesContractViolation(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.Forbidden#m()")
	if _, err := svc.RetireNode(ctx, NodeRetirementInput{
		NodeID: s.node.NodeID, RetiredAtSHA: "shaA",
	}); err != nil {
		t.Fatalf("seed retirement: %v", err)
	}
	err := svc.forceDeleteRetirementForTesting(ctx, s.node.NodeID)
	if err == nil {
		t.Fatal("expected WriteContractViolation; got nil")
	}
	var wcv *WriteContractViolation
	if !errors.As(err, &wcv) {
		t.Fatalf("err type = %T (%v); want *WriteContractViolation", err, err)
	}
	if wcv.SQLState != pgErrCodeInsufficientPrivilege {
		t.Errorf("SQLState = %q, want %q",
			wcv.SQLState, pgErrCodeInsufficientPrivilege)
	}
	if wcv.Op != "force_delete_for_testing" {
		t.Errorf("Op = %q, want force_delete_for_testing", wcv.Op)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Error("underlying *pq.Error not reachable")
	} else if string(pqErr.Code) != pgErrCodeInsufficientPrivilege {
		t.Errorf("inner SQLSTATE = %q, want %q",
			pqErr.Code, pgErrCodeInsufficientPrivilege)
	}
}

// TestRetireManyEdges_insertsAllRowsInOneTx is the edge-side
// integration test for the bulk path. It exercises the new
// RetireManyEdges entry point against the real edge_retirement
// UNIQUE / FK constraints. Mirrors TestRetireMany_insertsAllRowsInOneTx
// on the edge tombstone table.
func TestRetireManyEdges_insertsAllRowsInOneTx(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.EdgeBatch.A#m()")
	edges := []string{
		seedEdge(t, ctx, s, "pkg.EdgeBatch.B#m()").EdgeID,
		seedEdge(t, ctx, s, "pkg.EdgeBatch.C#m()").EdgeID,
		seedEdge(t, ctx, s, "pkg.EdgeBatch.D#m()").EdgeID,
	}

	res, err := svc.RetireManyEdges(ctx, edges, "shaEdgeBatch")
	if err != nil {
		t.Fatalf("RetireManyEdges: %v", err)
	}
	if res.InsertedCount != len(edges) {
		t.Errorf("InsertedCount = %d, want %d",
			res.InsertedCount, len(edges))
	}
	if len(res.Records) != len(edges) {
		t.Errorf("len(Records) = %d, want %d",
			len(res.Records), len(edges))
	}
	for i, r := range res.Records {
		if r.RetiredAtSHA != "shaEdgeBatch" {
			t.Errorf("Records[%d].RetiredAtSHA = %q, want shaEdgeBatch",
				i, r.RetiredAtSHA)
		}
		if r.RetirementID == "" {
			t.Errorf("Records[%d].RetirementID empty", i)
		}
		if r.RetiredAt.IsZero() {
			t.Errorf("Records[%d].RetiredAt zero", i)
		}
	}
	// Cross-check: every input id has exactly one tombstone row.
	for _, id := range edges {
		var n int
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT count(*) FROM edge_retirement WHERE edge_id::text = $1`,
			id,
		).Scan(&n); err != nil {
			t.Fatalf("count for %s: %v", id, err)
		}
		if n != 1 {
			t.Errorf("edge_retirement count for %s = %d, want 1", id, n)
		}
	}
}

// TestRetireManyEdges_emptyInputIsNoOp pins the documented
// zero-length behaviour on the edge-batch path.
func TestRetireManyEdges_emptyInputIsNoOp(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	res, err := svc.RetireManyEdges(ctx, nil, "shaIgnored")
	if err != nil {
		t.Errorf("RetireManyEdges(nil): err = %v, want nil", err)
	}
	if res.InsertedCount != 0 || len(res.Records) != 0 {
		t.Errorf("RetireManyEdges(nil): got %+v, want zero-value", res)
	}
}

// TestRetireManyEdges_anyDuplicateAbortsWholeBatch proves the
// atomic semantics on the edge tombstone path: if any id in the
// batch already has a tombstone the whole INSERT rolls back, so
// even the still-valid ids in the batch do not land.
func TestRetireManyEdges_anyDuplicateAbortsWholeBatch(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	svc := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	s := seedRepoAndNode(t, ctx, fix.app, "pkg.EdgeDup.A#m()")
	e1 := seedEdge(t, ctx, s, "pkg.EdgeDup.B#m()").EdgeID
	e2 := seedEdge(t, ctx, s, "pkg.EdgeDup.C#m()").EdgeID
	e3 := seedEdge(t, ctx, s, "pkg.EdgeDup.D#m()").EdgeID

	// Retire e2 by itself.
	if _, err := svc.RetireEdge(ctx, EdgeRetirementInput{
		EdgeID: e2, RetiredAtSHA: "shaFirst",
	}); err != nil {
		t.Fatalf("seed first retirement: %v", err)
	}

	// Now batch-retire [e1, e2, e3]; the e2 duplicate must roll
	// the whole batch back.
	_, err := svc.RetireManyEdges(ctx, []string{e1, e2, e3}, "shaBatch")
	if err == nil {
		t.Fatal("expected AlreadyRetired; got nil")
	}
	var already *AlreadyRetired
	if !errors.As(err, &already) {
		t.Fatalf("err type = %T (%v); want *AlreadyRetired", err, err)
	}
	if already.Kind != KindEdge {
		t.Errorf("Kind = %q, want %q", already.Kind, KindEdge)
	}
	// e1 and e3 must still be unretired (batch is all-or-nothing).
	for _, id := range []string{e1, e3} {
		var n int
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT count(*) FROM edge_retirement WHERE edge_id::text = $1`,
			id,
		).Scan(&n); err != nil {
			t.Fatalf("count for %s: %v", id, err)
		}
		if n != 0 {
			t.Errorf("edge_retirement count for %s = %d, want 0 (batch rolled back)",
				id, n)
		}
	}
}

// bytesEqual is a tiny helper to keep test assertions readable.
// bytes.Equal would do; we avoid the extra import in this file
// since assertions are the only consumer.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
