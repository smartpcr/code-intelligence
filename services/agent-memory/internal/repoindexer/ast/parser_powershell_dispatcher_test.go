//go:build canonical_dispatcher

package ast

// Stage 6.3 dispatcher-level fixture tests for the PowerShell
// parser. These tests address iter-2 evaluator findings #1 and
// #2 (the parser-level fixture test did NOT exercise the
// dispatcher's contains / static_calls / imports edge emission;
// the dot-source suppression assertion called `isRelativeImport`
// directly rather than going through `emitImportsEdges`).
//
// Design choice: we inject a `stubPowerShellParser` via
// `WithParsers(...)` rather than driving the real
// `NewPowerShellParser()` path. This serves two purposes:
//
//  1. Deterministic coverage on every CI host. The real
//     subprocess parser is `pwsh`-dependent; a `pwsh`-less host
//     would skip the only dispatcher-level coverage, leaving
//     finding #1 / #2 effectively unfixed (per the rubber-duck
//     critique on iter-3 plan). The stub returns a hand-built
//     `ParseResult` matching what the embedded extraction
//     script produces for the same fixture, so the
//     dispatcher's contains / static_calls / imports / Pass 2b
//     pipelines fire identically on every host.
//
//  2. Failure isolation. A dispatcher-side regression
//     (e.g. an edit that skips PowerShell in `emitImportsEdges`
//     wiring, or breaks the Pass 2b receiver-call resolver for
//     the `powershell` language) produces a test failure
//     attributable to the dispatcher code under test, NOT to a
//     subprocess / script flake.
//
// A separate `_EndToEnd_` test exercises the real
// `NewPowerShellParser()` path through the same dispatcher
// under the same fixture; it is `t.Skip`-gated on
// `exec.LookPath("pwsh")` so it only fires on hosts that have
// PowerShell installed.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// stubPowerShellParser implements `LanguageParser` and
// returns a hand-built `ParseResult` mirroring what the real
// `powershellParser` produces for `stubPowerShellFixturePath`.
// `Language()` and `Extensions()` match the production parser
// so the dispatcher's extension routing picks this stub for
// `.ps1` files.
type stubPowerShellParser struct {
	result ParseResult
	err    error
}

func (*stubPowerShellParser) Language() string         { return "powershell" }
func (*stubPowerShellParser) Extensions() []string     { return []string{".ps1", ".psm1", ".psd1"} }
func (s *stubPowerShellParser) Parse(_ string, _ []byte) (ParseResult, error) {
	return s.result, s.err
}

// powerShellFixtureParseResult returns the `ParseResult` shape
// the production parser produces for `powershellFixture`
// (defined in parser_powershell_test.go). Mirroring the
// production output here lets us drive the dispatcher
// deterministically without needing `pwsh` on PATH.
//
// The mapping mirrors `toParseResult()` in parser_powershell.go:
//   - `using module Bar`        -> Import{Module:"Bar", LangMeta.module_kind:"using_module"}
//   - `Import-Module Foo`       -> Import{Module:"Foo", LangMeta.module_kind:"Import-Module"}
//   - `. ./helpers.ps1`         -> Import{Module:"./helpers.ps1", LangMeta.module_kind:"dot_source"}
//   - `class Greeter { Format(); Greet(); }` -> ClassDecl + 2 MethodDecl
//   - `function Format-Hello`   -> MethodDecl with EnclosingClass=""
//   - `$this.Format($name)` in Greet -> MethodDecl.ReceiverCalls=["Format"]
func powerShellFixtureParseResult() ParseResult {
	return ParseResult{
		Classes: []ClassDecl{{
			QualifiedName: "Greeter",
			Kind:          "class",
			StartLine:     5,
			EndLine:       12,
		}},
		Methods: []MethodDecl{{
			QualifiedName:  "Greeter.Format",
			EnclosingClass: "Greeter",
			ParamSignature: "[string]$name",
			BodySource:     " return \"$($this.Prefix) $name\" ",
			StartLine:      6,
			EndLine:        8,
			BodyStartLine:  6,
			BodyEndLine:    8,
			BodyStartByte:  100,
			BodyEndByte:    140,
			MemberAccesses: []MemberAccess{{Name: "Prefix"}},
		}, {
			QualifiedName:  "Greeter.Greet",
			EnclosingClass: "Greeter",
			ParamSignature: "[string]$name",
			BodySource:     " return $this.Format($name) ",
			StartLine:      9,
			EndLine:        11,
			BodyStartLine:  9,
			BodyEndLine:    11,
			BodyStartByte:  200,
			BodyEndByte:    240,
			ReceiverCalls:  []string{"Format"},
		}, {
			QualifiedName:  "Format-Hello",
			EnclosingClass: "",
			ParamSignature: "[string]$Name",
			BodySource:     " return \"hi $Name\" ",
			StartLine:      14,
			EndLine:        16,
			BodyStartLine:  14,
			BodyEndLine:    16,
			BodyStartByte:  300,
			BodyEndByte:    330,
		}},
		Imports: []Import{{
			Path:     "Bar",
			Module:   "Bar",
			Line:     1,
			LangMeta: map[string]any{"module_kind": "using_module"},
		}, {
			Path:     "Foo",
			Module:   "Foo",
			Line:     2,
			LangMeta: map[string]any{"module_kind": "Import-Module"},
		}, {
			Path:     "./helpers.ps1",
			Module:   "./helpers.ps1",
			Line:     3,
			LangMeta: map[string]any{"module_kind": "dot_source"},
		}},
	}
}

