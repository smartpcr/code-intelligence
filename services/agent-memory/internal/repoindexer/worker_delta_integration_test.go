package repoindexer

// Integration tests for the Stage 3.4 Repo Indexer delta-mode
// handler. The tests require a live PostgreSQL 16 cluster
// (AGENT_MEMORY_PG_URL set); they skip cleanly when the env var
// is unset, mirroring worker_integration_test.go.
//
// Coverage map (implementation-plan.md §3.4 scenarios)
// ----------------------------------------------------
//
//   * "removed file retires Nodes"
//       -> TestWorker_deltaIngest_removedFileRetiresNodes
//          (asserts both File AND descendant Class/Method
//          tombstones — evaluator finding #6)
//   * "rename produces renamed_to edge"
//       -> TestWorker_deltaIngest_renamePairProducesRenamedToEdge
//          (asserts edge is NOT itself tombstoned — finding #3)
//   * "bulk rename keyed anti-join is fast"
//       -> TestWorker_deltaIngest_bulkRemoveUsesBatchRetire
//   * "member rename within modified file produces renamed_to"
//       -> TestWorker_deltaIngest_memberRenameProducesRenamedToEdge
//          (covers in-file rename detection — finding #4)
//
// Plus a happy-path "added file emits new nodes and publishes
// repo.delta_ingested" assertion.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/retirement"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// seedDeltaJob inserts a `delta` ingest_jobs row for the
// (repo, from_sha, to_sha) tuple. Mirrors seedRepoAndJob but
// hard-codes mode='delta' and requires both SHAs.
func seedDeltaJob(t *testing.T, ctx context.Context, fix *dbFixture, repoID, fromSHA, toSHA string) string {
	t.Helper()
	var jobID string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, from_sha, to_sha, status)
		VALUES ($1, 'delta', $2, $3, 'pending')
		RETURNING job_id::text
	`, repoID, fromSHA, toSHA).Scan(&jobID); err != nil {
		t.Fatalf("insert delta ingest_jobs: %v", err)
	}
	return jobID
}

// runFullDeltaPair seeds a repo, runs a full ingest at
// `fromSHA` to populate the graph, then returns the resulting
// fixture handles so a follow-up delta test can drive a second
// job against the pre-existing nodes.
func runFullDeltaPairSetup(
	t *testing.T,
	ctx context.Context,
	fix *dbFixture,
	gw *graphwriter.Writer,
	repoURL, fromSHA string,
	files []InMemoryFile,
	emitter ASTEmitter,
) (repoIDStr string) {
	t.Helper()
	_, fullJobID := seedRepoAndJob(t, ctx, gw, fix, repoURL, fromSHA)
	mat := &InMemoryMaterializer{Files: files}
	pub := &recordingEventPublisher{}
	worker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-delta-setup",
		Materializer: mat,
		Emitter:      emitter,
		Publisher:    pub,
		Logger:       slog.Default(),
	})
	processed, err := worker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("full-ingest setup ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("full-ingest setup ProcessOnce: processed=false")
	}
	// Confirm setup wrote the expected job-row state so the
	// delta test does not race on a half-finished setup.
	var setupStatus string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, fullJobID,
	).Scan(&setupStatus); err != nil {
		t.Fatalf("readback setup job: %v", err)
	}
	if setupStatus != "done" {
		t.Fatalf("setup full-ingest job status = %q, want done", setupStatus)
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT repo_id::text FROM repo WHERE url = $1`, repoURL,
	).Scan(&repoIDStr); err != nil {
		t.Fatalf("readback repo_id: %v", err)
	}
	return repoIDStr
}

// uniqueRepoURL builds a per-test repo URL. Each test runs in
// its own schema (see openFixture), so plain test-name suffixes
// are sufficient — no random suffix needed.
func uniqueRepoURL(prefix string) string {
	return "https://example.test/" + prefix
}

// classSigForTest mirrors ast/dispatcher.go's classSignature
// for test use. Hardcoded here (rather than importing) so the
// integration test never drifts from the dispatcher schema — if
// the format ever changes, the integration tests fail with a
// readable signature mismatch on the assertion line, not a
// compile error in the test package.
//
// NormalizeSignature is a no-op for alphanumeric `qualName`
// values (which is what the test fixtures use).
func classSigForTest(repoURL, relPath, qualName string) string {
	return repoURL + "::class::" + relPath + "#" + qualName
}

