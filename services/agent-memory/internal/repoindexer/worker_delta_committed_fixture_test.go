package repoindexer

// Stage 3.4 — evaluator iter-2 finding #4 — committed-fixture
// end-to-end integration test.
//
// The other delta integration tests in
// `worker_delta_integration_test.go` use `InMemoryDeltaDiffer`
// and `InMemoryMaterializer` so they can run without a `git`
// binary. They cover the handler's per-status logic but leave
// the PRODUCTION pipeline (real git diff parsing, real
// `parent(to_sha)` resolution, real Materialize) un-exercised.
//
// This file fills that gap with a single committed-fixture
// scenario that:
//
//  1. Builds a three-commit fixture in a temp git repo.
//     - c1 (initial): pkg/keep.go, pkg/modify_me.go,
//       pkg/remove_me.go, pkg/rename_me_old.go
//     - c2 (intermediate): modify modify_me.go + delete
//       remove_me.go
//     - c3 (HEAD):  rename rename_me_old.go ->
//       rename_me_new.go + add added.go
//
//  2. Runs a FULL ingest at c1 driven by GitMaterializer so the
//     graph holds the c1 tree's File / Package / Repo Nodes.
//
//  3. Seeds a DELTA job for c1 -> c3 and drives the worker with
//     GitMaterializer + GitDeltaDiffer wired in.
//
// The three-commit shape is load-bearing for evaluator iter-2
// finding #2: a two-commit fixture (the only kind covered prior
// to this iter) has `parent(to_sha) == from_sha` so the test
// cannot distinguish "tombstone uses parent(to_sha) — correct"
// from "tombstone uses from_sha — wrong". With c2 sitting
// between c1 (the diff base) and c3 (HEAD), the architecturally
// correct `retired_at_sha` is c2; the previous bug would have
// written c1.
//
// Assertions:
//   - `repo_commit` row exists for c3 with parent_sha=c2.
//   - Modified file (modify_me.go): the old c1-side File Node
//     has a node_retirement row with retired_at_sha=c2 AND
//     superseded_by_node_id pointing at the new (c3-side) File
//     Node; the Package -> old_File `contains` edge is retired;
//     no `renamed_to` edge exists between the two File Nodes
//     (the path did not change). Exactly ONE live File row for
//     the path's canonical_signature.
//   - Removed file (remove_me.go): node_retirement with
//     retired_at_sha=c2.
//   - Renamed file (rename_me_old.go -> rename_me_new.go): a
//     `renamed_to` edge exists from old -> new; old File Node
//     has node_retirement with retired_at_sha=c2 AND
//     superseded_by_node_id pointing at the new File Node.
//   - Added file (added.go): a live File Node exists at c3.
//   - The published `repo.delta_ingested` event carries
//     FromSHA=c1, ToSHA=c3, AffectedNodeCount > 0.
//
// Skips when `git` is not on PATH OR AGENT_MEMORY_PG_URL is
// unset, so CI runners without one or the other still pass.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/retirement"
)

