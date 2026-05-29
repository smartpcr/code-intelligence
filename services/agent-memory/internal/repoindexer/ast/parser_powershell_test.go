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
	"os"
	"os/exec"
	"path/filepath"
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

// TestPowerShellParser_RegisteredInActiveBuild is a smoke
// test for the parsers_cgo.go / parsers_nocgo.go
// `defaultParsers()` registration. Whichever build tag is
// active at compile time, the dispatcher's default parser set
// MUST contain a parser whose `Language() == "powershell"` so
// `.ps1` / `.psm1` / `.psd1` files route through this parser.
// This catches a regression where a future edit drops the
// `NewPowerShellParser()` entry from the file selected by the
// current build tag. Coverage of the OTHER build tag's file
// in the same test binary lives in the sister test
// `TestPowerShellParser_RegisteredInBothBuildTagSources`,
// which reads both source files directly so a one-binary
// `go test` provably exercises both registration paths.
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
// structural complement to `TestPowerShellParser_RegisteredInActiveBuild`.
// A single compiled test binary can only load one of
// `parsers_cgo.go` / `parsers_nocgo.go` (the `//go:build cgo`
// vs `//go:build !cgo` tags are mutually exclusive), so a
// runtime-only assertion against `defaultParsers()` can NEVER
// prove both files register `NewPowerShellParser()`. This test
// closes that gap by reading both source files from the
// repository tree and verifying each contains the literal
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

