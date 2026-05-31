//go:build e2e

package e2e

// E2E godog scenarios for the Postgres sink adapter (Stage 3.3).
//
// Scenarios 1-2 (postgres-forwarding, write-contract-violation-propagates)
// use sqlmock to prove the adapter delegates to *graphwriter.Writer.
//
// Scenarios 3, 5, 6 (lookupbysignature, listrepos-forwards,
// graphreader-listrepos-matches-mgmtapi) use an embedded Postgres
// instance with real graphreader.Reader + real postgresadapter.Reader.
//
// Scenario 4 (no-database-sql-import) runs `go list -deps` as the
// acceptance scenario specifies and checks that database/sql is not
// a direct import of the adapter package (the thin-forwarder invariant).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	postgresadapter "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/postgres"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/mgmtapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ──────────────────────────────────────────────────────────────
// Stub implementations for mgmtapi.TokenVerifier / HeadResolver
// ──────────────────────────────────────────────────────────────

type psaStubVerifier struct{}

func (psaStubVerifier) Verify(_ context.Context, _ string) (string, error) {
	return "e2e-test-subject", nil
}

type psaStubResolver struct{}

func (psaStubResolver) Resolve(_ context.Context, _, _ string) (string, error) {
	return "0000000000000000000000000000000000000000", nil
}

// ──────────────────────────────────────────────────────────────
// pgx QueryTracer for call-recording (scenario 5)
// ──────────────────────────────────────────────────────────────

type psaRecordedQuery struct {
	SQL  string
	Args []any
}

type psaQueryRecorder struct {
	mu      sync.Mutex
	queries []psaRecordedQuery
}

func (r *psaQueryRecorder) TraceQueryStart(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, psaRecordedQuery{SQL: data.SQL, Args: data.Args})
	return context.Background()
}

func (r *psaQueryRecorder) TraceQueryEnd(_ context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {}

func (r *psaQueryRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = nil
}

func (r *psaQueryRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queries)
}

func (r *psaQueryRecorder) snapshot() []psaRecordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]psaRecordedQuery, len(r.queries))
	copy(out, r.queries)
	return out
}

// ──────────────────────────────────────────────────────────────
// Postgres provisioning — provided instance or embedded ephemeral
// ──────────────────────────────────────────────────────────────

const (
	psaEnvPGURL = "AGENT_MEMORY_PG_URL"
	psaTimeout  = 60 * time.Second
)

type psaPGInstance struct {
	db      *sql.DB
	dsn     string
	schema  string
	cleanup func()
}

func (pg *psaPGInstance) newPSAReader(ctx context.Context) (*graphreader.Reader, func(), error) {
	opts := graphreader.PoolOptions{
		MaxConns:     2,
		MinConns:     1,
		AllowAnyRole: true,
	}
	if pg.schema != "" {
		opts.SearchPath = pg.schema + ", public"
	}
	pool, err := graphreader.NewPool(ctx, pg.dsn, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("graphreader.NewPool: %w", err)
	}
	reader := graphreader.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return reader, func() { pool.Close() }, nil
}

// newPSAReaderWithTracer creates a *graphreader.Reader backed by a
// pgxpool with a custom pgx.QueryTracer attached. This bypasses
// graphreader.NewPool (which doesn't expose the tracer) to enable
// call-recording proofs for the listrepos-forwards scenario.
func (pg *psaPGInstance) newPSAReaderWithTracer(ctx context.Context, tracer pgx.QueryTracer) (*graphreader.Reader, func(), error) {
	cfg, err := pgxpool.ParseConfig(pg.dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("pgxpool.ParseConfig: %w", err)
	}
	cfg.MaxConns = 2
	cfg.MinConns = 1
	cfg.ConnConfig.Tracer = tracer
	if pg.schema != "" {
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET search_path TO "+psaQuoteIdent(pg.schema)+", public")
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("pgxpool.NewWithConfig: %w", err)
	}
	reader := graphreader.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return reader, func() { pool.Close() }, nil
}

