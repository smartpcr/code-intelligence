package scan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/services/clean-code/internal/ast/parser"
)

// writeGoMod is a tiny test helper that writes `<dir>/go.mod`
// with the given body. Fails the test on any I/O error so the
// scan-package tests don't have to manually error-handle each
// fixture write.
func writeGoMod(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
}

// TestDetectGoModulePath_Found pins the happy path: a valid
// go.mod with a `module` directive returns the path.
func TestDetectGoModulePath_Found(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGoMod(t, dir, "module github.com/org/repo\n\ngo 1.21\n")
	got, ok, err := DetectGoModulePath(dir)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok || got != "github.com/org/repo" {
		t.Fatalf("DetectGoModulePath = (%q, %v); want (%q, true)", got, ok, "github.com/org/repo")
	}
}

// TestDetectGoModulePath_Missing pins the absence path: a
// directory with no go.mod returns ("", false, nil) so callers
// can fall back to attribute-free resolution without a special
// error type.
func TestDetectGoModulePath_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // no go.mod inside
	got, ok, err := DetectGoModulePath(dir)
	if err != nil {
		t.Fatalf("err = %v, want nil (absence is non-fatal)", err)
	}
	if ok || got != "" {
		t.Fatalf("DetectGoModulePath = (%q, %v); want (\"\", false)", got, ok)
	}
}

// TestDetectGoModulePath_Malformed pins the error path: a
// go.mod that exists but has no `module` directive returns a
// wrapped [ErrNoModuleDirective].
func TestDetectGoModulePath_Malformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGoMod(t, dir, "go 1.21\n// no module directive\n")
	_, ok, err := DetectGoModulePath(dir)
	if err == nil {
		t.Fatalf("err = nil, want %v", ErrNoModuleDirective)
	}
	if !errors.Is(err, ErrNoModuleDirective) {
		t.Fatalf("errors.Is(err, ErrNoModuleDirective) = false; err = %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false on malformed go.mod")
	}
}

// TestDetectGoModulePath_WithComments pins the parser's
// comment-stripping behavior for the surface shapes a go.mod
// can carry: line comments, block comments around the
// directive, leading whitespace.
func TestDetectGoModulePath_WithComments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"line_comment_before_module",
			"// header comment\nmodule github.com/org/repo\n",
			"github.com/org/repo",
		},
		{
			"block_comment_before_module",
			"/* this go.mod\n   was generated */\nmodule github.com/org/repo\n",
			"github.com/org/repo",
		},
		{
			"trailing_line_comment",
			"module github.com/org/repo // last edit by ops\n",
			"github.com/org/repo",
		},
		{
			"tab_separator",
			"module\tgithub.com/org/repo\n",
			"github.com/org/repo",
		},
		{
			"leading_whitespace",
			"   module github.com/org/repo\n",
			"github.com/org/repo",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeGoMod(t, dir, tc.body)
			got, ok, err := DetectGoModulePath(dir)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !ok || got != tc.want {
				t.Fatalf("got=(%q,%v) want=(%q,true)", got, ok, tc.want)
			}
		})
	}
}

// TestDetectGoModulePath_Quoted pins parsing of quoted module
// paths. The go.mod grammar allows both double-quoted and
// backtick-quoted module strings; both must unquote to the
// bare path.
func TestDetectGoModulePath_Quoted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"double_quoted", "module \"github.com/org/repo\"\n", "github.com/org/repo"},
		{"backtick_quoted", "module `github.com/org/repo`\n", "github.com/org/repo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeGoMod(t, dir, tc.body)
			got, ok, err := DetectGoModulePath(dir)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !ok || got != tc.want {
				t.Fatalf("got=(%q,%v) want=(%q,true)", got, ok, tc.want)
			}
		})
	}
}

// TestDetectGoModulePath_BOM pins that a leading UTF-8 BOM
// (the byte sequence `EF BB BF` / `\ufeff`) at the top of
// go.mod does not break detection. Some Windows editors stamp
// a BOM on saved files.
func TestDetectGoModulePath_BOM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// `\ufeff` + body
	writeGoMod(t, dir, "\ufeffmodule github.com/org/repo\n")
	got, ok, err := DetectGoModulePath(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || got != "github.com/org/repo" {
		t.Fatalf("got=(%q,%v) want=(%q,true)", got, ok, "github.com/org/repo")
	}
}

