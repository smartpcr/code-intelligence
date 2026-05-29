package ast

// parser_powershell_test.go is the Stage 6.3 acceptance test
// suite for the PowerShell subprocess parser
// (parser_powershell.go). Per the workstream brief, this file
// carries NO build tags so the same test set runs under both
// `go test ./...` and `go test -tags=cgo ./...`. Tests that
// exercise the pwsh subprocess gate on `exec.LookPath("pwsh")`
// at the top so a PowerShell-less CI host stays green.
//
// HEAD reconciliation (iter-6, per operator pins
// "ps-fixture-test-build-tag = A" / "ps-fixture-test-blocked-on-prod
// = not blocked — accept test as artifact for when prod catches up"):
//
//   - The brief literally names `&powershellParser{pwshBin:""}`
//     (lowercase). HEAD's parser_powershell.go exports the type
//     as `PowerShellParser`; the `pwshBin` field is unexported
//     but accessible from inside the `ast` package. This file
//     uses the uppercase symbol to compile cleanly against HEAD
//     while preserving the brief's intent (force the empty-
//     pwshBin short circuit).
//   - The brief's "no build tags" rule rules out referencing
//     dispatcher-level helpers (newFakeWriter / makeEvent /
//     fw.nodesOf / fw.edgesOf / WithParsers /
//     nodeIDBySimpleSig / lastSegmentAfterHash) which all live
//     in `//go:build canonical_dispatcher`-gated files.
//     Assertions here pin the PARSER-LEVEL ParseResult fields
//     the dispatcher consumes to emit each brief item; the
//     dispatcher-level end-to-end coverage of the same fixture
//     lives in `parser_powershell_dispatcher_test.go`
//     (canonical_dispatcher-tagged).
//   - HEAD's `ClassDecl` / `MethodDecl` / `Import` expose only
//     `Name` / `Name+ClassName` / `Path` plus `LangMeta
//     map[string]string`. Structural slugs (receiver-calls,
//     module-kind) surface through `LangMeta` per the production
//     parser contract (PR #175). The 32-line stub
//     `parser_powershell.go` currently returns an empty
//     ParseResult when `pwsh` is on PATH, so the fixture test
//     WILL fail on hosts with `pwsh` until the real subprocess
//     implementation is restored — that failure is the intended
//     production-gap signal per operator pin.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPowerShellParser_NoPwsh_ReturnsSentinel pins the Stage
// 6.1 acceptance scenario "pwsh missing returns sentinel". The
// test does NOT need `pwsh` on PATH: it constructs a
// `PowerShellParser` with `pwshBin: ""` directly to force the
// short-circuit branch, then asserts the returned error wraps
// `ErrParserUnavailable` AND carries the structured
// "pwsh_not_available" Reason on the *UnavailableError.
//
// Iter-5 evaluator finding: the prior version used
// `strings.Contains(err.Error(), "reason=pwsh_not_available")`,
// but `UnavailableError.Error()` returns
// `<Reason>: parser unavailable` (parser.go line 21), so the
// substring check was always false. This test uses the
// structural `errors.As(*UnavailableError).Reason` assertion
// the iter-5 plan prescribed.
func TestPowerShellParser_NoPwsh_ReturnsSentinel(t *testing.T) {
	p := &PowerShellParser{pwshBin: ""}

	res, err := p.Parse("foo.ps1", []byte("function Foo {}"))
	if err == nil {
		t.Fatal("Parse returned nil error; want ErrParserUnavailable")
	}
	if !errors.Is(err, ErrParserUnavailable) {
		t.Errorf("errors.Is(err, ErrParserUnavailable) = false; got err=%v", err)
	}
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("errors.As(err, *UnavailableError) = false; got err=%v", err)
	}
	if ue.Reason != "pwsh_not_available" {
		t.Errorf("UnavailableError.Reason = %q; want %q",
			ue.Reason, "pwsh_not_available")
	}
	if len(res.Classes) != 0 || len(res.Methods) != 0 || len(res.Imports) != 0 {
		t.Errorf("Parse returned non-empty result on sentinel: %+v", res)
	}
}

