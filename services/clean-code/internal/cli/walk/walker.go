package walk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
)

// ErrRootNotFound is the sentinel returned on the walker's
// error channel when the supplied root path does not exist
// on the filesystem. The orchestrator maps this to CLI exit
// code 2 per `tech-spec.md` Sec 8.6.
var ErrRootNotFound = errors.New("walk: root path not found")

// MaxFileSizeBytes is the inclusive per-file size cap the
// walker enforces. Files of exactly this size pass through;
// strictly larger files emit [SkipReasonSizeCap] and their
// bytes are NEVER read into memory.
//
// 2 MiB matches the production Metric Ingestor's parser-side
// cap pinned in `tech-spec.md` Sec 8.3. This is high enough
// that no hand-written source file in the four pinned
// languages realistically trips it; generated files and
// minified vendored assets do.
const MaxFileSizeBytes int64 = 2 * 1024 * 1024

// Canonical [WalkSkip.Reason] strings the walker emits.
// Centralising these as constants keeps the strings spelled
// identically in the walker, the orchestrator, the report
// writer, and the e2e scenarios.
const (
	// SkipReasonDirectory is emitted ONCE per directory in
	// the hard-coded skip list ([DefaultSkipDirs]); the
	// walker then issues [filepath.SkipDir] so no
	// descendant is visited.
	SkipReasonDirectory = "directory_skip"

	// SkipReasonGitignore is emitted per file (or per
	// directory; a gitignored directory yields one skip
	// followed by [filepath.SkipDir]) matched by the
	// accumulated `.gitignore` / `.git/info/exclude`
	// patterns.
	SkipReasonGitignore = "gitignore"

	// SkipReasonSizeCap is emitted when an entry's
	// [fs.FileInfo.Size] strictly exceeds
	// [MaxFileSizeBytes]. The file's bytes are NOT read.
	SkipReasonSizeCap = "size_cap"

	// SkipReasonUnsupportedLanguage is emitted when
	// [parser.DetectLanguage] cannot classify the file as
	// one of the four pinned v1 languages. The file's
	// bytes are NOT read (the walker tests by extension
	// alone before any stat or read).
	SkipReasonUnsupportedLanguage = "unsupported_language"

	// SkipReasonSymlinkLoop is emitted when a symlinked
	// directory's resolved target is an ancestor of the
	// current walk path. The walker does NOT recurse into
	// the symlink, so no infinite loop occurs even when
	// the guard misses a more elaborate cycle.
	SkipReasonSymlinkLoop = "symlink_loop"

	// SkipReasonReadError is emitted when reading a file's
	// bytes fails (permission denied, transient IO error).
	// Per `tech-spec.md` C8 the walker continues; the read
	// error is non-fatal on individual files.
	SkipReasonReadError = "read_error"

	// SkipReasonEmpty is emitted for zero-byte source files
	// because the downstream parser returns
	// [parser.ErrEmptyContent] for those. The architecture
	// (Sec 3.2) explicitly notes the walker filters
	// zero-byte files upstream for this reason.
	SkipReasonEmpty = "empty"
)

// DefaultSkipDirs is the hard-coded baseline list of
// directory names the walker never enters. Membership is
// matched on the directory's bare name (no path component),
// so `vendor/` matches both `vendor/` at the root and any
// nested `services/foo/vendor/`.
//
// Anchor: `architecture.md` Sec 3.1 ("a hard-coded baseline
// list"); `tech-spec.md` Sec 4.2. Both documents pin THIS
// exact list; changing it requires a docs+code coordinated
// edit.
var DefaultSkipDirs = []string{
	".git",
	"node_modules",
	"vendor",
	"target",
	"dist",
	"build",
	".next",
	"__pycache__",
	".venv",
	"venv",
}

// WalkedFile is the (path, content) pair the walker emits per
// kept file. Mirrors `architecture.md` Sec 4.2 verbatim.
type WalkedFile struct {
	// RepoRelPath is the file's path relative to the walk
	// root, normalised to forward slash so it matches the
	// `AstFile.path` shape regardless of host OS.
	RepoRelPath string

	// AbsPath is the OS-native absolute path. Used only
	// for IO; never embedded in downstream artifacts.
	AbsPath string

	// Language is the canonical language tag returned by
	// [parser.DetectLanguage] (`go`, `python`,
	// `typescript`, `java`).
	Language string

	// SizeBytes is the file size in bytes (matches
	// `len(Content)`). Retained for diagnostics and the
	// report writer's per-file summary.
	SizeBytes int64

	// Content is the raw file bytes read once by the
	// walker and handed downstream by reference. Owners
	// downstream MUST NOT mutate the slice.
	Content []byte
}