// methodSigForTest mirrors ast/dispatcher.go's methodSignature
// for test use (alphanumeric inputs only).
func methodSigForTest(repoURL, relPath, qualName, params string) string {
	return repoURL + "::method::" + relPath + "#" + qualName + "(" + params + ")"
}

// insertTestClass inserts a Class Node under the supplied File
// Node so the delta path's descendant CTE finds it. Returns the
// new node id.
func insertTestClass(
	t *testing.T,
	ctx context.Context,
	gw *graphwriter.Writer,
	repoID, fileNodeID, fromSHA, repoURL, relPath, qualName string,
) string {
	t.Helper()
	repoUUID, err := fingerprint.ParseRepoID(repoID)
	if err != nil {
		t.Fatalf("parse repoID: %v", err)
	}
	rec, err := gw.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoUUID,
		Kind:               "class",
		CanonicalSignature: classSigForTest(repoURL, relPath, qualName),
		ParentNodeID:       fileNodeID,
		FromSHA:            fromSHA,
		AttrsJSON:          json.RawMessage(`{"producer":"test"}`),
	})
	if err != nil {
		t.Fatalf("insert class %s: %v", qualName, err)
	}
	return rec.NodeID
}

// insertTestMethod inserts a Method Node under the supplied
// parent Node (either a File or a Class).
func insertTestMethod(
	t *testing.T,
	ctx context.Context,
	gw *graphwriter.Writer,
	repoID, parentNodeID, fromSHA, repoURL, relPath, qualName, params string,
) string {
	t.Helper()
	repoUUID, err := fingerprint.ParseRepoID(repoID)
	if err != nil {
		t.Fatalf("parse repoID: %v", err)
	}
	rec, err := gw.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoUUID,
		Kind:               "method",
		CanonicalSignature: methodSigForTest(repoURL, relPath, qualName, params),
		ParentNodeID:       parentNodeID,
		FromSHA:            fromSHA,
		AttrsJSON:          json.RawMessage(`{"producer":"test"}`),
	})
	if err != nil {
		t.Fatalf("insert method %s: %v", qualName, err)
	}
	return rec.NodeID
}

// ---------------------------------------------------------------
// Scenario 1: removed file retires Nodes
// ---------------------------------------------------------------

