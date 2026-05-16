package repoindexer

// Integration tests for the Stage 3.1 Repo Indexer worker
// scaffold. The tests require a live PostgreSQL 16 cluster
// (AGENT_MEMORY_PG_URL set); they skip cleanly when the env
// var is unset, mirroring the convention in
// internal/graphwriter/writer_integration_test.go and
// migrations/test_migrate_test.go.
//
// Coverage map (implementation-plan.md §3.1 acceptance scenarios)
// --------------------------------------------------------------
//
//   * "full ingest of a small fixture"
//       -> TestWorker_fullIngest_buildsRepoPackageFileAncestry
//   * "worker claim is exclusive"
//       -> TestWorker_claimIsExclusive_underContention
//   * "idempotent re-ingest"
//       -> TestWorker_fullIngest_idempotentReIngest
//
// Plus correctness coverage the brief implicitly requires:
//   * pg_notify channel delivery -- TestPGNotifyPublisher_deliversToListener
//   * ASTEmitter invoked once per File Node --
//     TestWorker_fullIngest_callsASTEmitterPerFile
//   * Pool spawns N workers --
//     TestPool_runsConfiguredWorkerCount

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/testpglock"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 60 * time.Second
)

// dbFixture mirrors the fixture in graphwriter; we re-implement
// rather than import because Go packages can't import test
// helpers across package boundaries.
type dbFixture struct {
	owner   *sql.DB
	app     *sql.DB
	schema  string
	cleanup func()
}

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