// WalkSkip is one entry in the walker's skip list, surfaced
// for diagnostics. The composition root aggregates skips into
// the `findings.json` `skips` array.
type WalkSkip struct {
	// Path is repo-relative, forward-slash.
	Path string

	// Reason is one of the `SkipReason*` constants.
	Reason string
}

// Walker is the L1 contract documented in `architecture.md`
// Sec 5.1. The returned channels are closed when the walk
// completes; the consumer MUST drain ALL THREE concurrently
// or cancel `ctx` to release the walker goroutine.
type Walker interface {
	Walk(ctx context.Context, root string) (<-chan WalkedFile, <-chan WalkSkip, <-chan error)
}

// DefaultWalker is the production [Walker] implementation.
// All fields are optional; the zero value is usable. Tests
// override [DefaultWalker.StatFn] / [DefaultWalker.ReadFileFn]
// to assert the "never read" invariant on oversize files
// (`implementation-plan.md` Stage 2.1 size-cap scenario).
type DefaultWalker struct {
	// SkipDirs is the set of directory names skipped
	// before traversal. Defaults to [DefaultSkipDirs] when
	// nil.
	SkipDirs map[string]struct{}

	// MaxBytes is the inclusive per-file size cap.
	// Defaults to [MaxFileSizeBytes] when zero.
	MaxBytes int64

	// DetectLanguage is the extension-only language
	// classifier; defaults to `parser.DetectLanguage(path,
	// nil)` semantics. Pinned as a field so tests can
	// stub it without globals.
	DetectLanguage func(path string) (string, bool)

	// StatFn is the size-only stat hook called for each
	// file BEFORE any read. Defaults to
	// [fs.DirEntry.Info] when nil. Tests override to
	// assert the walker never opens an oversize file.
	StatFn func(absPath string, d fs.DirEntry) (int64, error)

	// ReadFileFn is the file-bytes reader called only for
	// files within the size cap. Defaults to [os.ReadFile]
	// when nil. Tests override to assert non-call for
	// oversize fixtures.
	ReadFileFn func(absPath string) ([]byte, error)
}

// NewDefaultWalker returns a [DefaultWalker] with all hooks
// pointing at the production defaults.
func NewDefaultWalker() *DefaultWalker {
	return &DefaultWalker{}
}

// channelBufferSize is the buffered capacity of each output
// channel. Sized so the walker can stage a small batch of
// emissions ahead of the consumer without blocking the
// per-entry callback; the consumer is still expected to drain
// continuously. 64 is a balance between hiding consumer
// jitter and keeping memory predictable on huge repos.
const channelBufferSize = 64

// Walk implements [Walker]. The traversal runs in a
// background goroutine that closes all three channels on
// return. Fatal walk failures (missing root, root not a
// directory, root unreadable) appear on the error channel
// AND cause the file/skip channels to close with zero
// further emissions.
func (w *DefaultWalker) Walk(ctx context.Context, root string) (<-chan WalkedFile, <-chan WalkSkip, <-chan error) {
	files := make(chan WalkedFile, channelBufferSize)
	skips := make(chan WalkSkip, channelBufferSize)
	errs := make(chan error, 1)

	go w.run(ctx, root, files, skips, errs)

	return files, skips, errs
}