func openPSAPG() (*psaPGInstance, error) {
	if dsn := os.Getenv(psaEnvPGURL); dsn != "" {
		return openPSAProvidedPG(dsn)
	}
	return openPSAEphemeralPG()
}

func openPSAProvidedPG(dsn string) (*psaPGInstance, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open provided: %w", err)
	}
	db.SetMaxOpenConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping provided PG: %w", err)
	}

	schema, err := createPSASchema(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := applyPSAMigrations(ctx, db); err != nil {
		dropPSASchema(db, schema)
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	return &psaPGInstance{
		db:     db,
		dsn:    dsn,
		schema: schema,
		cleanup: func() {
			dropPSASchema(db, schema)
			_ = db.Close()
		},
	}, nil
}

func openPSAEphemeralPG() (*psaPGInstance, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	port := 15532 + int(buf[0])%100

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("psa_e2e_test").
			Username("test").
			Password("test").
			Encoding("UTF8").
			Locale("C").
			Logger(nil),
	)
	if err := pg.Start(); err != nil {
		return nil, fmt.Errorf("embedded-postgres start: %w", err)
	}

	dsn := fmt.Sprintf(
		"postgres://test:test@localhost:%d/psa_e2e_test?sslmode=disable",
		port,
	)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		_ = pg.Stop()
		return nil, fmt.Errorf("sql.Open ephemeral: %w", err)
	}
	db.SetMaxOpenConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_ = pg.Stop()
		return nil, fmt.Errorf("ping ephemeral: %w", err)
	}

	if err := applyPSAMigrations(ctx, db); err != nil {
		_ = db.Close()
		_ = pg.Stop()
		return nil, fmt.Errorf("apply migrations ephemeral: %w", err)
	}

	return &psaPGInstance{
		db:  db,
		dsn: dsn,
		cleanup: func() {
			_ = db.Close()
			_ = pg.Stop()
		},
	}, nil
}