func openAppRoleDB(t *testing.T, owner *sql.DB, baseURL, schema string) (*sql.DB, func()) {
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
	password := "amindexer_" + hex.EncodeToString(buf[:])

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

	// Pin search_path at the PostgreSQL protocol level via the
	// `options=-c search_path=...` startup parameter. lib/pq
	// passes this through unchanged, and PostgreSQL applies it
	// to EVERY backend the pool opens — so we can safely raise
	// MaxOpenConns without losing the per-test schema isolation
	// (which a session-level `SET search_path` would not do
	// because lib/pq has no per-connection-init hook).
	u2 := *u
	u2.User = url.UserPassword("agent_memory_app", password)
	q := u2.Query()
	q.Set("options", "-c search_path="+schema+",public,partman")
	u2.RawQuery = q.Encode()
	app, err := sql.Open("postgres", u2.String())
	if err != nil {
		revert()
		t.Fatalf("sql.Open app: %v", err)
	}
	// The worker spawns its own per-claim transactions and the
	// claim-exclusivity / pool integration tests race multiple
	// goroutines. Allow several backends so FOR UPDATE SKIP
	// LOCKED actually has distinct sessions to compete across.
	app.SetMaxOpenConns(8)
	app.SetMaxIdleConns(4)
	if err := app.PingContext(ctx); err != nil {
		_ = app.Close()
		revert()
		t.Fatalf("ping app DB: %v", err)
	}

	success = true
	return app, func() {
		_ = app.Close()
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
	return "amindexer_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// seedRepoAndJob inserts a Repo row and an `ingest_jobs` row
// for the supplied URL/SHA pair. Returns the repo_id and
// job_id so the test can drive Worker.Claim and inspect the
// resulting rows.
func seedRepoAndJob(t *testing.T, ctx context.Context, w *graphwriter.Writer, fix *dbFixture, repoURL, sha string) (fingerprint.RepoID, string) {
	t.Helper()
	rec, err := w.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            repoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: sha,
		LanguageHints:  []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	var jobID string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, from_sha, to_sha, status)
		VALUES ($1, 'full', NULL, $2, 'pending')
		RETURNING job_id::text
	`, rec.RepoID, sha).Scan(&jobID); err != nil {
		t.Fatalf("insert ingest_jobs: %v", err)
	}
	return rec.ID, jobID
}

// makeFixtureFiles builds a synthetic file set used by the
// "small fixture" scenario. Returns 50 files across three
// packages: 20 in `pkg/a`, 20 in `pkg/b`, 10 in repo root.
func makeFixtureFiles() []InMemoryFile {
	files := make([]InMemoryFile, 0, 50)
	for i := 0; i < 20; i++ {
		files = append(files, InMemoryFile{
			RelPath: fmt.Sprintf("pkg/a/file_%02d.go", i),
			Content: []byte(fmt.Sprintf("package a\nvar A%d = 1\n", i)),
		})
	}
	for i := 0; i < 20; i++ {
		files = append(files, InMemoryFile{
			RelPath: fmt.Sprintf("pkg/b/file_%02d.go", i),
			Content: []byte(fmt.Sprintf("package b\nvar B%d = 1\n", i)),
		})
	}
	for i := 0; i < 10; i++ {
		files = append(files, InMemoryFile{
			RelPath: fmt.Sprintf("root_%02d.md", i),
			Content: []byte("readme\n"),
		})
	}
	return files
}

// ----- TESTS ------------------------------------------------------

// TestWorker_fullIngest_buildsRepoPackageFileAncestry covers the
// implementation-plan.md §3.1 scenario "full ingest of a small
// fixture". A 50-file fixture is fed to the worker via the in-
// memory materializer; the assertions cover:
//
//   - one Repo Node, three Package Nodes (`pkg/a`, `pkg/b`, ""),
//     50 File Nodes total.
//   - every File Node's parent_node_id is a Package Node.
//   - every Package Node's parent_node_id is the Repo Node.
//   - contains-edges exist Repo→Package (×3) and Package→File
//     (×50).
//   - the AST emitter was invoked once per File Node.
//   - the job row reaches status='done' and attempt_index=1.
//   - both lifecycle events were published.
func TestWorker_fullIngest_buildsRepoPackageFileAncestry(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := "https://example.test/small-fixture"
	sha := "deadbeef00"
	_, jobID := seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)

	mat := &InMemoryMaterializer{Files: makeFixtureFiles()}
	emitter := &recordingASTEmitter{}
	pub := &recordingEventPublisher{}

	var logbuf bytes.Buffer
	worker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-1",
		Materializer: mat,
		Emitter:      emitter,
		Publisher:    pub,
		Logger:       slog.New(slog.NewJSONHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	processed, err := worker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("ProcessOnce returned processed=false; want true")
	}

	// 1. Job row reached status='done' with attempt_index=1.
	var status string
	var attempts int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text, attempt_index FROM ingest_jobs WHERE job_id = $1`, jobID,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("readback job: %v", err)
	}
	if status != "done" {
		t.Errorf("job status = %q, want done", status)
	}
	if attempts != 1 {
		t.Errorf("attempt_index = %d, want 1", attempts)
	}

	// 2. Node counts per kind.
	type cnt struct {
		kind  string
		count int
	}
	var counts []cnt
	rows, err := fix.owner.QueryContext(ctx, `
		SELECT kind::text, count(*) FROM node
		WHERE from_sha = $1
		GROUP BY kind ORDER BY kind
	`, sha)
	if err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c cnt
		if err := rows.Scan(&c.kind, &c.count); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts = append(counts, c)
	}
	wantCounts := map[string]int{
		"repo": 1, "package": 3, "file": 50,
	}
	got := map[string]int{}
	for _, c := range counts {
		got[c.kind] = c.count
	}
	for k, v := range wantCounts {
		if got[k] != v {
			t.Errorf("node count for %s = %d, want %d (all: %+v)", k, got[k], v, got)
		}
	}

	// 3. No orphan files: every File node has a Package parent.
	var orphans int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT count(*) FROM node f
		WHERE f.kind = 'file'
		  AND f.from_sha = $1
		  AND (f.parent_node_id IS NULL
		       OR NOT EXISTS (
		           SELECT 1 FROM node p
		           WHERE p.node_id = f.parent_node_id
		             AND p.kind = 'package'
		       ))
	`, sha).Scan(&orphans); err != nil {
		t.Fatalf("orphan check: %v", err)
	}
	if orphans != 0 {
		t.Errorf("found %d File nodes without a Package parent; want 0", orphans)
	}

	// 4. Packages all root through the Repo node.
	var pkgOrphans int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT count(*) FROM node p
		WHERE p.kind = 'package'
		  AND p.from_sha = $1
		  AND (p.parent_node_id IS NULL
		       OR NOT EXISTS (
		           SELECT 1 FROM node r
		           WHERE r.node_id = p.parent_node_id
		             AND r.kind = 'repo'
		       ))
	`, sha).Scan(&pkgOrphans); err != nil {
		t.Fatalf("pkg orphan check: %v", err)
	}
	if pkgOrphans != 0 {
		t.Errorf("found %d Package nodes without a Repo parent; want 0", pkgOrphans)
	}

	// 5. contains-edges: 3 (Repo→Package) + 50 (Package→File).
	var edgeCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT count(*) FROM edge
		WHERE kind = 'contains' AND from_sha = $1
	`, sha).Scan(&edgeCount); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edgeCount != 53 {
		t.Errorf("contains edge count = %d, want 53 (3 repo->pkg + 50 pkg->file)", edgeCount)
	}

	// 6. AST emitter was invoked once per File Node.
	if len(emitter.events()) != 50 {
		t.Errorf("ASTEmitter.EmitFile call count = %d, want 50", len(emitter.events()))
	}

	// 7. Both lifecycle events were published (cold registration).
	kinds := map[string]int{}
	for _, e := range pub.events() {
		kinds[e.Kind]++
	}
	if kinds[EventKindRepoRegistered] != 1 {
		t.Errorf("publisher saw %d %s events; want 1",
			kinds[EventKindRepoRegistered], EventKindRepoRegistered)
	}
	if kinds[EventKindRepoFullIngested] != 1 {
		t.Errorf("publisher saw %d %s events; want 1",
			kinds[EventKindRepoFullIngested], EventKindRepoFullIngested)
	}
}

// TestWorker_claimIsExclusive_underContention covers the brief's
// "worker claim is exclusive" scenario. Two workers race to
// claim the same single pending row; FOR UPDATE SKIP LOCKED
// must ensure only one wins.
func TestWorker_claimIsExclusive_underContention(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	_, jobID := seedRepoAndJob(t, ctx, gw, fix, "https://example.test/claim-exclusive", "abc123")

	mat := &InMemoryMaterializer{Files: []InMemoryFile{{RelPath: "x.go", Content: []byte("x")}}}
	pub := &recordingEventPublisher{}
	makeWorker := func(id string) *Worker {
		return NewWorker(fix.app, gw, WorkerOptions{
			WorkerID:     id,
			Materializer: mat,
			Emitter:      NoopASTEmitter{},
			Publisher:    pub,
		})
	}
	w1 := makeWorker("worker-A")
	w2 := makeWorker("worker-B")

	// Run the two Claim() calls concurrently. Exactly one
	// must succeed; the other must see ErrNoJob.
	var (
		wg         sync.WaitGroup
		jobA, jobB Job
		errA, errB error
	)
	wg.Add(2)
	start := make(chan struct{})
	go func() {
		defer wg.Done()
		<-start
		jobA, errA = w1.Claim(ctx)
	}()
	go func() {
		defer wg.Done()
		<-start
		jobB, errB = w2.Claim(ctx)
	}()
	close(start)
	wg.Wait()

	winners := 0
	losers := 0
	var winnerJob Job
	var winnerID string
	if errA == nil {
		winners++
		winnerJob = jobA
		winnerID = w1.WorkerID()
	} else if !errors.Is(errA, ErrNoJob) {
		t.Errorf("worker A returned unexpected error: %v", errA)
	} else {
		losers++
	}
	if errB == nil {
		winners++
		winnerJob = jobB
		winnerID = w2.WorkerID()
	} else if !errors.Is(errB, ErrNoJob) {
		t.Errorf("worker B returned unexpected error: %v", errB)
	} else {
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("want exactly one winner and one loser, got winners=%d losers=%d (errA=%v, errB=%v)",
			winners, losers, errA, errB)
	}
	if winnerJob.JobID != jobID {
		t.Errorf("winner claimed job_id %s; want %s", winnerJob.JobID, jobID)
	}

	// Persisted claim metadata should match the winner.
	var claimedBy string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT claimed_by FROM ingest_jobs WHERE job_id=$1`, jobID,
	).Scan(&claimedBy); err != nil {
		t.Fatalf("readback claimed_by: %v", err)
	}
	if claimedBy != winnerID {
		t.Errorf("claimed_by = %q, want %q", claimedBy, winnerID)
	}
}