// run is the goroutine body. Splitting it out of [Walk] keeps
// the deferred channel-close ordering explicit and makes the
// function unit-testable as a single call.
func (w *DefaultWalker) run(ctx context.Context, root string, files chan<- WalkedFile, skips chan<- WalkSkip, errs chan<- error) {
	defer close(files)
	defer close(skips)
	defer close(errs)

	maxBytes := w.MaxBytes
	if maxBytes == 0 {
		maxBytes = MaxFileSizeBytes
	}
	skipDirs := w.SkipDirs
	if skipDirs == nil {
		skipDirs = make(map[string]struct{}, len(DefaultSkipDirs))
		for _, name := range DefaultSkipDirs {
			skipDirs[name] = struct{}{}
		}
	}
	detectLang := w.DetectLanguage
	if detectLang == nil {
		detectLang = func(p string) (string, bool) {
			return parser.DetectLanguage(p, nil)
		}
	}
	statFn := w.StatFn
	if statFn == nil {
		statFn = func(_ string, d fs.DirEntry) (int64, error) {
			info, err := d.Info()
			if err != nil {
				return 0, err
			}
			return info.Size(), nil
		}
	}
	readFile := w.ReadFileFn
	if readFile == nil {
		readFile = os.ReadFile
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sendErr(ctx, errs, ErrRootNotFound)
			return
		}
		sendErr(ctx, errs, fmt.Errorf("walk: stat root %q: %w", root, err))
		return
	}
	if !rootInfo.IsDir() {
		sendErr(ctx, errs, fmt.Errorf("walk: root is not a directory: %s", root))
		return
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		sendErr(ctx, errs, fmt.Errorf("walk: abs root %q: %w", root, err))
		return
	}

	patterns := loadGitInfoExcludePatterns(absRoot)
	// Also load the root's own .gitignore before traversal.
	// The early-return on the root entry below means the
	// per-directory loader (Step 4 of the walk callback) would
	// otherwise skip the root, leaving root-anchored patterns
	// (the common case) unloaded.
	patterns = append(patterns, loadGitignorePatterns(absRoot, absRoot, "")...)

	walkErr := filepath.WalkDir(absRoot, func(absPath string, d fs.DirEntry, walkErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if walkErr != nil {
			// Per-entry errors are non-fatal: surface
			// as a read_error skip and keep walking
			// the rest of the tree. The only fatal
			// path is the root itself, which we
			// already validated above.
			relPath, _ := repoRelPath(absRoot, absPath)
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonReadError})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Never visit the root itself as an entry; it's
		// the implicit traversal anchor and has no
		// meaningful WalkedFile or skip semantics.
		if absPath == absRoot {
			return nil
		}

		relPath, ok := repoRelPath(absRoot, absPath)
		if !ok {
			// Defensive: WalkDir promised a descendant
			// of absRoot; if Rel ever fails we skip
			// the entry rather than emit a malformed
			// row.
			return nil
		}

		isDir := d.IsDir()

		// 1. Hard-coded skip directories (cheapest, by
		//    name).
		if isDir {
			if _, drop := skipDirs[d.Name()]; drop {
				sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonDirectory})
				return filepath.SkipDir
			}
		}

		// 2. Gitignore (parent matcher applies to BOTH
		//    files and directories; descend rules require
		//    we evaluate the directory entry BEFORE
		//    loading its own .gitignore so the directory
		//    itself isn't matched against its descendants'
		//    rules).
		matcher := gitignore.NewMatcher(patterns)
		if matcher.Match(splitToComponents(relPath), isDir) {
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonGitignore})
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		// 3. Symlink-loop guard. Symlinks to non-loop
		//    targets are NOT followed (WalkDir's default);
		//    only true ancestor cycles are reported.
		if isSymlink(d) {
			if loopsToAncestor(absPath) {
				sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonSymlinkLoop})
			}
			// Either way we do not recurse into the
			// symlink target.
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		// 4. On entering a non-ignored directory, load
		//    its .gitignore (if any) so descendants are
		//    evaluated against the deepest available
		//    rules. The directory's OWN entry was
		//    already checked above against the parent
		//    matcher.
		if isDir {
			loaded := loadGitignorePatterns(absRoot, absPath, relPath)
			if len(loaded) > 0 {
				patterns = append(patterns, loaded...)
			}
			return nil
		}

		// File path: extension language detect FIRST
		// (no syscalls, no reads) so unsupported files
		// short-circuit without touching the disk again.
		lang, supported := detectLang(relPath)
		if !supported {
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonUnsupportedLanguage})
			return nil
		}

		// 5. Size cap from the DirEntry's already-cached
		//    Info() (no Open call).
		size, err := statFn(absPath, d)
		if err != nil {
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonReadError})
			return nil
		}
		if size > maxBytes {
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonSizeCap})
			return nil
		}
		if size == 0 {
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonEmpty})
			return nil
		}

		// 6. Read content and emit.
		content, err := readFile(absPath)
		if err != nil {
			sendSkip(ctx, skips, WalkSkip{Path: relPath, Reason: SkipReasonReadError})
			return nil
		}

		sendFile(ctx, files, WalkedFile{
			RepoRelPath: relPath,
			AbsPath:     absPath,
			Language:    lang,
			SizeBytes:   int64(len(content)),
			Content:     content,
		})
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, context.Canceled) && !errors.Is(walkErr, context.DeadlineExceeded) {
		sendErr(ctx, errs, fmt.Errorf("walk: %w", walkErr))
	}
}

// repoRelPath turns an absolute path into a repo-relative,
// forward-slash-normalised path. Returns ("", false) when
// the absolute path is not a descendant of the root.
func repoRelPath(absRoot, absPath string) (string, bool) {
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", false
	}
	return rel, true
}

// splitToComponents splits a forward-slash repo-relative path
// into the component slice gitignore's matcher expects.
func splitToComponents(relPath string) []string {
	if relPath == "" {
		return nil
	}
	return strings.Split(relPath, "/")
}