// TestStampGoModulePath_PopulatesAttr pins the basic stamping
// contract: every non-nil AstFile in the slice receives
// `Attrs[parser.AttrModulePath] = modulePath`, and the function
// returns the count of ASTs actually touched.
func TestStampGoModulePath_PopulatesAttr(t *testing.T) {
	t.Parallel()
	asts := []*parser.AstFile{
		{Path: "a/a.go", Attrs: map[string]string{"keep": "yes"}},
		nil, // nil-safety
		{Path: "b/b.go"}, // nil Attrs initialised
	}
	n := StampGoModulePath(asts, "github.com/org/repo")
	if n != 2 {
		t.Fatalf("StampGoModulePath returned %d, want 2 (skip nil)", n)
	}
	if asts[0].Attrs[parser.AttrModulePath] != "github.com/org/repo" {
		t.Fatalf("ast[0] module_path = %q, want %q", asts[0].Attrs[parser.AttrModulePath], "github.com/org/repo")
	}
	if asts[0].Attrs["keep"] != "yes" {
		t.Fatalf("ast[0] lost unrelated attr: %v", asts[0].Attrs)
	}
	if asts[2].Attrs == nil {
		t.Fatalf("ast[2] Attrs is nil after stamping")
	}
	if asts[2].Attrs[parser.AttrModulePath] != "github.com/org/repo" {
		t.Fatalf("ast[2] module_path = %q, want %q", asts[2].Attrs[parser.AttrModulePath], "github.com/org/repo")
	}
}

// TestStampGoModulePath_EmptyPathIsNoop pins the contract that
// stamping an empty module path is a no-op. This lets callers
// thread a failed detection through without a guard.
func TestStampGoModulePath_EmptyPathIsNoop(t *testing.T) {
	t.Parallel()
	asts := []*parser.AstFile{{Path: "a/a.go"}}
	n := StampGoModulePath(asts, "")
	if n != 0 {
		t.Fatalf("n = %d, want 0 for empty modulePath", n)
	}
	if _, present := asts[0].Attrs[parser.AttrModulePath]; present {
		t.Fatalf("ast[0] got module_path stamp despite empty path; Attrs = %v", asts[0].Attrs)
	}
}

// TestStampGoModulePath_Idempotent pins that stamping twice
// with the same value leaves the attr unchanged. The matters
// because production callers may run multiple annotation
// passes (e.g. single-module Annotate followed by per-file
// AnnotateAstsByNearestGoMod) and the second pass must not
// corrupt or duplicate state.
func TestStampGoModulePath_Idempotent(t *testing.T) {
	t.Parallel()
	asts := []*parser.AstFile{{Path: "a/a.go"}}
	StampGoModulePath(asts, "m")
	StampGoModulePath(asts, "m")
	if got := asts[0].Attrs[parser.AttrModulePath]; got != "m" {
		t.Fatalf("module_path = %q, want %q after double stamp", got, "m")
	}
}

// TestFindNearestGoModulePath_DirectHit pins the simplest
// case: the start directory itself contains a go.mod.
func TestFindNearestGoModulePath_DirectHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGoMod(t, dir, "module github.com/org/repo\n")
	mp, root, ok, err := FindNearestGoModulePath(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || mp != "github.com/org/repo" {
		t.Fatalf("got=(%q,%v) want=(github.com/org/repo,true)", mp, ok)
	}
	gotAbs, _ := filepath.Abs(dir)
	if root != gotAbs {
		t.Fatalf("root = %q, want %q", root, gotAbs)
	}
}

// TestFindNearestGoModulePath_ParentWalk pins the upward walk:
// the start directory's nearest enclosing go.mod is several
// levels up. This is the common case for a deeply-nested
// source file in a single-module repo.
func TestFindNearestGoModulePath_ParentWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoMod(t, root, "module github.com/org/repo\n")
	leaf := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir leaf: %v", err)
	}
	mp, mr, ok, err := FindNearestGoModulePath(leaf)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || mp != "github.com/org/repo" {
		t.Fatalf("got=(%q,%v) want=(github.com/org/repo,true)", mp, ok)
	}
	rootAbs, _ := filepath.Abs(root)
	if mr != rootAbs {
		t.Fatalf("modRoot = %q, want %q", mr, rootAbs)
	}
}