// TestWorker_fullIngest_idempotentReIngest covers the brief's
// "idempotent re-ingest" scenario: running the full-mode handler
// twice against the same SHA must not insert new Node rows on
// the second pass.
func TestWorker_fullIngest_idempotentReIngest(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := "https://example.test/idempotent"
	sha := "1111111111"
	repoID, _ := seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)

	mat := &InMemoryMaterializer{Files: makeFixtureFiles()}
	w := NewWorker(fix.app, gw, WorkerOptions{
		Materializer: mat,
		Emitter:      NoopASTEmitter{},
		Publisher:    &recordingEventPublisher{},
	})

	job := Job{RepoID: repoID, Mode: ModeFull, ToSHA: sha, FromSHA: ""}
	sum1, err := w.runFull(ctx, job)
	if err != nil {
		t.Fatalf("first runFull: %v", err)
	}
	if !sum1.CommitInserted {
		t.Errorf("first runFull: CommitInserted = false; want true (cold ingest)")
	}
	if sum1.FilesInserted != 50 {
		t.Errorf("first runFull: FilesInserted = %d; want 50", sum1.FilesInserted)
	}
	if sum1.PackagesInserted != 3 {
		t.Errorf("first runFull: PackagesInserted = %d; want 3", sum1.PackagesInserted)
	}
	if sum1.ContainsEdgesInserted != 53 {
		t.Errorf("first runFull: ContainsEdgesInserted = %d; want 53", sum1.ContainsEdgesInserted)
	}

	var beforeNodes, beforeEdges int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node WHERE repo_id::text=$1`, repoID.String(),
	).Scan(&beforeNodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM edge WHERE repo_id::text=$1`, repoID.String(),
	).Scan(&beforeEdges); err != nil {
		t.Fatalf("count edges: %v", err)
	}

	// Re-run the handler. The graphwriter's ON CONFLICT DO
	// NOTHING is the load-bearing dedupe primitive; the
	// handler itself only needs to avoid re-computing
	// canonical signatures inconsistently.
	sum2, err := w.runFull(ctx, job)
	if err != nil {
		t.Fatalf("second runFull: %v", err)
	}
	if sum2.CommitInserted {
		t.Errorf("second runFull: CommitInserted = true; want false (replay)")
	}
	if sum2.FilesInserted != 0 || sum2.PackagesInserted != 0 || sum2.ContainsEdgesInserted != 0 {
		t.Errorf("second runFull: inserted counts non-zero: files=%d pkgs=%d edges=%d (want 0,0,0)",
			sum2.FilesInserted, sum2.PackagesInserted, sum2.ContainsEdgesInserted)
	}
	if sum2.FilesEnsured != 50 {
		t.Errorf("second runFull: FilesEnsured = %d; want 50 (every file still touched)", sum2.FilesEnsured)
	}

	var afterNodes, afterEdges int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM node WHERE repo_id::text=$1`, repoID.String(),
	).Scan(&afterNodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM edge WHERE repo_id::text=$1`, repoID.String(),
	).Scan(&afterEdges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if afterNodes != beforeNodes {
		t.Errorf("node row count changed: %d -> %d (want stable across idempotent re-ingest)",
			beforeNodes, afterNodes)
	}
	if afterEdges != beforeEdges {
		t.Errorf("edge row count changed: %d -> %d (want stable across idempotent re-ingest)",
			beforeEdges, afterEdges)
	}
}