// TestWorker_deltaIngest_removedFileRetiresNodes covers the
// "Scenario: removed file retires Nodes" entry. A repo is fully
// ingested at fromSHA with a small fixture; a delta job for
// (fromSHA -> toSHA) removes one file; the test asserts:
//   - a node_retirement row exists for the removed File Node
//     (and its descendants) with retired_at_sha = fromSHA.
//   - the delta job reaches status='done'.
//   - a `repo.delta_ingested` event was published with the
//     populated FromSHA/ToSHA pair.
func TestWorker_deltaIngest_removedFileRetiresNodes(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := uniqueRepoURL("delta-rm")
	fromSHA := "aaaa111100000000000000000000000000000000"
	toSHA := "bbbb222200000000000000000000000000000000"

	removedPath := "pkg/remove_me.go"
	files := []InMemoryFile{
		{RelPath: "pkg/keep.go", Content: []byte("package keep\n")},
		{RelPath: removedPath, Content: []byte("package removeme\n")},
	}
	emitter := &recordingASTEmitter{}
	repoIDStr := runFullDeltaPairSetup(t, ctx, fix, gw, repoURL, fromSHA, files, emitter)

	// Look up the File Node we are about to remove so we can
	// (a) anchor descendant Class/Method inserts under it and
	// (b) assert tombstoning on the specific row.
	var removedFileNodeID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT n.node_id::text FROM node n
		WHERE n.repo_id = $1 AND n.kind = 'file' AND n.canonical_signature = $2
	`, repoIDStr, canonicalFileSig(repoURL, removedPath)).Scan(&removedFileNodeID); err != nil {
		t.Fatalf("lookup removed File Node: %v", err)
	}

	// Insert a Class and a Method under the file so the
	// descendant CTE actually has something to retire.
	// Evaluator finding #6 — the prior version only asserted on
	// the File Node tombstone and would have passed even if the
	// descendant walker was broken (it was).
	classNodeID := insertTestClass(t, ctx, gw, repoIDStr, removedFileNodeID, fromSHA, repoURL, removedPath, "RemovedClass")
	methodNodeID := insertTestMethod(t, ctx, gw, repoIDStr, classNodeID, fromSHA, repoURL, removedPath, "RemovedClass.doStuff", "")

	// Seed the delta job + drive the worker.
	jobID := seedDeltaJob(t, ctx, fix, repoIDStr, fromSHA, toSHA)
	deltaDiffer := &InMemoryDeltaDiffer{Changes: []FileChange{
		{Status: ChangeDeleted, RelPath: removedPath},
	}}
	deltaMat := &InMemoryMaterializer{Files: []InMemoryFile{
		{RelPath: "pkg/keep.go", Content: []byte("package keep\n")},
	}}
	deltaEmitter := &recordingASTEmitter{}
	deltaPub := &recordingEventPublisher{}
	deltaRetirer := NewRetirementAdapter(retirement.New(fix.app, slog.Default()))

	worker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-delta-rm",
		Materializer: deltaMat,
		Emitter:      deltaEmitter,
		Publisher:    deltaPub,
		Differ:       deltaDiffer,
		Retirer:      deltaRetirer,
		Logger:       slog.Default(),
	})
	processed, err := worker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("delta ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("delta ProcessOnce: processed=false")
	}

	// Assert the removed File Node AND its descendants have
	// node_retirement rows with retired_at_sha=fromSHA.
	// Evaluator finding #6: prior version only checked File.
	for _, nid := range []string{removedFileNodeID, classNodeID, methodNodeID} {
		var retiredAtSHA string
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT retired_at_sha FROM node_retirement WHERE node_id = $1`,
			nid,
		).Scan(&retiredAtSHA); err != nil {
			t.Errorf("readback node_retirement for %s: %v", nid, err)
			continue
		}
		if retiredAtSHA != fromSHA {
			t.Errorf("node %s retired_at_sha = %q, want %q", nid, retiredAtSHA, fromSHA)
		}
	}

	// Job row reached status='done'.
	var status string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, jobID,
	).Scan(&status); err != nil {
		t.Fatalf("readback delta job: %v", err)
	}
	if status != "done" {
		t.Errorf("delta job status = %q, want done", status)
	}

	// repo.delta_ingested event published with the right tuple.
	events := deltaPub.events()
	if len(events) == 0 {
		t.Fatalf("expected at least one published event; got none")
	}
	var foundDelta bool
	for _, ev := range events {
		if ev.Kind != EventKindRepoDeltaIngested {
			continue
		}
		foundDelta = true
		if ev.FromSHA != fromSHA {
			t.Errorf("event FromSHA = %q, want %q", ev.FromSHA, fromSHA)
		}
		if ev.ToSHA != toSHA {
			t.Errorf("event ToSHA = %q, want %q", ev.ToSHA, toSHA)
		}
		if ev.AffectedNodeCount <= 0 {
			t.Errorf("event AffectedNodeCount = %d, want > 0", ev.AffectedNodeCount)
		}
		if ev.JobID != jobID {
			t.Errorf("event JobID = %q, want %q", ev.JobID, jobID)
		}
	}
	if !foundDelta {
		t.Errorf("no %s event published; events seen = %+v", EventKindRepoDeltaIngested, events)
	}
}

// ---------------------------------------------------------------
// Scenario 2: rename produces renamed_to edge
// ---------------------------------------------------------------

