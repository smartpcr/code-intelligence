package repoindexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Materializer materialises a repo at a specific SHA into a
// transient workspace the full-mode handler can walk. The
// abstraction lets the worker swap between the production
// `GitMaterializer` (shells out to `git`) and the test-only
// `InMemoryMaterializer` (synthesises a workspace without
// touching disk) without recompiling the worker.
type Materializer interface {
	// Materialize prepares a Workspace for the requested
	// (repoURL, sha) pair. The caller MUST invoke
	// `Workspace.Close` when finished to release the temp dir.
	Materialize(ctx context.Context, repoURL, sha string) (Workspace, error)
}

// Workspace is a read-only handle to a materialised repo. The
// implementation owns the underlying storage (disk for git,
// in-process map for tests); `Close` releases it.
type Workspace interface {
	// Root returns the absolute filesystem path of the
	// materialised tree. For `InMemoryMaterializer` the path
	// is a sentinel string and is not meant to be touched by
	// `os` calls -- callers must use `Walk` to enumerate
	// files.
	Root() string
	// Walk visits every source file in the workspace in a
	// stable, lexicographic order. Implementations skip VCS
	// directories (`.git`, `.hg`) and other configured
	// excludes; they DO NOT skip files by extension --
	// language filtering is the AST emitter's job, not the
	// materializer's.
	Walk(fn WalkFn) error
	// Close releases the workspace. Safe to call multiple
	// times; subsequent calls return nil.
	Close() error
}

// WalkFn is the visitor signature the materializer invokes for
// each file. Returning a non-nil error aborts the walk; returning
// `fs.SkipDir` from a file visit is undefined (the walker only
// surfaces files, never directories).
type WalkFn func(file WalkFile) error

// WalkFile describes a single file surfaced by `Workspace.Walk`.
type WalkFile struct {
	// RelPath is the file path relative to `Workspace.Root`,
	// always using forward slashes regardless of host OS.
	// This is the canonical key the full-mode handler hashes
	// into Node canonical signatures, so cross-platform
	// stability is load-bearing.
	RelPath string
	// AbsPath is the absolute filesystem path of the file.
	// For `InMemoryMaterializer` this is the empty string;
	// the AST emitter must read content via `Reader` instead.
	AbsPath string
	// SizeBytes is the file's logical size in bytes. -1 when
	// unknown (e.g. in-memory entries that don't track size).
	SizeBytes int64
	// Reader returns a fresh io.ReadCloser for the file's
	// contents. The caller is responsible for closing the
	// reader. Provided so unit tests can supply in-memory
	// content without writing to disk; the git materializer
	// returns a wrapped `os.File` opened on AbsPath.
	Reader func() (ReadCloser, error)
}

// ReadCloser is a minimal subset of io.ReadCloser exposed here
// to avoid a hard dependency on io from package consumers that
// only Walk file metadata.
type ReadCloser interface {
	Read(p []byte) (int, error)
	Close() error
}

// ----- GitMaterializer -------------------------------------------

// GitMaterializer materialises repos by shelling out to the `git`
// CLI. The pattern is:
//
//	git init <dir>
//	git -C <dir> remote add origin <url>
//	git -C <dir> fetch --depth=1 origin <sha>
//	git -C <dir> checkout FETCH_HEAD
//
// `--depth=1` keeps clones cheap on multi-GB monorepos; the
// per-SHA fetch relies on `uploadpack.allowReachableSHA1InWant`
// (default-on at GitHub / GitLab Enterprise, server-side toggle
// elsewhere). When the upstream server forbids per-SHA fetches
// the four-step sequence above fails on step 3 and the worker
// records `status='failed'`; this is the desired behaviour --
// silently falling back to a full clone would blow the §8.3
// 30-minute budget on large repos.
type GitMaterializer struct {
	// BaseDir is where temp workspaces are created. Empty
	// means `os.TempDir()`.
	BaseDir string
	// Runner exec.LookPath-resolved binary. Empty means "git"
	// (resolved from PATH at runtime).
	GitBinary string
	// FetchTimeout caps a single `git fetch` invocation. 0
	// means use `defaultFetchTimeout`.
	FetchTimeout time.Duration
	// ExcludeDirs is the set of directory names skipped during
	// Walk. Defaults applied if nil: `.git`, `.hg`, `.svn`,
	// `node_modules`, `vendor`, `target`, `bin`, `obj`.
	ExcludeDirs []string

	// runCmd is the underlying exec hook. Tests override it to
	// record commands without invoking git; production passes
	// nil so the default `runRealCmd` is used.
	runCmd func(ctx context.Context, dir, bin string, args ...string) error
}

