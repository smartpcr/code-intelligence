package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: write a manifest file with the given lines, return its path.
func writeManifest(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

// TestScanManyRequiresOutDir asserts the --out-dir flag is required.
func TestScanManyRequiresOutDir(t *testing.T) {
	manifest := writeManifest(t, "/tmp/repo")
	_, _, err := execute(t, "scan-many", manifest)
	if err == nil || !strings.Contains(err.Error(), "--out-dir") {
		t.Fatalf("expected --out-dir required error, got %v", err)
	}
}

// TestScanManyRequiresArgument asserts a missing manifest path errors.
func TestScanManyRequiresArgument(t *testing.T) {
	_, _, err := execute(t, "scan-many", "--out-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "requires exactly 1 argument") {
		t.Fatalf("expected arg-count error, got %v", err)
	}
}

// TestScanManyThreeReposWritesOneDBEach is the impl-plan scenario
// `scan-many-three-repos`: three valid local entries should
// produce three .db files under --out-dir and an aggregate
// summary line with succeeded=3, failed=0.
func TestScanManyThreeReposWritesOneDBEach(t *testing.T) {
	// Three independent fixture repos so the basenames differ.
	r1 := writeFixtureRepoNamed(t, "alpha")
	r2 := writeFixtureRepoNamed(t, "beta")
	r3 := writeFixtureRepoNamed(t, "gamma")
	manifest := writeManifest(t, r1, r2, r3)
	outDir := t.TempDir()

	stdout, _, err := execute(t,
		"--store", "sqlite",
		"scan-many", manifest,
		"--out-dir", outDir,
	)
	if err != nil {
		t.Fatalf("scan-many returned error: %v\nstdout=%s", err, stdout)
	}
	dbs, err := filepath.Glob(filepath.Join(outDir, "*.db"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(dbs) != 3 {
		t.Fatalf("expected 3 .db files under %s, got %d: %v", outDir, len(dbs), dbs)
	}
	if !strings.Contains(stdout, "succeeded: 3") {
		t.Errorf("expected 'succeeded: 3' in aggregate, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "failed:    0") {
		t.Errorf("expected 'failed:    0' in aggregate, got:\n%s", stdout)
	}
}

// TestScanManyPartialFailureContinuesLoop is the impl-plan
// scenario `scan-many-partial-failure`: a middle entry that is
// an invalid git URL (no SHA, treated as git URL) should be
// recorded as `failed:` and the loop must continue; first and
// third entries each produce a .db file and the exit code is
// non-zero.
func TestScanManyPartialFailureContinuesLoop(t *testing.T) {
	r1 := writeFixtureRepoNamed(t, "first")
	r3 := writeFixtureRepoNamed(t, "third")
	// Middle entry: a git URL without a SHA. runScan rejects
	// this with "--sha is required when the input is a git URL".
	manifest := writeManifest(t, r1,
		"https://example.invalid/owner/missing.git",
		r3)
	outDir := t.TempDir()

	stdout, _, err := execute(t,
		"--store", "sqlite",
		"scan-many", manifest,
		"--out-dir", outDir,
	)
	if err == nil {
		t.Fatalf("expected non-zero exit on partial failure, got nil err\nstdout=%s", stdout)
	}
	if !strings.Contains(err.Error(), "1 of 3 entries failed") {
		t.Errorf("expected failure count in error, got %v", err)
	}
	dbs, err := filepath.Glob(filepath.Join(outDir, "*.db"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(dbs) != 2 {
		t.Fatalf("expected 2 .db files (first and third), got %d: %v", len(dbs), dbs)
	}
	if !strings.Contains(stdout, "failed:") {
		t.Errorf("expected a per-entry 'failed:' line in stdout, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "succeeded: 2") {
		t.Errorf("expected 'succeeded: 2' in aggregate, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "failed:    1") {
		t.Errorf("expected 'failed:    1' in aggregate, got:\n%s", stdout)
	}
}

// TestScanManyCommentsAndBlanksSkipped is the impl-plan scenario
// `manifest-comments-skipped`. Confirms the loop only invokes
// the scan executor for valid entries.
func TestScanManyCommentsAndBlanksSkipped(t *testing.T) {
	r1 := writeFixtureRepoNamed(t, "only")
	manifest := writeManifest(t,
		"# leading comment",
		"",
		"   # indented comment",
		r1,
		"",
		"# trailing comment",
	)
	var invocations []string
	root := defaultRootFlags()
	root.store = "sqlite"
	flags := &scanManyFlags{outDir: t.TempDir()}
	var buf bytes.Buffer
	runner := scanManyRunner{
		stdout: &buf,
		runScan: func(ctx context.Context, _ *rootFlags, sf *scanFlags, input string, _ scanRunner) (scanSummary, error) {
			invocations = append(invocations, input)
			return scanSummary{Walked: 1, Parsed: 1,
				Nodes: map[string]int{"repo": 1},
				Edges: map[string]int{},
			}, nil
		},
	}
	if err := runScanMany(context.Background(), &root, flags, manifest, runner); err != nil {
		t.Fatalf("runScanMany: %v\nstdout=%s", err, buf.String())
	}
	if len(invocations) != 1 {
		t.Fatalf("expected exactly 1 scan invocation (comments/blanks skipped), got %d: %v",
			len(invocations), invocations)
	}
	if invocations[0] != r1 {
		t.Errorf("expected invocation for %q, got %q", r1, invocations[0])
	}
}

// TestScanManyAggregateAddsAcrossRepos verifies the aggregate
// sums walked/parsed/nodes/edges across successful entries.
func TestScanManyAggregateAddsAcrossRepos(t *testing.T) {
	manifest := writeManifest(t, "/a", "/b", "/c")
	root := defaultRootFlags()
	root.store = "sqlite"
	flags := &scanManyFlags{outDir: t.TempDir()}
	var buf bytes.Buffer
	runner := scanManyRunner{
		stdout: &buf,
		runScan: func(ctx context.Context, _ *rootFlags, sf *scanFlags, input string, _ scanRunner) (scanSummary, error) {
			return scanSummary{
				Walked: 2,
				Parsed: 1,
				Nodes:  map[string]int{"repo": 1, "file": 2},
				Edges:  map[string]int{"contains": 3},
			}, nil
		},
	}
	if err := runScanMany(context.Background(), &root, flags, manifest, runner); err != nil {
		t.Fatalf("runScanMany: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "succeeded: 3") {
		t.Errorf("missing succeeded count, got:\n%s", out)
	}
	if !strings.Contains(out, "walked:    6") {
		t.Errorf("expected aggregate walked=6 (3 repos * 2), got:\n%s", out)
	}
	if !strings.Contains(out, "parsed:    3") {
		t.Errorf("expected aggregate parsed=3, got:\n%s", out)
	}
	// Node and edge totals come from formatKindMap; check the
	// per-kind summed counts.
	if !strings.Contains(out, "repo=3") {
		t.Errorf("expected aggregate nodes repo=3, got:\n%s", out)
	}
	if !strings.Contains(out, "file=6") {
		t.Errorf("expected aggregate nodes file=6, got:\n%s", out)
	}
	if !strings.Contains(out, "contains=9") {
		t.Errorf("expected aggregate edges contains=9, got:\n%s", out)
	}
}

// TestScanManyJSONFormat verifies the JSON aggregate is a single
// parseable object with the expected keys.
func TestScanManyJSONFormat(t *testing.T) {
	manifest := writeManifest(t, "/a", "/b")
	root := defaultRootFlags()
	root.store = "sqlite"
	root.logFormat = "json"
	flags := &scanManyFlags{outDir: t.TempDir()}
	var buf bytes.Buffer
	runner := scanManyRunner{
		stdout: &buf,
		runScan: func(ctx context.Context, _ *rootFlags, sf *scanFlags, input string, _ scanRunner) (scanSummary, error) {
			if input == "/b" {
				return scanSummary{}, errors.New("simulated failure")
			}
			return scanSummary{
				Walked: 1, Parsed: 1,
				Nodes: map[string]int{"repo": 1},
				Edges: map[string]int{},
			}, nil
		},
	}
	if err := runScanMany(context.Background(), &root, flags, manifest, runner); err == nil {
		t.Fatalf("expected partial-failure error, got nil; out=%s", buf.String())
	}
	// Last non-empty line is the aggregate JSON object.
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	last := lines[len(lines)-1]
	var agg manyAggregateJSON
	if err := json.Unmarshal([]byte(last), &agg); err != nil {
		t.Fatalf("aggregate not JSON: %v\nline=%s", err, last)
	}
	if agg.Succeeded != 1 || agg.Failed != 1 {
		t.Errorf("expected succeeded=1, failed=1, got %+v", agg)
	}
	if len(agg.Failures) != 1 {
		t.Fatalf("expected 1 failure record, got %d", len(agg.Failures))
	}
	if agg.Failures[0].Input != "/b" {
		t.Errorf("expected failure for /b, got %+v", agg.Failures[0])
	}
}

// TestScanManySlugCollisionDisambiguated verifies that two
// entries whose basenames collide each get their own .db file
// (architecture S9.4 requires one .db per repo).
func TestScanManySlugCollisionDisambiguated(t *testing.T) {
	// Two fixture repos with the same basename in distinct parent dirs.
	parentA := t.TempDir()
	parentB := t.TempDir()
	r1 := filepath.Join(parentA, "collide")
	r2 := filepath.Join(parentB, "collide")
	for _, d := range []string{r1, r2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(d, "main.go"),
			[]byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	manifest := writeManifest(t, r1, r2)
	outDir := t.TempDir()
	stdout, _, err := execute(t,
		"--store", "sqlite",
		"scan-many", manifest,
		"--out-dir", outDir,
	)
	if err != nil {
		t.Fatalf("scan-many: %v\nstdout=%s", err, stdout)
	}
	dbs, err := filepath.Glob(filepath.Join(outDir, "*.db"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(dbs) != 2 {
		t.Fatalf("expected 2 .db files after slug disambiguation, got %d: %v", len(dbs), dbs)
	}
}

// TestScanManyFailedEntryDoesNotLeaveDB asserts the per-entry
// .db artifact is removed when an entry fails AFTER the sink
// has opened (evaluator iter-1 feedback item 2). The injected
// exec function creates the file to mimic the SQLite sink, then
// returns an error.
func TestScanManyFailedEntryDoesNotLeaveDB(t *testing.T) {
	manifest := writeManifest(t, "/ok", "/boom", "/ok2")
	outDir := t.TempDir()
	root := defaultRootFlags()
	root.store = "sqlite"
	flags := &scanManyFlags{outDir: outDir}
	var buf bytes.Buffer
	runner := scanManyRunner{
		stdout: &buf,
		runScan: func(ctx context.Context, _ *rootFlags, sf *scanFlags, input string, _ scanRunner) (scanSummary, error) {
			// Simulate the sink-opened-then-walk-failed path:
			// touch the .db file (so the artifact exists when
			// the error returns), then fail.
			if err := os.WriteFile(sf.out, []byte("partial sqlite header"), 0o644); err != nil {
				t.Fatalf("touch: %v", err)
			}
			if input == "/boom" {
				return scanSummary{}, errors.New("simulated post-sink-open failure")
			}
			return scanSummary{Walked: 1, Parsed: 1,
				Nodes: map[string]int{"repo": 1},
				Edges: map[string]int{},
			}, nil
		},
	}
	if err := runScanMany(context.Background(), &root, flags, manifest, runner); err == nil {
		t.Fatalf("expected partial-failure error, got nil")
	}
	dbs, err := filepath.Glob(filepath.Join(outDir, "*.db"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(dbs) != 2 {
		t.Fatalf("expected exactly 2 .db files (failure cleaned up), got %d: %v", len(dbs), dbs)
	}
	for _, p := range dbs {
		if strings.Contains(filepath.Base(p), "boom") {
			t.Errorf("failed entry's .db artifact was not removed: %s", p)
		}
	}
}

// TestScanManyPinsSqliteStore asserts scan-many runs each entry
// under --store=sqlite regardless of the operator's root --store
// (evaluator iter-1 feedback item 3). The injected exec captures
// the per-entry root flags.
func TestScanManyPinsSqliteStore(t *testing.T) {
	manifest := writeManifest(t, "/a", "/b")
	root := defaultRootFlags()
	root.store = "memory" // would otherwise be propagated to per-entry runs
	flags := &scanManyFlags{outDir: t.TempDir()}
	var seenStores []string
	runner := scanManyRunner{
		stdout: &bytes.Buffer{},
		runScan: func(ctx context.Context, r *rootFlags, sf *scanFlags, input string, _ scanRunner) (scanSummary, error) {
			seenStores = append(seenStores, r.store)
			return scanSummary{Nodes: map[string]int{}, Edges: map[string]int{}}, nil
		},
	}
	if err := runScanMany(context.Background(), &root, flags, manifest, runner); err != nil {
		t.Fatalf("runScanMany: %v", err)
	}
	if len(seenStores) != 2 {
		t.Fatalf("expected 2 per-entry calls, got %d", len(seenStores))
	}
	for i, s := range seenStores {
		if s != "sqlite" {
			t.Errorf("entry %d: per-entry root.store = %q, want sqlite", i, s)
		}
	}
	// And the operator's outer root must NOT have been mutated.
	if root.store != "memory" {
		t.Errorf("scan-many mutated outer root.store to %q, must remain %q", root.store, "memory")
	}
}

// TestScanManySuccessSummaryNotDuplicated asserts each successful
// per-repo summary appears exactly once in the combined stdout
// (evaluator iter-1 feedback item 1).
func TestScanManySuccessSummaryNotDuplicated(t *testing.T) {
	r1 := writeFixtureRepoNamed(t, "uniqalpha")
	r2 := writeFixtureRepoNamed(t, "uniqbeta")
	manifest := writeManifest(t, r1, r2)
	outDir := t.TempDir()
	stdout, _, err := execute(t,
		"--store", "sqlite",
		"scan-many", manifest,
		"--out-dir", outDir,
	)
	if err != nil {
		t.Fatalf("scan-many: %v", err)
	}
	// Each per-repo summary contains a unique "repo:" line whose
	// URL embeds the basename. It MUST appear exactly once per
	// successful repo.
	for _, name := range []string{"uniqalpha", "uniqbeta"} {
		n := strings.Count(stdout, "/"+name+" @")
		if n != 1 {
			t.Errorf("expected per-repo summary for %q exactly once, got %d occurrences\nstdout=%s",
				name, n, stdout)
		}
	}
}
func writeFixtureRepoNamed(t *testing.T, name string) string {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	src := fmt.Sprintf("package %s\n\nfunc Hello() string { return %q }\n", name, name)
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}