// TestWorker_fullIngest_callsASTEmitterPerFile pins the Stage 3.2
// integration contract: every File Node ensured by the worker
// triggers exactly one EmitFile call carrying the FileNodeID
// downstream stages will use to stitch Class/Method/Block nodes.
func TestWorker_fullIngest_callsASTEmitterPerFile(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := "https://example.test/emitter"
	sha := "feedface"
	repoID, _ := seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)

	files := []InMemoryFile{
		{RelPath: "main.go", Content: []byte("package main\n")},
		{RelPath: "pkg/util.go", Content: []byte("package pkg\n")},
		{RelPath: "pkg/util_test.go", Content: []byte("package pkg\n")},
	}
	mat := &InMemoryMaterializer{Files: files}
	emitter := &recordingASTEmitter{}
	w := NewWorker(fix.app, gw, WorkerOptions{
		Materializer: mat,
		Emitter:      emitter,
		Publisher:    &recordingEventPublisher{},
	})
	if _, err := w.runFull(ctx, Job{RepoID: repoID, Mode: ModeFull, ToSHA: sha}); err != nil {
		t.Fatalf("runFull: %v", err)
	}
	if got := len(emitter.events()); got != len(files) {
		t.Fatalf("EmitFile call count = %d, want %d", got, len(files))
	}
	// Each EmitFile event must carry a non-empty FileNodeID
	// pointing at a real File row; without that the downstream
	// AST dispatcher cannot stitch its Class/Method/Block
	// nodes into the ancestry.
	for i, ev := range emitter.events() {
		if ev.FileNodeID == "" {
			t.Errorf("event[%d].FileNodeID is empty", i)
		}
		if ev.RepoURL != repoURL {
			t.Errorf("event[%d].RepoURL = %q, want %q", i, ev.RepoURL, repoURL)
		}
		if ev.SHA != sha {
			t.Errorf("event[%d].SHA = %q, want %q", i, ev.SHA, sha)
		}
		if ev.Open == nil {
			t.Errorf("event[%d].Open is nil", i)
		}
	}
}