// TestPowerShellFixture_DispatcherEmitsExpectedNodesAndEdges
// pins the Stage 6.3 dispatcher-level acceptance for the
// canonical PowerShell fixture (the same fixture covered
// at parser level by
// `TestPowerShellFixture_EmitsExpectedParseResult`).
//
// The assertions mirror `TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet`
// shape:
//   - 1 class node ("Greeter")
//   - 3 method nodes ("Greeter.Format", "Greeter.Greet", "Format-Hello")
//   - contains edges: 1 file->class + 2 class->method + 1 file->method = 4
//   - 1 static_calls edge (Greeter.Greet -> Greeter.Format via ReceiverCalls)
//   - 2 imports edges (Foo, Bar) — the dot-source `./helpers.ps1`
//     MUST be dropped by `emitImportsEdges` via
//     `isRelativeImport` (architecture Section 6.1's
//     dot-source row).
//   - 0 package nodes for `./helpers.ps1` (a dropped import
//     must NOT mint an external package).
//
// Runs deterministically on every host (no pwsh dependency
// — uses a `stubPowerShellParser` injected via
// `WithParsers(...)`).
func TestPowerShellFixture_DispatcherEmitsExpectedNodesAndEdges(t *testing.T) {
	stub := &stubPowerShellParser{result: powerShellFixtureParseResult()}
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithParsers(stub))
	if _, err := d.EmitFile(context.Background(), makeEvent("scripts/hello.ps1", powershellFixture)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	classes := fw.nodesOf("class")
	if got, want := len(classes), 1; got != want {
		t.Errorf("class nodes = %d; want %d", got, want)
	}
	if len(classes) > 0 {
		if lang := attrString(t, classes[0].AttrsJSON, "language"); lang != "powershell" {
			t.Errorf("class node language attr = %q; want %q", lang, "powershell")
		}
	}

	methods := fw.nodesOf("method")
	if got, want := len(methods), 3; got != want {
		t.Errorf("method nodes = %d; want %d", got, want)
	}
	wantMethodSimpleNames := map[string]bool{
		"Greeter.Format": false,
		"Greeter.Greet":  false,
		"Format-Hello":   false,
	}
	for _, m := range methods {
		simple := lastSegmentAfterHash(m.CanonicalSignature)
		// Drop the trailing `(params)` suffix when present.
		if idx := strings.IndexByte(simple, '('); idx >= 0 {
			simple = simple[:idx]
		}
		if _, ok := wantMethodSimpleNames[simple]; ok {
			wantMethodSimpleNames[simple] = true
		}
	}
	for name, found := range wantMethodSimpleNames {
		if !found {
			t.Errorf("method node %q missing from emitted set", name)
		}
	}

	// Contains edges: file->class (1) + class->method (2) +
	// file->method (1 free fn) = 4.
	contains := fw.edgesOf("contains")
	if got, want := len(contains), 4; got != want {
		t.Errorf("contains edges = %d; want %d", got, want)
	}

	// static_calls: Greeter.Greet -> Greeter.Format (Pass 2b
	// receiver-qualified resolution).
	staticCalls := fw.edgesOf("static_calls")
	if got, want := len(staticCalls), 1; got != want {
		t.Errorf("static_calls edges = %d; want %d (Greeter.Greet -> Greeter.Format)", got, want)
	}

	// Imports edges: 2 (Foo + Bar). Dot-source dropped.
	importEdges := fw.edgesOf("imports")
	if got, want := len(importEdges), 2; got != want {
		t.Errorf("imports edges = %d; want %d (Foo, Bar; dot-source dropped)", got, want)
	}

	// External package nodes: 2 (Foo + Bar). Helpers.ps1
	// MUST NOT mint an external package per
	// `emitImportsEdges`'s `isRelativeImport` skip.
	pkgs := fw.nodesOf("package")
	if got, want := len(pkgs), 2; got != want {
		t.Errorf("package nodes = %d; want %d", got, want)
	}
	for _, p := range pkgs {
		if strings.Contains(p.CanonicalSignature, "helpers.ps1") {
			t.Errorf("found package node for dot-source ./helpers.ps1; emitImportsEdges should have dropped it (sig=%s)", p.CanonicalSignature)
		}
	}
}

