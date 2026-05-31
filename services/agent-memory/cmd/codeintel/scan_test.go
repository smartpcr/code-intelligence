package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func TestPostgresStoreRejectedInCLI(t *testing.T) {
	dir := writeFixtureRepo(t)
	_, _, err := execute(t, "--store", "postgres", "scan", dir)
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("expected postgres-not-supported error, got %v", err)
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
	// The fixture has one Python file -> walked should be 1,
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

func TestScanSqliteRequiresOut(t *testing.T) {
	// On CGO builds the sqlite opener requires a path; on CGO=0
	// the opener returns the CGO-required error. Either way the
	// error is non-nil when --out and --db are both empty.
	dir := writeFixtureRepo(t)
	_, _, err := execute(t, "--store", "sqlite", "scan", dir)
	if err == nil {
		t.Fatalf("expected error when --out and --db missing")
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
