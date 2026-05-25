package scan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
)

// goModFile is the Go-modules manifest filename, pinned as a
// const so a `grep -nF "go.mod"` lands the production scan
// path's contract anchor.
const goModFile = "go.mod"

// ErrNoModuleDirective is returned when `go.mod` exists but
// does not contain a `module <path>` directive. This is
// distinct from "no go.mod" (which is not an error) and from
// a filesystem I/O failure.
var ErrNoModuleDirective = errors.New("scan: no module directive in go.mod")

// DetectGoModulePath reads `<dir>/go.mod` and returns the
// module directive's path (e.g. `github.com/org/repo`).
//
// Return contract:
//
//   - `("", false, nil)` -- `<dir>/go.mod` does not exist. The
//     directory may be a non-Go project (Python, TypeScript,
//     Java) or a pre-modules Go layout. This is a normal
//     condition, NOT an error -- callers should treat the
//     absence of a module path as "module-path canonicalisation
//     not available; the recipe falls back to exact-dir /
//     exact-qn matching".
//
//   - `(<path>, true, nil)` -- the module directive was found
//     and parsed. `<path>` is the trimmed and unquoted path
//     string (e.g. `github.com/org/repo`).
//
//   - `("", false, err)` -- a real failure: filesystem I/O
//     error other than "not exist", or a `go.mod` file present
//     but missing the `module` directive (wraps
//     [ErrNoModuleDirective]).
//
// Parsing handles the common go.mod surface shape: a leading
// UTF-8 BOM, line and block comments, leading whitespace
// before `module`, and a quoted (`"..."` or `` `...` ``) or
// bare module path. Build constraints in the directive are
// not part of the go.mod grammar and are NOT supported.
//
// Pure: no side effects other than the single `os.ReadFile` of
// the named go.mod.
func DetectGoModulePath(dir string) (string, bool, error) {
	p := filepath.Join(dir, goModFile)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("scan: read %s: %w", p, err)
	}
	mp, ok := parseModuleDirective(data)
	if !ok {
		return "", false, fmt.Errorf("%w: %s", ErrNoModuleDirective, p)
	}
	return mp, true, nil
}

// FindNearestGoModulePath walks upward from `startDir`
// looking for the first `go.mod` file and returns its
// module path and the directory that contains it.
//
// Returns `("", "", false, nil)` when no `go.mod` is found
// before reaching the filesystem root.
//
// Used to canonicalise per-file module attribution in multi-
// module workspaces (nested `go.mod` files): a file at
// `<repo>/subA/foo.go` finds `<repo>/subA/go.mod` first,
// while a file at `<repo>/foo.go` finds `<repo>/go.mod`.
//
// `startDir` is resolved to an absolute path before walking
// so the result is independent of the caller's `cwd`.
func FindNearestGoModulePath(startDir string) (modulePath, modRoot string, ok bool, err error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", "", false, fmt.Errorf("scan: abs %q: %w", startDir, err)
	}
	for {
		mp, found, detectErr := DetectGoModulePath(dir)
		if detectErr != nil {
			return "", "", false, detectErr
		}
		if found {
			return mp, dir, true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false, nil
		}
		dir = parent
	}
}

// StampGoModulePath sets `Attrs[parser.AttrModulePath] =
// modulePath` on every non-nil AstFile in `asts`. Idempotent:
// re-stamping with the same value is a no-op; re-stamping with
// a different value OVERWRITES (callers should not mix module
// roots without picking the right helper).
//
// Stamping an empty `modulePath` is a no-op so a failed
// detection ([DetectGoModulePath] returning `("", false, nil)`)
// can be threaded through `StampGoModulePath(asts, mp)`
// without a separate guard.
//
// Returns the count of ASTs actually touched. Callers that
// expected every AST to be stamped can compare against
// `len(asts)`.
func StampGoModulePath(asts []*parser.AstFile, modulePath string) int {
	if modulePath == "" {
		return 0
	}
	n := 0
	for _, ast := range asts {
		if ast == nil {
			continue
		}
		if ast.Attrs == nil {
			ast.Attrs = map[string]string{}
		}
		ast.Attrs[parser.AttrModulePath] = modulePath
		n++
	}
	return n
}

