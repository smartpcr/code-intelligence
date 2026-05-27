package parser

import (
	"context"
	"strings"
	"testing"
)

// edgeKey is a stable, comparable key for an emitted AstEdge
// so tests can assert presence without depending on edge_id
// generation order.
type edgeKey struct {
	Kind string
	From string
	To   string
}

func edgeSet(out *AstFile) map[edgeKey]struct{} {
	m := map[edgeKey]struct{}{}
	for _, e := range out.GetEdges() {
		k := edgeKey{
			Kind: e.GetKind(),
			From: e.GetFrom().GetId(),
			To:   e.GetTo().GetId(),
		}
		m[k] = struct{}{}
	}
	return m
}

func scopeByName(out *AstFile, kind ScopeKind, name string) *AstScope {
	for _, s := range out.GetScopes() {
		if s.GetScopeKind() == kind && s.GetName() == name {
			return s
		}
	}
	return nil
}

// TestGoParser_EdgesAndParenting addresses evaluator findings
// #3 (edges) and #4 (receiver-method parenting).
func TestGoParser_EdgesAndParenting(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "go", "sample.go")
	out, err := (&goParser{}).Parse(context.Background(), "sample.go", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	memSampler := scopeByName(out, ScopeKindClass, "MemorySampler")
	if memSampler == nil {
		t.Fatalf("did not find MemorySampler class scope")
	}
	if memSampler.GetAttrs()["go_type"] != "struct" {
		t.Errorf("MemorySampler.attrs[go_type] = %q; want struct", memSampler.GetAttrs()["go_type"])
	}

	sampleMethod := scopeByName(out, ScopeKindMethod, "Sample")
	closeMethod := scopeByName(out, ScopeKindMethod, "Close")
	if sampleMethod == nil {
		t.Fatalf("did not find Sample() method scope")
	}
	if closeMethod == nil {
		t.Fatalf("did not find Close() method scope")
	}
	if got := sampleMethod.GetParentScopeId(); got != memSampler.GetScopeId() {
		t.Errorf("Sample().parent_scope_id = %q; want MemorySampler(%q) [finding #4]", got, memSampler.GetScopeId())
	}
	if got := closeMethod.GetParentScopeId(); got != memSampler.GetScopeId() {
		t.Errorf("Close().parent_scope_id = %q; want MemorySampler(%q) [finding #4]", got, memSampler.GetScopeId())
	}

	newFn := scopeByName(out, ScopeKindMethod, "New")
	if newFn == nil {
		t.Fatalf("did not find package function New")
	}
	if newFn.GetParentScopeId() == memSampler.GetScopeId() {
		t.Errorf("New() must NOT be parented to MemorySampler; got parent=MemorySampler. New has no receiver.")
	}

	edges := edgeSet(out)
	fileID := ""
	for _, s := range out.GetScopes() {
		if s.GetScopeKind() == ScopeKindFile {
			fileID = s.GetScopeId()
			break
		}
	}
	if fileID == "" {
		t.Fatalf("no file scope emitted")
	}
	for _, imp := range []string{"context", "errors", "fmt"} {
		key := edgeKey{Kind: "imports", From: fileID, To: "qualified:" + imp}
		if _, ok := edges[key]; !ok {
			t.Errorf("missing AstEdge {imports, file, %s} [finding #3]; have %v", imp, edges)
		}
	}
}

// TestPythonParser_EdgesAndParenting addresses findings #3
// (edges) and locks down ABC-detected interface scope shape.
func TestPythonParser_EdgesAndParenting(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "python", "sample.py")
	out, err := (&pythonParser{}).Parse(context.Background(), "sample.py", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sampler := scopeByName(out, ScopeKindInterface, "Sampler")
	if sampler == nil {
		t.Fatalf("did not find Sampler interface scope (ABC detection)")
	}
	memSampler := scopeByName(out, ScopeKindClass, "MemorySampler")
	if memSampler == nil {
		t.Fatalf("did not find MemorySampler class scope")
	}

	// Methods must be parented to the class that declared them.
	memSample := findMethodInClass(out, memSampler.GetScopeId(), "sample")
	memClose := findMethodInClass(out, memSampler.GetScopeId(), "close")
	if memSample == nil {
		t.Errorf("MemorySampler.sample method not found, or not parented under MemorySampler")
	}
	if memClose == nil {
		t.Errorf("MemorySampler.close method not found, or not parented under MemorySampler")
	}

	// Sampler ABC methods must be parented under the Sampler scope, not file.
	samplerSample := findMethodInClass(out, sampler.GetScopeId(), "sample")
	if samplerSample == nil {
		t.Errorf("Sampler.sample (abstractmethod) not found under Sampler interface")
	}

	edges := edgeSet(out)
	// `class Sampler(ABC)` should emit an `extends`/`implements` edge.
	hasAbcEdge := false
	for k := range edges {
		if k.From == sampler.GetScopeId() && (k.Kind == "extends" || k.Kind == "implements") && strings.Contains(k.To, "ABC") {
			hasAbcEdge = true
			break
		}
	}
	if !hasAbcEdge {
		t.Errorf("expected Sampler -> ABC inheritance edge; have %v [finding #3]", edges)
	}

	// `from abc import ABC, abstractmethod` should emit imports edges.
	importEdgeCount := 0
	for k := range edges {
		if k.Kind == "imports" {
			importEdgeCount++
		}
	}
	if importEdgeCount == 0 {
		t.Errorf("expected at least one imports edge; have %v [finding #3]", edges)
	}
}

