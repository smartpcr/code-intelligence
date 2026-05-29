package ast

// Stage 6.1 / 6.3 fixture and sentinel tests for the
// PowerShell subprocess parser (parser_powershell.go). The
// file deliberately carries NO build tags: the parser itself
// has no CGO dependency, so the same tests are valid under
// `go test ./...` whether or not the host has tree-sitter
// bindings compiled in. Tests that genuinely exercise the
// subprocess gate on `exec.LookPath("pwsh")` at the top so
// PowerShell-less CI stays green.

import (
	"errors"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestPowerShellParser_NoPwsh_ReturnsSentinel pins the Stage
// 6.1 acceptance scenario "pwsh missing returns sentinel".
// The test does NOT need `pwsh` on PATH: it constructs a
// `powershellParser{pwshBin:""}` directly to force the
// short-circuit branch, then asserts the returned error wraps
// `ErrParserUnavailable` AND carries the
// `reason=pwsh_not_available` slug the dispatcher's
// `parseUnavailableReason` helper extracts (see
// dispatcher.go::parseUnavailableReason and the existing
// dispatcher_pass2bd_test.go::TestDispatcher_ErrParserUnavailable_LogsSkip
// for the consuming side).
func TestPowerShellParser_NoPwsh_ReturnsSentinel(t *testing.T) {
	p := &powershellParser{pwshBin: ""}

	res, err := p.Parse("foo.ps1", []byte("function Foo {}"))
	if err == nil {
		t.Fatal("Parse returned nil error; want ErrParserUnavailable")
	}
	if !errors.Is(err, ErrParserUnavailable) {
		t.Errorf("errors.Is(err, ErrParserUnavailable) = false; got err=%v", err)
	}
	if !strings.Contains(err.Error(), "reason=pwsh_not_available") {
		t.Errorf("err.Error() = %q; want substring 'reason=pwsh_not_available'", err.Error())
	}
	if len(res.Classes) != 0 || len(res.Methods) != 0 || len(res.Imports) != 0 {
		t.Errorf("Parse returned non-empty result on sentinel: %+v", res)
	}
}

// TestPowerShellParser_Interface_Wired pins the trivial
// contract surface: `Language()` and `Extensions()` return
// the canonical id and the v1 extension set. Catches a
// regression where a future edit accidentally drops `.psm1`
// or renames the language id.
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

// TestPowerShellParser_Registered_InBothBuildTags is a smoke
// test for the parsers_cgo.go / parsers_nocgo.go
// `defaultParsers()` registration. Whichever build tag is
// active at compile time, the dispatcher's default parser set
// MUST contain a parser whose `Language() == "powershell"` so
// `.ps1` / `.psm1` / `.psd1` files route through this parser.
// This catches a regression where a future edit drops the
// `NewPowerShellParser()` entry from one of the two files.
func TestPowerShellParser_Registered_InBothBuildTags(t *testing.T) {
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

// powershellFixture is the canonical PowerShell file the
// Stage 6.3 plan calls for. It exercises:
//
//   - One class (`Greeter`) with one property (`$Prefix`) and
//     two methods (`Format`, `Greet`) so we get a same-file
//     class node + 2 same-class method nodes.
//   - `Greet` calls `$this.Format(...)` which the receiver-
//     calls extractor (`PS-ExtractReceiverCalls` in the
//     embedded script) emits as `ReceiverCalls=["Format"]`.
//     The dispatcher's Pass 2b resolves
//     `<EnclosingClass>.<receiverCall>` -> a same-file static
//     edge from `Greeter.Greet` to `Greeter.Format`.
//   - One free function (`Format-Hello`) so we get a method
//     node with `EnclosingClass=""`.
//   - One `Import-Module Foo` so we get an Import with
//     `LangMeta["module_kind"]=="Import-Module"`.
//   - One `using module Bar` so we get an Import with
//     `LangMeta["module_kind"]=="using_module"`.
//   - One `. ./helpers.ps1` dot-source so we get an Import
//     with `LangMeta["module_kind"]=="dot_source"` whose
//     relative module path the dispatcher's
//     `isRelativeImport` drops at edge-emission time.
//
// The `using module Bar` line lives FIRST per PowerShell's
// hard rule that `using` statements must precede any other
// statements in the script (verified the rule fires; the
// embedded script's `ModuleNotFoundDuringParse` filter
// tolerates the missing-module diagnostic so the fixture
// parses without an actual `Bar` module on the host).
const powershellFixture = `using module Bar
Import-Module Foo
. ./helpers.ps1

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

// TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 6.3 fixture acceptance. We assert directly on
// `ParseResult` (rather than the full dispatcher node/edge
// pipeline, which lives behind `//go:build canonical_dispatcher`):
//
//   - Exactly 1 class node `Greeter` with Kind=="class".
//   - Exactly 3 method nodes: `Greeter.Format`,
//     `Greeter.Greet`, and `Format-Hello`. The two class
//     methods carry `EnclosingClass="Greeter"`; the free
//     function carries the empty string.
//   - `Greeter.Greet.ReceiverCalls == ["Format"]` so the
//     dispatcher's Pass 2b receiver-qualified resolution can
//     emit the same-file static_calls edge to `Greeter.Format`.
//   - At least 3 imports with the expected
//     `LangMeta["module_kind"]` slugs.
//
// Gates on `exec.LookPath("pwsh")` so a PowerShell-less host
// reports a t.Skip instead of failing.
func TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH")
	}
	p := NewPowerShellParser()
	res, err := p.Parse("scripts/hello.ps1", []byte(powershellFixture))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Classes
	if got, want := len(res.Classes), 1; got != want {
		t.Fatalf("classes = %d; want %d (%v)", got, want, classNames(res.Classes))
	}
	if got, want := res.Classes[0].QualifiedName, "Greeter"; got != want {
		t.Errorf("class[0].QualifiedName = %q; want %q", got, want)
	}
	if got, want := res.Classes[0].Kind, "class"; got != want {
		t.Errorf("class[0].Kind = %q; want %q", got, want)
	}

	// Methods
	if got, want := len(res.Methods), 3; got != want {
		t.Fatalf("methods = %d; want %d (%v)", got, want, methodNames(res.Methods))
	}
	byName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		byName[m.QualifiedName] = m
	}
	greet, ok := byName["Greeter.Greet"]
	if !ok {
		t.Fatalf("method Greeter.Greet missing; got methods %v", methodNames(res.Methods))
	}
	if greet.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.Greet.EnclosingClass = %q; want %q", greet.EnclosingClass, "Greeter")
	}
	if !containsString(greet.ReceiverCalls, "Format") {
		t.Errorf("Greeter.Greet.ReceiverCalls = %v; want to include \"Format\" so dispatcher Pass 2b can emit Greeter.Greet -> Greeter.Format edge",
			greet.ReceiverCalls)
	}
	if format, ok := byName["Greeter.Format"]; !ok {
		t.Errorf("method Greeter.Format missing; got %v", methodNames(res.Methods))
	} else {
		if format.EnclosingClass != "Greeter" {
			t.Errorf("Greeter.Format.EnclosingClass = %q; want %q", format.EnclosingClass, "Greeter")
		}
		// $this.Prefix read inside Format must surface as a
		// member access so Pass 2c can emit a `reads` edge.
		if !containsMemberAccessName(format.MemberAccesses, "Prefix") {
			t.Errorf("Greeter.Format.MemberAccesses = %v; want to include \"Prefix\"",
				format.MemberAccesses)
		}
	}
	if free, ok := byName["Format-Hello"]; !ok {
		t.Errorf("method Format-Hello missing; got %v", methodNames(res.Methods))
	} else if free.EnclosingClass != "" {
		t.Errorf("Format-Hello.EnclosingClass = %q; want empty (free function)", free.EnclosingClass)
	}

	// Imports
	imports := indexImportsByModule(res.Imports)
	if foo, ok := imports["Foo"]; !ok {
		t.Errorf("import Foo missing; got modules %v", importModules(res.Imports))
	} else if got, want := langMetaString(foo, "module_kind"), "Import-Module"; got != want {
		t.Errorf("Foo.LangMeta.module_kind = %q; want %q", got, want)
	}
	if bar, ok := imports["Bar"]; !ok {
		t.Errorf("import Bar missing; got modules %v", importModules(res.Imports))
	} else if got, want := langMetaString(bar, "module_kind"), "using_module"; got != want {
		t.Errorf("Bar.LangMeta.module_kind = %q; want %q", got, want)
	}
	if dot, ok := imports["./helpers.ps1"]; !ok {
		t.Errorf("import ./helpers.ps1 missing; got modules %v", importModules(res.Imports))
	} else if got, want := langMetaString(dot, "module_kind"), "dot_source"; got != want {
		t.Errorf("./helpers.ps1.LangMeta.module_kind = %q; want %q", got, want)
	}
}