// TestPowerShellFixture_EmitsExpectedParseResult pins the
// parser-level shape the embedded extraction script must
// produce for the canonical fixture. This is a
// PARSER-LEVEL assertion only (it does NOT exercise the
// dispatcher's node/edge materialisation pipeline); the
// dispatcher-level assertions for the same fixture
// (contains / static_calls / imports edges, dot-source
// drop) live in `parser_powershell_dispatcher_test.go`
// behind the `//go:build canonical_dispatcher` tag AND
// run pwsh-independently via a stub parser so a CI host
// without `pwsh` still gets full dispatcher coverage.
//
// What this test pins:
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
// reports a t.Skip instead of failing; the embedded script
// regression coverage on pwsh-less hosts is handled by
// `TestPowerShellEnvelope_ToParseResult_MapsAllFields`
// (synthetic envelope -> ParseResult mapping, no pwsh
// required).
func TestPowerShellFixture_EmitsExpectedParseResult(t *testing.T) {
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

// TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 6.3 acceptance set per the workstream brief verbatim
// (story code-intelligence:AST-PARSER-FOR-ADDIT, phase
// powershell-parser, stage powershell-fixture-test).
//
// The fixture is intentionally the brief's simpler shape:
//
//	class Greeter {
//	    [string] $Prefix
//	    [string] Format([string]$name) { return "$($this.Prefix) $name" }
//	    [string] Greet([string]$name)  { return $this.Format($name) }
//	}
//	function Format-Hello { param([string]$Name) return "hi $Name" }
//	Import-Module Foo
//
// Brief assertions (dispatcher-level vocabulary the brief uses):
//
//   - 1 class node (`Greeter`)
//   - 3 method nodes (`Greeter.Format`, `Greeter.Greet`,
//     `Format-Hello`)
//   - 1 contains edge per node + file
//   - 1 imports edge to `Foo`
//   - `Greeter.Greet`'s `static_calls` to `Greeter.Format`
//     resolves through the `$this.Format(...)` receiver-
//     qualified path covered by the Stage 6.1 `$this.X(...)`
//     extractor (tech-spec Section 5.6).
//
// Note: `[Greeter]::Format(...)` static-class invocations are
// explicitly out of v1 scope per the brief; the fixture uses
// an instance receiver call (`$this.Format(...)`) instead.
//
// IMPLEMENTATION NOTE — parser-level assertions, by design:
// The brief requires this test be in an UNTAGGED file
// (`parser_powershell_test.go`, no `//go:build` line). The
// dispatcher-level helpers that would let us assert on
// emitted Node / Edge inserts directly — `newFakeWriter`,
// `NewDispatcher(fw, WithParsers(...))`, `makeEvent`,
// `fw.nodesOf`, `fw.edgesOf`, `fw.nodeIDBySimpleSig`,
// `lastSegmentAfterHash` — all live in
// `//go:build canonical_dispatcher`-gated test files
// (dispatcher_test.go, parser_typescript_test.go). An
// untagged test file cannot reference any of those symbols
// without producing a hard compile error in the default
// (no-tag) build path, regardless of any runtime
// `t.Skip("pwsh not on PATH")` guard.
//
// We therefore assert at the PARSER level on the
// `ParseResult` fields the dispatcher consumes to emit each
// brief item, with the mapping documented inline beside each
// assertion. The dispatcher-level end-to-end coverage of the
// same fixture (real node/edge emission via the
// `fakeNodeEdgeWriter` capture path) lives in
// `parser_powershell_dispatcher_test.go` (canonical_dispatcher
// build tag), and the Pass 2b receiver-qualified resolution
// of `$this.Format(...)` -> `Greeter.Format` is structurally
// pinned by `TestDispatcher_PowerShell_*` cases in
// `dispatcher_pass2bd_test.go` (also canonical_dispatcher).
//
// Skipped when `pwsh` is not on PATH, per brief, so a
// PowerShell-less CI host stays green.
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
	// Dispatcher mapping: each ClassDecl in ParseResult.Classes
	// produces exactly one `class` node + one `contains` edge
	// from the file node (see `Dispatcher.emit` /
	// `Dispatcher.emitContains` in dispatcher.go on the
	// canonical_dispatcher branch).
	if got, want := len(res.Classes), 1; got != want {
		t.Fatalf("classes = %d; want %d (Greeter); got names %v", got, want, classNames(res.Classes))
	}
	if got, want := res.Classes[0].QualifiedName, "Greeter"; got != want {
		t.Errorf("class[0].QualifiedName = %q; want %q", got, want)
	}

	// Brief item 2: "3 method nodes (`Greeter.Format`,
	// `Greeter.Greet`, `Format-Hello`)".
	// Asserted by NAME (not just by count) so a regression that
	// emits "any 3 methods" — e.g. extracting a phantom
	// `$Prefix` accessor — fails loudly.
	if got, want := len(res.Methods), 3; got != want {
		t.Fatalf("methods = %d; want %d (%v)", got, want, methodNames(res.Methods))
	}
	byName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		byName[m.QualifiedName] = m
	}
	wantEnclosing := map[string]string{
		"Greeter.Format": "Greeter",
		"Greeter.Greet":  "Greeter",
		"Format-Hello":   "",
	}
	for name, enclosing := range wantEnclosing {
		m, ok := byName[name]
		if !ok {
			t.Errorf("method %q missing from emitted set; got %v", name, methodNames(res.Methods))
			continue
		}
		// Brief item 3: "1 contains edge per node + file"
		// (file->class, class->method for in-class methods,
		// file->method for the free function). The dispatcher
		// emits these from `EnclosingClass`: a non-empty value
		// produces `class->method` contains; an empty value
		// produces `file->method`. Asserting `EnclosingClass`
		// per method is the parser-level invariant the
		// dispatcher relies on to emit the brief's expected
		// 4-edge containment set.
		if got, want := m.EnclosingClass, enclosing; got != want {
			t.Errorf("method %q EnclosingClass = %q; want %q (controls file-vs-class containment edge in dispatcher emit)",
				name, got, want)
		}
	}

	// Brief item 4: "1 imports edge to `Foo`".
	// Dispatcher mapping: each non-relative Import in
	// ParseResult.Imports produces one external `package` node
	// (signature `repo::package::<module>`) + one `imports`
	// edge from the file node to that package node. Asserting
	// (a) exactly one Import, (b) Module="Foo",
	// (c) LangMeta["module_kind"]="Import-Module" pins the
	// data the dispatcher consumes to emit that single edge.
	if got, want := len(res.Imports), 1; got != want {
		t.Fatalf("imports = %d; want %d (Foo via Import-Module)", got, want)
	}
	if got, want := res.Imports[0].Module, "Foo"; got != want {
		t.Errorf("import.Module = %q; want %q", got, want)
	}
	if got, want := langMetaString(res.Imports[0], "module_kind"), "Import-Module"; got != want {
		t.Errorf("import.LangMeta[module_kind] = %q; want %q", got, want)
	}

	// Brief item 5: "`Greeter.Greet`'s `static_calls` to
	// `Greeter.Format` resolves through the
	// `$this.Format(...)` receiver-qualified path covered by
	// the Stage 6.1 `$this.X(...)` extractor".
	//
	// Parser contract: the embedded extraction script's
	// `PS-ExtractReceiverCalls` emits the simple member name
	// "Format" (NOT the qualified "Greeter.Format") onto
	// `Greeter.Greet.ReceiverCalls`. The dispatcher's Pass 2b
	// then resolves `<EnclosingClass>.<receiverCallName>` ->
	// the same-file method NodeID and emits the
	// `static_calls` edge. The brief's note that
	// `[Greeter]::Format(...)` static-class invocations are
	// out of v1 scope means the receiver-qualified path is
	// the ONLY route from `Greet` to `Format` in v1; this
	// test pins that route exists.
	greet, ok := byName["Greeter.Greet"]
	if !ok {
		t.Fatalf("Greeter.Greet missing; got %v", methodNames(res.Methods))
	}
	if !containsString(greet.ReceiverCalls, "Format") {
		t.Errorf("Greeter.Greet.ReceiverCalls = %v; want to include \"Format\" so dispatcher Pass 2b emits the same-file static_calls edge Greeter.Greet -> Greeter.Format (tech-spec Section 5.6 $this.X(...) extractor)",
			greet.ReceiverCalls)
	}
	// Negative-resolution guard: the receiver-qualified path
	// must resolve to a method in the SAME CLASS, never to a
	// same-file free function. If the extractor mistakenly
	// emitted "Format-Hello" (e.g. from a bug that walked
	// command calls instead of MemberExpressionAst), Pass 2b
	// would then mis-resolve `$this.Format(...)` to the free
	// function `Format-Hello` because that name happens to
	// share the simple prefix. Guarding against it here
	// catches that regression at the parser layer.
	if containsString(greet.ReceiverCalls, "Format-Hello") {
		t.Errorf("Greeter.Greet.ReceiverCalls = %v; must NOT include \"Format-Hello\" — receiver-qualified $this.X(...) resolves within the enclosing class only, not to same-file free functions",
			greet.ReceiverCalls)
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
// AND set the per-call timeout to a value so small that the
// `context.WithTimeout` deadline is GUARANTEED to fire before
// the subprocess could possibly emit JSON, regardless of how
// warm pwsh's startup cache is.
//
// Determinism: `timeout: 1 * time.Nanosecond` makes the ctx
// already-expired by the time `exec.CommandContext.Run()` is
// reached. The runtime detects the cancelled context, kills
// the subprocess (or never starts it), and `Run()` returns a
// non-nil error. Our `Parse()` then inspects `ctx.Err()`,
// finds `context.DeadlineExceeded`, and returns the
// "subprocess timeout" wrapper — un-wrapped from
// `ErrParserUnavailable` so `safeParse` routes the failure to
// `ast.parse.error` instead of the (misleading)
// `ast.dispatch.skip{reason="pwsh_not_available"}` branch.
//
// Replaces the prior 50ms-vs-cold-start approach (iter-2
// evaluator finding #5: "timing-sensitive and potentially
// flaky on faster or warmed environments"). The 1ns timeout
// is deterministic on every host.
//
// Skipped if `pwsh` is not on PATH because the test needs to
// actually launch the subprocess to reach the timeout branch
// (the no-pwsh path returns the sentinel before any context
// work happens).
func TestPowerShellParser_Timeout_ReturnsNonSentinelError(t *testing.T) {
	bin, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not on PATH")
	}
	p := &powershellParser{pwshBin: bin, timeout: 1 * time.Nanosecond}
	_, err = p.Parse("foo.ps1", []byte("function Foo {}"))
	if err == nil {
		t.Fatal("Parse returned nil error on timeout")
	}
	if errors.Is(err, ErrParserUnavailable) {
		t.Errorf("Parse returned wrapped ErrParserUnavailable on timeout; want plain error so safeParse routes to ast.parse.error")
	}
}

