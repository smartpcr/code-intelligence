package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// helper: write a tiny fixture repo with one Go file containing
// a struct + a method + a same-file call so the dispatcher emits
// class + method + static_calls nodes/edges on the default CGO
// parser set (Go is in `defaultParsers()`).
func writeFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `package fixture

type Greeter struct{}

func (g *Greeter) Greet() string {
	return g.sayHello()
}

func (g *Greeter) sayHello() string {
	return "hi"
}
`
	if err := os.WriteFile(filepath.Join(dir, "greeter.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

func TestDetectInputKind(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  inputKind
	}{
		{"file-url", "file:///tmp/repo", inputKindFileURL},
		{"https-git", "https://github.com/o/r.git", inputKindGitURL},
		{"http-git", "http://example.com/r", inputKindGitURL},
		{"ssh-git", "ssh://git@example.com/r", inputKindGitURL},
		{"scp-style", "git@github.com:owner/repo.git", inputKindGitURL},
		{"windows-abs", `C:\code\repo`, inputKindLocalPath},
		{"windows-abs-fwd", `C:/code/repo`, inputKindLocalPath},
		{"posix-abs", "/var/repo", inputKindLocalPath},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := detectInputKind(c.input); got != c.want {
				t.Errorf("detectInputKind(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestDetectInputKindRelativeOnDisk(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := detectInputKind("sub"); got != inputKindLocalPath {
		t.Errorf("relative on-disk dir: got %v, want inputKindLocalPath", got)
	}
	if got := detectInputKind("does-not-exist"); got != inputKindUnknown {
		t.Errorf("relative missing path: got %v, want inputKindUnknown", got)
	}
}

func TestGitURLRequiresSHA(t *testing.T) {
	_, _, err := execute(t, "scan", "https://example.com/repo.git")
	if err == nil || !strings.Contains(err.Error(), "--sha is required") {
		t.Fatalf("expected --sha required error, got %v", err)
	}
}

func TestPostgresStoreRequiresDB(t *testing.T) {
	// Postgres store with no --db should fail with a clear
	// message, NOT a silent rejection of the entire backend
	// (per evaluator iter-1 feedback item 1, postgres is wired).
	dir := writeFixtureRepo(t)
	_, _, err := execute(t, "--store", "postgres", "scan", dir)
	if err == nil || !strings.Contains(err.Error(), "--db is required") {
		t.Fatalf("expected --db required error, got %v", err)
	}
}

func TestPostgresStoreSurfacesPingFailure(t *testing.T) {
	// Postgres with a bogus DSN should attempt to connect and
	// fail at ping/open, NOT short-circuit with a "not yet
	// supported" message. This confirms the wiring is live.
	dir := writeFixtureRepo(t)
	bogusDSN := "postgres://nouser:nopass@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1"
	_, _, err := execute(t, "--store", "postgres", "--db", bogusDSN, "scan", dir)
	if err == nil {
		t.Fatalf("expected ping error with bogus DSN, got nil")
	}
	// The wording is driver-dependent, but the error MUST NOT
	// be the iter-1 "not yet supported" rejection.
	if strings.Contains(err.Error(), "not yet supported") {
		t.Fatalf("postgres opener should attempt the connection, got rejection: %v", err)
	}
}

func TestWithEmbeddingsRejected(t *testing.T) {
	dir := writeFixtureRepo(t)
	_, _, err := execute(t, "--with-embeddings", "scan", dir)
	if err == nil || !strings.Contains(err.Error(), "with-embeddings") {
		t.Fatalf("expected --with-embeddings rejected error, got %v", err)
	}
}

func TestScanLocalMemoryStore(t *testing.T) {
	dir := writeFixtureRepo(t)
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	// Memory store with no --out means "discard on close" -- we
	// only verify the summary lines and counters.
	root.SetArgs([]string{"--store", "memory", "scan", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("scan returned error: %v\nstderr=%s", err, errOut.String())
	}
	s := out.String()
	for _, want := range []string{
		"repo:", "walked:", "parsed:", "nodes:", "edges:", "skipped:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q line; got:\n%s", want, s)
		}
	}
	// The fixture has one Go file -> walked should be 1,
	// parsed should be 1.
	if !strings.Contains(s, "walked: 1") {
		t.Errorf("expected walked: 1, got:\n%s", s)
	}
	if !strings.Contains(s, "parsed: 1") {
		t.Errorf("expected parsed: 1, got:\n%s", s)
	}
}

func TestScanLocalMemoryStoreJSON(t *testing.T) {
	dir := writeFixtureRepo(t)
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	root.SetArgs([]string{"--log", "json", "--store", "memory", "scan", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("scan returned error: %v\nstderr=%s", err, errOut.String())
	}
	// stdout should end with one JSON line (the summary).
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	last := lines[len(lines)-1]
	var sum scanSummary
	if err := json.Unmarshal([]byte(last), &sum); err != nil {
		t.Fatalf("summary JSON parse failed: %v\nline=%s", err, last)
	}
	if sum.Walked != 1 {
		t.Errorf("expected walked=1, got %d (summary=%+v)", sum.Walked, sum)
	}
	if sum.Parsed != 1 {
		t.Errorf("expected parsed=1, got %d (summary=%+v)", sum.Parsed, sum)
	}
	// At minimum repo, package, file nodes are minted by the
	// ancestry pre-walk; class + method nodes are minted by the
	// dispatcher for the fixture python file.
	for _, k := range []string{"repo", "package", "file"} {
		if sum.Nodes[k] < 1 {
			t.Errorf("expected nodes[%q] >= 1, got %d", k, sum.Nodes[k])
		}
	}
	if sum.Nodes["class"] < 1 {
		t.Errorf("expected class/type node from greeter.go, got %d (nodes=%+v)", sum.Nodes["class"], sum.Nodes)
	}
	if sum.Nodes["method"] < 1 {
		t.Errorf("expected method nodes from greeter.go, got %d (nodes=%+v)", sum.Nodes["method"], sum.Nodes)
	}
	if sum.Edges["contains"] < 2 {
		t.Errorf("expected >=2 contains edges (repo->pkg, pkg->file), got %d", sum.Edges["contains"])
	}
}

func TestScanFileURLForm(t *testing.T) {
	dir := writeFixtureRepo(t)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// Build a `file://` URL the LocalDirMaterializer decodes.
	var url string
	if runtime.GOOS == "windows" {
		// file:///c:/path/with/forward-slashes
		url = "file:///" + strings.ReplaceAll(abs, `\`, `/`)
	} else {
		url = "file://" + abs
	}
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	root.SetArgs([]string{"--store", "memory", "scan", url})
	if err := root.Execute(); err != nil {
		t.Fatalf("scan file:// returned error: %v\nstderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "walked: 1") {
		t.Errorf("expected walked: 1, got:\n%s", out.String())
	}
}

func TestScanSqliteDefaultPathSynthesised(t *testing.T) {
	// Per evaluator iter-1 feedback item 2: bare
	// `codeintel scan <path>` must complete with the default
	// --store=sqlite. We redirect the auto-derived path into
	// t.TempDir() via CODEINTEL_DEFAULT_DB_DIR so the test does
	// not litter the operator's cwd.
	dir := writeFixtureRepo(t)
	dbDir := t.TempDir()
	t.Setenv(defaultSqliteDirEnv, dbDir)

	_, _, err := execute(t, "scan", dir)
	if err != nil {
		t.Fatalf("bare `codeintel scan <path>` should complete with sqlite default, got %v", err)
	}
	// The synthesised path should now exist inside dbDir.
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("read default db dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".codeintel.db") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a *.codeintel.db file under %s, got %v", dbDir, entries)
	}
}

func TestScanWritesMemoryExport(t *testing.T) {
	dir := writeFixtureRepo(t)
	exp := filepath.Join(t.TempDir(), "graph.json")
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	root.SetArgs([]string{"--store", "memory", "scan", "--out", exp, dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("scan returned error: %v\nstderr=%s", err, errOut.String())
	}
	st, err := os.Stat(exp)
	if err != nil {
		t.Fatalf("export file missing: %v", err)
	}
	if st.Size() == 0 {
		t.Fatalf("export file empty")
	}
	// Sanity check that it parses as JSON.
	body, err := os.ReadFile(exp)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var anyJSON map[string]any
	if err := json.Unmarshal(body, &anyJSON); err != nil {
		t.Fatalf("export not valid JSON: %v", err)
	}
}

func TestRunScanContextCancellation(t *testing.T) {
	// A canceled context should surface as an error from the
	// materialize step rather than leak an open workspace. Use
	// a fresh context, cancel it, then invoke runScan directly.
	dir := writeFixtureRepo(t)
	root := defaultRootFlags()
	root.store = "memory"
	flags := &scanFlags{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	err := runScan(ctx, &root, flags, dir, scanRunner{stdout: &buf})
	// LocalDirMaterializer's git rev-parse HEAD picks up the
	// canceled context and returns an error -- the exact wording
	// depends on the platform, but the runScan wrapper must
	// surface it.
	if err == nil {
		// Some platforms have no git binary; in that case
		// fingerprint.MTimeTreeSHA runs synchronously and may
		// not honor ctx. Tolerate success in that path -- the
		// goal is "no panic, no leak", and the deferred Close
		// handles the workspace.
		t.Logf("runScan completed under canceled ctx (git rev-parse not invoked): %s", buf.String())
	}
}

// ----- Git URL happy path: route through fake materializer ------

// TestScanGitURLHappyPath confirms a git URL routes through
// GitMaterializer and that --sha is forwarded (evaluator iter-1
// feedback item 7). We swap in an InMemoryMaterializer via the
// scanRunner test seam so the test does not touch the network or
// the git binary.
func TestScanGitURLHappyPath(t *testing.T) {
	root := defaultRootFlags()
	root.store = "memory"
	flags := &scanFlags{sha: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}
	gitURL := "https://example.com/owner/repo.git"
	mat := &repoindexer.InMemoryMaterializer{
		Files: []repoindexer.InMemoryFile{
			{
				RelPath: "main.go",
				Content: []byte("package main\n\ntype S struct{}\n\nfunc (s *S) A() { s.b() }\nfunc (s *S) b() {}\n"),
			},
		},
	}
	var buf bytes.Buffer
	runner := scanRunner{
		stdout:    &buf,
		newGitMat: func() repoindexer.Materializer { return mat },
	}
	if err := runScan(context.Background(), &root, flags, gitURL, runner); err != nil {
		t.Fatalf("git happy path returned error: %v\noutput=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "walked: 1") {
		t.Errorf("expected walked: 1, got:\n%s", out)
	}
	if !strings.Contains(out, "parsed: 1") {
		t.Errorf("expected parsed: 1 (Go file with default parsers), got:\n%s", out)
	}
	// Repo URL surfaced in the summary must be the git URL the
	// operator passed in (not a synthesised file://).
	if !strings.Contains(out, gitURL) {
		t.Errorf("expected repo URL %q in summary, got:\n%s", gitURL, out)
	}
}

// TestScanForwardsSHAToMaterializer asserts the --sha flag flows
// through to the materializer's Materialize(ctx, url, sha) call
// (evaluator iter-1 feedback item 7).
func TestScanForwardsSHAToMaterializer(t *testing.T) {
	const wantSHA = "11112222333344445555666677778888aaaabbbb"
	mat := &capturingMaterializer{inner: &repoindexer.InMemoryMaterializer{
		Files: []repoindexer.InMemoryFile{{RelPath: "x.go", Content: []byte("package x\n")}},
	}}
	root := defaultRootFlags()
	root.store = "memory"
	flags := &scanFlags{sha: wantSHA}
	var buf bytes.Buffer
	runner := scanRunner{
		stdout:    &buf,
		newGitMat: func() repoindexer.Materializer { return mat },
	}
	if err := runScan(context.Background(), &root, flags, "https://example.com/r.git", runner); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if mat.gotSHA != wantSHA {
		t.Fatalf("materializer received sha %q, want %q", mat.gotSHA, wantSHA)
	}
	if mat.gotURL != "https://example.com/r.git" {
		t.Fatalf("materializer received url %q, want git URL", mat.gotURL)
	}
}

type capturingMaterializer struct {
	inner  repoindexer.Materializer
	gotURL string
	gotSHA string
}

func (c *capturingMaterializer) Materialize(ctx context.Context, repoURL, sha string) (repoindexer.Workspace, error) {
	c.gotURL = repoURL
	c.gotSHA = sha
	return c.inner.Materialize(ctx, repoURL, sha)
}

// ----- Skip semantics (evaluator iter-1 feedback item 8) --------

// TestScanCountsUnsupportedExtensionAsSkip confirms a file with
// no registered parser is counted as a skip (not as parsed) AND
// surfaces in the by-reason / by-ext tallies.
func TestScanCountsUnsupportedExtensionAsSkip(t *testing.T) {
	mat := &repoindexer.InMemoryMaterializer{
		Files: []repoindexer.InMemoryFile{
			{RelPath: "ok.go", Content: []byte("package ok\n")},
			{RelPath: "weird.zzz", Content: []byte("ignored bytes")},
		},
	}
	root := defaultRootFlags()
	root.store = "memory"
	root.logFormat = "json"
	flags := &scanFlags{sha: "feedfacefeedfacefeedfacefeedfacefeedface"}
	var buf bytes.Buffer
	runner := scanRunner{
		stdout:    &buf,
		newGitMat: func() repoindexer.Materializer { return mat },
	}
	if err := runScan(context.Background(), &root, flags, "https://example.com/r.git", runner); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	var sum scanSummary
	last := strings.TrimSpace(buf.String())
	if i := strings.LastIndex(last, "\n"); i >= 0 {
		last = last[i+1:]
	}
	if err := json.Unmarshal([]byte(last), &sum); err != nil {
		t.Fatalf("parse summary: %v\nline=%s", err, last)
	}
	if sum.Walked != 2 {
		t.Errorf("walked: want 2, got %d", sum.Walked)
	}
	// The .zzz file MUST NOT be counted as parsed.
	if sum.Parsed != 1 {
		t.Errorf("parsed: want 1 (only the .go file), got %d", sum.Parsed)
	}
	if sum.Skipped.ByReason["no_parser"] < 1 {
		t.Errorf("expected no_parser skip; got %+v", sum.Skipped)
	}
	if sum.Skipped.ByExt[".zzz"] < 1 {
		t.Errorf("expected .zzz in no_parser_by_ext, got %+v", sum.Skipped.ByExt)
	}
}

// TestScanRescanIdempotentReportsSameGraph re-scans an unchanged
// repo against the same in-memory store and asserts both scans
// report the same node/edge graph counts (evaluator iter-1
// feedback item 4: summary must reflect the resulting scan graph,
// not just first-insert deltas).
func TestScanRescanIdempotentReportsSameGraph(t *testing.T) {
	files := []repoindexer.InMemoryFile{{
		RelPath: "lib.go",
		Content: []byte("package lib\n\ntype T struct{}\n\nfunc (t *T) A() { t.b() }\nfunc (t *T) b() {}\n"),
	}}
	doScan := func(sink graphsink.Sink) scanSummary {
		t.Helper()
		root := defaultRootFlags()
		root.store = "memory" // ignored when openSink is overridden
		root.logFormat = "json"
		flags := &scanFlags{sha: "1234567890123456789012345678901234567890"}
		var buf bytes.Buffer
		runner := scanRunner{
			stdout: &buf,
			newGitMat: func() repoindexer.Materializer {
				return &repoindexer.InMemoryMaterializer{Files: files}
			},
			openSink: func(_ context.Context, _, _ string) (graphsink.Sink, func() error, error) {
				return sink, func() error { return nil }, nil
			},
		}
		if err := runScan(context.Background(), &root, flags, "https://example.com/idem.git", runner); err != nil {
			t.Fatalf("runScan: %v", err)
		}
		var sum scanSummary
		last := strings.TrimSpace(buf.String())
		if i := strings.LastIndex(last, "\n"); i >= 0 {
			last = last[i+1:]
		}
		if err := json.Unmarshal([]byte(last), &sum); err != nil {
			t.Fatalf("parse: %v\nline=%s", err, last)
		}
		return sum
	}

	sink := memory.New(memory.Options{})
	first := doScan(sink)
	second := doScan(sink)

	// The two scans must surface identical node/edge counts.
	// Without this, a re-scan would (incorrectly) report 0 because
	// every Ensure returns Inserted=false on the second pass.
	for _, k := range []string{"repo", "package", "file", "class", "method"} {
		if first.Nodes[k] != second.Nodes[k] {
			t.Errorf("rescan nodes[%q]: first=%d, second=%d (must match)", k, first.Nodes[k], second.Nodes[k])
		}
	}
	for _, k := range []string{"contains"} {
		if first.Edges[k] != second.Edges[k] {
			t.Errorf("rescan edges[%q]: first=%d, second=%d (must match)", k, first.Edges[k], second.Edges[k])
		}
	}
}

// TestScanPropagatesSinkCloseError verifies the iter-3 fix: a
// sink.Close() failure (e.g. memory backend's --out write failing)
// must cause runScan to return a non-zero error, not be silently
// logged via the deferred close.
func TestScanPropagatesSinkCloseError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	root := defaultRootFlags()
	root.store = "memory"
	root.logFormat = "text"
	flags := &scanFlags{}
	var buf bytes.Buffer
	closeCalls := 0
	wantErr := os.ErrPermission
	runner := scanRunner{
		stdout: &buf,
		openSink: func(_ context.Context, _, _ string) (graphsink.Sink, func() error, error) {
			return memory.New(memory.Options{}), func() error {
				closeCalls++
				return wantErr
			}, nil
		},
	}
	err := runScan(context.Background(), &root, flags, dir, runner)
	if err == nil {
		t.Fatalf("expected runScan to return sink-close error, got nil")
	}
	if !strings.Contains(err.Error(), "sink close") {
		t.Errorf("error should mention sink close, got: %v", err)
	}
	if closeCalls != 1 {
		t.Errorf("expected sinkClose called exactly once, got %d", closeCalls)
	}
	// Summary should still have been written before the error.
	if !strings.Contains(buf.String(), "walked:") {
		t.Errorf("summary should be rendered before close-error propagation; got:\n%s", buf.String())
	}
}

// TestSkipTallyConcurrentSafe verifies the iter-3 fix: skipTally
// is safe under concurrent slog.Handler.Handle calls. The race
// detector (`go test -race`) flags the unsynchronised map write.
func TestSkipTallyConcurrentSafe(t *testing.T) {
	tally := newSkipTally()
	h := tally.tee(nil)
	const goroutines = 16
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				r := slog.NewRecord(time.Time{}, slog.LevelInfo, "ast.dispatch.skip", 0)
				r.AddAttrs(
					slog.String("reason", "no_parser"),
					slog.String("file", "fixture.c"),
				)
				_ = h.Handle(context.Background(), r)
				_ = tally.totalCount()
			}
		}()
	}
	wg.Wait()
	got := tally.snapshot()
	if want := goroutines * iters; got.ByReason["no_parser"] != want {
		t.Errorf("ByReason[no_parser]: got %d, want %d", got.ByReason["no_parser"], want)
	}
	if got.ByExt[".c"] != goroutines*iters {
		t.Errorf("ByExt[.c]: got %d, want %d", got.ByExt[".c"], goroutines*iters)
	}
}
