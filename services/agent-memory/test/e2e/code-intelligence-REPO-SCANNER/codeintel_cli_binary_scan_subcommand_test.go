//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"
	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// CGO=0 binary build (once per test process)
// ---------------------------------------------------------------------------

var (
	nocgoBinaryPath string
	nocgoBuildOnce  sync.Once
	nocgoBuildErr   error
)

func buildNoCGOBinary() (string, error) {
	nocgoBuildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			nocgoBuildErr = fmt.Errorf("module root: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "codeintel-nocgo-*")
		if err != nil {
			nocgoBuildErr = fmt.Errorf("mkdtemp: %w", err)
			return
		}
		name := "codeintel-nocgo"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/codeintel")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			nocgoBuildErr = fmt.Errorf("go build CGO=0: %w\n%s", err, string(out))
			return
		}
		nocgoBinaryPath = binPath
	})
	return nocgoBinaryPath, nocgoBuildErr
}

// ---------------------------------------------------------------------------
// Minimal smart-HTTP git server (stateless-rpc via git upload-pack)
// ---------------------------------------------------------------------------

func newSmartHTTPGitServer(bareRepoPath string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/info/refs"):
			svc := r.URL.Query().Get("service")
			if svc != "git-upload-pack" {
				http.Error(w, "unsupported service", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			w.Header().Set("Cache-Control", "no-cache")

			hdr := "# service=git-upload-pack\n"
			fmt.Fprintf(w, "%04x%s", len(hdr)+4, hdr)
			fmt.Fprint(w, "0000")

			cmd := exec.Command("git", "upload-pack", "--stateless-rpc", "--advertise-refs", bareRepoPath)
			cmd.Stdout = w
			cmd.Stderr = io.Discard
			_ = cmd.Run()

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Header().Set("Cache-Control", "no-cache")

			cmd := exec.Command("git", "upload-pack", "--stateless-rpc", bareRepoPath)
			cmd.Stdin = r.Body
			cmd.Stdout = w
			cmd.Stderr = io.Discard
			_ = cmd.Run()

		default:
			http.NotFound(w, r)
		}
	}))
}

// ---------------------------------------------------------------------------
// Scenario state (fresh per scenario via InitializeScenario)
// ---------------------------------------------------------------------------

type scanSubcommandState struct {
	tmpDir   string
	fixture  string
	binary   string
	nocgoBin string
	dbPath   string
	gitURL   string
	gitSHA   string
	gitSrv   *httptest.Server
	stdout   string
	stderr   string
	exitCode int
}

func (s *scanSubcommandState) cleanup() {
	if s.gitSrv != nil {
		s.gitSrv.Close()
	}
	if s.tmpDir != "" {
		os.RemoveAll(s.tmpDir)
	}
}

func (s *scanSubcommandState) ensureTmpDir() {
	if s.tmpDir == "" {
		s.tmpDir, _ = os.MkdirTemp("", "scan-e2e-*")
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *scanSubcommandState) aSmallLocalFixtureRepo() error {
	s.ensureTmpDir()
	s.fixture = filepath.Join(s.tmpDir, "fixture")
	if err := os.MkdirAll(s.fixture, 0o755); err != nil {
		return err
	}
	src := "package fixture\n\ntype Greeter struct{}\n\n" +
		"func (g *Greeter) Greet() string {\n\treturn g.sayHello()\n}\n\n" +
		"func (g *Greeter) sayHello() string {\n\treturn \"hi\"\n}\n"
	return os.WriteFile(filepath.Join(s.fixture, "greeter.go"), []byte(src), 0o644)
}

func (s *scanSubcommandState) aBuiltCodeintelBinary() error {
	bin, err := buildCodeintelBinary()
	if err != nil {
		return fmt.Errorf("build codeintel: %w", err)
	}
	s.binary = bin
	return nil
}

func (s *scanSubcommandState) aGitRepoServedOverHTTP() error {
	s.ensureTmpDir()

	srcDir := filepath.Join(s.tmpDir, "git-src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}

	git := func(dir string, args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}

	if err := git(srcDir, "init"); err != nil {
		return err
	}
	goSrc := "package main\n\ntype Service struct{}\n\n" +
		"func (s *Service) Run() string { return s.process() }\n" +
		"func (s *Service) process() string { return \"done\" }\n"
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(goSrc), 0o644); err != nil {
		return err
	}
	if err := git(srcDir, "add", "."); err != nil {
		return err
	}
	if err := git(srcDir, "commit", "-m", "init"); err != nil {
		return err
	}

	shaOut, err := exec.Command("git", "-C", srcDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("rev-parse HEAD: %w", err)
	}
	s.gitSHA = strings.TrimSpace(string(shaOut))

	bareDir := filepath.Join(s.tmpDir, "bare.git")
	cloneCmd := exec.Command("git", "clone", "--bare", srcDir, bareDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone --bare: %w\n%s", err, out)
	}
	// Allow per-SHA fetch (required for git fetch <sha> over smart HTTP).
	_ = exec.Command("git", "-C", bareDir, "config",
		"uploadpack.allowReachableSHA1InWant", "true").Run()

	s.gitSrv = newSmartHTTPGitServer(bareDir)
	s.gitURL = s.gitSrv.URL
	return nil
}

func (s *scanSubcommandState) aFixtureWithCAndPy() error {
	s.ensureTmpDir()
	s.fixture = filepath.Join(s.tmpDir, "fixture")
	if err := os.MkdirAll(s.fixture, 0o755); err != nil {
		return err
	}
	cSrc := "#include <stdio.h>\nvoid hello() { printf(\"hi\\n\"); }\n"
	pySrc := "class Greeter:\n    def greet(self):\n        return self.say_hello()\n\n" +
		"    def say_hello(self):\n        return \"hi\"\n"
	if err := os.WriteFile(filepath.Join(s.fixture, "hello.c"), []byte(cSrc), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.fixture, "greeter.py"), []byte(pySrc), 0o644)
}