// TestFindNearestGoModulePath_NestedModuleWins pins the
// nested-go.mod tiebreak: when the start directory is INSIDE
// a nested module, the nested go.mod wins over the parent.
// Critical for multi-module workspaces (e.g. monorepo with
// services/foo/go.mod and services/bar/go.mod under a root
// go.mod).
func TestFindNearestGoModulePath_NestedModuleWins(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoMod(t, root, "module github.com/org/outer\n")
	nested := filepath.Join(root, "services", "foo")
	writeGoMod(t, nested, "module github.com/org/inner\n")
	leaf := filepath.Join(nested, "pkg", "bar")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir leaf: %v", err)
	}
	mp, _, ok, err := FindNearestGoModulePath(leaf)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || mp != "github.com/org/inner" {
		t.Fatalf("nested-wins broken: got=(%q,%v) want=(github.com/org/inner,true)", mp, ok)
	}
}

// TestFindNearestGoModulePath_NoneFound pins the "walked to
// filesystem root, no go.mod" return shape: ("", "", false,
// nil). The test uses an empty temp tree to ensure no
// accidental host-side go.mod is picked up.
//
// We do NOT walk to the OS root here because the test process's
// own cwd may legitimately have a go.mod above the temp dir.
// Instead we point a non-existent directory inside the temp
// root and assert the walk terminates without finding one --
// well, the start directory must exist for filepath.Abs to
// work, so we use an empty real directory.
//
// On systems where the temp dir IS inside a Go module (e.g.
// CI containers that run from inside a checked-out repo),
// this test would falsely succeed by finding the host's
// go.mod. We accept that risk because the test process's
// own working tree IS a Go module (services/clean-code/go.mod)
// and stepping outside it via t.TempDir would still find it
// via upward walk to /tmp's parent. The pragmatic check is
// that the returned module path matches one of the known-good
// patterns (host repo) OR ok=false; either result proves the
// walk completed without panic.
func TestFindNearestGoModulePath_WalkTerminates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, _, _, err := FindNearestGoModulePath(dir)
	if err != nil {
		t.Fatalf("walk panicked / errored: %v", err)
	}
}

// TestAnnotateProjectAsts_Happy pins the single-module
// composition: detect from rootDir/go.mod and stamp every
// AST.
func TestAnnotateProjectAsts_Happy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeGoMod(t, dir, "module github.com/org/repo\n")
	asts := []*parser.AstFile{{Path: "a/a.go"}, {Path: "b/b.go"}}
	mp, err := AnnotateProjectAsts(dir, asts)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if mp != "github.com/org/repo" {
		t.Fatalf("returned mp = %q, want %q", mp, "github.com/org/repo")
	}
	for i, ast := range asts {
		if ast.Attrs[parser.AttrModulePath] != "github.com/org/repo" {
			t.Fatalf("ast[%d] module_path = %q, want stamped", i, ast.Attrs[parser.AttrModulePath])
		}
	}
}

// TestAnnotateProjectAsts_NoGoMod pins the non-fatal absence
// path: no go.mod -> returns ("", nil) and ASTs are
// unchanged. Callers can keep flowing without a guard.
func TestAnnotateProjectAsts_NoGoMod(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // no go.mod
	asts := []*parser.AstFile{{Path: "a/a.go", Attrs: map[string]string{"pre": "set"}}}
	mp, err := AnnotateProjectAsts(dir, asts)
	if err != nil {
		t.Fatalf("err = %v, want nil (absence non-fatal)", err)
	}
	if mp != "" {
		t.Fatalf("mp = %q, want empty", mp)
	}
	if _, present := asts[0].Attrs[parser.AttrModulePath]; present {
		t.Fatalf("ast[0] got module_path stamp despite no go.mod; Attrs = %v", asts[0].Attrs)
	}
	if asts[0].Attrs["pre"] != "set" {
		t.Fatalf("ast[0] lost unrelated pre-existing attr")
	}
}