const defaultFetchTimeout = 5 * time.Minute

// defaultExcludeDirs is the conservative list of directories
// the Walk skips when the user passes no override. The list is
// intentionally short -- the goal is to skip VCS and obvious
// build-artefact directories that would inflate the file count
// without contributing to the Repo→Package→File ancestry.
// Language-specific exclusions belong to the Stage 3.2 AST
// dispatcher, not the materializer.
var defaultExcludeDirs = []string{
	".git", ".hg", ".svn",
	"node_modules", "vendor", "target", "bin", "obj",
	"__pycache__", ".venv", ".tox",
}

// Materialize clones the requested SHA into a fresh temp dir.
// Returns a `*gitWorkspace` whose `Close` removes the temp dir.
func (g *GitMaterializer) Materialize(ctx context.Context, repoURL, sha string) (Workspace, error) {
	if repoURL == "" {
		return nil, errors.New("repoindexer: GitMaterializer.Materialize: empty repoURL")
	}
	if sha == "" {
		return nil, errors.New("repoindexer: GitMaterializer.Materialize: empty sha")
	}
	bin := g.GitBinary
	if bin == "" {
		bin = "git"
	}
	base := g.BaseDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "repoindexer-")
	if err != nil {
		return nil, fmt.Errorf("repoindexer: mkdtemp: %w", err)
	}
	runner := g.runCmd
	if runner == nil {
		runner = runRealCmd
	}
	fetchTO := g.FetchTimeout
	if fetchTO == 0 {
		fetchTO = defaultFetchTimeout
	}

	// `git init` is fast; bound it with the parent context only.
	if err := runner(ctx, dir, bin, "init", "--quiet"); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("repoindexer: git init: %w", err)
	}
	if err := runner(ctx, dir, bin, "remote", "add", "origin", repoURL); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("repoindexer: git remote add: %w", err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTO)
	defer cancel()
	if err := runner(fetchCtx, dir, bin, "fetch", "--depth=1", "--quiet", "origin", sha); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("repoindexer: git fetch %s: %w", sha, err)
	}
	if err := runner(ctx, dir, bin, "checkout", "--quiet", "FETCH_HEAD"); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("repoindexer: git checkout: %w", err)
	}

	excludes := g.ExcludeDirs
	if excludes == nil {
		excludes = defaultExcludeDirs
	}
	excludeSet := make(map[string]struct{}, len(excludes))
	for _, d := range excludes {
		excludeSet[d] = struct{}{}
	}
	return &gitWorkspace{root: dir, excludeDirs: excludeSet}, nil
}

// runRealCmd is the production exec hook. Captures combined
// stdout/stderr into the error so failures land in the structured
// log with enough context to triage.
func runRealCmd(ctx context.Context, dir, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)",
			bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitWorkspace is the on-disk Workspace implementation backed by
// a temp dir. Walk delegates to `filepath.WalkDir` and skips the
// configured exclude-dirs.
type gitWorkspace struct {
	root        string
	excludeDirs map[string]struct{}
	closed      bool
}

func (w *gitWorkspace) Root() string { return w.root }