// TestPowerShellEnvelope_ToParseResult_MapsAllFields pins
// the JSON-envelope -> `ParseResult` mapping that the
// subprocess `Parse()` path relies on. The test runs WITHOUT
// `pwsh` (so it executes on every CI host) by constructing
// a synthetic `powershellEnvelope` directly — the same shape
// the embedded extraction script writes to stdout — and
// asserting the dispatcher-facing `ParseResult` carries the
// expected classes / methods / imports with the correct
// canonical names, enclosing classes, base-type split,
// module-kind metadata, member-access conversion, and
// dedup behaviour.
//
// This addresses iter-2 evaluator finding #4: "substantive
// extraction tests all skip when pwsh is unavailable,
// leaving CI hosts without PowerShell to validate only
// constructor/sentinel/registration behavior and not [...]
// JSON mapping". Together with the structural dispatcher
// tests in `parser_powershell_dispatcher_test.go`, this
// gives a pwsh-less host full coverage of the
// pwsh-independent parser surface.
func TestPowerShellEnvelope_ToParseResult_MapsAllFields(t *testing.T) {
	env := powershellEnvelope{
		Types: []psTypeRecord{{
			Name:      "Greeter",
			Kind:      "class",
			BaseTypes: []string{"BaseClass", "IFormatter"},
			StartLine: 5,
			EndLine:   12,
			Methods: []psMethodRecord{{
				Name:            "Format",
				Params:          "[string]$name",
				StartLine:       6,
				EndLine:         8,
				BodyStartLine:   6,
				BodyEndLine:     8,
				BodyStartOffset: 100,
				BodyEndOffset:   130,
				BodyText:        "{ return $name }",
				Modifiers:       []string{"hidden"},
				Calls:           []string{"Write-Host", "Write-Host"}, // dedup
				MemberAccesses:  []psMemberAccessRecord{{Name: "Prefix", IsWrite: false}},
			}, {
				Name:            "Greet",
				Params:          "[string]$name",
				StartLine:       9,
				EndLine:         11,
				BodyStartLine:   9,
				BodyEndLine:     11,
				BodyStartOffset: 200,
				BodyEndOffset:   230,
				BodyText:        "{ return $this.Format($name) }",
				ReceiverCalls:   []string{"Format"},
			}},
		}},
		Functions: []psFunctionRecord{{
			Name:            "Format-Hello",
			Params:          "[string]$Name",
			StartLine:       14,
			EndLine:         16,
			BodyStartLine:   14,
			BodyEndLine:     16,
			BodyStartOffset: 300,
			BodyEndOffset:   330,
			BodyText:        "{ return \"hi $Name\" }",
			Calls:           []string{"Write-Host"},
		}},
		Imports: []psImportRecord{{
			Module:     "Foo",
			ModuleKind: "Import-Module",
			Line:       2,
		}, {
			Module:     "Bar",
			ModuleKind: "using_module",
			Line:       1,
		}, {
			Module:     "./helpers.ps1",
			ModuleKind: "dot_source",
			Line:       3,
		}, {
			Module:     "PSScriptAnalyzer",
			ModuleKind: "command_call",
			CmdletVerb: "Invoke",
			Line:       4,
		}},
	}

	res := env.toParseResult()

	// Class.
	if got, want := len(res.Classes), 1; got != want {
		t.Fatalf("classes = %d; want %d", got, want)
	}
	c := res.Classes[0]
	if c.QualifiedName != "Greeter" {
		t.Errorf("class.QualifiedName = %q; want %q", c.QualifiedName, "Greeter")
	}
	if c.Kind != "class" {
		t.Errorf("class.Kind = %q; want %q", c.Kind, "class")
	}
	if got, want := strings.Join(c.Extends, ","), "BaseClass"; got != want {
		t.Errorf("class.Extends = %v; want [%q] (first base type splits to Extends per splitPowerShellBaseTypes)", c.Extends, want)
	}
	if got, want := strings.Join(c.Implements, ","), "IFormatter"; got != want {
		t.Errorf("class.Implements = %v; want [%q] (remaining base types split to Implements)", c.Implements, want)
	}

	// Methods: 2 class methods + 1 free function = 3.
	if got, want := len(res.Methods), 3; got != want {
		t.Fatalf("methods = %d; want %d (%v)", got, want, methodNames(res.Methods))
	}
	byName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		byName[m.QualifiedName] = m
	}

	format, ok := byName["Greeter.Format"]
	if !ok {
		t.Fatalf("method Greeter.Format missing; got %v", methodNames(res.Methods))
	}
	if format.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.Format.EnclosingClass = %q; want %q", format.EnclosingClass, "Greeter")
	}
	if format.ParamSignature != "[string]$name" {
		t.Errorf("Greeter.Format.ParamSignature = %q; want %q", format.ParamSignature, "[string]$name")
	}
	if format.BodySource != " return $name " {
		t.Errorf("Greeter.Format.BodySource = %q; want %q (outer { } stripped by stripPowerShellBraces)", format.BodySource, " return $name ")
	}
	if format.BodyStartByte != 101 || format.BodyEndByte != 128 {
		t.Errorf("Greeter.Format body offsets = (%d, %d); want (101, 128) — brace-stripping shifts by 1 on each side", format.BodyStartByte, format.BodyEndByte)
	}
	// Modifiers preserved.
	if !containsString(format.Modifiers, "hidden") {
		t.Errorf("Greeter.Format.Modifiers = %v; want to include \"hidden\"", format.Modifiers)
	}
	// dedupeStrings collapsed the duplicate Write-Host call.
	if got, want := len(format.Calls), 1; got != want {
		t.Errorf("Greeter.Format.Calls = %v; want exactly 1 entry after dedupe (input had 2x Write-Host)", format.Calls)
	}
	if len(format.Calls) > 0 && format.Calls[0] != "Write-Host" {
		t.Errorf("Greeter.Format.Calls[0] = %q; want %q", format.Calls[0], "Write-Host")
	}
	// MemberAccesses converted from psMemberAccessRecord -> MemberAccess.
	if !containsMemberAccessName(format.MemberAccesses, "Prefix") {
		t.Errorf("Greeter.Format.MemberAccesses = %v; want to include {Name:\"Prefix\"}", format.MemberAccesses)
	}

	greet, ok := byName["Greeter.Greet"]
	if !ok {
		t.Fatalf("method Greeter.Greet missing; got %v", methodNames(res.Methods))
	}
	if !containsString(greet.ReceiverCalls, "Format") {
		t.Errorf("Greeter.Greet.ReceiverCalls = %v; want to include \"Format\"", greet.ReceiverCalls)
	}

	free, ok := byName["Format-Hello"]
	if !ok {
		t.Fatalf("method Format-Hello missing; got %v", methodNames(res.Methods))
	}
	if free.EnclosingClass != "" {
		t.Errorf("Format-Hello.EnclosingClass = %q; want empty (free function)", free.EnclosingClass)
	}

	// Imports: 4 entries, LangMeta carries module_kind and cmdlet_verb.
	if got, want := len(res.Imports), 4; got != want {
		t.Fatalf("imports = %d; want %d", got, want)
	}
	importsByMod := indexImportsByModule(res.Imports)
	for mod, wantKind := range map[string]string{
		"Foo":              "Import-Module",
		"Bar":              "using_module",
		"./helpers.ps1":    "dot_source",
		"PSScriptAnalyzer": "command_call",
	} {
		imp, ok := importsByMod[mod]
		if !ok {
			t.Errorf("import for module %q missing; got modules %v", mod, importModules(res.Imports))
			continue
		}
		if got := langMetaString(imp, "module_kind"); got != wantKind {
			t.Errorf("import %q LangMeta.module_kind = %q; want %q", mod, got, wantKind)
		}
	}
	if got := langMetaString(importsByMod["PSScriptAnalyzer"], "cmdlet_verb"); got != "Invoke" {
		t.Errorf("PSScriptAnalyzer LangMeta.cmdlet_verb = %q; want %q (command_call carries the verb)", got, "Invoke")
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

// isRelativeImport reports whether a PowerShell import module
// specifier is a workspace-relative path (dot-source, e.g.
// `./helpers.ps1` or `../shared/foo.ps1`). The dispatcher's
// Pass 0 drops relative imports because they map onto in-repo
// File nodes that a later cross-file resolver workstream will
// stitch (see ParseResult.Imports docstring at parser.go).
func isRelativeImport(module string) bool {
	return strings.HasPrefix(module, "./") || strings.HasPrefix(module, "../")
}