// TestPool_runsConfiguredWorkerCount exercises the pool's
// fan-out. Two pending jobs are seeded; a pool of 4 workers
// must process both within the test timeout. The number of
// distinct claimed_by identities observed across the queue
// must be at least 2 -- proving the pool actually spawned
// independent workers and didn't process both jobs through
// one goroutine.
//
// The materializer is wrapped in a one-shot `testBarrier(2)`
// so a worker that claimed one of the seeded rows CANNOT
// complete Materialize until a sibling worker has also claimed
// the OTHER row. Without the barrier the test was
// scheduler-flaky: an in-memory job is fast enough that one
// goroutine could legally claim+process both rows before
// siblings woke up to poll, and the post-run "distinct
// claimed_by >= 2" assertion would intermittently fail. The
// barrier makes the concurrent-claim invariant deterministic.
func TestPool_runsConfiguredWorkerCount(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Seed two distinct jobs (different SHAs so the
	// (repo, mode, from_sha, to_sha) UNIQUE doesn't reject
	// the second).
	repoURL := "https://example.test/pool"
	for i, sha := range []string{"sha-pool-a", "sha-pool-b"} {
		t.Logf("seeding job %d (sha=%s)", i, sha)
		_, _ = seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)
	}

	// Wrap the in-memory materializer with a barrier of size 2 so
	// each Materialize call blocks until BOTH workers have
	// claimed a row. This is the structural fix for the
	// scheduler-flake the prior implementation suffered from.
	bar := newTestBarrier(2)
	mat := &barrierMaterializer{
		Inner:   &InMemoryMaterializer{Files: []InMemoryFile{{RelPath: "x.go", Content: []byte("x")}}},
		Barrier: bar,
	}
	pool, err := NewPool(fix.app, gw, PoolConfig{
		Workers: 4,
		PerWorker: WorkerOptions{
			Materializer: mat,
			Emitter:      NoopASTEmitter{},
			Publisher:    &recordingEventPublisher{},
			PollEvery:    50 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if got := len(pool.Workers()); got != 4 {
		t.Errorf("pool spawned %d workers; want 4", got)
	}
	// Each spawned worker's WorkerID must be distinct -- the
	// claim path writes it to ingest_jobs.claimed_by, and the
	// post-run "distinct claimed_by" assertion below relies on
	// the pool actually generating a unique identity per
	// goroutine (not, say, four workers sharing one ID by
	// accident).
	seenIDs := map[string]struct{}{}
	for _, w := range pool.Workers() {
		seenIDs[w.WorkerID()] = struct{}{}
	}
	if len(seenIDs) != 4 {
		t.Errorf("pool produced %d distinct WorkerIDs; want 4 (ids=%v)", len(seenIDs), seenIDs)
	}

	runCtx, cancelRun := context.WithTimeout(ctx, 15*time.Second)
	defer cancelRun()
	done := make(chan error, 1)
	go func() { done <- pool.Run(runCtx) }()

	// Wait until both jobs are done.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT count(*) FROM ingest_jobs WHERE status='done'`,
		).Scan(&n); err != nil {
			t.Fatalf("count done: %v", err)
		}
		if n == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancelRun()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("pool.Run returned: %v", err)
	}

	var n int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM ingest_jobs WHERE status='done'`,
	).Scan(&n); err != nil {
		t.Fatalf("count done: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 done jobs, got %d", n)
	}

	// Per the test's docstring: at least 2 distinct claimed_by
	// identities must show up in the queue. With only 2 jobs
	// and 4 workers, the upper bound is 2 -- but the lower
	// bound proves the pool actually fanned out across
	// goroutines instead of one worker draining both rows
	// while three sat idle. With the barrier above, this is
	// guaranteed (not merely probable): both rows must be
	// claimed concurrently before either Materialize returns.
	var distinctClaimers int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(DISTINCT claimed_by) FROM ingest_jobs WHERE status='done'`,
	).Scan(&distinctClaimers); err != nil {
		t.Fatalf("count distinct claimed_by: %v", err)
	}
	if distinctClaimers < 2 {
		// Dump the rows so triage is straightforward when this
		// fails -- we want operator-visible evidence of which
		// worker drained which row.
		rows, qErr := fix.owner.QueryContext(ctx,
			`SELECT job_id::text, claimed_by FROM ingest_jobs WHERE status='done' ORDER BY job_id`)
		if qErr == nil {
			defer rows.Close()
			for rows.Next() {
				var j, c string
				if sErr := rows.Scan(&j, &c); sErr == nil {
					t.Logf("done job %s claimed_by=%s", j, c)
				}
			}
		}
		t.Fatalf("expected >=2 distinct claimed_by identities across done jobs, got %d -- pool did not fan out across goroutines",
			distinctClaimers)
	}
}

