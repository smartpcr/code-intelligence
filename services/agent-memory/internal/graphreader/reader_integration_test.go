package graphreader_test

// Integration tests exercising the GraphReader library against a
// live PostgreSQL 16 instance. Skips cleanly when
// AGENT_MEMORY_PG_URL is unset, matching the convention in
// migrations/test_migrate_test.go and
// internal/graphwriter/writer_integration_test.go.
//
// Implementation-plan.md Stage 2.2 acceptance scenarios covered:
//
//   * "retired node hidden by default"
//       -> TestGetNode_retiredHiddenByDefault
//       -> TestListEdgesFrom_retiredHiddenByDefault
//       -> TestListNodes_retiredHiddenByDefault
//   * "retired node visible with opt-in"
//       -> TestGetNode_retiredVisibleWithOptIn
//       -> TestGetEdge_retiredVisibleWithOptIn
//       -> TestNeighborhoodCard_retiredEdgesVisibleWithOptIn
//   * "neighborhood card resolves observed_calls"
//       -> TestNeighborhoodCard_resolvesObservedCallsCount42
//
// The tests use a per-test schema (mirrors the writer pattern)
// so they can be run concurrently with the writer integration
// suite without colliding on table names. The reader is wired
// over a `pgxpool.Pool` authenticated as `agent_memory_ro` —
// the same wiring the production binaries use.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/testpglock"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 30 * time.Second
)

// readerFixture is the per-test PostgreSQL substrate for the
// GraphReader integration suite. `owner` is the connection
// that created the schema and ran migrations (it has
// CREATEROLE / superuser privileges and is used by tests to
// seed rows directly via INSERT). `pool` is a pgxpool.Pool
// authenticated as `agent_memory_ro` — the production-shaped
// wiring the GraphReader runs over.
type readerFixture struct {
	owner   *sql.DB
	pool    *pgxpool.Pool
	schema  string
	cleanup func()
}