// TestTypeScriptParser_EdgesAndInterfaceMembers addresses
// findings #3 (edges) and #5 (interface semicolon method bug).
func TestTypeScriptParser_EdgesAndInterfaceMembers(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "typescript", "sample.ts")
	out, err := (&tsParser{}).Parse(context.Background(), "sample.ts", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sampler := scopeByName(out, ScopeKindInterface, "Sampler")
	if sampler == nil {
		t.Fatalf("did not find Sampler interface")
	}
	// Finding #5: both `sample()` and `close()` are
	// semicolon-terminated interface members. After the fix,
	// both must appear as method scopes parented under
	// Sampler -- not collapsed because the first one pushed
	// a phantom brace frame.
	samplerSample := findMethodInClass(out, sampler.GetScopeId(), "sample")
	samplerClose := findMethodInClass(out, sampler.GetScopeId(), "close")
	if samplerSample == nil {
		t.Errorf("Sampler.sample() method missing under interface [finding #5]")
	}
	if samplerClose == nil {
		t.Errorf("Sampler.close() method missing under interface [finding #5]")
	}

	mem := scopeByName(out, ScopeKindClass, "MemorySampler")
	if mem == nil {
		t.Fatalf("did not find MemorySampler class")
	}
	for _, want := range []string{"sample", "close"} {
		if findMethodInClass(out, mem.GetScopeId(), want) == nil {
			t.Errorf("MemorySampler.%s method missing; check method-parenting", want)
		}
	}

	edges := edgeSet(out)
	hasImpl := false
	for k := range edges {
		if k.From == mem.GetScopeId() && k.Kind == "implements" && strings.Contains(k.To, "Sampler") {
			hasImpl = true
			break
		}
	}
	if !hasImpl {
		t.Errorf("expected MemorySampler -> Sampler implements edge; have %v [finding #3]", edges)
	}

	hasImport := false
	for k := range edges {
		if k.Kind == "imports" && strings.Contains(k.To, "node:assert") {
			hasImport = true
			break
		}
	}
	if !hasImport {
		t.Errorf("expected imports edge for node:assert; have %v [finding #3]", edges)
	}
}

// TestJavaParser_EdgesAndInterfaceMembers addresses findings
// #3 (edges) and #5 (Java interface semicolon method bug).
func TestJavaParser_EdgesAndInterfaceMembers(t *testing.T) {
	t.Parallel()
	content := readFixture(t, "java", "Sample.java")
	out, err := (&javaParser{}).Parse(context.Background(), "Sample.java", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sample := scopeByName(out, ScopeKindInterface, "Sample")
	if sample == nil {
		t.Fatalf("did not find Sample interface")
	}
	for _, want := range []string{"sample", "close"} {
		if findMethodInClass(out, sample.GetScopeId(), want) == nil {
			t.Errorf("Sample.%s() (interface method) missing -- semicolon frames must not swallow siblings [finding #5]", want)
		}
	}

	mem := scopeByName(out, ScopeKindClass, "MemorySample")
	if mem == nil {
		t.Fatalf("did not find MemorySample class")
	}
	for _, want := range []string{"sample", "close"} {
		if findMethodInClass(out, mem.GetScopeId(), want) == nil {
			t.Errorf("MemorySample.%s() missing [finding #5]", want)
		}
	}

	edges := edgeSet(out)
	hasImpl := false
	for k := range edges {
		if k.From == mem.GetScopeId() && k.Kind == "implements" && strings.Contains(k.To, "Sample") {
			hasImpl = true
			break
		}
	}
	if !hasImpl {
		t.Errorf("expected MemorySample -> Sample implements edge; have %v [finding #3]", edges)
	}

	hasImport := false
	for k := range edges {
		if k.Kind == "imports" && (strings.Contains(k.To, "java.util.ArrayList") || strings.Contains(k.To, "java.util.List")) {
			hasImport = true
			break
		}
	}
	if !hasImport {
		t.Errorf("expected imports edge for java.util.*; have %v [finding #3]", edges)
	}
}

func findMethodInClass(out *AstFile, classID, name string) *AstScope {
	for _, s := range out.GetScopes() {
		if s.GetScopeKind() == ScopeKindMethod && s.GetName() == name && s.GetParentScopeId() == classID {
			return s
		}
	}
	return nil
}