// AnnotateProjectAsts is the convenience composition for a
// SINGLE-MODULE scan root: detect the module path from
// `<rootDir>/go.mod` and stamp every AstFile in `asts`.
//
// Returns the detected module path (or "" when no go.mod was
// found). The empty-result case is non-fatal: AstFiles are
// returned unstamped and downstream cycle detection falls
// back to exact-dir / exact-qn matching (which still produces
// correct results for in-project bare-name imports such as
// Python `import foo` or relative imports).
//
// Multi-module workspaces (a scan root containing nested
// go.mod files) should use [AnnotateAstsByNearestGoMod]
// instead so each AstFile is attributed to its OWN module's
// path -- the longest-prefix module-path matcher in
// `recipes.cycle_member` requires this.
func AnnotateProjectAsts(rootDir string, asts []*parser.AstFile) (string, error) {
	mp, ok, err := DetectGoModulePath(rootDir)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	StampGoModulePath(asts, mp)
	return mp, nil
}

// AnnotateAstsByNearestGoMod walks each AstFile's `Path`,
// resolves it relative to `rootDir`, finds the nearest
// enclosing `go.mod`, and stamps `Attrs[parser.AttrModulePath]`
// with that module's path. Files outside any go.mod are left
// unstamped.
//
// Used by scan-layer composition when `rootDir` may contain
// multiple nested `go.mod` files (multi-module workspace).
// Directory-level caching means each unique directory is
// walked at most once per call.
//
// The AstFile's `Path` must be the project-relative path
// stamped by the parser; this helper combines it with
// `rootDir` to obtain a filesystem-resolvable directory for
// the `go.mod` walk. AstFiles whose `Path` is empty are
// skipped.
//
// Returns the first I/O error encountered (the helper
// short-circuits on error so partial-state ASTs are not left
// in an undefined annotation state).
func AnnotateAstsByNearestGoMod(rootDir string, asts []*parser.AstFile) error {
	cache := map[string]string{}
	for _, ast := range asts {
		if ast == nil {
			continue
		}
		relPath := ast.GetPath()
		if relPath == "" {
			continue
		}
		astDir := filepath.Dir(filepath.Join(rootDir, filepath.FromSlash(relPath)))
		mp, hit := cache[astDir]
		if !hit {
			found, _, _, err := FindNearestGoModulePath(astDir)
			if err != nil {
				return err
			}
			mp = found
			cache[astDir] = mp
		}
		if mp == "" {
			continue
		}
		if ast.Attrs == nil {
			ast.Attrs = map[string]string{}
		}
		ast.Attrs[parser.AttrModulePath] = mp
	}
	return nil
}

// parseModuleDirective scans `go.mod` content for the
// `module <path>` directive and returns the (unquoted) path.
//
// Supports:
//   - UTF-8 BOM at the start of the file
//   - leading whitespace before the directive
//   - `//` line comments anywhere on the directive line
//   - `/* ... */` block comments spanning multiple lines
//   - quoted module path (`"..."` or `` `...` ``)
//   - tab- or space-separated directive token
//
// Returns `("", false)` when no directive is found OR the
// `module` token is present but the path is empty.
//
// The go.mod grammar does not include build constraints, so
// this parser does NOT attempt to handle `//go:build` lines
// inside go.mod (they are invalid go.mod and the Go toolchain
// would reject the file). Multi-line `module` declarations
// are likewise not part of the grammar.
func parseModuleDirective(data []byte) (string, bool) {
	s := strings.TrimPrefix(string(data), "\ufeff")
	inBlock := false
	for _, raw := range strings.Split(s, "\n") {
		line := raw
		// Close any open block comment first.
		if inBlock {
			i := strings.Index(line, "*/")
			if i < 0 {
				continue
			}
			line = line[i+2:]
			inBlock = false
		}
		// Strip block comments that open AND close on this
		// line; track an opener with no closer as
		// `inBlock=true` carry-over.
		for {
			i := strings.Index(line, "/*")
			if i < 0 {
				break
			}
			j := strings.Index(line[i+2:], "*/")
			if j < 0 {
				line = line[:i]
				inBlock = true
				break
			}
			line = line[:i] + line[i+2+j+2:]
		}
		// Strip the line comment (`// ...`).
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// `module` must be the first token on the directive
		// line. Followed by at least one whitespace then the
		// path. We accept either a space or a tab as the
		// separator (the go.mod grammar lexer treats both as
		// whitespace).
		const tok = "module"
		if !strings.HasPrefix(line, tok) {
			continue
		}
		rest := line[len(tok):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			// `modulefoo` or `module(` -- not the directive.
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return "", false
		}
		// Unquote if the path is wrapped in `"..."` or
		// `` `...` ``.
		if len(rest) >= 2 {
			if rest[0] == '"' && rest[len(rest)-1] == '"' {
				return rest[1 : len(rest)-1], true
			}
			if rest[0] == '`' && rest[len(rest)-1] == '`' {
				return rest[1 : len(rest)-1], true
			}
		}
		return rest, true
	}
	return "", false
}