// TestPowerShellFixture_DotSourceDropped pins the Stage 6.3
// "dot source dropped" sub-scenario: a `. ./helpers.ps1`
// dot-source MUST surface in `Imports` with
// `LangMeta["module_kind"]=="dot_source"` AND its module path
// MUST be detected by the dispatcher's `isRelativeImport`
// helper so the import edge is suppressed (per architecture
// Section 6.1's reference-table row for dot-source).
//
// We assert the parser side here (parser emits the import
// with the right module text and module_kind); the dispatcher
// drop is exercised end-to-end by the existing
// `TestDispatcher_RoutesByExtension` / `EmitsImportsEdges`
// suites in dispatcher_test.go (canonical_dispatcher tag).
func TestPowerShellFixture_DotSourceDropped(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH")
	}
	const src = `. ./helpers.ps1

function Bar {
    Write-Host hi
}
`
	p := NewPowerShellParser()
	res, err := p.Parse("scripts/runner.ps1", []byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	var dot *Import
	for i := range res.Imports {
		if res.Imports[i].Module == "./helpers.ps1" {
			dot = &res.Imports[i]
			break
		}
	}
	if dot == nil {
		t.Fatalf("dot-source import missing; got imports=%v", importModules(res.Imports))
	}
	if got, want := langMetaString(*dot, "module_kind"), "dot_source"; got != want {
		t.Errorf("dot-source module_kind = %q; want %q", got, want)
	}
	if !isRelativeImport(dot.Module) {
		t.Errorf("dot.Module = %q; isRelativeImport = false; want true so dispatcher suppresses the import edge",
			dot.Module)
	}
}

