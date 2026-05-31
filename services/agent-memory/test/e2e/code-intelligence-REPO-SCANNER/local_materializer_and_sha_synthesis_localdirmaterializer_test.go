//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// defaultExcludeDirs mirrors the package-level var in materialize.go.
// Kept in sync manually; the local-non-git-sha scenario validates
// equality with the real implementation so any drift is caught.
var localDirDefaultExcludeDirs = []string{
	".git", ".hg", ".svn",
	"node_modules", "vendor", "target", "bin", "obj",
	"__pycache__", ".venv", ".tox",
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type localDirMaterializerState struct {
	root           string
	workspace      repoindexer.Workspace
	operatorSHA    string
	walkFiles      []string
	gitHeadSHA     string
	materializeErr error
	fileURL        string // file:// URL for the root directory
	gitBinary      string // override GitBinary; empty means default
}

func (s *localDirMaterializerState) writeFile(rel, content string) error {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func (s *localDirMaterializerState) cleanup() {
	if s.workspace != nil {
		_ = s.workspace.Close()
	}
	if s.root != "" {
		os.RemoveAll(s.root)
	}
}

// ---------------------------------------------------------------------------
// Git helpers
// ---------------------------------------------------------------------------

func gitExe() string {
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("git.exe"); err == nil {
			return p
		}
	}
	return "git"
}

// toFileURL replicates the unexported synthesizeFileURL logic from
// materialize.go so the e2e test passes a file:// URL to Materialize
// exactly as the acceptance scenario requires.
func toFileURL(abs string) string {
	slashed := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" && len(slashed) >= 2 && slashed[1] == ':' {
		slashed = strings.ToLower(slashed[:2]) + slashed[2:]
		return "file:///" + slashed
	}
	return "file://" + slashed
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command(gitExe(), args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %s (%w)", args, out, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func initGitRepo(dir string) (string, error) {
	if _, err := runGit(dir, "init"); err != nil {
		return "", err
	}
	if _, err := runGit(dir, "config", "user.email", "test@test.com"); err != nil {
		return "", err
	}
	if _, err := runGit(dir, "config", "user.name", "Test"); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		return "", err
	}
	if _, err := runGit(dir, "add", "."); err != nil {
		return "", err
	}
	if _, err := runGit(dir, "commit", "-m", "init"); err != nil {
		return "", err
	}
	sha, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return sha, nil
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *localDirMaterializerState) aTempDirWithoutGit() error {
	s.root = filepath.Join(os.TempDir(), fmt.Sprintf("ldm-nongit-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	if err := s.writeFile("src/main.go", "package main\n"); err != nil {
		return err
	}
	return s.writeFile("README.md", "hello\n")
}

func (s *localDirMaterializerState) aTempDirThatIsGitCheckout() error {
	s.root = filepath.Join(os.TempDir(), fmt.Sprintf("ldm-git-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	sha, err := initGitRepo(s.root)
	if err != nil {
		return fmt.Errorf("initGitRepo: %w", err)
	}
	s.gitHeadSHA = sha
	return nil
}

func (s *localDirMaterializerState) anOperatorSuppliedSHA(sha string) error {
	s.operatorSHA = sha
	return nil
}

func (s *localDirMaterializerState) gitBinarySetToNonexistent() error {
	s.gitBinary = "/nonexistent-git-binary-e2e-proof-no-rev-parse"
	return nil
}

func (s *localDirMaterializerState) aTempDirWithExcludedDirs() error {
	s.root = filepath.Join(os.TempDir(), fmt.Sprintf("ldm-walk-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"src/main.go":                    "package main\n",
		"README.md":                      "readme\n",
		"node_modules/leftpad/index.js":  "module.exports = 1;\n",
		".git/HEAD":                      "ref: refs/heads/main\n",
		".git/objects/ab/cd":             "blob\n",
	}
	for rel, content := range files {
		if err := s.writeFile(rel, content); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *localDirMaterializerState) materializeWithFileURLAndEmptySHA() error {
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return fmt.Errorf("abs: %w", err)
	}
	s.fileURL = toFileURL(absRoot)
	m := &repoindexer.LocalDirMaterializer{}
	ws, err := m.Materialize(context.Background(), s.fileURL, "")
	if err != nil {
		return fmt.Errorf("Materialize(file://): %w", err)
	}
	s.workspace = ws
	return nil
}

func (s *localDirMaterializerState) materializeWithEmptySHA() error {
	m := &repoindexer.LocalDirMaterializer{}
	ws, err := m.Materialize(context.Background(), s.root, "")
	if err != nil {
		return fmt.Errorf("Materialize: %w", err)
	}
	s.workspace = ws
	return nil
}

func (s *localDirMaterializerState) materializeWithOperatorSHA() error {
	m := &repoindexer.LocalDirMaterializer{GitBinary: s.gitBinary}
	ws, err := m.Materialize(context.Background(), s.root, s.operatorSHA)
	if err != nil {
		return fmt.Errorf("Materialize: %w", err)
	}
	s.workspace = ws
	return nil
}

func (s *localDirMaterializerState) walkRunsOnWorkspace() error {
	m := &repoindexer.LocalDirMaterializer{}
	ws, err := m.Materialize(context.Background(), s.root, "walk-test-sha")
	if err != nil {
		return fmt.Errorf("Materialize: %w", err)
	}
	s.workspace = ws
	s.walkFiles = nil
	return ws.Walk(func(f repoindexer.WalkFile) error {
		s.walkFiles = append(s.walkFiles, f.RelPath)
		return nil
	})
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *localDirMaterializerState) shaEqualsMTimeTreeSHA() error {
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return fmt.Errorf("abs: %w", err)
	}
	want, err := fingerprint.MTimeTreeSHA(absRoot, localDirDefaultExcludeDirs)
	if err != nil {
		return fmt.Errorf("MTimeTreeSHA: %w", err)
	}
	got := s.workspace.SHA()
	if got != want {
		return fmt.Errorf("SHA mismatch: got %q, want MTimeTreeSHA=%q", got, want)
	}
	return nil
}

func (s *localDirMaterializerState) shaEqualsGitRevParseHEAD() error {
	got := s.workspace.SHA()
	if got != s.gitHeadSHA {
		return fmt.Errorf("SHA mismatch: got %q, want git rev-parse HEAD=%q", got, s.gitHeadSHA)
	}
	return nil
}

func (s *localDirMaterializerState) shaEqualsOperatorSHA() error {
	got := s.workspace.SHA()
	if got != s.operatorSHA {
		return fmt.Errorf("SHA mismatch: got %q, want operator sha=%q", got, s.operatorSHA)
	}
	return nil
}

func (s *localDirMaterializerState) noWalkFileFromExcludedDirs() error {
	excluded := []string{"node_modules/", ".git/"}
	for _, f := range s.walkFiles {
		for _, ex := range excluded {
			if strings.HasPrefix(f, ex) {
				return fmt.Errorf("Walk returned file %q which is inside excluded dir %q", f, ex)
			}
		}
	}
	if len(s.walkFiles) == 0 {
		return fmt.Errorf("Walk returned zero files; expected at least the non-excluded files")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_local_materializer_and_sha_synthesis_localdirmaterializer(ctx *godog.ScenarioContext) {
	s := &localDirMaterializerState{}

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx, nil
	})

	// Given
	ctx.Given(`^a temporary directory without "\.git/"$`, s.aTempDirWithoutGit)
	ctx.Given(`^a temporary directory that is a git checkout with at least one commit$`, s.aTempDirThatIsGitCheckout)
	ctx.Given(`^an operator-supplied sha "([^"]*)"$`, s.anOperatorSuppliedSHA)
	ctx.Given(`^GitBinary is set to a nonexistent path so any git invocation would fail$`, s.gitBinarySetToNonexistent)
	ctx.Given(`^a temporary directory containing "node_modules/" and "\.git/" with files inside$`, s.aTempDirWithExcludedDirs)

	// When
	ctx.When(`^Materialize runs with a "file://" URL and an empty sha$`, s.materializeWithFileURLAndEmptySHA)
	ctx.When(`^Materialize runs with an empty sha$`, s.materializeWithEmptySHA)
	ctx.When(`^Materialize runs with the operator-supplied sha$`, s.materializeWithOperatorSHA)
	ctx.When(`^Workspace\.Walk runs on the materialized workspace$`, s.walkRunsOnWorkspace)

	// Then
	ctx.Then(`^Workspace\.SHA equals MTimeTreeSHA of the directory with defaultExcludeDirs$`, s.shaEqualsMTimeTreeSHA)
	ctx.Then(`^Workspace\.SHA equals the output of "git rev-parse HEAD" in that directory$`, s.shaEqualsGitRevParseHEAD)
	ctx.Then(`^Workspace\.SHA equals the operator-supplied sha$`, s.shaEqualsOperatorSHA)
	ctx.Then(`^no WalkFile originates inside "node_modules/" or "\.git/"$`, s.noWalkFileFromExcludedDirs)
}

func TestE2E_local_materializer_and_sha_synthesis_localdirmaterializer(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"local_materializer_and_sha_synthesis_localdirmaterializer.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_local_materializer_and_sha_synthesis_localdirmaterializer,
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