// TestPowerShellFixture_DispatcherDropsDotSourceImport is the
// targeted regression test for iter-2 finding #2: dot-source
// suppression MUST be exercised through
// `emitImportsEdges`, not via a direct call to
// `isRelativeImport`. We feed the dispatcher a ParseResult
// whose only import is a dot-source `./helpers.ps1`. The
// dispatcher MUST emit zero `imports` edges AND zero
// `package` nodes for the dot-source target. A regression
// that wires PowerShell imports through a code path that
// bypasses `emitImportsEdges` (or that drops the
// `isRelativeImport` check) will fail this test.
func TestPowerShellFixture_DispatcherDropsDotSourceImport(t *testing.T) {
	stub := &stubPowerShellParser{result: ParseResult{
		Methods: []MethodDecl{{
			QualifiedName: "Bar",
			BodySource:    " Write-Host hi ",
			StartLine:     3,
			EndLine:       5,
		}},
		Imports: []Import{{
			Path:     "./helpers.ps1",
			Module:   "./helpers.ps1",
			Line:     1,
			LangMeta: map[string]any{"module_kind": "dot_source"},
		}},
	}}
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithParsers(stub))
	if _, err := d.EmitFile(context.Background(), makeEvent("scripts/runner.ps1", "ignored")); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	if got := len(fw.edgesOf("imports")); got != 0 {
		t.Errorf("imports edges = %d; want 0 (only import is dot-source ./helpers.ps1, dispatcher MUST drop)", got)
	}
	if got := len(fw.nodesOf("package")); got != 0 {
		t.Errorf("package nodes = %d; want 0 (dot-source must not mint an external package node)", got)
	}
}

// TestPowerShellFixture_DispatcherEndToEnd_WithRealPwsh is the
// integration check that the production
// `NewPowerShellParser()` path drives the same dispatcher
// pipeline correctly. Skipped on `pwsh`-less hosts; the stub-
// driven tests above provide the regression coverage that
// MUST run everywhere.
func TestPowerShellFixture_DispatcherEndToEnd_WithRealPwsh(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH")
	}
	parser := NewPowerShellParser()
	fw := newFakeWriter()
	d := NewDispatcher(fw, WithParsers(parser))
	if _, err := d.EmitFile(context.Background(), makeEvent("scripts/hello.ps1", powershellFixture)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	if got := len(fw.nodesOf("class")); got != 1 {
		t.Errorf("class nodes = %d; want 1 (end-to-end with real pwsh)", got)
	}
	if got := len(fw.nodesOf("method")); got != 3 {
		t.Errorf("method nodes = %d; want 3 (end-to-end with real pwsh)", got)
	}
	if got := len(fw.edgesOf("imports")); got != 2 {
		t.Errorf("imports edges = %d; want 2 (Foo + Bar; end-to-end with real pwsh)", got)
	}
	if got := len(fw.edgesOf("static_calls")); got != 1 {
		t.Errorf("static_calls edges = %d; want 1 (Greeter.Greet -> Greeter.Format end-to-end)", got)
	}
}