func (s *scanSubcommandState) aNoCGOBinary() error {
	bin, err := buildNoCGOBinary()
	if err != nil {
		return fmt.Errorf("build nocgo binary: %w", err)
	}
	s.nocgoBin = bin
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *scanSubcommandState) execBin(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	s.stdout = outBuf.String()
	s.stderr = errBuf.String()
	s.exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("exec %s: %w", filepath.Base(bin), err)
		}
	}
	return nil
}

func (s *scanSubcommandState) runScanWithStoreAndOutput(store, outFile string) error {
	s.dbPath = filepath.Join(s.tmpDir, outFile)
	return s.execBin(s.binary, "scan", s.fixture, "--store", store, "--out", s.dbPath)
}

func (s *scanSubcommandState) runScanWithGitURLAndSHA(store, outFile string) error {
	s.dbPath = filepath.Join(s.tmpDir, outFile)
	return s.execBin(s.binary, "scan", s.gitURL,
		"--sha", s.gitSHA, "--store", store, "--out", s.dbPath)
}

func (s *scanSubcommandState) runNoCGOScan(store string) error {
	return s.execBin(s.nocgoBin, "scan", s.fixture, "--store", store)
}

func (s *scanSubcommandState) runScanWithNonexistentOutput() error {
	badPath := filepath.Join(s.tmpDir, "nonexistent", "deep", "foo.db")
	return s.execBin(s.binary, "scan", s.fixture, "--store", "sqlite", "--out", badPath)
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *scanSubcommandState) theOutputDBExists() error {
	if _, err := os.Stat(s.dbPath); err != nil {
		return fmt.Errorf("output db %s missing: %w\nstdout:\n%s\nstderr:\n%s",
			s.dbPath, err, s.stdout, s.stderr)
	}
	return nil
}

func (s *scanSubcommandState) dbHasAtLeast1NodeOfKind(kind string) error {
	db, err := sql.Open("sqlite3", s.dbPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM node WHERE kind = ?", kind).Scan(&n); err != nil {
		return fmt.Errorf("query kind %q: %w", kind, err)
	}
	if n < 1 {
		return fmt.Errorf("expected >=1 node of kind %q, got %d", kind, n)
	}
	return nil
}

func (s *scanSubcommandState) summaryHasNonZeroCounts() error {
	for _, want := range []string{"walked:", "parsed:", "nodes:", "edges:"} {
		if !strings.Contains(s.stdout, want) {
			return fmt.Errorf("summary missing %q:\n%s", want, s.stdout)
		}
	}
	// Parse nodes: line and verify each required kind has count >= 1.
	nodesLine := extractSummaryLine(s.stdout, "nodes:")
	if nodesLine == "" {
		return fmt.Errorf("cannot find nodes: line in stdout:\n%s", s.stdout)
	}
	for _, kind := range []string{"repo", "package", "file", "class", "method"} {
		n, err := extractKindCount(nodesLine, kind)
		if err != nil {
			return fmt.Errorf("nodes line missing kind %q: %v\nline: %s", kind, err, nodesLine)
		}
		if n < 1 {
			return fmt.Errorf("expected nodes[%q] >= 1, got %d\nline: %s", kind, n, nodesLine)
		}
	}
	return nil
}

func (s *scanSubcommandState) dbHasAtLeast1Node() error {
	db, err := sql.Open("sqlite3", s.dbPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM node").Scan(&n); err != nil {
		return fmt.Errorf("query nodes: %w", err)
	}
	if n < 1 {
		return fmt.Errorf("expected >=1 node, got %d\nstdout:\n%s\nstderr:\n%s",
			n, s.stdout, s.stderr)
	}
	return nil
}