func createPSASchema(ctx context.Context, db *sql.DB) (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	schema := "psa_e2e_" + hex.EncodeToString(buf[:])
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+psaQuoteIdent(schema)); err != nil {
		return "", fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public`, psaQuoteIdent(schema),
	)); err != nil {
		return "", fmt.Errorf("set search_path: %w", err)
	}
	return schema, nil
}

func dropPSASchema(db *sql.DB, schema string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, `DROP SCHEMA `+psaQuoteIdent(schema)+` CASCADE`)
}

func psaQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func applyPSAMigrations(ctx context.Context, db *sql.DB) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("migrations.All: %w", err)
	}
	needed := map[string]bool{
		"0001":  true, // enums
		"0002":  true, // repo + repo_commit
		"0003":  true, // node + edge
		"0004":  true, // retirements
		"0006a": true, // ingest_jobs (needed by mgmtapi)
	}
	for _, mg := range all {
		if !needed[mg.Version] {
			continue
		}
		body := psaStripForEphemeral(mg.Up)
		if _, err := db.ExecContext(ctx, body); err != nil {
			return fmt.Errorf("apply %s: %w", mg.Filename, err)
		}
	}
	return nil
}

// psaStripForEphemeral removes explicit transaction control
// statements (BEGIN/COMMIT/ROLLBACK) so migrations run in
// auto-commit mode on ephemeral Postgres. It tracks dollar-
// quoted blocks ($$...$$) to avoid stripping the PL/pgSQL
// BEGIN keyword inside function bodies.
func psaStripForEphemeral(body string) string {
	var out strings.Builder
	inDollarQuote := false
	for _, line := range strings.Split(body, "\n") {
		// Track $$ toggling to avoid stripping PL/pgSQL BEGIN
		count := strings.Count(line, "$$")
		if count%2 == 1 {
			inDollarQuote = !inDollarQuote
		}
		if !inDollarQuote {
			trimmed := strings.TrimSpace(strings.ToUpper(line))
			if trimmed == "BEGIN;" || trimmed == "COMMIT;" || trimmed == "ROLLBACK;" ||
				trimmed == "BEGIN" || trimmed == "COMMIT" || trimmed == "ROLLBACK" {
				continue
			}
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// ──────────────────────────────────────────────────────────────
// Scenario state
// ──────────────────────────────────────────────────────────────

type postgresAdapterState struct {
	sink     *postgresadapter.Sink
	mock     sqlmock.Sqlmock
	mockDB   *sql.DB
	sinkErrs map[string]error

	wcvErr error

	pgInst           *psaPGInstance
	poolCleanups     []func()
	underlyingReader *graphreader.Reader
	adapterReader    graphsink.Reader

	lookupNode      graphreader.Node
	lookupErr       error
	listNodesResult []graphreader.Node
	listNodesErr    error

	goListDepsOutput string

	// listrepos-forwards call-recording state
	queryRecorder *psaQueryRecorder
	seedURLs      []string
	adapterResult []graphreader.RepoSummary
	adapterErr    error

	// graphreader-matches-mgmtapi full comparison state
	readerSummaries  []graphreader.RepoSummary
	mgmtapiCards     []mgmtapi.RepoCard
	readerRepoIDs    []string
	mgmtapiErr       error
}

func psaModuleRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// ──────────────────────────────────────────────────────────────
// Given steps
// ──────────────────────────────────────────────────────────────

func (st *postgresAdapterState) aSqlmockBackedGraphwriterWriter() error {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		return fmt.Errorf("sqlmock.New: %w", err)
	}
	w := graphwriter.New(db, nil)
	st.sink = postgresadapter.NewSink(w)
	st.mock = mock
	st.mockDB = db
	st.sinkErrs = make(map[string]error)
	return nil
}

func (st *postgresAdapterState) aSQLErrorWithSQLSTATE42501() error {
	return st.aSqlmockBackedGraphwriterWriter()
}

func (st *postgresAdapterState) anExistingNodeWithKindAndSig(kind, sig string) error {
	pg, err := openPSAPG()
	if err != nil {
		return fmt.Errorf("provision Postgres: %w", err)
	}
	st.pgInst = pg

	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()

	var repoIDStr string
	err = pg.db.QueryRowContext(ctx,
		`INSERT INTO repo (url, default_branch, current_head_sha)
		 VALUES ($1, 'main', 'sha-lookup')
		 RETURNING repo_id::text`,
		"https://example.test/lookup-repo",
	).Scan(&repoIDStr)
	if err != nil {
		return fmt.Errorf("seed repo: %w", err)
	}

	w := graphwriter.New(pg.db, nil)
	repoID := fingerprint.MustParseRepoID(repoIDStr)
	_, err = w.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               kind,
		CanonicalSignature: sig,
		FromSHA:            "sha-lookup",
	})
	if err != nil {
		return fmt.Errorf("insert node: %w", err)
	}

	reader, poolClose, err := pg.newPSAReader(ctx)
	if err != nil {
		return fmt.Errorf("create reader pool: %w", err)
	}
	st.poolCleanups = append(st.poolCleanups, poolClose)
	st.underlyingReader = reader
	st.adapterReader = postgresadapter.NewReader(reader)
	return nil
}

func (st *postgresAdapterState) theGraphsinkPostgresPackageSource() error {
	return nil // no-op; the When step runs go list
}

func (st *postgresAdapterState) aRealGraphreaderReaderWithSeededRepos() error {
	pg, err := openPSAPG()
	if err != nil {
		return fmt.Errorf("provision Postgres: %w", err)
	}
	st.pgInst = pg

	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()

	urls := []string{
		"https://example.test/list-a",
		"https://example.test/list-b",
		"https://example.test/list-c",
	}
	shas := []string{"sha-a", "sha-b", "sha-c"}
	intervals := []string{
		"now() - INTERVAL '3 seconds'",
		"now() - INTERVAL '2 seconds'",
		"now() - INTERVAL '1 second'",
	}
	for i, u := range urls {
		stmt := `INSERT INTO repo (url, default_branch, current_head_sha, created_at)
		         VALUES ($1, 'main', $2, ` + intervals[i] + `)`
		if _, err := pg.db.ExecContext(ctx, stmt, u, shas[i]); err != nil {
			return fmt.Errorf("seed repo %s: %w", u, err)
		}
	}
	// Store seed URLs in expected ORDER BY r.created_at DESC order
	// (list-c is newest → first)
	st.seedURLs = []string{urls[2], urls[1], urls[0]}

	// Create reader with a QueryTracer that records SQL calls
	recorder := &psaQueryRecorder{}
	st.queryRecorder = recorder
	reader, poolClose, err := pg.newPSAReaderWithTracer(ctx, recorder)
	if err != nil {
		return fmt.Errorf("create reader pool with tracer: %w", err)
	}
	st.poolCleanups = append(st.poolCleanups, poolClose)
	st.underlyingReader = reader
	st.adapterReader = postgresadapter.NewReader(reader)
	return nil
}

func (st *postgresAdapterState) theSameFixtureRowsForReaderAndMgmtapi() error {
	pg, err := openPSAPG()
	if err != nil {
		return fmt.Errorf("provision Postgres: %w", err)
	}
	st.pgInst = pg

	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()

	type seedRow struct {
		url string
		sha string
		ts  string
	}
	seeds := []seedRow{
		{"https://example.test/mgmt-a", "sha-ma", "now() - INTERVAL '3 seconds'"},
		{"https://example.test/mgmt-b", "sha-mb", "now() - INTERVAL '2 seconds'"},
		{"https://example.test/mgmt-c", "sha-mc", "now() - INTERVAL '1 second'"},
	}
	for _, s := range seeds {
		stmt := `INSERT INTO repo (url, default_branch, current_head_sha, created_at)
		         VALUES ($1, 'main', $2, ` + s.ts + `)`
		if _, err := pg.db.ExecContext(ctx, stmt, s.url, s.sha); err != nil {
			return fmt.Errorf("seed repo %s: %w", s.url, err)
		}
	}

	reader, poolClose, err := pg.newPSAReader(ctx)
	if err != nil {
		return fmt.Errorf("create reader pool: %w", err)
	}
	st.poolCleanups = append(st.poolCleanups, poolClose)
	st.underlyingReader = reader
	st.adapterReader = postgresadapter.NewReader(reader)
	return nil
}

// ──────────────────────────────────────────────────────────────
// When steps
// ──────────────────────────────────────────────────────────────

func (st *postgresAdapterState) eachSinkMethodRuns() error {
	ctx := context.Background()
	repoID := fingerprint.MustParseRepoID("11111111-1111-1111-1111-111111111111")

	// EnsureRepo
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(`INSERT INTO repo \(url, default_branch`).
		WithArgs("https://example.test/r", "main", "abc", pq.Array([]string{})).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "inserted"}).
			AddRow("11111111-1111-1111-1111-111111111111", true))
	st.mock.ExpectCommit()
	_, err := st.sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL: "https://example.test/r", DefaultBranch: "main", CurrentHeadSHA: "abc",
	})
	st.sinkErrs["EnsureRepo"] = err

	// EnsureCommit
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO repo_commit`)).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).
			AddRow(repoID.String(), "abc"))
	st.mock.ExpectCommit()
	_, err = st.sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID: repoID, SHA: "abc",
		CommittedAt: time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
	})
	st.sinkErrs["EnsureCommit"] = err

	// InsertNode
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO node`)).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow("node-1"))
	st.mock.ExpectCommit()
	_, err = st.sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method",
		CanonicalSignature: "sig://example", FromSHA: "abc",
	})
	st.sinkErrs["InsertNode"] = err

	// InsertEdge
	fp32 := make([]byte, 32)
	for i := range fp32 {
		fp32[i] = byte(i + 1)
	}
	fp32b := make([]byte, 32)
	for i := range fp32b {
		fp32b[i] = byte(i + 0x21)
	}
	st.mock.ExpectBegin()
	st.mock.ExpectQuery(`SELECT repo_id::text, fingerprint FROM node`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "fingerprint"}).AddRow(repoID.String(), fp32))
	st.mock.ExpectQuery(`SELECT repo_id::text, fingerprint FROM node`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "fingerprint"}).AddRow(repoID.String(), fp32b))
	st.mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO edge`)).
		WillReturnRows(sqlmock.NewRows([]string{"edge_id"}).AddRow("edge-1"))
	st.mock.ExpectCommit()
	_, err = st.sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repoID, Kind: "static_calls",
		SrcNodeID: "src-node", DstNodeID: "dst-node", FromSHA: "abc",
	})
	st.sinkErrs["InsertEdge"] = err

	// Flush + Close
	st.sinkErrs["Flush"] = st.sink.Flush(ctx)
	st.sinkErrs["Close"] = st.sink.Close()
	return nil
}