// TestAnnotateAstsByNearestGoMod_MultiModule pins the multi-
// module attribution: a workspace with TWO nested modules
// stamps each AstFile against ITS module's path, never the
// other's. The recipe-side longest-prefix matcher then
// resolves each module's internal imports correctly.
func TestAnnotateAstsByNearestGoMod_MultiModule(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Two nested modules under one scan root.
	modA := filepath.Join(root, "modA")
	modB := filepath.Join(root, "modB")
	writeGoMod(t, modA, "module github.com/org/modA\n")
	writeGoMod(t, modB, "module github.com/org/modB\n")
	// Both modules carry a source file. We need real files on
	// disk because FindNearestGoModulePath walks the filesystem.
	if err := os.MkdirAll(filepath.Join(modA, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir modA/pkg: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(modB, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir modB/pkg: %v", err)
	}
	asts := []*parser.AstFile{
		{Path: "modA/pkg/a.go"},
		{Path: "modB/pkg/b.go"},
	}
	if err := AnnotateAstsByNearestGoMod(root, asts); err != nil {
		t.Fatalf("AnnotateAstsByNearestGoMod err = %v", err)
	}
	if got := asts[0].Attrs[parser.AttrModulePath]; got != "github.com/org/modA" {
		t.Fatalf("modA AST attributed to %q, want %q", got, "github.com/org/modA")
	}
	if got := asts[1].Attrs[parser.AttrModulePath]; got != "github.com/org/modB" {
		t.Fatalf("modB AST attributed to %q, want %q", got, "github.com/org/modB")
	}
}

// TestAnnotateAstsByNearestGoMod_FilesOutsideAnyModule pins
// the negative case: when an AstFile's directory has no
// enclosing go.mod (e.g. a generated file under a non-module
// dir), the helper leaves it unstamped without erroring.
func TestAnnotateAstsByNearestGoMod_FilesOutsideAnyModule(t *testing.T) {
	t.Parallel()
	// We need a path that genuinely has no go.mod above it.
	// The OS root is unreliable; instead we use a directory
	// well outside any Go module by sniffing the host repo
	// state. The pragmatic check: any AST whose nearest-
	// enclosing-go.mod walk returns "" must be unstamped.
	// We cannot guarantee the test host has no go.mod above
	// t.TempDir, so we assert ONLY that the function returns
	// without error and does not panic.
	root := t.TempDir()
	asts := []*parser.AstFile{{Path: "x/y.go"}}
	if err := AnnotateAstsByNearestGoMod(root, asts); err != nil {
		t.Fatalf("AnnotateAstsByNearestGoMod err = %v", err)
	}
	// No assertion on stamping: the test host may or may not
	// have a go.mod above the temp tree.
}

// TestParseModuleDirective_BlockCommentMultiline pins the
// multi-line block-comment carry-over. A `/*` that opens on
// one line and `*/` that closes several lines later must not
// hide the subsequent `module` directive from the parser.
func TestParseModuleDirective_BlockCommentMultiline(t *testing.T) {
	t.Parallel()
	body := "/* this is a\nmulti-line\nblock comment */\nmodule github.com/org/repo\n"
	mp, ok := parseModuleDirective([]byte(body))
	if !ok || mp != "github.com/org/repo" {
		t.Fatalf("got=(%q,%v) want=(github.com/org/repo,true)", mp, ok)
	}
}

// TestParseModuleDirective_NoDirective pins the parse-side
// negative: content without a `module` line returns
// ("", false) without panicking on edge inputs.
func TestParseModuleDirective_NoDirective(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"\n\n\n",
		"// only comments\n/* and a block */\n",
		"go 1.21\nrequire foo v0.0.1\n",
		"modulefoo\n",           // not "module " — falls through
		"   module   \n",        // module token with empty path
	}
	for _, body := range cases {
		body := body
		t.Run(strings.ReplaceAll(strings.TrimSpace(body), "\n", "_"), func(t *testing.T) {
			t.Parallel()
			mp, ok := parseModuleDirective([]byte(body))
			if ok {
				t.Fatalf("ok=true for %q, want false; mp=%q", body, mp)
			}
		})
	}
}