// openReaderFixture provisions a per-test schema, applies all
// migrations (0001..0017 — the 0017 row creates the
// `agent_memory_ro` role with SELECT-only grants), flips the
// role to LOGIN with a per-test random password gated by the
// cluster-wide advisory lock (`testpglock.AcquireRoRoleLogin`),
// and returns the seed *sql.DB plus a pgxpool.Pool authenticated
// as the reader role.
func openReaderFixture(t *testing.T) *readerFixture {
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

	// Apply every migration so the agent_memory_ro role exists
	// alongside the schema this test owns.
	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	pool, revertRole := openRoRolePool(t, owner, base, schema)

	cleanup := func() {
		pool.Close()
		revertRole()
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		// Mirror the writer fixture: drop partman.part_config
		// rows for this schema's tables to leave cluster
		// state clean.
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &readerFixture{owner: owner, pool: pool, schema: schema, cleanup: cleanup}
}

// openRoRolePool flips agent_memory_ro to LOGIN with a per-test
// random password and returns a *pgxpool.Pool authenticated as
// that role with search_path pinned to the per-test schema. The
// returned cleanup func reverts the role to NOLOGIN. Pattern
// mirrors writer_integration_test.go's openAppRoleDB.
func openRoRolePool(
	t *testing.T, owner *sql.DB, baseURL, schema string,
) (*pgxpool.Pool, func()) {
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
	password := "amreader_" + hex.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Acquire the cross-package advisory lock BEFORE the role
	// flip — see testpglock.AcquireRoRoleLogin docs.
	releaseLock, err := testpglock.AcquireRoRoleLogin(ctx, baseURL)
	if err != nil {
		t.Fatalf("acquire ro-role login lock: %v", err)
	}
	success := false
	defer func() {
		if !success {
			releaseLock()
		}
	}()

	if _, err := owner.ExecContext(ctx,
		`ALTER ROLE agent_memory_ro WITH LOGIN PASSWORD `+pq.QuoteLiteral(password),
	); err != nil {
		t.Fatalf("ALTER ROLE LOGIN: %v", err)
	}
	revert := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		_, _ = owner.ExecContext(ctx2, `ALTER ROLE agent_memory_ro WITH NOLOGIN`)
	}

	u2 := *u
	u2.User = url.UserPassword("agent_memory_ro", password)
	pool, err := graphreader.NewPool(ctx, u2.String(), graphreader.PoolOptions{
		MaxConns:     4,
		MinConns:     1,
		SearchPath:   schema + ", public",
		ExpectedRole: "agent_memory_ro",
	})
	if err != nil {
		revert()
		t.Fatalf("graphreader.NewPool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		revert()
		t.Fatalf("ping ro pool: %v", err)
	}
	success = true
	return pool, func() {
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
	return "amreader_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// seededRow is the column tuple the per-test seed helpers insert
// into `node`. Kept exported-ish (lowercase, package-local) so
// individual scenarios assemble different fixtures cleanly.
type seededRow struct {
	NodeID             string
	RepoID             string
	Kind               string
	CanonicalSignature string
	FromSHA            string
	ParentNodeID       sql.NullString
}

// seedRepo inserts a Repo row and returns its repo_id. The
// reader tests don't go through EnsureRepo because they want to
// be independent of the writer package — owner-role direct
// INSERTs keep the dependency tree shallow.
func seedRepo(t *testing.T, ctx context.Context, owner *sql.DB) string {
	t.Helper()
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	repoURL := "https://example.test/reader/" + hex.EncodeToString(buf[:])
	var repoID string
	err := owner.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ($1, 'main', 'deadbeef')
		RETURNING repo_id::text
	`, repoURL).Scan(&repoID)
	if err != nil {
		t.Fatalf("seedRepo: %v", err)
	}
	return repoID
}

// seedNode inserts a Node row with a random fingerprint and
// returns the populated seededRow (with the generated node_id).
// `repoID` must come from seedRepo. `parentNodeID` may be empty.
func seedNode(
	t *testing.T, ctx context.Context, owner *sql.DB,
	repoID, kind, canonicalSig, fromSHA, parentNodeID string,
) seededRow {
	t.Helper()
	var fp [32]byte
	if _, err := rand.Read(fp[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	row := seededRow{
		RepoID:             repoID,
		Kind:               kind,
		CanonicalSignature: canonicalSig,
		FromSHA:            fromSHA,
	}
	if parentNodeID != "" {
		row.ParentNodeID = sql.NullString{String: parentNodeID, Valid: true}
	}
	err := owner.QueryRowContext(ctx, `
		INSERT INTO node (
			fingerprint, repo_id, kind, canonical_signature,
			parent_node_id, from_sha, attrs_json
		)
		VALUES ($1, $2::uuid, $3::node_kind, $4, $5::uuid, $6, '{}'::jsonb)
		RETURNING node_id::text
	`, fp[:], repoID, kind, canonicalSig, row.ParentNodeID, fromSHA).Scan(&row.NodeID)
	if err != nil {
		t.Fatalf("seedNode: %v", err)
	}
	return row
}

// seedEdge inserts an Edge row with a random fingerprint between
// the two endpoints. Returns the generated edge_id.
func seedEdge(
	t *testing.T, ctx context.Context, owner *sql.DB,
	repoID, kind, srcNodeID, dstNodeID, fromSHA string,
) string {
	t.Helper()
	var fp [32]byte
	if _, err := rand.Read(fp[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	var edgeID string
	err := owner.QueryRowContext(ctx, `
		INSERT INTO edge (
			fingerprint, repo_id, kind, src_node_id, dst_node_id,
			from_sha, attrs_json
		)
		VALUES ($1, $2::uuid, $3::edge_kind, $4::uuid, $5::uuid, $6, '{}'::jsonb)
		RETURNING edge_id::text
	`, fp[:], repoID, kind, srcNodeID, dstNodeID, fromSHA).Scan(&edgeID)
	if err != nil {
		t.Fatalf("seedEdge: %v", err)
	}
	return edgeID
}

// retireNode writes a tombstone for the supplied node_id at
// `retiredAtSHA`. If `supersededByNodeID` is non-empty the
// rename-target column is populated too.
func retireNode(
	t *testing.T, ctx context.Context, owner *sql.DB,
	nodeID, retiredAtSHA, supersededByNodeID string,
) {
	t.Helper()
	var supersede sql.NullString
	if supersededByNodeID != "" {
		supersede = sql.NullString{String: supersededByNodeID, Valid: true}
	}
	_, err := owner.ExecContext(ctx, `
		INSERT INTO node_retirement (node_id, retired_at_sha, superseded_by_node_id)
		VALUES ($1::uuid, $2, $3::uuid)
	`, nodeID, retiredAtSHA, supersede)
	if err != nil {
		t.Fatalf("retireNode: %v", err)
	}
}

// retireEdge writes a tombstone for the supplied edge_id.
func retireEdge(
	t *testing.T, ctx context.Context, owner *sql.DB,
	edgeID, retiredAtSHA string,
) {
	t.Helper()
	_, err := owner.ExecContext(ctx, `
		INSERT INTO edge_retirement (edge_id, retired_at_sha)
		VALUES ($1::uuid, $2)
	`, edgeID, retiredAtSHA)
	if err != nil {
		t.Fatalf("retireEdge: %v", err)
	}
}

// upsertTraceObservation seeds the trace_observation row used by
// the neighborhood card test scenario. Per migration 0005 the
// schema is one-row-per-edge, so an INSERT is sufficient for
// freshly seeded edges.
func upsertTraceObservation(
	t *testing.T, ctx context.Context, owner *sql.DB,
	edgeID string, observationCount int64,
	p50, p95 float64, latestSpanRef string,
) {
	t.Helper()
	_, err := owner.ExecContext(ctx, `
		INSERT INTO trace_observation (
			edge_id, observation_count,
			p50_latency_ms, p95_latency_ms,
			latest_span_ref, last_observed_at
		)
		VALUES ($1::uuid, $2, $3, $4, $5, now())
	`, edgeID, observationCount, p50, p95, latestSpanRef)
	if err != nil {
		t.Fatalf("upsertTraceObservation: %v", err)
	}
}

// ----- Tests --------------------------------------------------------

// TestGetNode_currentVisible is the baseline: a Node with no
// retirement row resolves via GetNode regardless of the
// IncludeRetired flag.
func TestGetNode_currentVisible(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	n := seedNode(t, ctx, fix.owner, repoID, "method", "com.example.Foo#bar()", "sha1", "")

	r := graphreader.New(fix.pool, nil)
	got, err := r.GetNode(ctx, n.NodeID, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("GetNode (default): %v", err)
	}
	if got.NodeID != n.NodeID || got.Kind != "method" || got.CanonicalSignature != "com.example.Foo#bar()" {
		t.Errorf("unexpected Node: %+v", got)
	}
	if got.Retirement != nil {
		t.Errorf("non-retired node should not carry Retirement, got %+v", got.Retirement)
	}

	gotInc, err := r.GetNode(ctx, n.NodeID, graphreader.ReaderOptions{IncludeRetired: true})
	if err != nil {
		t.Fatalf("GetNode (IncludeRetired): %v", err)
	}
	if gotInc.Retirement != nil {
		t.Errorf("non-retired node with IncludeRetired must still have nil Retirement, got %+v", gotInc.Retirement)
	}
}

// TestGetNode_retiredHiddenByDefault is Stage 2.2 scenario #1.
// Given a Node has a node_retirement row, GetNode with the
// default ReaderOptions MUST return ErrNotFound.
func TestGetNode_retiredHiddenByDefault(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	n := seedNode(t, ctx, fix.owner, repoID, "method", "com.example.Foo#removed()", "sha1", "")
	retireNode(t, ctx, fix.owner, n.NodeID, "sha2", "")

	r := graphreader.New(fix.pool, nil)
	_, err := r.GetNode(ctx, n.NodeID, graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("retired node must surface ErrNotFound, got %v", err)
	}
}

// TestGetNode_retiredVisibleWithOptIn is Stage 2.2 scenario #2.
// Given the same retired Node, GetNode with
// ReaderOptions.IncludeRetired = true MUST return the Node AND
// populate Node.Retirement with the tombstone metadata
// (retired_at_sha + retired_at + superseded_by_node_id).
func TestGetNode_retiredVisibleWithOptIn(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	original := seedNode(t, ctx, fix.owner, repoID, "method", "com.example.Foo#renamed()", "sha1", "")
	replacement := seedNode(t, ctx, fix.owner, repoID, "method", "com.example.Foo#newName()", "sha2", "")
	retireNode(t, ctx, fix.owner, original.NodeID, "sha2", replacement.NodeID)

	r := graphreader.New(fix.pool, nil)
	got, err := r.GetNode(ctx, original.NodeID,
		graphreader.ReaderOptions{IncludeRetired: true})
	if err != nil {
		t.Fatalf("GetNode (IncludeRetired): %v", err)
	}
	if got.NodeID != original.NodeID {
		t.Errorf("expected original NodeID %s, got %s", original.NodeID, got.NodeID)
	}
	if got.Retirement == nil {
		t.Fatalf("Retirement must be populated for opt-in retired Node, got nil")
	}
	if got.Retirement.RetiredAtSHA != "sha2" {
		t.Errorf("RetiredAtSHA: want sha2, got %q", got.Retirement.RetiredAtSHA)
	}
	if got.Retirement.SupersededByNodeID != replacement.NodeID {
		t.Errorf("SupersededByNodeID: want %s, got %s",
			replacement.NodeID, got.Retirement.SupersededByNodeID)
	}
	if got.Retirement.RetiredAt.IsZero() {
		t.Errorf("RetiredAt should be non-zero, got %v", got.Retirement.RetiredAt)
	}
}

// TestGetNode_notFoundUnknownID confirms ErrNotFound also covers
// the "never existed" case — collapsing missing + retired onto
// one sentinel is part of the documented contract.
func TestGetNode_notFoundUnknownID(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	r := graphreader.New(fix.pool, nil)
	_, err := r.GetNode(ctx, "00000000-0000-0000-0000-000000000000", graphreader.ReaderOptions{})
	if !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("unknown id must surface ErrNotFound, got %v", err)
	}
}

// TestGetEdge_retiredVisibleWithOptIn is the Edge analogue of
// the GetNode opt-in test: retired Edge surfaces with
// EdgeRetirement metadata.
func TestGetEdge_retiredVisibleWithOptIn(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "src()", "sha1", "")
	dst := seedNode(t, ctx, fix.owner, repoID, "method", "dst()", "sha1", "")
	edgeID := seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, dst.NodeID, "sha1")
	retireEdge(t, ctx, fix.owner, edgeID, "sha2")

	r := graphreader.New(fix.pool, nil)

	// default path: hidden.
	if _, err := r.GetEdge(ctx, edgeID, graphreader.ReaderOptions{}); !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("default GetEdge for retired must return ErrNotFound, got %v", err)
	}

	// opt-in path: visible with metadata.
	got, err := r.GetEdge(ctx, edgeID, graphreader.ReaderOptions{IncludeRetired: true})
	if err != nil {
		t.Fatalf("opt-in GetEdge: %v", err)
	}
	if got.EdgeID != edgeID {
		t.Errorf("EdgeID: want %s, got %s", edgeID, got.EdgeID)
	}
	if got.Retirement == nil {
		t.Fatalf("opt-in retired edge must carry Retirement, got nil")
	}
	if got.Retirement.RetiredAtSHA != "sha2" {
		t.Errorf("RetiredAtSHA: want sha2, got %q", got.Retirement.RetiredAtSHA)
	}
}

// TestListEdgesFrom_retiredHiddenByDefault asserts the anti-join
// filter applies to bulk reads too — a retired Edge MUST NOT
// surface from ListEdgesFrom without IncludeRetired.
func TestListEdgesFrom_retiredHiddenByDefault(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "caller()", "sha1", "")
	d1 := seedNode(t, ctx, fix.owner, repoID, "method", "callee1()", "sha1", "")
	d2 := seedNode(t, ctx, fix.owner, repoID, "method", "callee2()", "sha1", "")
	keepID := seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, d1.NodeID, "sha1")
	dropID := seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, d2.NodeID, "sha1")
	retireEdge(t, ctx, fix.owner, dropID, "sha2")

	r := graphreader.New(fix.pool, nil)
	got, err := r.ListEdgesFrom(ctx, src.NodeID, []string{"static_calls"}, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	if len(got) != 1 || got[0].EdgeID != keepID {
		t.Fatalf("expected only keepID %s, got %+v", keepID, got)
	}

	// opt-in: both surface; the retired one carries metadata.
	inc, err := r.ListEdgesFrom(ctx, src.NodeID, []string{"static_calls"}, graphreader.ReaderOptions{IncludeRetired: true})
	if err != nil {
		t.Fatalf("ListEdgesFrom (opt-in): %v", err)
	}
	if len(inc) != 2 {
		t.Fatalf("opt-in: want 2 edges, got %d", len(inc))
	}
	var retiredCount int
	for _, e := range inc {
		if e.Retirement != nil {
			retiredCount++
			if e.EdgeID != dropID {
				t.Errorf("Retirement attached to wrong edge: want %s, got %s", dropID, e.EdgeID)
			}
		}
	}
	if retiredCount != 1 {
		t.Errorf("expected exactly 1 retired entry, got %d", retiredCount)
	}
}

// TestListNodes_retiredHiddenByDefault is the ListNodes analogue.
func TestListNodes_retiredHiddenByDefault(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	a := seedNode(t, ctx, fix.owner, repoID, "method", "a()", "sha1", "")
	b := seedNode(t, ctx, fix.owner, repoID, "method", "b()", "sha1", "")
	retireNode(t, ctx, fix.owner, b.NodeID, "sha2", "")

	rid, err := fingerprint.ParseRepoID(repoID)
	if err != nil {
		t.Fatalf("parse repo_id: %v", err)
	}
	r := graphreader.New(fix.pool, nil)

	got, err := r.ListNodes(ctx, rid, []string{"method"}, graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != a.NodeID {
		t.Fatalf("expected only %s, got %+v", a.NodeID, got)
	}

	inc, err := r.ListNodes(ctx, rid, []string{"method"}, graphreader.ListNodesFilter{}, graphreader.ReaderOptions{IncludeRetired: true})
	if err != nil {
		t.Fatalf("ListNodes (opt-in): %v", err)
	}
	if len(inc) != 2 {
		t.Fatalf("opt-in: want 2 nodes, got %d", len(inc))
	}
}

// TestNeighborhoodCard_resolvesObservedCallsCount42 is Stage 2.2
// scenario #3 — given a Method Node with one outbound
// observed_calls edge whose trace_observation.observation_count
// is 42, the card MUST list the edge with observation_count = 42.
func TestNeighborhoodCard_resolvesObservedCallsCount42(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "caller()", "sha1", "")
	dst := seedNode(t, ctx, fix.owner, repoID, "method", "callee()", "sha1", "")
	edgeID := seedEdge(t, ctx, fix.owner, repoID, "observed_calls", src.NodeID, dst.NodeID, "sha1")
	upsertTraceObservation(t, ctx, fix.owner, edgeID, 42, 12.5, 41.0, "trace-abc")

	r := graphreader.New(fix.pool, nil)
	card, err := r.NeighborhoodCard(ctx, src.NodeID, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("NeighborhoodCard: %v", err)
	}
	if card.Node.NodeID != src.NodeID {
		t.Errorf("card.Node.NodeID: want %s, got %s", src.NodeID, card.Node.NodeID)
	}
	if len(card.Edges) != 1 {
		t.Fatalf("expected 1 outbound edge, got %d", len(card.Edges))
	}
	ce := card.Edges[0]
	if ce.EdgeID != edgeID {
		t.Errorf("EdgeID: want %s, got %s", edgeID, ce.EdgeID)
	}
	if ce.Kind != "observed_calls" {
		t.Errorf("Kind: want observed_calls, got %s", ce.Kind)
	}
	if ce.TraceObservation == nil {
		t.Fatalf("TraceObservation must be populated, got nil")
	}
	if ce.TraceObservation.ObservationCount != 42 {
		t.Errorf("ObservationCount: want 42, got %d", ce.TraceObservation.ObservationCount)
	}
	if ce.TraceObservation.LatestSpanRef != "trace-abc" {
		t.Errorf("LatestSpanRef: want trace-abc, got %q", ce.TraceObservation.LatestSpanRef)
	}
	if ce.TraceObservation.P50LatencyMs != 12.5 {
		t.Errorf("P50LatencyMs: want 12.5, got %v", ce.TraceObservation.P50LatencyMs)
	}
}

// TestNeighborhoodCard_edgeWithoutObservationHasNilAggregate
// asserts the LEFT JOIN semantics: a static_calls edge with no
// trace_observation row surfaces with CardEdge.TraceObservation =
// nil rather than a zero-value struct. The distinction lets
// callers render "never observed" vs. "observed zero times".
func TestNeighborhoodCard_edgeWithoutObservationHasNilAggregate(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "caller()", "sha1", "")
	dst := seedNode(t, ctx, fix.owner, repoID, "method", "callee()", "sha1", "")
	_ = seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, dst.NodeID, "sha1")

	r := graphreader.New(fix.pool, nil)
	card, err := r.NeighborhoodCard(ctx, src.NodeID, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("NeighborhoodCard: %v", err)
	}
	if len(card.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(card.Edges))
	}
	if card.Edges[0].TraceObservation != nil {
		t.Errorf("edge without trace_observation must have nil aggregate, got %+v", card.Edges[0].TraceObservation)
	}
}

// TestNeighborhoodCard_retiredNodeHiddenByDefault asserts the
// seed-Node retirement check shares semantics with GetNode:
// the call surfaces ErrNotFound rather than returning a card
// whose Node is missing.
func TestNeighborhoodCard_retiredNodeHiddenByDefault(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "removed()", "sha1", "")
	retireNode(t, ctx, fix.owner, src.NodeID, "sha2", "")

	r := graphreader.New(fix.pool, nil)
	if _, err := r.NeighborhoodCard(ctx, src.NodeID, graphreader.ReaderOptions{}); !errors.Is(err, graphreader.ErrNotFound) {
		t.Fatalf("retired seed Node must surface ErrNotFound, got %v", err)
	}
}

// TestNeighborhoodCard_retiredEdgesVisibleWithOptIn confirms
// that with IncludeRetired = true the card returns retired
// outbound Edges alongside current ones, each tagged with its
// EdgeRetirement metadata.
func TestNeighborhoodCard_retiredEdgesVisibleWithOptIn(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "caller()", "sha1", "")
	d1 := seedNode(t, ctx, fix.owner, repoID, "method", "kept()", "sha1", "")
	d2 := seedNode(t, ctx, fix.owner, repoID, "method", "dropped()", "sha1", "")
	keepID := seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, d1.NodeID, "sha1")
	dropID := seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, d2.NodeID, "sha1")
	retireEdge(t, ctx, fix.owner, dropID, "sha2")

	r := graphreader.New(fix.pool, nil)
	card, err := r.NeighborhoodCard(ctx, src.NodeID, graphreader.ReaderOptions{IncludeRetired: true})
	if err != nil {
		t.Fatalf("NeighborhoodCard (opt-in): %v", err)
	}
	if len(card.Edges) != 2 {
		t.Fatalf("opt-in card: want 2 edges, got %d", len(card.Edges))
	}
	var sawKept, sawDropped bool
	for _, ce := range card.Edges {
		switch ce.EdgeID {
		case keepID:
			sawKept = true
			if ce.Retirement != nil {
				t.Errorf("kept edge must have nil Retirement, got %+v", ce.Retirement)
			}
		case dropID:
			sawDropped = true
			if ce.Retirement == nil {
				t.Fatalf("dropped edge must carry Retirement, got nil")
			}
			if ce.Retirement.RetiredAtSHA != "sha2" {
				t.Errorf("RetiredAtSHA: want sha2, got %q", ce.Retirement.RetiredAtSHA)
			}
		}
	}
	if !sawKept || !sawDropped {
		t.Errorf("expected both edges in result, sawKept=%v sawDropped=%v", sawKept, sawDropped)
	}
}

// TestListEdgesFrom_kindFilter exercises the ANY($N::text[])
// branch — a method with one static_calls edge and one
// observed_calls edge returns just the static_calls when the
// kinds filter restricts to that kind.
func TestListEdgesFrom_kindFilter(t *testing.T) {
	fix := openReaderFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID := seedRepo(t, ctx, fix.owner)
	src := seedNode(t, ctx, fix.owner, repoID, "method", "caller()", "sha1", "")
	d1 := seedNode(t, ctx, fix.owner, repoID, "method", "c1()", "sha1", "")
	d2 := seedNode(t, ctx, fix.owner, repoID, "method", "c2()", "sha1", "")
	staticEdge := seedEdge(t, ctx, fix.owner, repoID, "static_calls", src.NodeID, d1.NodeID, "sha1")
	_ = seedEdge(t, ctx, fix.owner, repoID, "observed_calls", src.NodeID, d2.NodeID, "sha1")

	r := graphreader.New(fix.pool, nil)
	got, err := r.ListEdgesFrom(ctx, src.NodeID, []string{"static_calls"}, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom: %v", err)
	}
	if len(got) != 1 || got[0].EdgeID != staticEdge {
		t.Fatalf("kind filter: want [%s], got %+v", staticEdge, got)
	}

	all, err := r.ListEdgesFrom(ctx, src.NodeID, nil, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListEdgesFrom (no filter): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("no filter: want 2 edges, got %d", len(all))
	}
}

// (No additional helpers needed below.)