func (s *scanSubcommandState) exitCodeIs0() error {
	if s.exitCode != 0 {
		return fmt.Errorf("exit code %d (want 0)\nstdout:\n%s\nstderr:\n%s",
			s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *scanSubcommandState) summaryHasSkippedNoParser() error {
	skippedLine := extractSummaryLine(s.stdout, "skipped:")
	if skippedLine == "" {
		return fmt.Errorf("cannot find skipped: line in stdout:\n%s", s.stdout)
	}
	n, err := extractKindCount(skippedLine, "no_parser")
	if err != nil {
		return fmt.Errorf("skipped line missing no_parser: %v\nline: %s", err, skippedLine)
	}
	if n < 1 {
		return fmt.Errorf("expected skipped.no_parser >= 1, got %d\nline: %s", n, skippedLine)
	}
	return nil
}

func (s *scanSubcommandState) stderrHasExtSkip(ext string) error {
	// Verify stderr contains a skip entry mentioning the extension.
	if !strings.Contains(s.stderr, ext) {
		return fmt.Errorf("stderr missing extension %q:\n%s", ext, s.stderr)
	}
	// Verify stdout's per-extension count shows >= 1 for this extension.
	byExtLine := extractSummaryLine(s.stdout, "skipped.no_parser_by_ext:")
	if byExtLine == "" {
		return fmt.Errorf("stdout missing skipped.no_parser_by_ext line:\n%s", s.stdout)
	}
	n, err := extractKindCount(byExtLine, ext)
	if err != nil {
		return fmt.Errorf("per-ext line missing %q: %v\nline: %s", ext, err, byExtLine)
	}
	if n < 1 {
		return fmt.Errorf("expected per-extension count for %q >= 1, got %d\nline: %s",
			ext, n, byExtLine)
	}
	return nil
}

func (s *scanSubcommandState) exitCodeIsNonZero() error {
	if s.exitCode == 0 {
		return fmt.Errorf("exit code 0 (want non-zero)\nstdout:\n%s\nstderr:\n%s",
			s.stdout, s.stderr)
	}
	return nil
}

func (s *scanSubcommandState) stderrNamesIOError() error {
	combined := strings.ToLower(s.stderr)
	markers := []string{
		"no such file", "cannot find the path",
		"the system cannot find", "unable to open database",
		"not a directory", "open sqlite", "open store",
	}
	for _, m := range markers {
		if strings.Contains(combined, m) {
			return nil
		}
	}
	return fmt.Errorf("stderr does not name an IO error:\n%s", s.stderr)
}

// ---------------------------------------------------------------------------
// Summary parsing helpers
// ---------------------------------------------------------------------------

// extractSummaryLine returns the first line whose trimmed form starts
// with prefix (e.g. "nodes:", "skipped:").
func extractSummaryLine(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return line
		}
	}
	return ""
}

// extractKindCount finds `key=N` in a formatted kind-map line like
// `{class=1, file=2, method=3}` and returns N as an int.
func extractKindCount(line, key string) (int, error) {
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `=(\d+)`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, fmt.Errorf("key %q not found in %q", key, line)
	}
	return strconv.Atoi(m[1])
}

// ---------------------------------------------------------------------------
// Initializer & entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_codeintel_cli_binary_scan_subcommand(ctx *godog.ScenarioContext) {
	s := &scanSubcommandState{}
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx, nil
	})

	// Given
	ctx.Given(`^a small local fixture repo with a Go source file$`, s.aSmallLocalFixtureRepo)
	ctx.Given(`^a built codeintel binary$`, s.aBuiltCodeintelBinary)
	ctx.Given(`^a git repository served over HTTP with a known commit SHA$`, s.aGitRepoServedOverHTTP)
	ctx.Given(`^a fixture repo containing "\.c" and "\.py" source files$`, s.aFixtureWithCAndPy)
	ctx.Given(`^a codeintel binary built without CGO$`, s.aNoCGOBinary)

	// When
	ctx.When(`^I run codeintel scan on the fixture with store "([^"]*)" and output "([^"]*)"$`, s.runScanWithStoreAndOutput)
	ctx.When(`^I run codeintel scan with the git URL and SHA using store "([^"]*)" and output "([^"]*)"$`, s.runScanWithGitURLAndSHA)
	ctx.When(`^I run the nocgo binary scan on the fixture with store "([^"]*)"$`, s.runNoCGOScan)
	ctx.When(`^I run codeintel scan on the fixture with output in a nonexistent directory$`, s.runScanWithNonexistentOutput)

	// Then
	ctx.Then(`^the output database file exists$`, s.theOutputDBExists)
	ctx.Then(`^the database contains at least 1 node of kind "([^"]*)"$`, s.dbHasAtLeast1NodeOfKind)
	ctx.Then(`^the stdout summary lists non-zero counts for each kind$`, s.summaryHasNonZeroCounts)
	ctx.Then(`^the database contains at least 1 node of any kind$`, s.dbHasAtLeast1Node)
	ctx.Then(`^the exit code is 0$`, s.exitCodeIs0)
	ctx.Then(`^the stdout summary reports skipped no_parser >= 1$`, s.summaryHasSkippedNoParser)
	ctx.Then(`^stderr contains the per-extension skip count for "([^"]*)"$`, s.stderrHasExtSkip)
	ctx.Then(`^the exit code is non-zero$`, s.exitCodeIsNonZero)
	ctx.Then(`^stderr names the IO error$`, s.stderrNamesIOError)
}

func TestE2E_codeintel_cli_binary_scan_subcommand(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_codeintel_cli_binary_scan_subcommand,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"codeintel_cli_binary_scan_subcommand.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
