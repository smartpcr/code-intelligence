// Package repocontext assembles the CLI-local [RepoContext]
// every `cleanc` sub-command threads through the L1 - L6
// pipeline. It is the single place repo-root identity is
// minted; the rest of the CLI receives a frozen [RepoContext]
// value and never re-derives `repo_id` or `head_sha` on its
// own.
//
// The package is the home of two CLEAN-CODE arch G2 invariants:
//
//  1. [MintRepoID] mints `repo_id` as a deterministic UUID-v5
//     so re-runs on the same path yield the same id.
//  2. [DetectHeadSHA] returns the working-tree HEAD SHA when
//     the root is a git repo, else the literal sentinel
//     `"working-copy"` so an un-versioned tree still produces
//     stable downstream IDs across re-runs.
//
// Anchors: REFACTOR-GUIDE `architecture.md` Sec 4.1 (RepoContext),
// Sec 1.4 (invariants G2); `tech-spec.md` Sec 4.11 (stable
// identity) and constraint C3.
package repocontext

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/gofrs/uuid"
)

// RepoIDNamespaceNamePrefix is the literal prefix prepended to
// the normalised root-path before it is hashed by UUID-v5.
//
// Pinned in source so a future edit (and the resulting
// per-row identity drift) is loud rather than silent; the
// `TestMintRepoID_*` golden assertions in repocontext_test.go
// lock in both this prefix AND the derived UUID bytes for a
// known fixture path.
//
// The path slash MUST stay forward-slash so the pre-image
// matches the normalised root-path that follows it; mixing
// separators here would defeat the [MintRepoID] normalisation
// path on Windows.
const RepoIDNamespaceNamePrefix = "cleanc.local-repo/"

// HeadSHAWorkingCopySentinel is the literal string [DetectHeadSHA]
// returns when the supplied path is not a git working tree.
//
// Anchor: `architecture.md` Sec 4.1 ("HeadSHA: ... else
// `"working-copy"`"); Sec 1.4 G2 invariant ("`first_seen_sha`
// is the current `HEAD` commit if a git repo is present, else
// the literal string `"working-copy"`"). The sentinel is
// stamped onto every `MetricSample`, `Finding`, `HotSpot`,
// and `RefactorTask` so two un-versioned runs against the
// same tree still produce byte-identical artifacts.
const HeadSHAWorkingCopySentinel = "working-copy"

// RepoContext is the immutable, CLI-local descriptor of the
// root path under analysis. Every reused engine package
// (`rule_engine`, `refactor`, `metrics/recipes`) consumes it
// indirectly through the orchestrator: the orchestrator stamps
// `RepoID` / `HeadSHA` onto each `rule_engine.Sample` and
// forwards `ModulePath` to the parser via `AttrModulePath`.
//
// Fields mirror `architecture.md` Sec 4.1.
type RepoContext struct {
	// RootPath is the absolute path of the repo under
	// analysis, normalised to forward slash so the same
	// value travels into [MintRepoID] regardless of host
	// OS.
	RootPath string

	// RepoID is the deterministic UUID-v5 minted via
	// [MintRepoID] from [RootPath]; identical re-runs on
	// the same root produce identical ids per CLEAN-CODE
	// arch G2 (`tech-spec.md` C3).
	RepoID uuid.UUID

	// HeadSHA is the result of `git rev-parse HEAD` when
	// the root is a git working tree, else the literal
	// [HeadSHAWorkingCopySentinel]. Forwarded as
	// `first_seen_sha` into [scope.DeriveScopeID].
	HeadSHA string

	// ModulePath is the per-language module identifier
	// extracted via [DetectModulePath]; forwarded to the
	// parser as `AttrModulePath` so `cycle_member` can
	// resolve intra-repo imports.
	ModulePath string

	// IsGitRepo is true when the root contains a `.git`
	// entry (file or directory). Gates the future
	// `--with-churn` code path; orthogonal to whether
	// [HeadSHA] actually parsed (a freshly `git init`'d
	// tree with no commits sets `IsGitRepo=true` but
	// `HeadSHA=working-copy`).
	IsGitRepo bool
}