// TestPGNotifyPublisher_deliversToListener wires the pg_notify
// publisher to a LISTEN'ing connection and confirms the
// {kind, repo_id, sha, job_id, time} payload arrives intact.
// Uses a per-test channel so concurrent test binaries don't
// cross-talk on the production `agent_memory_events` name.
func TestPGNotifyPublisher_deliversToListener(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// pq.NewListener wants the same DSN the rest of the
	// fixture uses; the fixture pinned the schema via the
	// search_path URL param.
	dsn := os.Getenv(envPGURL)
	if dsn == "" {
		t.Skip("AGENT_MEMORY_PG_URL unset")
	}

	// Per-test channel so this test does not collide with
	// other Stage-3 tests that publish on the production
	// channel.
	channel := "agent_memory_events_test_" + fix.schema

	listener := pq.NewListener(dsn, 1*time.Second, 5*time.Second, nil)
	defer listener.Close()
	if err := listener.Listen(channel); err != nil {
		t.Fatalf("listener.Listen: %v", err)
	}

	pub := NewPGNotifyPublisher(fix.app, slog.Default()).WithChannel(channel)
	ev := Event{
		Kind:   EventKindRepoFullIngested,
		RepoID: "00000000-0000-0000-0000-000000000001",
		SHA:    "deadbeef",
		JobID:  "11111111-2222-3333-4444-555555555555",
		Time:   time.Now().UTC().Truncate(time.Second),
	}
	if err := pub.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case n := <-listener.Notify:
		if n == nil {
			t.Fatal("nil notification (listener closed?)")
		}
		if n.Channel != channel {
			t.Errorf("notify channel = %q, want %q", n.Channel, channel)
		}
		if !strings.Contains(n.Extra, EventKindRepoFullIngested) {
			t.Errorf("payload missing kind: %s", n.Extra)
		}
		if !strings.Contains(n.Extra, ev.RepoID) {
			t.Errorf("payload missing repo_id: %s", n.Extra)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for NOTIFY delivery")
	}
}

// TestWorker_publishFailure_requeuesAndRetries pins the
// fix #2 contract: a transient publish failure during
// markDoneAndPublish MUST roll back the atomic tx, transition
// the row BACK to `pending` (not terminal `failed`), and let a
// subsequent ProcessOnce -- once the publisher recovers --
// drive the row to `done` with both events delivered. The
// prior implementation marked the row `failed` after one
// publish error, which Claim could not recover from since it
// only consumes pending rows; downstream subscribers would
// permanently miss the completed ingest until an operator
// re-enqueued by hand.
//
// Two ProcessOnce invocations:
//
//  1. failing publisher -> assert status='pending',
//     attempt_index=1, no events recorded, status NEVER reached
//     'done'.
//  2. publisher recovered (failOn cleared) -> assert
//     status='done', both events recorded, attempt_index=2.
func TestWorker_publishFailure_requeuesAndRetries(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := "https://example.test/publish-requeue"
	sha := "requeue-sha"
	_, jobID := seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)

	mat := &InMemoryMaterializer{Files: []InMemoryFile{
		{RelPath: "a.go", Content: []byte("package a\n")},
	}}
	// failOn covers BOTH event kinds because on a fresh ingest
	// the registered event would be the first publish to enroll
	// in the tx; without covering both we can't be sure which
	// one tripped the rollback.
	failingPub := &recordingEventPublisher{
		failOn: map[string]error{
			EventKindRepoRegistered:   errors.New("simulated publisher outage"),
			EventKindRepoFullIngested: errors.New("simulated publisher outage"),
		},
	}
	w := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "requeue-worker",
		Materializer: mat,
		Emitter:      NoopASTEmitter{},
		Publisher:    failingPub,
		MaxAttempts:  5, // explicit so the test does not rely on the default
	})

	// Pass 1: publisher is broken, expect requeue.
	processed, err := w.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("ProcessOnce #1: %v", err)
	}
	if !processed {
		t.Fatal("ProcessOnce #1 returned processed=false; want true")
	}
	var status string
	var attempts int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text, attempt_index FROM ingest_jobs WHERE job_id=$1`, jobID,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("readback status #1: %v", err)
	}
	if status != "pending" {
		t.Errorf("status after pass 1 = %q, want 'pending' (requeue path)", status)
	}
	if attempts != 1 {
		t.Errorf("attempt_index after pass 1 = %d, want 1", attempts)
	}
	if got := len(failingPub.events()); got != 0 {
		t.Errorf("publisher recorded %d events during outage; want 0", got)
	}

	// Pass 2: clear the publisher fault and re-run. The same
	// row must move pending->done with both events delivered.
	failingPub.mu.Lock()
	failingPub.failOn = nil
	failingPub.mu.Unlock()

	processed, err = w.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("ProcessOnce #2: %v", err)
	}
	if !processed {
		t.Fatal("ProcessOnce #2 returned processed=false; want true")
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text, attempt_index FROM ingest_jobs WHERE job_id=$1`, jobID,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("readback status #2: %v", err)
	}
	if status != "done" {
		t.Errorf("status after pass 2 = %q, want 'done'", status)
	}
	if attempts != 2 {
		t.Errorf("attempt_index after pass 2 = %d, want 2 (one failed claim + one success)", attempts)
	}
	kinds := map[string]int{}
	for _, ev := range failingPub.events() {
		kinds[ev.Kind]++
	}
	if kinds[EventKindRepoRegistered] != 1 {
		t.Errorf("EventKindRepoRegistered count after pass 2 = %d, want 1",
			kinds[EventKindRepoRegistered])
	}
	if kinds[EventKindRepoFullIngested] != 1 {
		t.Errorf("EventKindRepoFullIngested count after pass 2 = %d, want 1",
			kinds[EventKindRepoFullIngested])
	}
}