// TestPowerShellParser_Timeout_ReturnsNonSentinelError pins
// the Stage 6.1 acceptance scenario "pwsh timeout returns
// error not sentinel". We point `pwshBin` at the real pwsh
// AND set the per-call timeout to a tiny value; the embedded
// extraction script sleeps via `Start-Sleep` long enough to
// blow past the deadline. The returned error MUST NOT wrap
// `ErrParserUnavailable` so the dispatcher falls through to
// `ast.parse.error` instead of the (misleading)
// `ast.dispatch.skip{reason="pwsh_not_available"}` branch.
//
// Skipped if `pwsh` is not on PATH because the test needs to
// actually launch the subprocess.
func TestPowerShellParser_Timeout_ReturnsNonSentinelError(t *testing.T) {
	bin, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not on PATH")
	}
	// 50 ms is well below pwsh's cold-start time on every
	// supported host, so the subprocess hits the context
	// deadline before it ever reaches our extraction script.
	p := &powershellParser{pwshBin: bin, timeout: 50 * time.Millisecond}
	_, err = p.Parse("foo.ps1", []byte("function Foo {}"))
	if err == nil {
		t.Fatal("Parse returned nil error on timeout")
	}
	if errors.Is(err, ErrParserUnavailable) {
		t.Errorf("Parse returned wrapped ErrParserUnavailable on timeout; want plain error so safeParse routes to ast.parse.error")
	}
}

// --- test helpers ------------------------------------------

func classNames(cs []ClassDecl) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.QualifiedName)
	}
	return out
}

func methodNames(ms []MethodDecl) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.QualifiedName)
	}
	return out
}

func importModules(is []Import) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Module)
	}
	return out
}

func indexImportsByModule(is []Import) map[string]Import {
	out := make(map[string]Import, len(is))
	for _, i := range is {
		out[i.Module] = i
	}
	return out
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsMemberAccessName(as []MemberAccess, name string) bool {
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	return false
}

func langMetaString(imp Import, key string) string {
	if imp.LangMeta == nil {
		return ""
	}
	v, ok := imp.LangMeta[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