// MintRepoID derives a deterministic UUID-v5 identifier for
// the supplied root path so two `cleanc analyze <path>` runs
// against the same checkout produce byte-identical findings.
//
// Algorithm (pinned by `tech-spec.md` constraint C3):
//
//   - Normalise `rootPath` by [filepath.Clean] then
//     [filepath.ToSlash] so a Windows path like
//     `C:\Users\dev\repo` and its forward-slash twin
//     `C:/Users/dev/repo` produce the SAME id.
//   - Prepend [RepoIDNamespaceNamePrefix] to that string.
//   - Hash via `uuid.NewV5(uuid.NamespaceURL, name)` (UUID-v5
//     uses SHA-1; equivalent to `uuid.NewSHA1` in the
//     google/uuid API the story brief references).
//
// Anchors: `architecture.md` Sec 1.4 G2, Sec 4.1; `tech-spec.md`
// Sec 4.11 and constraint C3.
func MintRepoID(rootPath string) uuid.UUID {
	return uuid.NewV5(uuid.NamespaceURL, RepoIDNamespaceNamePrefix+NormalisePath(rootPath))
}

// NormalisePath returns the canonical, slash-normalised form
// of `rootPath` used by [MintRepoID]. Exported so callers
// that want to log the exact pre-image hashed into [RepoID]
// (e.g. the `--diagnostics` JSON sink) can do so without
// reaching into UUID internals.
//
// The normalisation is host-OS-independent: a Windows-shaped
// input such as `C:\Users\dev\repo` MUST produce the same
// canonical bytes whether the caller is running on Windows,
// Linux, or macOS. On Linux/macOS, [filepath.Clean] and
// [filepath.ToSlash] both treat `\` as a literal byte, so we
// cannot rely on them to fold separators; instead we rewrite
// `\` to `/` ourselves and then run the POSIX-only
// [path.Clean], which is slash-based on every host.
//
// Concretely:
//
//   - `\` is replaced with `/` so backslashes become real
//     separators regardless of the host's [filepath.Separator].
//   - [path.Clean] (NOT [filepath.Clean]) then collapses
//     trailing slashes, `.`, `..`, and duplicate slashes
//     uniformly across operating systems.
//
// Anchor: REFACTOR-GUIDE `architecture.md` Sec 4.1 (the
// `repo_id` pre-image MUST be stable across the analyzer's
// host OS so re-runs on the same logical path -- whether
// invoked from a Windows laptop or a Linux CI runner -- mint
// the same UUID); e2e Phase 1 "MintRepoID is deterministic
// across re-runs" pins the Windows / forward-slash
// equivalence on every host.
func NormalisePath(rootPath string) string {
	// 1. Fold backslashes -> forward slashes. On POSIX this
	//    is the only way to turn a Windows-shaped input into
	//    a slash-delimited path, because filepath.Clean on
	//    POSIX keeps `\` as a literal byte.
	slashed := strings.ReplaceAll(rootPath, `\`, "/")
	// 2. Use path.Clean (not filepath.Clean) so the
	//    collapsing rules are identical on every host.
	//    path.Clean is documented as always using "/" as the
	//    path separator, which is exactly what the pre-image
	//    requires.
	return path.Clean(slashed)
}

// DetectHeadSHA shells out to `git rev-parse HEAD` for the
// supplied root and returns `(sha, true)` on success.
//
// Fallbacks (per `architecture.md` Sec 4.1):
//
//   - `(working-copy, false)` when there is no `.git` entry at
//     `rootPath` (the root is not a git working tree).
//   - `(working-copy, true)` when `.git` is present but
//     `git rev-parse HEAD` fails (e.g. a freshly initialised
//     repo with no commits, or `git` not installed). The
//     `IsGitRepo=true` half preserves the `--with-churn`
//     gating signal for the caller even though no SHA is
//     available.
//
// No go-git dependency: the CLI binary must remain a single
// static Go binary with no native git bindings
// (`tech-spec.md` C3 / Sec 4.11).
func DetectHeadSHA(rootPath string) (string, bool) {
	if !hasGitDir(rootPath) {
		return HeadSHAWorkingCopySentinel, false
	}
	sha, ok := runGitRevParse(rootPath)
	if !ok {
		return HeadSHAWorkingCopySentinel, true
	}
	return sha, true
}

// hasGitDir reports whether the supplied root contains a
// `.git` entry. A `.git` file (worktree pointer for git
// submodules / linked worktrees) counts the same as a
// `.git` directory because both indicate the path is part
// of a git repository.
func hasGitDir(rootPath string) bool {
	_, err := os.Stat(filepath.Join(rootPath, ".git"))
	return err == nil
}

// runGitRevParse invokes `git -C <rootPath> rev-parse HEAD`
// and returns the trimmed stdout on success. Any non-zero
// exit (including `git` not on PATH) yields `("", false)`
// so the caller can fall back to the [HeadSHAWorkingCopySentinel].
func runGitRevParse(rootPath string) (string, bool) {
	cmd := exec.Command("git", "-C", rootPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", false
	}
	return sha, true
}

// DetectModulePath returns the module identifier of the
// supplied root for `language` using the canonical per-language
// manifest:
//
//   - `go`: the `module` directive in `<rootPath>/go.mod`.
//   - `typescript`: the `name` field in `<rootPath>/package.json`.
//   - `python`: the `name` field under `[project]` in
//     `<rootPath>/pyproject.toml` (PEP 621).
//   - `java`: the top-level `package` declaration of the
//     first `.java` file discovered by a depth-first walk.
//
// Returns the empty string when the manifest is missing,
// unreadable, or does not contain the expected identifier.
// Failure is non-fatal: an empty `ModulePath` simply means
// the `cycle_member` recipe cannot resolve intra-repo
// imports for this run.
//
// Anchor: `architecture.md` Sec 4.1 ModulePath row;
// `implementation-plan.md` Stage 1.2.
func DetectModulePath(rootPath, language string) string {
	switch language {
	case "go":
		return detectGoModule(rootPath)
	case "typescript":
		return detectTSModule(rootPath)
	case "python":
		return detectPyModule(rootPath)
	case "java":
		return detectJavaModule(rootPath)
	default:
		return ""
	}
}

// detectGoModule reads `<rootPath>/go.mod` and returns the
// value of the first `module <path>` directive. Returns ""
// when go.mod is absent or does not declare a module.
func detectGoModule(rootPath string) string {
	f, err := os.Open(filepath.Join(rootPath, "go.mod"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "module ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "module"))
			// `module "github.com/foo"` form: strip the
			// surrounding quotes that `go mod` tolerates.
			rest = strings.Trim(rest, "\"")
			return rest
		}
	}
	return ""
}

// detectTSModule parses `<rootPath>/package.json` and returns
// the top-level `name` field. Returns "" when package.json
// is absent, unparseable, or has no `name`.
func detectTSModule(rootPath string) string {
	data, err := os.ReadFile(filepath.Join(rootPath, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Name
}

// detectPyModule reads `<rootPath>/pyproject.toml` and returns
// the `name` value under the `[project]` table (PEP 621).
// Uses a line-by-line scan rather than a TOML parser because
// the CLI module graph deliberately avoids new external deps
// (`tech-spec.md` Sec 8.10 dependency-budget linter).
//
// The scanner only honours assignments whose `[project]`
// section is currently in scope; assignments under unrelated
// tables (`[tool.poetry]`, `[build-system]`) are skipped so a
// repo that ships both PEP 621 and poetry metadata still
// returns the canonical PEP 621 name.
func detectPyModule(rootPath string) string {
	f, err := os.Open(filepath.Join(rootPath, "pyproject.toml"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	inProject := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProject = line == "[project]"
			continue
		}
		if !inProject {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key != "name" {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		// Strip an inline comment after the value (`name =
		// "foo" # primary`) then unquote.
		if hash := strings.IndexByte(val, '#'); hash >= 0 {
			val = strings.TrimSpace(val[:hash])
		}
		val = strings.Trim(val, "\"'")
		return val
	}
	return ""
}