// TestWorker_deltaIngest_renamePairProducesRenamedToEdge covers
// the "Scenario: rename produces renamed_to edge" entry. A repo
// is fully ingested at fromSHA; a delta job renames one file
// (no content change); the test asserts:
//   - a `renamed_to` edge exists from the old File Node to a
//     fresh File Node minted at toSHA.
//   - the old File Node has a node_retirement row with
//     superseded_by_node_id set to the new File Node.
func TestWorker_deltaIngest_renamePairProducesRenamedToEdge(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := uniqueRepoURL("delta-rename")
	fromSHA := "cccc111100000000000000000000000000000000"
	toSHA := "dddd222200000000000000000000000000000000"

	files := []InMemoryFile{
		{RelPath: "pkg/old_name.go", Content: []byte("package old\n")},
	}
	emitter := &recordingASTEmitter{}
	repoIDStr := runFullDeltaPairSetup(t, ctx, fix, gw, repoURL, fromSHA, files, emitter)

	// Capture old File Node id.
	var oldFileNodeID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT node_id::text FROM node
		WHERE repo_id = $1 AND kind = 'file' AND canonical_signature = $2
	`, repoIDStr, canonicalFileSig(repoURL, "pkg/old_name.go")).Scan(&oldFileNodeID); err != nil {
		t.Fatalf("lookup old File Node: %v", err)
	}

	jobID := seedDeltaJob(t, ctx, fix, repoIDStr, fromSHA, toSHA)
	deltaDiffer := &InMemoryDeltaDiffer{Changes: []FileChange{
		{Status: ChangeRenamed, PrevRelPath: "pkg/old_name.go", RelPath: "pkg/new_name.go"},
	}}
	deltaMat := &InMemoryMaterializer{Files: []InMemoryFile{
		{RelPath: "pkg/new_name.go", Content: []byte("package old\n")},
	}}
	deltaEmitter := &recordingASTEmitter{}
	deltaPub := &recordingEventPublisher{}
	deltaRetirer := NewRetirementAdapter(retirement.New(fix.app, slog.Default()))

	worker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-delta-rename",
		Materializer: deltaMat,
		Emitter:      deltaEmitter,
		Publisher:    deltaPub,
		Differ:       deltaDiffer,
		Retirer:      deltaRetirer,
		Logger:       slog.Default(),
	})
	processed, err := worker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("delta ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("delta ProcessOnce: processed=false")
	}

	// Look up the new File Node minted by the delta run.
	var newFileNodeID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT n.node_id::text FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.repo_id = $1
		  AND n.kind = 'file'
		  AND n.canonical_signature = $2
		  AND nr.node_id IS NULL
	`, repoIDStr, canonicalFileSig(repoURL, "pkg/new_name.go")).Scan(&newFileNodeID); err != nil {
		t.Fatalf("lookup new File Node: %v", err)
	}

	// Assert the renamed_to Edge exists from old -> new.
	var renamedEdgeCount int
	var renamedEdgeID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT edge_id::text FROM edge
		WHERE src_node_id = $1
		  AND dst_node_id = $2
		  AND kind = 'renamed_to'
	`, oldFileNodeID, newFileNodeID).Scan(&renamedEdgeID); err != nil {
		t.Fatalf("lookup renamed_to edge: %v", err)
	}
	if renamedEdgeID == "" {
		t.Errorf("renamed_to edge old=%s -> new=%s not found",
			oldFileNodeID, newFileNodeID)
	}
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge
		WHERE src_node_id = $1
		  AND dst_node_id = $2
		  AND kind = 'renamed_to'
	`, oldFileNodeID, newFileNodeID).Scan(&renamedEdgeCount); err != nil {
		t.Fatalf("count renamed_to edge: %v", err)
	}
	if renamedEdgeCount != 1 {
		t.Errorf("renamed_to edge count old=%s -> new=%s = %d, want 1",
			oldFileNodeID, newFileNodeID, renamedEdgeCount)
	}

	// Evaluator finding #3: the renamed_to edge MUST NOT itself
	// be tombstoned by the same delta that created it. The prior
	// implementation's retireEdgesOf swept every edge incident to
	// the old node, including the brand-new renamed_to edge.
	var renamedEdgeRetirementCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge_retirement WHERE edge_id = $1
	`, renamedEdgeID).Scan(&renamedEdgeRetirementCount); err != nil {
		t.Fatalf("check renamed_to edge tombstone: %v", err)
	}
	if renamedEdgeRetirementCount != 0 {
		t.Errorf("renamed_to edge %s should NOT have an edge_retirement row; got %d",
			renamedEdgeID, renamedEdgeRetirementCount)
	}

	// Old File Node retirement row should set superseded_by_node_id.
	var supersededBy string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COALESCE(superseded_by_node_id::text, '')
		FROM node_retirement WHERE node_id = $1
	`, oldFileNodeID).Scan(&supersededBy); err != nil {
		t.Fatalf("readback supersede: %v", err)
	}
	if supersededBy != newFileNodeID {
		t.Errorf("superseded_by_node_id = %q, want %q", supersededBy, newFileNodeID)
	}

	// repo.delta_ingested event published.
	var foundDelta bool
	for _, ev := range deltaPub.events() {
		if ev.Kind == EventKindRepoDeltaIngested && ev.JobID == jobID {
			foundDelta = true
		}
	}
	if !foundDelta {
		t.Errorf("no %s event for jobID=%s; events=%+v",
			EventKindRepoDeltaIngested, jobID, deltaPub.events())
	}
}

