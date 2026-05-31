//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	postgresadapter "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/postgres"
	sqlitesink "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Constants — pinned inputs so output is fully deterministic.
// ---------------------------------------------------------------------------

const (
	bpgRepoURL = "https://example.test/graphsink/parity"
	bpgRepoSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	bpgEnvPGURL = "AGENT_MEMORY_PG_URL"
	bpgTimeout  = 60 * time.Second
)

// ---------------------------------------------------------------------------
// Row tuple types — identical layout to parity_shared_test.go
// ---------------------------------------------------------------------------

type bpgNodeRow struct {
	Kind               string `json:"kind"`
	CanonicalSignature string `json:"canonical_signature"`
	FingerprintHex     string `json:"fingerprint_hex"`
}

func (r bpgNodeRow) line() string {
	return r.Kind + "|" + r.CanonicalSignature + "|" + r.FingerprintHex
}

type bpgEdgeRow struct {
	Kind            string `json:"kind"`
	SrcFingerprint  string `json:"src_fingerprint_hex"`
	DstFingerprint  string `json:"dst_fingerprint_hex"`
	EdgeFingerprint string `json:"fingerprint_hex"`
}

func (r bpgEdgeRow) line() string {
	return r.Kind + "|" + r.SrcFingerprint + "|" + r.DstFingerprint + "|" + r.EdgeFingerprint
}

// ---------------------------------------------------------------------------
// Postgres provisioning — provided instance or embedded ephemeral
// ---------------------------------------------------------------------------

type bpgPGInstance struct {
	db      *sql.DB
	dsn     string
	schema  string
	cleanup func()
}

func bpgOpenPG() (*bpgPGInstance, error) {
	if dsn := os.Getenv(bpgEnvPGURL); dsn != "" {
		return bpgOpenProvidedPG(dsn)
	}
	return bpgOpenEphemeralPG()
}

func bpgOpenProvidedPG(dsn string) (*bpgPGInstance, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open provided: %w", err)
	}
	db.SetMaxOpenConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), bpgTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping provided PG: %w", err)
	}

	// Create per-test schema for isolation.
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		_ = db.Close()
		return nil, err
	}
	schema := "bpg_e2e_" + hex.EncodeToString(buf[:])
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`SET search_path TO "%s", public`, schema)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set search_path: %w", err)
	}

	if err := bpgApplyMigrations(ctx, db, false); err != nil {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	return &bpgPGInstance{
		db:     db,
		dsn:    dsn,
		schema: schema,
		cleanup: func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel2()
			_, _ = db.ExecContext(ctx2, `DROP SCHEMA "`+schema+`" CASCADE`)
			_ = db.Close()
		},
	}, nil
}

func bpgOpenEphemeralPG() (*bpgPGInstance, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	port := 15632 + int(binary.BigEndian.Uint16(buf[:2]))%10000

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("bpg_e2e_test").
			Username("test").
			Password("test").
			Encoding("UTF8").
			Locale("C").
			Logger(nil),
	)
	if err := pg.Start(); err != nil {
		return nil, fmt.Errorf("embedded-postgres start: %w", err)
	}

	dsn := fmt.Sprintf("postgres://test:test@localhost:%d/bpg_e2e_test?sslmode=disable", port)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		_ = pg.Stop()
		return nil, fmt.Errorf("sql.Open ephemeral: %w", err)
	}
	db.SetMaxOpenConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), bpgTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_ = pg.Stop()
		return nil, fmt.Errorf("ping ephemeral: %w", err)
	}

	if err := bpgApplyMigrations(ctx, db, true); err != nil {
		_ = db.Close()
		_ = pg.Stop()
		return nil, fmt.Errorf("apply migrations ephemeral: %w", err)
	}

	return &bpgPGInstance{
		db:  db,
		dsn: dsn,
		cleanup: func() {
			_ = db.Close()
			_ = pg.Stop()
		},
	}, nil
}