// TestWorker_publishFailure_marksFailedAfterMaxAttempts asserts
// the cap on the requeue-for-retry path. With MaxAttempts=2,
// the worker may re-queue ONCE; the second publish failure
// pushes the row to terminal `failed` so a permanently broken
// publisher cannot hot-loop the materializer forever.
func TestWorker_publishFailure_marksFailedAfterMaxAttempts(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := "https://example.test/publish-cap"
	sha := "cap-sha"
	_, jobID := seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)

	mat := &InMemoryMaterializer{Files: []InMemoryFile{
		{RelPath: "a.go", Content: []byte("package a\n")},
	}}
	failingPub := &recordingEventPublisher{
		failOn: map[string]error{
			EventKindRepoRegistered:   errors.New("permanent outage"),
			EventKindRepoFullIngested: errors.New("permanent outage"),
		},
	}
	w := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "cap-worker",
		Materializer: mat,
		Emitter:      NoopASTEmitter{},
		Publisher:    failingPub,
		MaxAttempts:  2,
	})

	// First attempt: AttemptIndex becomes 1 < MaxAttempts(2),
	// row requeues to pending.
	if _, err := w.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce #1: %v", err)
	}
	var status string
	var attempts int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text, attempt_index FROM ingest_jobs WHERE job_id=$1`, jobID,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("readback after #1: %v", err)
	}
	if status != "pending" || attempts != 1 {
		t.Fatalf("after pass 1: status=%q attempts=%d; want pending,1", status, attempts)
	}

	// Second attempt: AttemptIndex becomes 2 == MaxAttempts(2),
	// the requeue branch's `< MaxAttempts` predicate is false,
	// row gets pushed to terminal `failed`.
	if _, err := w.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce #2: %v", err)
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text, attempt_index FROM ingest_jobs WHERE job_id=$1`, jobID,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("readback after #2: %v", err)
	}
	if status != "failed" {
		t.Errorf("after pass 2: status=%q; want 'failed' (MaxAttempts cap)", status)
	}
	if attempts != 2 {
		t.Errorf("after pass 2: attempts=%d; want 2", attempts)
	}
	if got := len(failingPub.events()); got != 0 {
		t.Errorf("publisher recorded %d events despite permanent outage; want 0", got)
	}
}