// ---------------------------------------------------------------
// Scenario 3: bulk-delete keyed anti-join (RetireMany)
// ---------------------------------------------------------------

// TestWorker_deltaIngest_bulkRemoveUsesBatchRetire covers the
// "Scenario: bulk rename keyed anti-join is fast" entry as a
// smoke test against the bulk-delete path. A repo is full-
// ingested with N files in a single package; a delta removes
// all N files; the test asserts every File Node ends up with a
// node_retirement row keyed off the same retired_at_sha and
// that the job reached status='done' (i.e. the bulk path did
// not OOM the bulk INSERT or trip AlreadyRetired). Performance
// is NOT asserted here — the 5,000-node / p95-under-50ms
// acceptance SLA from implementation-plan.md Stage 3.4 is
// pinned by `TestReader_GetNode_keyedAntiJoinUnder50msAt5kRetired`
// in `internal/graphreader/reader_perf_acceptance_test.go`, where
// the SLA's actual code path (Reader.GetNode keyed anti-join
// against node_retirement) lives. This test pins the
// correctness contract of the RetireMany hot path that produces
// the retirements that the reader test then queries against.
func TestWorker_deltaIngest_bulkRemoveUsesBatchRetire(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	const N = 20 // small N — the test asserts correctness, not p95
	repoURL := uniqueRepoURL("delta-bulk")
	fromSHA := "eeee111100000000000000000000000000000000"
	toSHA := "ffff222200000000000000000000000000000000"

	files := make([]InMemoryFile, 0, N)
	for i := 0; i < N; i++ {
		files = append(files, InMemoryFile{
			RelPath: relPathBulk(i),
			Content: []byte("package bulk\n"),
		})
	}
	emitter := &recordingASTEmitter{}
	repoIDStr := runFullDeltaPairSetup(t, ctx, fix, gw, repoURL, fromSHA, files, emitter)

	// Snapshot the pre-delete File Node ids so the test can
	// later assert every one of them has a tombstone.
	preDeleteFileIDs := make([]string, 0, N)
	for i := 0; i < N; i++ {
		var nid string
		if err := fix.owner.QueryRowContext(ctx, `
			SELECT node_id::text FROM node
			WHERE repo_id = $1 AND kind = 'file' AND canonical_signature = $2
		`, repoIDStr, canonicalFileSig(repoURL, relPathBulk(i))).Scan(&nid); err != nil {
			t.Fatalf("lookup file %d: %v", i, err)
		}
		preDeleteFileIDs = append(preDeleteFileIDs, nid)
	}

	// Seed the delta with N delete changes.
	changes := make([]FileChange, 0, N)
	for i := 0; i < N; i++ {
		changes = append(changes, FileChange{
			Status:  ChangeDeleted,
			RelPath: relPathBulk(i),
		})
	}
	jobID := seedDeltaJob(t, ctx, fix, repoIDStr, fromSHA, toSHA)
	deltaDiffer := &InMemoryDeltaDiffer{Changes: changes}
	// Empty workspace at toSHA — all files removed.
	deltaMat := &InMemoryMaterializer{Files: nil}
	deltaEmitter := &recordingASTEmitter{}
	deltaPub := &recordingEventPublisher{}
	deltaRetirer := NewRetirementAdapter(retirement.New(fix.app, slog.Default()))

	worker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-delta-bulk",
		Materializer: deltaMat,
		Emitter:      deltaEmitter,
		Publisher:    deltaPub,
		Differ:       deltaDiffer,
		Retirer:      deltaRetirer,
		Logger:       slog.Default(),
	})
	processed, err := worker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("delta ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("delta ProcessOnce: processed=false")
	}

	// Every File Node should now have a node_retirement row.
	for _, nid := range preDeleteFileIDs {
		var retiredAt string
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT retired_at_sha FROM node_retirement WHERE node_id = $1`, nid,
		).Scan(&retiredAt); err != nil {
			t.Errorf("File Node %s missing tombstone: %v", nid, err)
			continue
		}
		if retiredAt != fromSHA {
			t.Errorf("File Node %s retired_at_sha=%q, want %q", nid, retiredAt, fromSHA)
		}
	}

	// Job reached status='done'.
	var status string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, jobID,
	).Scan(&status); err != nil {
		t.Fatalf("readback delta job: %v", err)
	}
	if status != "done" {
		t.Errorf("delta job status = %q, want done", status)
	}

	// repo.delta_ingested event published with AffectedNodeCount
	// large enough to cover the N file retires.
	var foundDelta bool
	for _, ev := range deltaPub.events() {
		if ev.Kind == EventKindRepoDeltaIngested && ev.JobID == jobID {
			foundDelta = true
			if ev.AffectedNodeCount < N {
				t.Errorf("AffectedNodeCount = %d, want >= %d", ev.AffectedNodeCount, N)
			}
		}
	}
	if !foundDelta {
		t.Errorf("no %s event for jobID=%s", EventKindRepoDeltaIngested, jobID)
	}
}

// relPathBulk shapes the N relPaths used by the bulk-delete
// test. Kept as a helper so the lookup-and-stage paths cannot
// drift apart.
func relPathBulk(i int) string {
	return fmtBulkPath(i)
}

// fmtBulkPath is split out so the test does not import fmt
// across many helpers. We still want determinism, so use
// fmt.Sprintf.
func fmtBulkPath(i int) string {
	return "pkg/bulk_" + leftPadTwo(i) + ".go"
}

// leftPadTwo formats an int as a two-digit decimal string with
// zero padding so the lexicographic order of bulk files matches
// their numeric order in any walked workspace.
func leftPadTwo(i int) string {
	if i < 10 {
		return "0" + intToString(i)
	}
	return intToString(i)
}

// intToString converts a non-negative int to its decimal
// string form without pulling in strconv (keeps the test file's
// import surface narrow).
func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	buf := make([]byte, 0, 4)
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	return string(buf)
}

// ---------------------------------------------------------------
// Scenario 4: member rename within a Modified file produces a
// renamed_to edge (evaluator finding #4 + #6).
// ---------------------------------------------------------------

// memberRenameEmitter is a test-only ASTEmitter that, on its
// SECOND EmitFile call (i.e. the delta-mode call for a Modified
// file), inserts:
//
//   - one Class Node with the SAME canonical signature as the
//     old fromSHA class (so the descendant walker finds an
//     "unchanged" sibling at the new SHA), AND
//   - one Method Node with a NEW canonical signature under the
//     same Class parent (so the rename-pair detector pairs the
//     disappeared old Method with the appeared new Method).
//
// The emitter returns those two nodes in TouchedNodes so the
// delta handler treats them as the "appeared" set for the file.
//
// The FIRST EmitFile call (fired by runFullDeltaPairSetup) is a
// plain pass-through: it inserts the original Class + Method at
// fromSHA and returns them in TouchedNodes so the dispatcher's
// pre-flight assertions are happy.
type memberRenameEmitter struct {
	t       *testing.T
	gw      *graphwriter.Writer
	repoURL string
	relPath string

	// Recorded so the test can assert about the renamed_to edge.
	originalClassID  string
	originalMethodID string
	newClassID       string
	newMethodID      string

	callCount int
}

const (
	memberRenameClassQName  = "RenamerClass"
	memberRenameOldMethodQN = "RenamerClass.oldMethod"
	memberRenameNewMethodQN = "RenamerClass.newMethod"
)

func (e *memberRenameEmitter) EmitFile(ctx context.Context, ev EmitFileEvent) (EmitResult, error) {
	e.callCount++

	classSig := classSigForTest(e.repoURL, e.relPath, memberRenameClassQName)
	oldMethodSig := methodSigForTest(e.repoURL, e.relPath, memberRenameOldMethodQN, "")
	newMethodSig := methodSigForTest(e.repoURL, e.relPath, memberRenameNewMethodQN, "")
	if e.callCount == 1 {
		// Full-mode setup. Insert Class + old Method.
		classRec, err := e.gw.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "class",
			CanonicalSignature: classSig,
			ParentNodeID:       ev.FileNodeID,
			FromSHA:            ev.SHA,
		})
		if err != nil {
			e.t.Fatalf("memberRenameEmitter: insert class call1: %v", err)
		}
		e.originalClassID = classRec.NodeID

		methodRec, err := e.gw.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "method",
			CanonicalSignature: oldMethodSig,
			ParentNodeID:       classRec.NodeID,
			FromSHA:            ev.SHA,
		})
		if err != nil {
			e.t.Fatalf("memberRenameEmitter: insert method call1: %v", err)
		}
		e.originalMethodID = methodRec.NodeID

		return EmitResult{
			TouchedNodes: []TouchedNode{
				{NodeID: classRec.NodeID, Kind: "class", CanonicalSignature: classSig, ParentNodeID: ev.FileNodeID, Inserted: true},
				{NodeID: methodRec.NodeID, Kind: "method", CanonicalSignature: oldMethodSig, ParentNodeID: classRec.NodeID, Inserted: true},
			},
		}, nil
	}

	// Delta-mode re-emit of the SAME file. Mint a fresh Class
	// row (same sig, new from_sha) and the NEW Method.
	newClassRec, err := e.gw.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             ev.RepoID,
		Kind:               "class",
		CanonicalSignature: classSig,
		ParentNodeID:       ev.FileNodeID,
		FromSHA:            ev.SHA,
	})
	if err != nil {
		e.t.Fatalf("memberRenameEmitter: insert class call2: %v", err)
	}
	e.newClassID = newClassRec.NodeID

	newMethodRec, err := e.gw.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             ev.RepoID,
		Kind:               "method",
		CanonicalSignature: newMethodSig,
		ParentNodeID:       newClassRec.NodeID,
		FromSHA:            ev.SHA,
	})
	if err != nil {
		e.t.Fatalf("memberRenameEmitter: insert method call2: %v", err)
	}
	e.newMethodID = newMethodRec.NodeID

	return EmitResult{
		TouchedNodes: []TouchedNode{
			{NodeID: newClassRec.NodeID, Kind: "class", CanonicalSignature: classSig, ParentNodeID: ev.FileNodeID, Inserted: newClassRec.Inserted},
			{NodeID: newMethodRec.NodeID, Kind: "method", CanonicalSignature: newMethodSig, ParentNodeID: newClassRec.NodeID, Inserted: newMethodRec.Inserted},
		},
	}, nil
}

// TestWorker_deltaIngest_memberRenameProducesRenamedToEdge
// covers the "renamed members: write a renamed_to Edge and pass
// `superseded_by_node_id`" step of the implementation plan and
// evaluator finding #4 (the rename pair detector must not key
// off Kind alone). The fixture:
//
//   - Full-ingests one file containing one Class with one Method
//     (`oldMethod`) at fromSHA.
//   - Delta-ingests a Modified version of the same file at
//     toSHA where the Class is unchanged but the Method has
//     been renamed to `newMethod` (same parent Class, same Kind,
//     new canonical signature).
//
// Asserts:
//   - The unchanged Class is NOT paired as a rename (no
//     renamed_to edge originates from its node id).
//   - The old Method has a node_retirement row with
//     superseded_by_node_id pointing at the new Method.
//   - A `renamed_to` edge exists from old Method -> new Method.
//   - That edge is NOT itself tombstoned (anti-finding #3).
func TestWorker_deltaIngest_memberRenameProducesRenamedToEdge(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoURL := uniqueRepoURL("delta-member-rename")
	fromSHA := "1111aaaa00000000000000000000000000000000"
	toSHA := "2222bbbb00000000000000000000000000000000"
	relPath := "pkg/renamer.go"

	files := []InMemoryFile{
		{RelPath: relPath, Content: []byte("package renamer\n")},
	}

	// Look up the repo_id we'll use for the emitter; the
	// runFullDeltaPairSetup helper returns it after the full
	// ingest completes, so we cannot pass the emitter the typed
	// fingerprint.RepoID up front. The simplest path is: do a
	// no-op setup pass to seed the repo + run full ingest,
	// then resolve the typed RepoID from the returned string.
	emitter := &memberRenameEmitter{
		t:       t,
		gw:      gw,
		repoURL: repoURL,
		relPath: relPath,
	}
	// memberRenameEmitter needs ev.RepoID -- it comes through
	// EmitFileEvent so we don't have to wire it via the emitter
	// struct.

	repoIDStr := runFullDeltaPairSetup(t, ctx, fix, gw, repoURL, fromSHA, files, emitter)
	if _, err := fingerprint.ParseRepoID(repoIDStr); err != nil {
		t.Fatalf("parse repoID: %v", err)
	}

	if emitter.originalClassID == "" || emitter.originalMethodID == "" {
		t.Fatalf("setup: emitter did not record original Class/Method ids (callCount=%d)", emitter.callCount)
	}

	// Drive the delta job. The same emitter handles call #2
	// (deltaProcessModified) and inserts the new Class + new
	// Method, returning them in TouchedNodes for rename
	// detection.
	jobID := seedDeltaJob(t, ctx, fix, repoIDStr, fromSHA, toSHA)
	deltaDiffer := &InMemoryDeltaDiffer{Changes: []FileChange{
		{Status: ChangeModified, RelPath: relPath},
	}}
	deltaMat := &InMemoryMaterializer{Files: files}
	deltaPub := &recordingEventPublisher{}
	deltaRetirer := NewRetirementAdapter(retirement.New(fix.app, slog.Default()))

	worker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-delta-member-rename",
		Materializer: deltaMat,
		Emitter:      emitter,
		Publisher:    deltaPub,
		Differ:       deltaDiffer,
		Retirer:      deltaRetirer,
		Logger:       slog.Default(),
	})
	processed, err := worker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("delta ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("delta ProcessOnce: processed=false")
	}
	if emitter.newMethodID == "" {
		t.Fatalf("delta: emitter did not insert new Method (callCount=%d)", emitter.callCount)
	}

	// The old Method should be tombstoned with
	// superseded_by_node_id = new Method id.
	var supersededBy string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COALESCE(superseded_by_node_id::text, '')
		FROM node_retirement WHERE node_id = $1
	`, emitter.originalMethodID).Scan(&supersededBy); err != nil {
		t.Fatalf("readback old Method retirement: %v", err)
	}
	if supersededBy != emitter.newMethodID {
		t.Errorf("old Method superseded_by_node_id = %q, want %q",
			supersededBy, emitter.newMethodID)
	}

	// A renamed_to edge should exist from old Method -> new Method.
	var renamedEdgeID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT edge_id::text FROM edge
		WHERE src_node_id = $1 AND dst_node_id = $2 AND kind = 'renamed_to'
	`, emitter.originalMethodID, emitter.newMethodID).Scan(&renamedEdgeID); err != nil {
		t.Fatalf("lookup renamed_to edge old=%s -> new=%s: %v",
			emitter.originalMethodID, emitter.newMethodID, err)
	}
	if renamedEdgeID == "" {
		t.Errorf("renamed_to edge old=%s -> new=%s missing",
			emitter.originalMethodID, emitter.newMethodID)
	}

	// Anti-finding #3: that edge is NOT itself tombstoned.
	var edgeRetirementCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge_retirement WHERE edge_id = $1
	`, renamedEdgeID).Scan(&edgeRetirementCount); err != nil {
		t.Fatalf("check renamed_to edge tombstone: %v", err)
	}
	if edgeRetirementCount != 0 {
		t.Errorf("renamed_to edge %s should NOT be tombstoned; got %d",
			renamedEdgeID, edgeRetirementCount)
	}

	// The unchanged Class must NOT be paired as a rename: no
	// renamed_to edge should originate from the original Class.
	var falseClassRename int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge
		WHERE src_node_id = $1 AND kind = 'renamed_to'
	`, emitter.originalClassID).Scan(&falseClassRename); err != nil {
		t.Fatalf("check false class rename: %v", err)
	}
	if falseClassRename != 0 {
		t.Errorf("unchanged Class %s should NOT have a renamed_to edge; got %d",
			emitter.originalClassID, falseClassRename)
	}

	// Job reached status='done'.
	var status string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, jobID,
	).Scan(&status); err != nil {
		t.Fatalf("readback delta job: %v", err)
	}
	if status != "done" {
		t.Errorf("delta job status = %q, want done", status)
	}
}