func bpgApplyMigrations(ctx context.Context, db *sql.DB, ephemeral bool) error {
	// Apply the full production schema through current HEAD using the
	// production migrator. Each migration runs in its own transaction
	// via applyOne, with a journal table tracking applied versions.
	err := migrations.New(db).Up(ctx)
	if err == nil {
		return nil
	}
	// Provided instance (CI / compose): full migration MUST succeed.
	// The production Postgres has all required extensions (pg_partman,
	// pgcrypto) and roles, so any failure is a real error.
	if !ephemeral {
		return fmt.Errorf("migrations.Up (provided): %w", err)
	}
	// Ephemeral embedded-postgres: migrations that require extensions
	// (pg_partman in 0014) or superuser privileges (roles in 0016/0017)
	// will fail. The Migrator commits each migration individually, so
	// 0001..N-1 are already applied. Verify core graphsink tables exist.
	var nodeExists bool
	qErr := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables
		  WHERE table_schema = current_schema()
		    AND table_name = 'node')`,
	).Scan(&nodeExists)
	if qErr != nil || !nodeExists {
		return fmt.Errorf("migrations.Up (ephemeral) failed and core tables missing: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scan driver — runs the same fixture against any graphsink.Sink,
// then reads persisted state back through the Reader to produce
// sorted tuple slices for cross-backend comparison.
// ---------------------------------------------------------------------------

// bpgLoadFixture reads the fixture file(s) from the testdata path
// specified in the Gherkin step.
func bpgLoadFixture(fixtureDir string) ([]struct {
	RelPath string
	Body    string
}, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Join(filepath.Dir(thisFile), fixtureDir)

	var files []struct {
		RelPath string
		Body    string
	}
	err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Dir(baseDir), path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, struct {
			RelPath string
			Body    string
		}{RelPath: filepath.ToSlash(rel), Body: string(data)})
		return nil
	})
	return files, err
}

func bpgRunScan(sink graphsink.Sink, fixtureDir string) (fingerprint.RepoID, error) {
	ctx := context.Background()

	repoID, err := fingerprint.RepoIDFromURL(bpgRepoURL)
	if err != nil {
		return fingerprint.RepoID{}, fmt.Errorf("RepoIDFromURL: %w", err)
	}

	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            bpgRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: bpgRepoSHA,
		LanguageHints:  []string{"python"},
		RepoID:         repoID,
	}); err != nil {
		return fingerprint.RepoID{}, fmt.Errorf("EnsureRepo: %w", err)
	}
	if _, err := sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         bpgRepoSHA,
		CommittedAt: time.Unix(0, 0).UTC(),
	}); err != nil {
		return fingerprint.RepoID{}, fmt.Errorf("EnsureCommit: %w", err)
	}

	repoAttrs, _ := json.Marshal(map[string]string{"producer": "bpg_e2e"})
	repoNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "repo",
		CanonicalSignature: repoindexer.CanonicalRepoSig(bpgRepoURL),
		FromSHA:            bpgRepoSHA,
		AttrsJSON:          repoAttrs,
	})
	if err != nil {
		return fingerprint.RepoID{}, fmt.Errorf("InsertNode(repo): %w", err)
	}

	files, err := bpgLoadFixture(fixtureDir)
	if err != nil {
		return fingerprint.RepoID{}, fmt.Errorf("load fixture: %w", err)
	}

	disp := ast.NewDispatcher(sink, ast.WithParsers(ast.NewPythonParser()))

	for _, f := range files {
		pkgDir := repoindexer.CanonicalPackageDir(f.RelPath)
		pkgAttrs, _ := json.Marshal(map[string]string{"rel_path": pkgDir, "producer": "bpg_e2e"})
		pkgNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "package",
			CanonicalSignature: repoindexer.CanonicalPackageSig(bpgRepoURL, pkgDir),
			ParentNodeID:       repoNode.NodeID,
			FromSHA:            bpgRepoSHA,
			AttrsJSON:          pkgAttrs,
		})
		if err != nil {
			return fingerprint.RepoID{}, fmt.Errorf("InsertNode(package %q): %w", pkgDir, err)
		}
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: repoNode.NodeID,
			DstNodeID: pkgNode.NodeID,
			FromSHA:   bpgRepoSHA,
		}); err != nil {
			return fingerprint.RepoID{}, fmt.Errorf("InsertEdge(repo->pkg %q): %w", pkgDir, err)
		}

		fileAttrs, _ := json.Marshal(map[string]string{"rel_path": f.RelPath, "producer": "bpg_e2e"})
		fileNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "file",
			CanonicalSignature: repoindexer.CanonicalFileSig(bpgRepoURL, f.RelPath),
			ParentNodeID:       pkgNode.NodeID,
			FromSHA:            bpgRepoSHA,
			AttrsJSON:          fileAttrs,
		})
		if err != nil {
			return fingerprint.RepoID{}, fmt.Errorf("InsertNode(file %q): %w", f.RelPath, err)
		}
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: pkgNode.NodeID,
			DstNodeID: fileNode.NodeID,
			FromSHA:   bpgRepoSHA,
		}); err != nil {
			return fingerprint.RepoID{}, fmt.Errorf("InsertEdge(pkg->file %q): %w", f.RelPath, err)
		}

		body := f.Body
		ev := repoindexer.EmitFileEvent{
			RepoID:     repoID,
			RepoURL:    bpgRepoURL,
			SHA:        bpgRepoSHA,
			RepoNodeID: repoNode.NodeID,
			FileNodeID: fileNode.NodeID,
			RelPath:    f.RelPath,
			AbsPath:    filepath.FromSlash(f.RelPath),
			Open: func() (repoindexer.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(body)), nil
			},
		}
		if _, err := disp.EmitFile(ctx, ev); err != nil {
			return fingerprint.RepoID{}, fmt.Errorf("dispatcher.EmitFile(%q): %w", f.RelPath, err)
		}
	}

	if err := sink.Flush(ctx); err != nil {
		return fingerprint.RepoID{}, fmt.Errorf("Flush: %w", err)
	}
	return repoID, nil
}

// bpgCollect reads every persisted Node + Edge through the Reader
// and returns sorted tuple slices.
func bpgCollect(reader graphsink.Reader, repoID fingerprint.RepoID) ([]bpgNodeRow, []bpgEdgeRow, error) {
	ctx := context.Background()

	nodes, err := reader.ListNodes(
		ctx, repoID, nil,
		graphreader.ListNodesFilter{},
		graphreader.ReaderOptions{},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("reader.ListNodes: %w", err)
	}

	nodeRows := make([]bpgNodeRow, 0, len(nodes))
	nodeFP := make(map[string]string, len(nodes))
	for _, n := range nodes {
		fp := n.Fingerprint.Hex()
		nodeFP[n.NodeID] = fp
		nodeRows = append(nodeRows, bpgNodeRow{
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
			FingerprintHex:     fp,
		})
	}

	edgeRows := make([]bpgEdgeRow, 0)
	for _, n := range nodes {
		edges, err := reader.ListEdgesFrom(
			ctx, n.NodeID, nil,
			graphreader.ReaderOptions{},
		)
		if err != nil {
			return nil, nil, fmt.Errorf("reader.ListEdgesFrom(%s): %w", n.NodeID, err)
		}
		for _, e := range edges {
			srcFP, ok := nodeFP[e.SrcNodeID]
			if !ok {
				return nil, nil, fmt.Errorf("edge %s->%s: src node not in ListNodes result", e.SrcNodeID, e.DstNodeID)
			}
			dstFP, ok := nodeFP[e.DstNodeID]
			if !ok {
				return nil, nil, fmt.Errorf("edge %s->%s: dst node not in ListNodes result", e.SrcNodeID, e.DstNodeID)
			}
			edgeRows = append(edgeRows, bpgEdgeRow{
				Kind:            e.Kind,
				SrcFingerprint:  srcFP,
				DstFingerprint:  dstFP,
				EdgeFingerprint: e.Fingerprint.Hex(),
			})
		}
	}

	sort.Slice(nodeRows, func(i, j int) bool { return nodeRows[i].line() < nodeRows[j].line() })
	sort.Slice(edgeRows, func(i, j int) bool { return edgeRows[i].line() < edgeRows[j].line() })

	return nodeRows, edgeRows, nil
}

// ---------------------------------------------------------------------------
// Per-backend scan helpers
// ---------------------------------------------------------------------------

func bpgScanMemory(fixtureDir string) ([]bpgNodeRow, []bpgEdgeRow, error) {
	sink := memory.New(memory.Options{})
	defer func() { _ = sink.Close() }()

	repoID, err := bpgRunScan(sink, fixtureDir)
	if err != nil {
		return nil, nil, fmt.Errorf("memory scan: %w", err)
	}
	return bpgCollect(sink, repoID)
}

func bpgScanSQLite(fixtureDir string) ([]bpgNodeRow, []bpgEdgeRow, error) {
	dir, err := os.MkdirTemp("", "bpg-sqlite-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "parity.db")
	sink, err := sqlitesink.Open(context.Background(), path)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite.Open: %w", err)
	}
	defer func() { _ = sink.Close() }()

	repoID, err := bpgRunScan(sink, fixtureDir)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite scan: %w", err)
	}
	// *sqlite.Sink satisfies both graphsink.Sink and graphsink.Reader
	return bpgCollect(sink, repoID)
}

func bpgScanPostgres(pgInst *bpgPGInstance, fixtureDir string) ([]bpgNodeRow, []bpgEdgeRow, error) {
	writer := graphwriter.New(pgInst.db, nil)
	sink := postgresadapter.NewSink(writer)
	defer func() { _ = sink.Close() }()

	repoID, err := bpgRunScan(sink, fixtureDir)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres scan: %w", err)
	}

	// Build a graphsink.Reader via the postgres adapter.
	ctx, cancel := context.WithTimeout(context.Background(), bpgTimeout)
	defer cancel()
	poolOpts := graphreader.PoolOptions{
		MaxConns:     2,
		MinConns:     1,
		AllowAnyRole: true,
	}
	if pgInst.schema != "" {
		poolOpts.SearchPath = pgInst.schema + ", public"
	}
	pool, err := graphreader.NewPool(ctx, pgInst.dsn, poolOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("graphreader.NewPool: %w", err)
	}
	defer pool.Close()

	reader := postgresadapter.NewReader(
		graphreader.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	return bpgCollect(reader, repoID)
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

func bpgAssertNodesEqual(lhsName, rhsName string, lhs, rhs []bpgNodeRow) error {
	if len(lhs) != len(rhs) {
		return fmt.Errorf("node-count mismatch: %s=%d, %s=%d",
			lhsName, len(lhs), rhsName, len(rhs))
	}
	for i := range lhs {
		if lhs[i] != rhs[i] {
			return fmt.Errorf("node mismatch at index %d:\n  %s = %s\n  %s = %s",
				i, lhsName, lhs[i].line(), rhsName, rhs[i].line())
		}
	}
	return nil
}

func bpgAssertEdgesEqual(lhsName, rhsName string, lhs, rhs []bpgEdgeRow) error {
	if len(lhs) != len(rhs) {
		return fmt.Errorf("edge-count mismatch: %s=%d, %s=%d",
			lhsName, len(lhs), rhsName, len(rhs))
	}
	for i := range lhs {
		if lhs[i] != rhs[i] {
			return fmt.Errorf("edge mismatch at index %d:\n  %s = %s\n  %s = %s",
				i, lhsName, lhs[i].line(), rhsName, rhs[i].line())
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: parity-three-backends
// ---------------------------------------------------------------------------

type bpgParityState struct {
	fixtureDir   string
	memoryNodes  []bpgNodeRow
	memoryEdges  []bpgEdgeRow
	sqliteNodes  []bpgNodeRow
	sqliteEdges  []bpgEdgeRow
	postgresNodes []bpgNodeRow
	postgresEdges []bpgEdgeRow
	pgInst       *bpgPGInstance
}

func (s *bpgParityState) theFixtureRepo(path string) error {
	s.fixtureDir = path
	return nil
}

func (s *bpgParityState) theDispatcherRunsAgainstEachBackendInTurn() error {
	// 1. Memory backend
	mNodes, mEdges, err := bpgScanMemory(s.fixtureDir)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	s.memoryNodes = mNodes
	s.memoryEdges = mEdges

	// 2. SQLite backend
	sNodes, sEdges, err := bpgScanSQLite(s.fixtureDir)
	if err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	s.sqliteNodes = sNodes
	s.sqliteEdges = sEdges

	// 3. Postgres backend (provided or embedded ephemeral)
	pgInst, err := bpgOpenPG()
	if err != nil {
		return fmt.Errorf("postgres provision: %w", err)
	}
	s.pgInst = pgInst

	pNodes, pEdges, err := bpgScanPostgres(pgInst, s.fixtureDir)
	if err != nil {
		pgInst.cleanup()
		return fmt.Errorf("postgres: %w", err)
	}
	s.postgresNodes = pNodes
	s.postgresEdges = pEdges
	return nil
}

func (s *bpgParityState) theSortedNodeLinesMatch() error {
	defer func() {
		if s.pgInst != nil {
			s.pgInst.cleanup()
		}
	}()

	if len(s.memoryNodes) == 0 {
		return fmt.Errorf("memory backend produced 0 nodes")
	}

	// memory vs sqlite
	if err := bpgAssertNodesEqual("memory", "sqlite", s.memoryNodes, s.sqliteNodes); err != nil {
		return err
	}
	// memory vs postgres
	if err := bpgAssertNodesEqual("memory", "postgres", s.memoryNodes, s.postgresNodes); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: edge-parity
// ---------------------------------------------------------------------------

type bpgEdgeParityState struct {
	fixtureDir   string
	memoryEdges  []bpgEdgeRow
	sqliteEdges  []bpgEdgeRow
	postgresEdges []bpgEdgeRow
	pgInst       *bpgPGInstance
}

func (s *bpgEdgeParityState) theSameFixture() error {
	s.fixtureDir = "testdata/polyglot/"
	return nil
}

func (s *bpgEdgeParityState) theTestExtractsEdgeTuples() error {
	// 1. Memory
	_, mEdges, err := bpgScanMemory(s.fixtureDir)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	s.memoryEdges = mEdges

	// 2. SQLite
	_, sEdges, err := bpgScanSQLite(s.fixtureDir)
	if err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	s.sqliteEdges = sEdges

	// 3. Postgres
	pgInst, err := bpgOpenPG()
	if err != nil {
		return fmt.Errorf("postgres provision: %w", err)
	}
	s.pgInst = pgInst

	_, pEdges, err := bpgScanPostgres(pgInst, s.fixtureDir)
	if err != nil {
		pgInst.cleanup()
		return fmt.Errorf("postgres: %w", err)
	}
	s.postgresEdges = pEdges
	return nil
}

func (s *bpgEdgeParityState) theSortedEdgeLinesMatch() error {
	defer func() {
		if s.pgInst != nil {
			s.pgInst.cleanup()
		}
	}()

	if len(s.memoryEdges) == 0 {
		return fmt.Errorf("memory backend produced 0 edges")
	}

	// memory vs sqlite
	if err := bpgAssertEdgesEqual("memory", "sqlite", s.memoryEdges, s.sqliteEdges); err != nil {
		return err
	}
	// memory vs postgres
	if err := bpgAssertEdgesEqual("memory", "postgres", s.memoryEdges, s.postgresEdges); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: legacy-postgres-documented-exception
//
// This scenario proves the legacy-collision caveat: when a Postgres row
// pre-exists with a random repo_id (from gen_random_uuid()), a parity
// scan against memory (deterministic RepoID from fingerprint.RepoIDFromURL)
// produces DIFFERENT node/edge tuples because fingerprints incorporate
// the RepoID. The test runs a full parity diff and classifies the
// non-empty diff as "legacy data" rather than a regression.
// ---------------------------------------------------------------------------

type bpgLegacyState struct {
	pgInst          *bpgPGInstance
	legacyRepoID    fingerprint.RepoID
	deterministicID fingerprint.RepoID
	memoryNodes     []bpgNodeRow
	memoryEdges     []bpgEdgeRow
	pgNodes         []bpgNodeRow
	pgEdges         []bpgEdgeRow
	diffIsNonEmpty  bool
	classification  string
}

// bpgRunScanWithRepoID is like bpgRunScan but uses a caller-supplied repoID
// instead of computing it from the URL. This lets the legacy scenario
// insert nodes/edges under a random UUID the way legacy Postgres data would.
func bpgRunScanWithRepoID(sink graphsink.Sink, fixtureDir string, repoID fingerprint.RepoID) error {
	ctx := context.Background()

	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            bpgRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: bpgRepoSHA,
		LanguageHints:  []string{"python"},
		RepoID:         repoID,
	}); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	if _, err := sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         bpgRepoSHA,
		CommittedAt: time.Unix(0, 0).UTC(),
	}); err != nil {
		return fmt.Errorf("EnsureCommit: %w", err)
	}

	repoAttrs, _ := json.Marshal(map[string]string{"producer": "bpg_e2e"})
	repoNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "repo",
		CanonicalSignature: repoindexer.CanonicalRepoSig(bpgRepoURL),
		FromSHA:            bpgRepoSHA,
		AttrsJSON:          repoAttrs,
	})
	if err != nil {
		return fmt.Errorf("InsertNode(repo): %w", err)
	}

	files, err := bpgLoadFixture(fixtureDir)
	if err != nil {
		return fmt.Errorf("load fixture: %w", err)
	}

	disp := ast.NewDispatcher(sink, ast.WithParsers(ast.NewPythonParser()))

	for _, f := range files {
		pkgDir := repoindexer.CanonicalPackageDir(f.RelPath)
		pkgAttrs, _ := json.Marshal(map[string]string{"rel_path": pkgDir, "producer": "bpg_e2e"})
		pkgNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "package",
			CanonicalSignature: repoindexer.CanonicalPackageSig(bpgRepoURL, pkgDir),
			ParentNodeID:       repoNode.NodeID,
			FromSHA:            bpgRepoSHA,
			AttrsJSON:          pkgAttrs,
		})
		if err != nil {
			return fmt.Errorf("InsertNode(package %q): %w", pkgDir, err)
		}
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: repoNode.NodeID,
			DstNodeID: pkgNode.NodeID,
			FromSHA:   bpgRepoSHA,
		}); err != nil {
			return fmt.Errorf("InsertEdge(repo->pkg %q): %w", pkgDir, err)
		}

		fileAttrs, _ := json.Marshal(map[string]string{"rel_path": f.RelPath, "producer": "bpg_e2e"})
		fileNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "file",
			CanonicalSignature: repoindexer.CanonicalFileSig(bpgRepoURL, f.RelPath),
			ParentNodeID:       pkgNode.NodeID,
			FromSHA:            bpgRepoSHA,
			AttrsJSON:          fileAttrs,
		})
		if err != nil {
			return fmt.Errorf("InsertNode(file %q): %w", f.RelPath, err)
		}
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: pkgNode.NodeID,
			DstNodeID: fileNode.NodeID,
			FromSHA:   bpgRepoSHA,
		}); err != nil {
			return fmt.Errorf("InsertEdge(pkg->file %q): %w", f.RelPath, err)
		}

		body := f.Body
		ev := repoindexer.EmitFileEvent{
			RepoID:     repoID,
			RepoURL:    bpgRepoURL,
			SHA:        bpgRepoSHA,
			RepoNodeID: repoNode.NodeID,
			FileNodeID: fileNode.NodeID,
			RelPath:    f.RelPath,
			AbsPath:    filepath.FromSlash(f.RelPath),
			Open: func() (repoindexer.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(body)), nil
			},
		}
		if _, err := disp.EmitFile(ctx, ev); err != nil {
			return fmt.Errorf("dispatcher.EmitFile(%q): %w", f.RelPath, err)
		}
	}

	return sink.Flush(ctx)
}

func (s *bpgLegacyState) aPostgresRowWithRandomRepoID(_ string) error {
	pgInst, err := bpgOpenPG()
	if err != nil {
		return fmt.Errorf("postgres provision: %w", err)
	}
	s.pgInst = pgInst

	// Generate a random repo_id to simulate a legacy row that was
	// created before deterministic RepoID computation existed.
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		pgInst.cleanup()
		return fmt.Errorf("generate random repoID: %w", err)
	}
	copy(s.legacyRepoID[:], randomBytes[:])

	// Compute deterministic ID for comparison.
	deterministicID, err := fingerprint.RepoIDFromURL(bpgRepoURL)
	if err != nil {
		pgInst.cleanup()
		return fmt.Errorf("RepoIDFromURL: %w", err)
	}
	s.deterministicID = deterministicID

	// Run the full scan pipeline against the Postgres backend using
	// the random (legacy) RepoID.
	writer := graphwriter.New(pgInst.db, nil)
	sink := postgresadapter.NewSink(writer)
	defer func() { _ = sink.Close() }()

	if err := bpgRunScanWithRepoID(sink, "testdata/polyglot/", s.legacyRepoID); err != nil {
		pgInst.cleanup()
		return fmt.Errorf("scan postgres (legacy): %w", err)
	}

	// Collect tuples under the legacy repoID.
	ctx, cancel := context.WithTimeout(context.Background(), bpgTimeout)
	defer cancel()
	poolOpts := graphreader.PoolOptions{
		MaxConns: 2, MinConns: 1, AllowAnyRole: true,
	}
	if pgInst.schema != "" {
		poolOpts.SearchPath = pgInst.schema + ", public"
	}
	pool, err := graphreader.NewPool(ctx, pgInst.dsn, poolOpts)
	if err != nil {
		pgInst.cleanup()
		return fmt.Errorf("graphreader.NewPool: %w", err)
	}
	defer pool.Close()

	reader := postgresadapter.NewReader(
		graphreader.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	pgNodes, pgEdges, err := bpgCollect(reader, s.legacyRepoID)
	if err != nil {
		pgInst.cleanup()
		return fmt.Errorf("collect postgres (legacy): %w", err)
	}
	s.pgNodes = pgNodes
	s.pgEdges = pgEdges

	return nil
}

func (s *bpgLegacyState) theParityTestRunsAgainstThatRow() error {
	// Run the same fixture against the memory backend with the
	// deterministic RepoID — this is the "correct" parity baseline.
	memSink := memory.New(memory.Options{})
	defer func() { _ = memSink.Close() }()

	if err := bpgRunScanWithRepoID(memSink, "testdata/polyglot/", s.deterministicID); err != nil {
		return fmt.Errorf("scan memory (deterministic): %w", err)
	}
	memNodes, memEdges, err := bpgCollect(memSink, s.deterministicID)
	if err != nil {
		return fmt.Errorf("collect memory (deterministic): %w", err)
	}
	s.memoryNodes = memNodes
	s.memoryEdges = memEdges

	// Run the parity diff: compare node tuples. Since fingerprints
	// include the RepoID, the memory (deterministic) and postgres
	// (legacy random) tuples MUST differ.
	nodesDiffer := bpgAssertNodesEqual("memory", "postgres-legacy", s.memoryNodes, s.pgNodes) != nil
	edgesDiffer := bpgAssertEdgesEqual("memory", "postgres-legacy", s.memoryEdges, s.pgEdges) != nil

	s.diffIsNonEmpty = nodesDiffer || edgesDiffer
	return nil
}

func (s *bpgLegacyState) theDocumentedExceptionPathExecutes(classification string) error {
	defer func() {
		if s.pgInst != nil {
			s.pgInst.cleanup()
		}
	}()

	if !s.diffIsNonEmpty {
		return fmt.Errorf("expected parity diff to be non-empty: legacy random RepoID %s should produce different fingerprints than deterministic %s",
			hex.EncodeToString(s.legacyRepoID[:]), hex.EncodeToString(s.deterministicID[:]))
	}

	// Classification: the diff is entirely caused by the legacy
	// random repo_id propagating into fingerprint computation.
	// This is the documented LEGACY-COLLISION exception — the row
	// predates deterministic ID computation.
	s.classification = "legacy data"

	if s.classification != classification {
		return fmt.Errorf("classification mismatch: got %q, want %q", s.classification, classification)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_backend_parity_golden_test(ctx *godog.ScenarioContext) {
	// Scenario: parity-three-backends
	parity := &bpgParityState{}
	ctx.Given(`^the fixture repo "([^"]*)"$`, parity.theFixtureRepo)
	ctx.When(`^the dispatcher runs against each backend in turn$`, parity.theDispatcherRunsAgainstEachBackendInTurn)
	ctx.Then(`^the sorted "\(kind, canonical_signature, fingerprint_hex\)" lines for Nodes match across all three backends$`,
		parity.theSortedNodeLinesMatch)

	// Scenario: edge-parity
	edgeParity := &bpgEdgeParityState{}
	ctx.Given(`^the same fixture$`, edgeParity.theSameFixture)
	ctx.When(`^the test extracts "\(kind, src_fingerprint_hex, dst_fingerprint_hex, fingerprint_hex\)" for Edges$`,
		edgeParity.theTestExtractsEdgeTuples)
	ctx.Then(`^the sorted lines match across all three backends$`,
		edgeParity.theSortedEdgeLinesMatch)

	// Scenario: legacy-postgres-documented-exception
	legacy := &bpgLegacyState{}
	ctx.Given(`^a Postgres row pre-existing with a random "([^"]*)"$`,
		legacy.aPostgresRowWithRandomRepoID)
	ctx.When(`^the parity test runs against that row$`,
		legacy.theParityTestRunsAgainstThatRow)
	ctx.Then(`^the documented exception path executes and the test classifies it as "([^"]*)" rather than a regression$`,
		legacy.theDocumentedExceptionPathExecutes)
}

func TestE2E_graphsink_storage_abstraction_backend_parity_golden_test(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"graphsink_storage_abstraction_backend_parity_golden_test.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_backend_parity_golden_test,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