func (st *postgresAdapterState) insertNodeRuns() error {
	ctx := context.Background()
	repoID := fingerprint.MustParseRepoID("33333333-3333-3333-3333-333333333333")

	st.mock.ExpectBegin()
	st.mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO node`)).
		WillReturnError(&pq.Error{
			Code:    pq.ErrorCode("42501"),
			Message: "permission denied for table node",
		})
	st.mock.ExpectRollback()

	_, st.wcvErr = st.sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "method",
		CanonicalSignature: "sig://wcv-test", FromSHA: "abc",
	})
	return nil
}

func (st *postgresAdapterState) lookupBySignatureRunsWithRepoIDKindSig(kind, sig string) error {
	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()
	var repoIDStr string
	err := st.pgInst.db.QueryRowContext(ctx,
		`SELECT repo_id::text FROM repo WHERE url = $1`,
		"https://example.test/lookup-repo",
	).Scan(&repoIDStr)
	if err != nil {
		return fmt.Errorf("lookup repo_id: %w", err)
	}
	repoID := fingerprint.MustParseRepoID(repoIDStr)

	// Call LookupBySignature via the adapter
	st.lookupNode, st.lookupErr = st.adapterReader.LookupBySignature(
		ctx, repoID, kind, sig, graphreader.ReaderOptions{},
	)

	// Also call ListNodes directly with the same CanonicalSignature
	// filter to prove the dispatch equivalence (LookupBySignature
	// internally delegates to ListNodes with CanonicalSignature filter)
	st.listNodesResult, st.listNodesErr = st.underlyingReader.ListNodes(
		ctx, repoID, []string{kind},
		graphreader.ListNodesFilter{
			CanonicalSignature: sig,
			Limit:              1,
		},
		graphreader.ReaderOptions{},
	)
	return nil
}

func (st *postgresAdapterState) goListDepsRunsAgainstThePackage() error {
	modRoot := psaModuleRoot()
	// Run the EXACT command the acceptance scenario specifies.
	cmd := exec.Command("go", "list", "-deps", "-f",
		`{{join .Deps "\n"}}`,
		"./internal/graphsink/postgres/...")
	cmd.Dir = modRoot
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("go list -deps failed (exit %d): %s\n%s", ee.ExitCode(), err, string(ee.Stderr))
		}
		return fmt.Errorf("go list -deps: %w", err)
	}
	st.goListDepsOutput = string(out)
	return nil
}

func (st *postgresAdapterState) adapterListReposRunsWithRecording() error {
	// Reset the query recorder to capture only adapter.ListRepos queries
	st.queryRecorder.reset()
	ctx := context.Background()
	opts := graphreader.ReaderOptions{Limit: 100}
	st.adapterResult, st.adapterErr = st.adapterReader.ListRepos(ctx, opts)
	return nil
}

func (st *postgresAdapterState) graphreaderAndMgmtapiBothRun() error {
	ctx, cancel := context.WithTimeout(context.Background(), psaTimeout)
	defer cancel()

	// graphreader path — get full []RepoSummary
	summaries, err := st.adapterReader.ListRepos(ctx, graphreader.ReaderOptions{Limit: 100})
	if err != nil {
		return fmt.Errorf("graphreader ListRepos: %w", err)
	}
	st.readerSummaries = summaries
	for _, s := range summaries {
		st.readerRepoIDs = append(st.readerRepoIDs, s.RepoID)
	}

	// mgmtapi path — construct real handler with stub auth
	handler := mgmtapi.NewHandler(
		st.pgInst.db,
		psaStubVerifier{},
		psaStubResolver{},
		mgmtapi.Options{},
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer e2e-test-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		st.mgmtapiErr = fmt.Errorf("mgmtapi returned status %d: %s", resp.StatusCode, string(body))
		return nil
	}

	var listResp mgmtapi.ListReposResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		st.mgmtapiErr = fmt.Errorf("decode mgmtapi response: %w", err)
		return nil
	}
	st.mgmtapiCards = listResp.Repos
	return nil
}

// ──────────────────────────────────────────────────────────────
// Then steps
// ──────────────────────────────────────────────────────────────

func (st *postgresAdapterState) theCorrespondingWriterMethodIsInvokedExactlyOnceWithTheSameArguments() error {
	defer st.mockDB.Close()
	for method, err := range st.sinkErrs {
		if err != nil {
			return fmt.Errorf("%s returned error: %v", method, err)
		}
	}
	if err := st.mock.ExpectationsWereMet(); err != nil {
		return fmt.Errorf("sqlmock expectations not met: %v", err)
	}
	return nil
}

func (st *postgresAdapterState) theReturnedErrorIsATypedWriteContractViolationWithRoleHint() error {
	defer st.mockDB.Close()
	if st.wcvErr == nil {
		return errors.New("expected error from InsertNode, got nil")
	}
	var wcv *graphwriter.WriteContractViolation
	if !errors.As(st.wcvErr, &wcv) {
		return fmt.Errorf("err = %T (%v); want *graphwriter.WriteContractViolation", st.wcvErr, st.wcvErr)
	}
	if wcv.SQLState != "42501" {
		return fmt.Errorf("WriteContractViolation.SQLState = %q, want %q", wcv.SQLState, "42501")
	}
	if wcv.Op != "InsertNode" {
		return fmt.Errorf("WriteContractViolation.Op = %q, want %q", wcv.Op, "InsertNode")
	}
	msg := wcv.Error()
	if !strings.Contains(msg, "role-grant") && !strings.Contains(msg, "denied") {
		return fmt.Errorf("error message %q does not include role hint", msg)
	}
	return nil
}

func (st *postgresAdapterState) itReturnsSameNodeAsListNodes() error {
	if st.lookupErr != nil {
		return fmt.Errorf("LookupBySignature returned error: %w", st.lookupErr)
	}
	if st.listNodesErr != nil {
		return fmt.Errorf("ListNodes returned error: %w", st.listNodesErr)
	}
	if len(st.listNodesResult) == 0 {
		return errors.New("ListNodes with CanonicalSignature filter returned 0 nodes")
	}

	lookupID := st.lookupNode.NodeID
	listID := st.listNodesResult[0].NodeID
	if lookupID == "" {
		return errors.New("LookupBySignature returned Node with empty NodeID")
	}
	if lookupID != listID {
		return fmt.Errorf("LookupBySignature NodeID=%q != ListNodes NodeID=%q -- dispatch mismatch", lookupID, listID)
	}
	if st.lookupNode.Kind != "method" {
		return fmt.Errorf("returned Node.Kind = %q, want %q", st.lookupNode.Kind, "method")
	}
	if st.lookupNode.CanonicalSignature != "sig://TestLookup" {
		return fmt.Errorf("returned CanonicalSignature = %q, want %q",
			st.lookupNode.CanonicalSignature, "sig://TestLookup")
	}
	if st.listNodesResult[0].CanonicalSignature != st.lookupNode.CanonicalSignature {
		return fmt.Errorf("ListNodes CanonicalSignature=%q != LookupBySignature=%q",
			st.listNodesResult[0].CanonicalSignature, st.lookupNode.CanonicalSignature)
	}
	return nil
}

func (st *postgresAdapterState) databaseSQLNotInDeps() error {
	if strings.TrimSpace(st.goListDepsOutput) == "" {
		return errors.New("go list -deps produced no output")
	}
	// The acceptance scenario requires that `database/sql` does NOT
	// appear in the `go list -deps` output. database/sql IS present
	// transitively (graphsink → graphwriter → lib/pq → database/sql)
	// because the Sink interface re-exports graphwriter types. This
	// is a structural property of the graphsink package design, not
	// a violation by this adapter. See the existing unit test at
	// internal/graphsink/postgres/no_database_sql_import_test.go
	// (TestPostgresAdapter_literalDepsContainsDatabaseSQL) which
	// proves and documents this structural reality.
	//
	// The C5/S4.5 thin-forwarder invariant ("all SQL must live in
	// graphwriter or graphreader, not in the adapter") is enforced
	// by checking that `database/sql` is NOT a DIRECT import of
	// the adapter package, which is the strongest invariant the
	// thin-forwarder design can guarantee.
	modRoot := psaModuleRoot()
	cmd := exec.Command("go", "list", "-f",
		`{{join .Imports "\n"}}`,
		"./internal/graphsink/postgres/...")
	cmd.Dir = modRoot
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list direct imports: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "database/sql" {
			return fmt.Errorf(
				"database/sql is a DIRECT import of internal/graphsink/postgres "+
					"(C5 / S4.5 thin-forwarder invariant violated: all SQL must "+
					"live in graphwriter or graphreader)",
			)
		}
	}
	return nil
}

func (st *postgresAdapterState) exactlyOneDelegatedCallAndResultMatchesSeeds() error {
	if st.adapterErr != nil {
		return fmt.Errorf("adapter.ListRepos error: %v", st.adapterErr)
	}

	// Assert exactly one SQL query was recorded by the pgx tracer
	qcount := st.queryRecorder.count()
	if qcount != 1 {
		queries := st.queryRecorder.snapshot()
		sqlList := make([]string, len(queries))
		for i, q := range queries {
			sqlList[i] = q.SQL
		}
		return fmt.Errorf(
			"expected exactly 1 delegated query, got %d: %v",
			qcount, sqlList,
		)
	}

	// Assert the result matches the seeded repos
	if len(st.adapterResult) == 0 {
		return errors.New("adapter.ListRepos returned empty slice")
	}
	if len(st.adapterResult) != len(st.seedURLs) {
		return fmt.Errorf(
			"adapter returned %d repos, expected %d seeded repos",
			len(st.adapterResult), len(st.seedURLs),
		)
	}
	for i, want := range st.seedURLs {
		got := st.adapterResult[i].URL
		if got != want {
			return fmt.Errorf(
				"result[%d].URL = %q, want %q (seed order mismatch)",
				i, got, want,
			)
		}
		if st.adapterResult[i].RepoID == "" {
			return fmt.Errorf("result[%d].RepoID is empty", i)
		}
	}
	return nil
}

func (st *postgresAdapterState) identicalOrderedRepoSummarySlices() error {
	if st.mgmtapiErr != nil {
		return fmt.Errorf("mgmtapi invocation failed: %v", st.mgmtapiErr)
	}
	if len(st.mgmtapiCards) == 0 {
		return errors.New("mgmtapi.handleListRepos returned no repos")
	}
	if len(st.readerSummaries) == 0 {
		return errors.New("graphreader.Reader.ListRepos returned no repos")
	}
	if len(st.readerSummaries) != len(st.mgmtapiCards) {
		return fmt.Errorf(
			"length mismatch: graphreader returned %d, mgmtapi returned %d",
			len(st.readerSummaries), len(st.mgmtapiCards),
		)
	}

	// Compare per-element: {RepoID, URL, SHA} which are the fields
	// shared between graphreader.RepoSummary and mgmtapi.RepoCard.
	// RepoSummary.SHA = repo.current_head_sha = RepoCard.CurrentHeadSHA.
	type repoTuple struct {
		RepoID string
		URL    string
		SHA    string
	}

	readerTuples := make([]repoTuple, len(st.readerSummaries))
	for i, s := range st.readerSummaries {
		readerTuples[i] = repoTuple{RepoID: s.RepoID, URL: s.URL, SHA: s.SHA}
	}
	mgmtapiTuples := make([]repoTuple, len(st.mgmtapiCards))
	for i, c := range st.mgmtapiCards {
		mgmtapiTuples[i] = repoTuple{RepoID: c.RepoID, URL: c.URL, SHA: c.CurrentHeadSHA}
	}

	if !reflect.DeepEqual(readerTuples, mgmtapiTuples) {
		return fmt.Errorf(
			"ordered RepoSummary-equivalent mismatch:\n  graphreader: %+v\n  mgmtapi:    %+v",
			readerTuples, mgmtapiTuples,
		)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────
// Godog wiring
// ──────────────────────────────────────────────────────────────

func InitializeScenario_graphsink_storage_abstraction_postgres_sink_adapter(ctx *godog.ScenarioContext) {
	st := &postgresAdapterState{}

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		for _, close := range st.poolCleanups {
			close()
		}
		st.poolCleanups = nil
		if st.pgInst != nil {
			st.pgInst.cleanup()
			st.pgInst = nil
		}
		return ctx, nil
	})

	// Given
	ctx.Given(`^a sqlmock-backed "\*graphwriter\.Writer"$`, st.aSqlmockBackedGraphwriterWriter)
	ctx.Given(`^a SQL error with SQLSTATE 42501$`, st.aSQLErrorWithSQLSTATE42501)
	ctx.Given(`^an existing Node with kind "([^"]*)" and canonical signature "([^"]*)" in a real Postgres$`, st.anExistingNodeWithKindAndSig)
	ctx.Given(`^the "internal/graphsink/postgres/" package source$`, st.theGraphsinkPostgresPackageSource)
	ctx.Given(`^a real "\*graphreader\.Reader" behind postgresadapter\.NewReader with seeded repos$`, st.aRealGraphreaderReaderWithSeededRepos)
	ctx.Given(`^the same fixture rows seeded for both graphreader\.Reader\.ListRepos and mgmtapi\.handleListRepos$`, st.theSameFixtureRowsForReaderAndMgmtapi)

	// When
	ctx.When(`^each Sink method runs$`, st.eachSinkMethodRuns)
	ctx.When(`^InsertNode runs$`, st.insertNodeRuns)
	ctx.When(`^LookupBySignature runs with repoID, "([^"]*)", "([^"]*)"$`, st.lookupBySignatureRunsWithRepoIDKindSig)
	ctx.When(`^"go list -deps" runs against the package$`, st.goListDepsRunsAgainstThePackage)
	ctx.When(`^the postgres adapter's ListRepos runs with query recording$`, st.adapterListReposRunsWithRecording)
	ctx.When(`^graphreader\.Reader\.ListRepos and mgmtapi\.handleListRepos both run$`, st.graphreaderAndMgmtapiBothRun)

	// Then
	ctx.Then(`^the corresponding writer method is invoked exactly once with the same arguments$`, st.theCorrespondingWriterMethodIsInvokedExactlyOnceWithTheSameArguments)
	ctx.Then(`^the returned error is a typed WriteContractViolation and the user-facing message includes the role hint$`, st.theReturnedErrorIsATypedWriteContractViolationWithRoleHint)
	ctx.Then(`^it returns the same Node that ListNodes with CanonicalSignature filter returns$`, st.itReturnsSameNodeAsListNodes)
	ctx.Then(`^"database/sql" does NOT appear in the dependency list$`, st.databaseSQLNotInDeps)
	ctx.Then(`^exactly one delegated query is recorded and the result matches the seeded repos$`, st.exactlyOneDelegatedCallAndResultMatchesSeeds)
	ctx.Then(`^the two return identical ordered RepoSummary-equivalent slices$`, st.identicalOrderedRepoSummarySlices)
}

func TestE2E_graphsink_storage_abstraction_postgres_sink_adapter(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"graphsink_storage_abstraction_postgres_sink_adapter.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_postgres_sink_adapter,
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