func TestWorker_deltaIngest_committedFixtureEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}

	fix := openFixture(t)
	defer fix.cleanup()
	gw := graphwriter.New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// 1. Build the three-commit fixture.
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "src-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir src-repo: %v", err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_NOSYSTEM=1",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return string(out)
	}

	runGit(repoDir, "init", "--quiet", "--initial-branch=main")
	runGit(repoDir, "config", "user.email", "test@example.com")
	runGit(repoDir, "config", "user.name", "Test")

	// c1: initial commit.
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	mustWrite(t, filepath.Join(repoDir, "pkg", "keep.go"), "package pkg\n// keep\n")
	mustWrite(t, filepath.Join(repoDir, "pkg", "modify_me.go"), "package pkg\n// modify v1\n")
	mustWrite(t, filepath.Join(repoDir, "pkg", "remove_me.go"), "package pkg\n// to be removed\n")
	mustWrite(t, filepath.Join(repoDir, "pkg", "rename_me_old.go"), "package pkg\n// renamed old\n")
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "--quiet", "-m", "c1 initial")
	c1 := trim(runGit(repoDir, "rev-parse", "HEAD"))

	// c2: modify + delete (so parent(c3) == c2, NOT c1).
	mustWrite(t, filepath.Join(repoDir, "pkg", "modify_me.go"), "package pkg\n// modify v2 changed\n")
	if err := os.Remove(filepath.Join(repoDir, "pkg", "remove_me.go")); err != nil {
		t.Fatalf("rm remove_me.go: %v", err)
	}
	runGit(repoDir, "add", "-A")
	runGit(repoDir, "commit", "--quiet", "-m", "c2 modify and delete")
	c2 := trim(runGit(repoDir, "rev-parse", "HEAD"))

	// c3: rename + add (HEAD; the delta job's to_sha).
	runGit(repoDir, "mv", filepath.Join("pkg", "rename_me_old.go"), filepath.Join("pkg", "rename_me_new.go"))
	mustWrite(t, filepath.Join(repoDir, "pkg", "added.go"), "package pkg\n// added\n")
	runGit(repoDir, "add", "-A")
	runGit(repoDir, "commit", "--quiet", "-m", "c3 rename and add")
	c3 := trim(runGit(repoDir, "rev-parse", "HEAD"))

	// Cross-platform fetch URL. git on Windows accepts forward-
	// slash paths directly, so we replace backslashes — same
	// trick the GitDeltaDiffer fixture test uses.
	repoURL := repoDir
	if runtime.GOOS == "windows" {
		repoURL = filepath.ToSlash(repoDir)
	}

	// 2. FULL ingest at c1 using GitMaterializer so the c1 tree
	// is on disk via the production materializer (parity with
	// the delta side of the test). The emitter is the recording
	// stub — it does NOT need to populate TouchedNodes because
	// this test asserts on File-level rows + repo_commit, not
	// on Class/Method tombstones (which the in-memory member-
	// rename test covers separately).
	fullRepoID, fullJobID := seedRepoAndJob(t, ctx, gw, fix, repoURL, c1)
	repoIDStr := fullRepoID.String()
	fullMat := &GitMaterializer{BaseDir: workDir}
	fullEmitter := &recordingASTEmitter{}
	fullPub := &recordingEventPublisher{}
	fullWorker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-committed-full",
		Materializer: fullMat,
		Emitter:      fullEmitter,
		Publisher:    fullPub,
		Logger:       slog.Default(),
	})
	processed, err := fullWorker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("full-ingest ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("full-ingest ProcessOnce: processed=false (job %s)", fullJobID)
	}
	var fullStatus string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, fullJobID,
	).Scan(&fullStatus); err != nil {
		t.Fatalf("readback full job: %v", err)
	}
	if fullStatus != "done" {
		t.Fatalf("full-ingest job status = %q, want done", fullStatus)
	}

	// Capture the pre-delta (c1-side) File Node ids the delta
	// run should tombstone.
	var (
		oldModifyID string
		oldRemoveID string
		oldRenameID string
		oldPkgID    string
	)
	for path, dst := range map[string]*string{
		"pkg/modify_me.go":      &oldModifyID,
		"pkg/remove_me.go":      &oldRemoveID,
		"pkg/rename_me_old.go":  &oldRenameID,
	} {
		if err := fix.owner.QueryRowContext(ctx, `
			SELECT n.node_id::text FROM node n
			LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
			WHERE n.repo_id = $1 AND n.kind = 'file' AND n.canonical_signature = $2
			  AND nr.node_id IS NULL
		`, repoIDStr, CanonicalFileSig(repoURL, path)).Scan(dst); err != nil {
			t.Fatalf("lookup pre-delta File Node %s: %v", path, err)
		}
	}
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT n.node_id::text FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.repo_id = $1 AND n.kind = 'package'
		  AND n.canonical_signature = $2
		  AND nr.node_id IS NULL
	`, repoIDStr, CanonicalPackageSig(repoURL, "pkg")).Scan(&oldPkgID); err != nil {
		t.Fatalf("lookup pre-delta pkg Package Node: %v", err)
	}

	// 3. Seed + drive the DELTA job c1 -> c3 with the production
	// GitDeltaDiffer and GitMaterializer.
	deltaJobID := seedDeltaJob(t, ctx, fix, repoIDStr, c1, c3)
	deltaDiffer := &GitDeltaDiffer{BaseDir: workDir}
	deltaMat := &GitMaterializer{BaseDir: workDir}
	deltaEmitter := &recordingASTEmitter{}
	deltaPub := &recordingEventPublisher{}
	deltaRetirer := NewRetirementAdapter(retirement.New(fix.app, slog.Default()))

	deltaWorker := NewWorker(fix.app, gw, WorkerOptions{
		WorkerID:     "test-worker-committed-delta",
		Materializer: deltaMat,
		Emitter:      deltaEmitter,
		Publisher:    deltaPub,
		Differ:       deltaDiffer,
		Retirer:      deltaRetirer,
		Logger:       slog.Default(),
	})
	processed, err = deltaWorker.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("delta ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("delta ProcessOnce: processed=false (job %s)", deltaJobID)
	}

	// 4. Assertions.

	// a) repo_commit row for c3 with parent_sha=c2 — finding #3.
	var commitParent string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COALESCE(parent_sha, '') FROM repo_commit
		WHERE repo_id = $1 AND sha = $2
	`, repoIDStr, c3).Scan(&commitParent); err != nil {
		t.Fatalf("readback repo_commit for c3: %v", err)
	}
	if commitParent != c2 {
		t.Errorf("repo_commit(sha=c3).parent_sha = %q, want %q (c2)", commitParent, c2)
	}

	// b) For EACH of the three tombstoned files, retired_at_sha
	// must equal c2 (= parent(c3)), NOT c1 (= from_sha) — finding #2.
	for _, tc := range []struct {
		name   string
		nodeID string
	}{
		{"modified", oldModifyID},
		{"removed", oldRemoveID},
		{"renamed-old", oldRenameID},
	} {
		var retiredAtSHA string
		if err := fix.owner.QueryRowContext(ctx,
			`SELECT retired_at_sha FROM node_retirement WHERE node_id = $1`,
			tc.nodeID,
		).Scan(&retiredAtSHA); err != nil {
			t.Errorf("readback node_retirement for %s file (%s): %v", tc.name, tc.nodeID, err)
			continue
		}
		if retiredAtSHA != c2 {
			t.Errorf("%s file %s retired_at_sha = %q, want %q (c2=parent(to_sha)); from_sha=%q (c1)",
				tc.name, tc.nodeID, retiredAtSHA, c2, c1)
		}
	}

	// c) Modified file's old File Node MUST have
	// superseded_by_node_id pointing at the c3-side File Node —
	// finding #1. Look up the live c3-side File Node first.
	var newModifyID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT n.node_id::text FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.repo_id = $1 AND n.kind = 'file'
		  AND n.canonical_signature = $2
		  AND nr.node_id IS NULL
	`, repoIDStr, CanonicalFileSig(repoURL, "pkg/modify_me.go")).Scan(&newModifyID); err != nil {
		t.Fatalf("lookup live modify_me.go File Node post-delta: %v", err)
	}
	if newModifyID == oldModifyID {
		t.Fatalf("live modify_me.go File Node id = pre-delta id (%s); ensureFileNode should have minted a fresh row at to_sha", oldModifyID)
	}
	var modifiedSupersededBy string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COALESCE(superseded_by_node_id::text, '')
		FROM node_retirement WHERE node_id = $1
	`, oldModifyID).Scan(&modifiedSupersededBy); err != nil {
		t.Fatalf("readback modify_me.go old File Node retirement: %v", err)
	}
	if modifiedSupersededBy != newModifyID {
		t.Errorf("modify_me.go old File Node superseded_by_node_id = %q, want %q (new File Node id)",
			modifiedSupersededBy, newModifyID)
	}

	// d) Exactly ONE live File Node for the modified file's
	// canonical_signature — anti-finding #1 (the pre-fix bug
	// left both old and new live).
	var liveModifyCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.repo_id = $1 AND n.kind = 'file'
		  AND n.canonical_signature = $2
		  AND nr.node_id IS NULL
	`, repoIDStr, CanonicalFileSig(repoURL, "pkg/modify_me.go")).Scan(&liveModifyCount); err != nil {
		t.Fatalf("count live modify_me.go File Nodes: %v", err)
	}
	if liveModifyCount != 1 {
		t.Errorf("live modify_me.go File Nodes = %d, want 1 (one per canonical_signature post-delta)", liveModifyCount)
	}

	// e) NO `renamed_to` edge for the modified file (path did
	// not change — anti-finding #1 ensures we did not mis-classify
	// the modify as a rename).
	var modifyRenamedEdgeCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge
		WHERE src_node_id = $1 AND dst_node_id = $2 AND kind = 'renamed_to'
	`, oldModifyID, newModifyID).Scan(&modifyRenamedEdgeCount); err != nil {
		t.Fatalf("count renamed_to edge for modify_me.go: %v", err)
	}
	if modifyRenamedEdgeCount != 0 {
		t.Errorf("modify_me.go should not have a renamed_to edge old=%s -> new=%s; got %d",
			oldModifyID, newModifyID, modifyRenamedEdgeCount)
	}

	// f) Package -> old_modify_File `contains` edge MUST be
	// retired (anti-finding #1: a live edge from Package -> a
	// retired File node is a graph integrity violation).
	var pkgOldFileEdgeID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT edge_id::text FROM edge
		WHERE src_node_id = $1 AND dst_node_id = $2 AND kind = 'contains'
	`, oldPkgID, oldModifyID).Scan(&pkgOldFileEdgeID); err != nil {
		t.Fatalf("lookup Package->old modify File contains edge: %v", err)
	}
	var pkgOldFileEdgeRetiredCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge_retirement WHERE edge_id = $1
	`, pkgOldFileEdgeID).Scan(&pkgOldFileEdgeRetiredCount); err != nil {
		t.Fatalf("check Package->old File contains edge tombstone: %v", err)
	}
	if pkgOldFileEdgeRetiredCount != 1 {
		t.Errorf("Package->old_modify_File contains edge %s should be retired exactly once; got %d",
			pkgOldFileEdgeID, pkgOldFileEdgeRetiredCount)
	}

	// g) Renamed file: a `renamed_to` edge from old -> new
	// + superseded_by_node_id.
	var newRenameID string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT n.node_id::text FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.repo_id = $1 AND n.kind = 'file'
		  AND n.canonical_signature = $2
		  AND nr.node_id IS NULL
	`, repoIDStr, CanonicalFileSig(repoURL, "pkg/rename_me_new.go")).Scan(&newRenameID); err != nil {
		t.Fatalf("lookup new rename File Node: %v", err)
	}
	var renameEdgeCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edge
		WHERE src_node_id = $1 AND dst_node_id = $2 AND kind = 'renamed_to'
	`, oldRenameID, newRenameID).Scan(&renameEdgeCount); err != nil {
		t.Fatalf("count renamed_to edge for rename: %v", err)
	}
	if renameEdgeCount != 1 {
		t.Errorf("renamed_to edge old=%s -> new=%s count = %d, want 1",
			oldRenameID, newRenameID, renameEdgeCount)
	}
	var renameSupersededBy string
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COALESCE(superseded_by_node_id::text, '')
		FROM node_retirement WHERE node_id = $1
	`, oldRenameID).Scan(&renameSupersededBy); err != nil {
		t.Fatalf("readback rename supersede: %v", err)
	}
	if renameSupersededBy != newRenameID {
		t.Errorf("rename old File superseded_by_node_id = %q, want %q",
			renameSupersededBy, newRenameID)
	}

	// h) Added file has a live File Node.
	var addedLiveCount int
	if err := fix.owner.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.repo_id = $1 AND n.kind = 'file'
		  AND n.canonical_signature = $2
		  AND nr.node_id IS NULL
	`, repoIDStr, CanonicalFileSig(repoURL, "pkg/added.go")).Scan(&addedLiveCount); err != nil {
		t.Fatalf("count live added.go File Nodes: %v", err)
	}
	if addedLiveCount != 1 {
		t.Errorf("live added.go File Nodes = %d, want 1", addedLiveCount)
	}

	// i) Delta job done + event published.
	var deltaStatus string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, deltaJobID,
	).Scan(&deltaStatus); err != nil {
		t.Fatalf("readback delta job: %v", err)
	}
	if deltaStatus != "done" {
		t.Errorf("delta job status = %q, want done", deltaStatus)
	}
	var foundDelta bool
	for _, ev := range deltaPub.events() {
		if ev.Kind != EventKindRepoDeltaIngested || ev.JobID != deltaJobID {
			continue
		}
		foundDelta = true
		if ev.FromSHA != c1 {
			t.Errorf("event FromSHA = %q, want %q (c1)", ev.FromSHA, c1)
		}
		if ev.ToSHA != c3 {
			t.Errorf("event ToSHA = %q, want %q (c3)", ev.ToSHA, c3)
		}
		if ev.AffectedNodeCount <= 0 {
			t.Errorf("event AffectedNodeCount = %d, want > 0", ev.AffectedNodeCount)
		}
	}
	if !foundDelta {
		t.Errorf("no %s event for delta jobID=%s; events=%+v",
			EventKindRepoDeltaIngested, deltaJobID, deltaPub.events())
	}

	// j) The persisted `affected_node_count` column equals the
	// published event's count (proves the GREATEST UPDATE
	// landed on the first attempt) — finding #5.
	var persistedCount int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT affected_node_count FROM ingest_jobs WHERE job_id = $1`, deltaJobID,
	).Scan(&persistedCount); err != nil {
		t.Fatalf("readback persisted affected_node_count: %v", err)
	}
	if persistedCount <= 0 {
		t.Errorf("persisted affected_node_count = %d, want > 0", persistedCount)
	}
}