// TestWorker_repoRegisteredFiresOnRetryAfterCommitWasInserted
// pins the fix #1 contract: the `repo.registered` event must
// fire on the FIRST `done` row for a (repo_id, mode='full',
// from_sha, to_sha) tuple, even when an earlier attempt's
// EnsureCommit succeeded but a later step failed (so the
// `repo_commit` row already exists and EnsureCommit returns
// Inserted=false on retry).
//
// Setup mirrors the failure-then-recovery path the prior
// implementation regressed on:
//
//  1. Pre-insert a `repo_commit` row directly via the owner DB
//     so EnsureCommit on the worker's run will return
//     Inserted=false. (Equivalent to "the previous attempt
//     reached EnsureCommit before failing".)
//  2. Seed a fresh pending ingest_jobs row.
//  3. Run the worker once.
//  4. Assert: repo.registered DID fire (proving the predicate
//     no longer keys off CommitInserted), and CommitInserted
//     in the in-process FullSummary IS false (proving the
//     fixture really did simulate the regression scenario).
func TestWorker_repoRegisteredFiresOnRetryAfterCommitWasInserted(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := "https://example.test/registered-on-retry"
	sha := "retry-after-commit"
	repoID, jobID := seedRepoAndJob(t, ctx, gw, fix, repoURL, sha)

	// Pre-insert the commit row so the worker's EnsureCommit
	// will return Inserted=false. We use the OWNER role here
	// because the app role's grants on `repo_commit` may not
	// allow direct INSERT in test fixtures; the owner has full
	// rights.
	if _, err := fix.owner.ExecContext(ctx, `
		INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at)
		VALUES ($1, $2, NULL, now())
	`, repoID.String(), sha); err != nil {
		t.Fatalf("pre-insert repo_commit: %v", err)
	}

	mat := &InMemoryMaterializer{Files: []InMemoryFile{
		{RelPath: "a.go", Content: []byte("package a\n")},
	}}
	pub := &recordingEventPublisher{}
	w := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "registered-on-retry-worker",
		Materializer: mat,
		Emitter:      NoopASTEmitter{},
		Publisher:    pub,
	})

	// Drive runFull directly so we can introspect the
	// FullSummary AND let ProcessOnce drive the
	// markDoneAndPublish atomic tx. Doing both keeps the test
	// self-checking: if CommitInserted came back true, the
	// fixture didn't actually simulate the regression scenario
	// and the assertion below would be testing nothing.
	job, err := w.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job.JobID != jobID {
		t.Fatalf("claimed wrong job: got %s want %s", job.JobID, jobID)
	}
	if _, err := fix.app.ExecContext(ctx,
		`UPDATE ingest_jobs SET status='running' WHERE job_id=$1`, jobID,
	); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	summary, err := w.runFull(ctx, job)
	if err != nil {
		t.Fatalf("runFull: %v", err)
	}
	if summary.CommitInserted {
		t.Fatal("CommitInserted=true; the fixture did not simulate the post-EnsureCommit retry scenario")
	}
	// markDoneAndPublish is the load-bearing surface here.
	if err := w.markDoneAndPublish(ctx, job, summary); err != nil {
		t.Fatalf("markDoneAndPublish: %v", err)
	}

	// The new predicate is "no prior done row for
	// (repo, mode='full', from_sha, to_sha)" -- so even though
	// EnsureCommit returned Inserted=false, repo.registered
	// MUST still fire because no prior done job exists.
	kinds := map[string]int{}
	for _, ev := range pub.events() {
		kinds[ev.Kind]++
	}
	if kinds[EventKindRepoRegistered] != 1 {
		t.Errorf("EventKindRepoRegistered count = %d, want 1 -- fix #1 regressed; predicate must use 'first done', not CommitInserted",
			kinds[EventKindRepoRegistered])
	}
	if kinds[EventKindRepoFullIngested] != 1 {
		t.Errorf("EventKindRepoFullIngested count = %d, want 1", kinds[EventKindRepoFullIngested])
	}

	// Sanity: row reached done.
	var status string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id=$1`, jobID,
	).Scan(&status); err != nil {
		t.Fatalf("readback status: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}

	// And: a SECOND ingest_jobs row for the SAME (repo, sha)
	// is rejected by the dedupe UNIQUE INDEX, so the
	// concurrency invariant my fix #1 predicate relies on
	// (only one row per tuple) is enforced at the database
	// level. We assert that defensively here so a future
	// schema relaxation that drops the unique key surfaces as
	// a test regression rather than a silent registered-event
	// duplication.
	_, dupErr := fix.app.ExecContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, from_sha, to_sha, status)
		VALUES ($1, 'full', NULL, $2, 'pending')
	`, repoID.String(), sha)
	if dupErr == nil {
		t.Errorf("expected duplicate ingest_jobs INSERT to be rejected by UNIQUE index; got nil error")
	}
}