// isSymlink reports whether the DirEntry represents a symbolic
// link (file or directory). [filepath.WalkDir] surfaces the
// Lstat type via [fs.DirEntry.Type], so symlinks remain
// distinguishable from their targets here.
func isSymlink(d fs.DirEntry) bool {
	if d == nil {
		return false
	}
	return d.Type()&fs.ModeSymlink != 0
}

// loopsToAncestor reports whether `absPath` is a symlink whose
// resolved target is an ancestor (or equal) of `absPath`
// itself. This is the v1 best-effort cycle detector
// `architecture.md` Sec 3.1 calls out; chains that do not
// trace through an ancestor are NOT caught (filepath.WalkDir
// does not follow symlinks, so no infinite loop occurs even
// when this guard misses).
func loopsToAncestor(absPath string) bool {
	target, err := os.Readlink(absPath)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(absPath), target)
	}
	target = filepath.Clean(target)

	cur := filepath.Clean(absPath)
	for {
		parent := filepath.Dir(cur)
		if pathsEqual(parent, target) {
			return true
		}
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// pathsEqual compares two filesystem paths for equality with
// case-insensitive semantics on Windows (where NTFS is
// case-insensitive in practice) and exact semantics on
// POSIX. Symlink-loop detection trips on either spelling.
func pathsEqual(a, b string) bool {
	if a == b {
		return true
	}
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return false
}

// loadGitInfoExcludePatterns reads `<absRoot>/.git/info/exclude`
// if present, returning the parsed gitignore patterns with an
// empty domain (the file's rules apply to the entire repo per
// real-git semantics).
func loadGitInfoExcludePatterns(absRoot string) []gitignore.Pattern {
	path := filepath.Join(absRoot, ".git", "info", "exclude")
	return parseGitignoreFile(path, nil)
}

// loadGitignorePatterns parses `<absDir>/.gitignore` (if
// present) into gitignore patterns whose domain is the
// directory's components relative to the repo root. An empty
// `relDir` signals the file lives at the repo root.
func loadGitignorePatterns(absRoot, absDir, relDir string) []gitignore.Pattern {
	_ = absRoot // retained for symmetry with loadGitInfoExcludePatterns
	domain := splitToComponents(relDir)
	return parseGitignoreFile(filepath.Join(absDir, ".gitignore"), domain)
}

// parseGitignoreFile opens a gitignore-format file, filters
// blank and comment lines per the gitignore spec, and returns
// one [gitignore.Pattern] per surviving line. A missing file
// is non-fatal and yields a nil slice. A read error is also
// non-fatal so a malformed `.git/info/exclude` does not block
// the entire walk.
//
// Pattern order in the returned slice mirrors the file: when
// callers append the result to a growing patterns list, the
// [gitignore.Matcher.Match] iteration order (last pattern
// wins) preserves git's "deepest / latest match decides" rule.
func parseGitignoreFile(path string, domain []string) []gitignore.Pattern {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var out []gitignore.Pattern
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Strip a UTF-8 BOM if it slipped into the first
		// line; git itself tolerates it silently.
		line = strings.TrimPrefix(line, "\uFEFF")
		// Trim trailing whitespace per the spec (gitignore
		// drops trailing spaces unless backslash-quoted;
		// the parser handles the quoted case, we only need
		// to drop the trailing whitespace on bare lines).
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, gitignore.ParsePattern(line, domain))
	}
	// A scanner error is non-fatal; we still return any
	// patterns parsed before the failure.
	return out
}

// sendFile writes a [WalkedFile] honouring context
// cancellation; a cancelled context drops the send and lets
// the walker goroutine wind down cleanly.
func sendFile(ctx context.Context, ch chan<- WalkedFile, f WalkedFile) {
	select {
	case ch <- f:
	case <-ctx.Done():
	}
}

// sendSkip writes a [WalkSkip] honouring context cancellation.
func sendSkip(ctx context.Context, ch chan<- WalkSkip, s WalkSkip) {
	select {
	case ch <- s:
	case <-ctx.Done():
	}
}

// sendErr writes an error honouring context cancellation. The
// errs channel is buffered to 1 so this send completes
// without a reader in the common case (the consumer reads it
// after draining files / skips).
func sendErr(ctx context.Context, ch chan<- error, err error) {
	select {
	case ch <- err:
	case <-ctx.Done():
	}
}

// Skipped wraps a [WalkSkip] slice with a deterministic sort.
// Callers can use it to surface a stable ordering in the
// findings.json `skips` array per `tech-spec.md` C11.
//
// The sort is `(Path asc, Reason asc)`; ties on Path are rare
// in practice but the secondary key keeps the ordering total.
func Skipped(in []WalkSkip) []WalkSkip {
	out := make([]WalkSkip, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