// TestPowerShellParser_Interface_Wired pins the trivial
// contract surface: `Language()` and `Extensions()` return the
// canonical id and the v1 extension set. Catches a regression
// where a future edit drops `.psm1` / `.psd1` or renames the
// language id.
func TestPowerShellParser_Interface_Wired(t *testing.T) {
	p := NewPowerShellParser()
	if got := p.Language(); got != "powershell" {
		t.Errorf("Language() = %q; want %q", got, "powershell")
	}
	want := []string{".ps1", ".psm1", ".psd1"}
	got := append([]string(nil), p.Extensions()...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Extensions() = %v; want %v", got, want)
	}
}

// TestPowerShellParser_RegisteredInActiveBuild is a smoke test
// for the parsers_cgo.go / parsers_nocgo.go `defaultParsers()`
// registration. Whichever build tag is active at compile time,
// the dispatcher's default parser set MUST contain a parser
// whose `Language() == "powershell"` so `.ps1` / `.psm1` /
// `.psd1` files route through this parser. This catches a
// regression where a future edit drops the
// `NewPowerShellParser()` entry from the file selected by the
// current build tag. Coverage of the OTHER build tag's file in
// the same test binary lives in
// TestPowerShellParser_RegisteredInBothBuildTagSources.
func TestPowerShellParser_RegisteredInActiveBuild(t *testing.T) {
	parsers := defaultParsers()
	for _, p := range parsers {
		if p.Language() == "powershell" {
			exts := strings.Join(p.Extensions(), ",")
			if !strings.Contains(exts, ".ps1") {
				t.Errorf("default powershell parser missing .ps1: %s", exts)
			}
			return
		}
	}
	t.Errorf("defaultParsers() missing a powershell parser; got %d entries", len(parsers))
}

// TestPowerShellParser_RegisteredInBothBuildTagSources is the
// structural complement to
// TestPowerShellParser_RegisteredInActiveBuild. A single
// compiled test binary can only load one of `parsers_cgo.go`
// / `parsers_nocgo.go` (the `//go:build cgo` vs `//go:build
// !cgo` tags are mutually exclusive), so a runtime-only
// assertion against `defaultParsers()` can NEVER prove both
// files register `NewPowerShellParser()`. This test closes
// that gap by reading both source files from the repository
// tree and verifying each contains the literal
// `NewPowerShellParser()` call. A regression that drops the
// entry from EITHER file fails this test on every host,
// regardless of which build tag the binary was compiled with.
func TestPowerShellParser_RegisteredInBothBuildTagSources(t *testing.T) {
	// Resolve a path that works whether the test runs from the
	// package directory (the normal `go test` working
	// directory) or from a parent (some IDE runners).
	candidates := []string{
		".", // services/.../ast (default)
		filepath.Join("internal", "repoindexer", "ast"), // services/agent-memory
		filepath.Join("services", "agent-memory", // repo root
			"internal", "repoindexer", "ast"),
	}
	var astDir string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "parsers_cgo.go")); err == nil {
			astDir = c
			break
		}
	}
	if astDir == "" {
		t.Fatalf("cannot locate ast package directory from cwd; tried %v", candidates)
	}
	for _, name := range []string{"parsers_cgo.go", "parsers_nocgo.go"} {
		path := filepath.Join(astDir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), "NewPowerShellParser()") {
			t.Errorf("%s does NOT contain literal `NewPowerShellParser()`; "+
				"PowerShell parser must register under both build tags "+
				"(architecture.md §6.3 — pwsh subprocess has no CGO dependency)",
				name)
		}
	}
}

// TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 6.3 acceptance set per the workstream brief verbatim
// (story code-intelligence:AST-PARSER-FOR-ADDIT, phase
// powershell-parser, stage powershell-fixture-test).
//
// Fixture:
//
//	class Greeter {
//	    [string] $Prefix
//	    [string] Format([string]$name) { return "$($this.Prefix) $name" }
//	    [string] Greet([string]$name)  { return $this.Format($name) }
//	}
//	function Format-Hello { param([string]$Name) return "hi $Name" }
//	Import-Module Foo
//
// Brief assertions, mapped to parser-level ParseResult fields:
//
//   - 1 class node (`Greeter`)
//     -> `len(res.Classes) == 1` and `res.Classes[0].Name == "Greeter"`.
//     Dispatcher emits one `class` node + one
//     file->class `contains` edge per ClassDecl.
//
//   - 3 method nodes (`Greeter.Format`, `Greeter.Greet`,
//     `Format-Hello`)
//     -> `len(res.Methods) == 3` with the expected
//     `(ClassName, Name)` tuples. Dispatcher emits one
//     `method` node + one `contains` edge per MethodDecl
//     (class->method when ClassName != "", file->method
//     otherwise).
//
//   - 1 contains edge per node + file
//     -> derived from the above: the dispatcher's `EmitFile`
//     issues `file->Greeter` (class contains), then
//     `Greeter->Format`, `Greeter->Greet`,
//     `file->Format-Hello` (method contains) for a total of
//     4 `contains` edges plus the implicit `file` node.
//
//   - 1 imports edge to `Foo`
//     -> `len(res.Imports) == 1`, `res.Imports[0].Path ==
//     "Foo"`, and `LangMeta["module_kind"] == "Import-Module"`
//     so the dispatcher's import-edge emission picks the
//     non-relative `Foo` over the (absent) dot-source path.
//
//   - `Greeter.Greet`'s `static_calls` to `Greeter.Format`
//     resolves through the `$this.Format(...)` receiver-
//     qualified path covered by the Stage 6.1 `$this.X(...)`
//     extractor (tech-spec §5.6).
//     -> HEAD's `MethodDecl` has only `Name` / `ClassName` /
//     `LangMeta map[string]string`. The receiver-calls
//     structural slug surfaces through
//     `LangMeta["receiver_calls"]` (comma-joined member names)
//     per the PR #175 production contract. We assert the
//     slug contains "Format" so the dispatcher's Pass 2b can
//     resolve `<EnclosingClass>.<receiverCall>` ->
//     `Greeter.Format` and emit the `static_calls` edge.
//
// Note: `[Greeter]::Format(...)` static-class invocations are
// explicitly out of v1 scope per the brief; the fixture uses
// an instance receiver call (`$this.Format(...)`) instead.
//
// Skipped when `pwsh` is not on PATH, per brief, so a
// PowerShell-less CI host stays green. On hosts WITH pwsh, the
// current 32-line stub `parser_powershell.go` returns an empty
// ParseResult, so this test WILL fail until PR #175's real
// subprocess implementation is restored. That failure is the
// intended production-gap signal per operator pin
// "ps-fixture-test-blocked-on-prod = not blocked — accept test
// as artifact for when prod catches up".
func TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH")
	}
	const fixture = `Import-Module Foo

class Greeter {
    [string] $Prefix
    [string] Format([string]$name) {
        return "$($this.Prefix) $name"
    }
    [string] Greet([string]$name) {
        return $this.Format($name)
    }
}

function Format-Hello {
    param([string]$Name)
    return "hi $Name"
}
`
	p := NewPowerShellParser()
	res, err := p.Parse("scripts/hello.ps1", []byte(fixture))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Brief item 1: "1 class node (`Greeter`)".
	if got, want := len(res.Classes), 1; got != want {
		t.Fatalf("classes = %d; want %d (Greeter); got names %v",
			got, want, psClassNames(res.Classes))
	}
	if got, want := res.Classes[0].Name, "Greeter"; got != want {
		t.Errorf("class[0].Name = %q; want %q", got, want)
	}

	// Brief item 2: "3 method nodes (`Greeter.Format`,
	// `Greeter.Greet`, `Format-Hello`)".
	if got, want := len(res.Methods), 3; got != want {
		t.Fatalf("methods = %d; want %d (%v)",
			got, want, psMethodNames(res.Methods))
	}
	// Index by (ClassName, Name) so the assertion catches both
	// the wrong-class regression (Format-Hello mis-tagged as
	// Greeter.Format-Hello) and the wrong-name regression.
	wantMethods := map[[2]string]bool{
		{"Greeter", "Format"}: true,
		{"Greeter", "Greet"}:  true,
		{"", "Format-Hello"}:  true,
	}
	gotMethods := map[[2]string]bool{}
	for _, m := range res.Methods {
		gotMethods[[2]string{m.ClassName, m.Name}] = true
	}
	for key := range wantMethods {
		if !gotMethods[key] {
			t.Errorf("method ClassName=%q Name=%q missing; got %v",
				key[0], key[1], psMethodNames(res.Methods))
		}
	}

	// Brief item 3: "1 contains edge per node + file".
	// Dispatcher emits one file->class `contains` edge per
	// ClassDecl, one class->method per MethodDecl with
	// non-empty ClassName, and one file->method per MethodDecl
	// with empty ClassName. The (ClassName, Name) check above
	// pins the parser-level inputs the dispatcher consumes to
	// emit the brief's expected `contains` edge set.

	// Brief item 4: "1 imports edge to `Foo`".
	if got, want := len(res.Imports), 1; got != want {
		t.Fatalf("imports = %d; want %d (Foo via Import-Module); got %v",
			got, want, psImportPaths(res.Imports))
	}
	if got, want := res.Imports[0].Path, "Foo"; got != want {
		t.Errorf("import[0].Path = %q; want %q", got, want)
	}
	if got, want := psLangMetaStr(res.Imports[0].LangMeta, "module_kind"), "Import-Module"; got != want {
		t.Errorf("import[0].LangMeta[module_kind] = %q; want %q "+
			"(dispatcher routes module_kind=Import-Module to a real "+
			"`imports` edge vs dot_source/relative which it drops)",
			got, want)
	}

	// Brief item 5: static_calls `Greeter.Greet` ->
	// `Greeter.Format` via `$this.Format(...)`. HEAD's
	// MethodDecl has no `ReceiverCalls`/`Calls` field; the
	// production parser surfaces receiver calls through
	// `LangMeta["receiver_calls"]` (comma-joined member names)
	// per the PR #175 contract. We assert the slug here; on
	// the current 32-line stub this assertion surfaces as a
	// failure that signals "real subprocess implementation not
	// yet restored".
	var greet *MethodDecl
	for i := range res.Methods {
		if res.Methods[i].ClassName == "Greeter" && res.Methods[i].Name == "Greet" {
			greet = &res.Methods[i]
			break
		}
	}
	if greet == nil {
		t.Fatalf("Greeter.Greet missing; cannot assert receiver-call slug")
	}
	receiverCalls := psLangMetaStr(greet.LangMeta, "receiver_calls")
	if !strings.Contains(receiverCalls, "Format") {
		t.Errorf("Greeter.Greet.LangMeta[receiver_calls] = %q; want to contain %q "+
			"(receiver-qualified $this.Format(...) extractor, tech-spec §5.6; "+
			"dispatcher Pass 2b resolves <EnclosingClass>.<receiverCall> -> "+
			"Greeter.Format and emits the static_calls edge)",
			receiverCalls, "Format")
	}
	// Negative-resolution guard: the receiver-qualified path
	// must resolve within the enclosing class only, never to a
	// same-file free function. A regression that walked
	// command calls instead of MemberExpressionAst would emit
	// "Format-Hello" and Pass 2b would mis-resolve
	// $this.Format(...) to the free function.
	if strings.Contains(receiverCalls, "Format-Hello") {
		t.Errorf("Greeter.Greet.LangMeta[receiver_calls] = %q; must NOT contain %q "+
			"(receiver-qualified $this.X(...) resolves within the enclosing class only)",
			receiverCalls, "Format-Hello")
	}
}

// --- helpers ------------------------------------------------
// Helpers are prefixed `ps*` to avoid clashes with same-named
// helpers in cgo-tagged sibling tests (parser_treesitter_cpp_test.go
// has `classNames`, parser_treesitter_go_test.go has
// `methodNames`, `importModules`, `containsString`).

func psClassNames(cs []ClassDecl) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func psMethodNames(ms []MethodDecl) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.ClassName != "" {
			out = append(out, m.ClassName+"."+m.Name)
		} else {
			out = append(out, m.Name)
		}
	}
	return out
}

func psImportPaths(is []Import) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Path)
	}
	return out
}

func psLangMetaStr(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}