// detectJavaModule walks the root for the first `.java`
// source file and returns the value of its top-level
// `package x.y.z;` declaration. Skipped directories follow
// the walker's default ignore set (`.git`, `target`, `build`,
// `out`, `node_modules`) so a Maven / Gradle project's
// generated output never wins over a real source file.
//
// Returns "" when no `.java` file declares a package.
func detectJavaModule(rootPath string) string {
	var pkg string
	skipDirs := map[string]struct{}{
		".git":         {},
		"target":       {},
		"build":        {},
		"out":          {},
		"node_modules": {},
		"vendor":       {},
	}
	_ = filepath.WalkDir(rootPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A read error on one subtree is non-fatal;
			// skip it and keep walking the rest.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, skip := skipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".java") {
			return nil
		}
		if got := readJavaPackage(path); got != "" {
			pkg = got
			return filepath.SkipAll
		}
		return nil
	})
	return pkg
}

// readJavaPackage scans the supplied `.java` file for the
// first `package x.y.z;` declaration and returns the dotted
// package name. Returns "" if no such declaration is found
// before EOF.
func readJavaPackage(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if !strings.HasPrefix(line, "package ") {
			// Java permits comments / blank lines before
			// the package statement but not arbitrary
			// declarations; stop scanning once we hit a
			// non-package, non-comment, non-blank line.
			if strings.HasPrefix(line, "/*") || strings.HasPrefix(line, "*") {
				continue
			}
			return ""
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "package"))
		rest = strings.TrimSuffix(rest, ";")
		return strings.TrimSpace(rest)
	}
	return ""
}