func (w *gitWorkspace) Walk(fn WalkFn) error {
	if fn == nil {
		return errors.New("repoindexer: Walk: nil WalkFn")
	}
	// Collect first so the surface order is deterministic
	// (filepath.WalkDir already sorts per-directory, but we
	// re-sort the full collected list to keep cross-test
	// expectations stable when callers rely on lexicographic
	// global order).
	var files []WalkFile
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != w.root {
				if _, skip := w.excludeDirs[name]; skip {
					return fs.SkipDir
				}
			}
			return nil
		}
		// Regular files only -- skip symlinks, sockets, etc.
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(w.root, path)
		if err != nil {
			return fmt.Errorf("repoindexer: relpath %s: %w", path, err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("repoindexer: stat %s: %w", path, err)
		}
		abs := path
		files = append(files, WalkFile{
			RelPath:   filepath.ToSlash(rel),
			AbsPath:   abs,
			SizeBytes: info.Size(),
			Reader: func() (ReadCloser, error) {
				return os.Open(abs)
			},
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	for _, f := range files {
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func (w *gitWorkspace) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return os.RemoveAll(w.root)
}

// ----- InMemoryMaterializer --------------------------------------

// InMemoryMaterializer is the test-only Materializer that hands
// out a synthetic Workspace from in-process state. Used by the
// Stage 3.1 integration tests so the small-fixture and
// idempotent-re-ingest scenarios run without a git binary and
// without network access.
//
// A single InMemoryMaterializer can serve multiple `Materialize`
// calls; the same `Files` slice is reused for every workspace
// it returns. Tests that want different content per call should
// instantiate a fresh materializer per call.
type InMemoryMaterializer struct {
	// Files is the synthetic file set. Each entry's RelPath
	// MUST use forward slashes and MUST NOT be empty. The
	// materializer returns them in lexicographic order
	// regardless of insertion order, matching the on-disk
	// Walk contract.
	Files []InMemoryFile
}

// InMemoryFile is one entry in an InMemoryMaterializer's file
// set. Content is held verbatim; the test-side Reader returns a
// `bytes.Reader`-wrapped copy on each call so multiple readers
// see independent positions.
type InMemoryFile struct {
	RelPath string
	Content []byte
}

// Materialize returns a Workspace whose `Walk` yields the
// configured files. The `repoURL` and `sha` arguments are
// recorded on the returned workspace for assertion convenience
// but are otherwise unused.
func (m *InMemoryMaterializer) Materialize(_ context.Context, repoURL, sha string) (Workspace, error) {
	if repoURL == "" {
		return nil, errors.New("repoindexer: InMemoryMaterializer.Materialize: empty repoURL")
	}
	if sha == "" {
		return nil, errors.New("repoindexer: InMemoryMaterializer.Materialize: empty sha")
	}
	files := make([]InMemoryFile, len(m.Files))
	copy(files, m.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	for _, f := range files {
		if f.RelPath == "" {
			return nil, errors.New("repoindexer: InMemoryMaterializer.Materialize: empty RelPath")
		}
		if strings.ContainsRune(f.RelPath, '\\') {
			return nil, fmt.Errorf(
				"repoindexer: InMemoryMaterializer.Materialize: RelPath %q must use forward slashes",
				f.RelPath,
			)
		}
	}
	return &inMemoryWorkspace{files: files, sha: sha, repoURL: repoURL}, nil
}

type inMemoryWorkspace struct {
	files   []InMemoryFile
	sha     string
	repoURL string
	closed  bool
}

func (w *inMemoryWorkspace) Root() string {
	// Sentinel string -- callers that try to os.Open it should
	// fail loudly so the test author notices they reached for
	// the wrong API.
	return "<in-memory>"
}

func (w *inMemoryWorkspace) Walk(fn WalkFn) error {
	if fn == nil {
		return errors.New("repoindexer: Walk: nil WalkFn")
	}
	if w.closed {
		return errors.New("repoindexer: Walk on closed workspace")
	}
	for _, f := range w.files {
		content := f.Content
		entry := WalkFile{
			RelPath:   f.RelPath,
			AbsPath:   "",
			SizeBytes: int64(len(content)),
			Reader: func() (ReadCloser, error) {
				return newBytesReadCloser(content), nil
			},
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return nil
}

func (w *inMemoryWorkspace) Close() error {
	w.closed = true
	return nil
}

// bytesReadCloser wraps a byte slice as a ReadCloser. Kept tiny
// so the in-memory path doesn't pull bytes.Buffer (which carries
// write-side state we don't need).
type bytesReadCloser struct {
	data []byte
	pos  int
}

func newBytesReadCloser(b []byte) *bytesReadCloser {
	return &bytesReadCloser{data: b}
}

func (b *bytesReadCloser) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

func (b *bytesReadCloser) Close() error { return nil }
