//go:build canonical_dispatcher

// V1-only dispatcher + emitter tests are gated behind
// `canonical_dispatcher` because they exercise the old
// `NewDispatcher(opts...)` / `WithParser(...)` / `EmitFile(string, []byte)`
// surface plus `NewEmitter` / `Pass2dOverrides`, all of which
// live in the gated V1 stub files (types.go / dispatcher.go /
// emitter.go / pass2d.go). The Stage 3.2 dispatcher landing
// workstream will retire these tests in favour of the
// canonical pinned scenarios in dispatcher_test.go.
package ast

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TestErrParserUnavailable — sentinel identity and wrapping
// ---------------------------------------------------------------------------

func TestErrParserUnavailable_Sentinel(t *testing.T) {
	wrapped := fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ErrParserUnavailable)
	if !errors.Is(wrapped, ErrParserUnavailable) {
		t.Fatal("errors.Is(wrapped, ErrParserUnavailable) returned false, want true")
	}
}

// ---------------------------------------------------------------------------
// TestDispatcher_SkipLog — ErrParserUnavailable produces ast.dispatch.skip
// ---------------------------------------------------------------------------

type testParser struct {
	err error
}

func (p *testParser) Parse(_ string, _ []byte) (ParseResult, error) {
	return ParseResult{}, p.err
}
func (p *testParser) Language() string     { return "test" }
func (p *testParser) Extensions() []string { return []string{".test"} }

type testLogger struct {
	events []string
}

func (l *testLogger) Log(event string, _ map[string]string) {
	l.events = append(l.events, event)
}

type testWriter struct {
	nodeCalls int
	edgeCalls int
}

func (w *testWriter) InsertNode(_ Node) error { w.nodeCalls++; return nil }
func (w *testWriter) InsertEdge(_ Edge) error { w.edgeCalls++; return nil }

func TestDispatcher_ErrParserUnavailable_SkipLog(t *testing.T) {
	logger := &testLogger{}
	writer := &testWriter{}

	d := NewDispatcher(
		WithParser(&testParser{
			err: fmt.Errorf("test: %w (reason=stub_missing)", ErrParserUnavailable),
		}),
		WithWriter(writer),
		WithLogger(logger),
	)

	result, err := d.EmitFile("file.test", []byte("content"))
	if err != nil {
		t.Fatalf("EmitFile returned error: %v", err)
	}
	if result != (EmitResult{}) {
		t.Fatalf("expected zero EmitResult, got %+v", result)
	}
	if writer.nodeCalls != 0 || writer.edgeCalls != 0 {
		t.Fatalf("writer should have zero calls, got nodes=%d edges=%d", writer.nodeCalls, writer.edgeCalls)
	}

	found := false
	for _, ev := range logger.events {
		if ev == "ast.dispatch.skip" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ast.dispatch.skip log event, got %v", logger.events)
	}
}

// ---------------------------------------------------------------------------
// TestPass2bMultimap — collision drops and pointer-only resolves
// ---------------------------------------------------------------------------

func TestPass2bMultimap_CollisionDrops(t *testing.T) {
	pr := ParseResult{
		Methods: []MethodDecl{
			{Name: "Bar", ClassName: "Foo"},
			{Name: "Bar", ClassName: "*Foo"},
			{Name: "Caller", ClassName: "Foo"},
		},
	}

	em := NewEmitter(pr)
	edges, callsRaw := em.Emit()

	// No static_calls edge for Bar (collision: set size > 1).
	for _, e := range edges {
		if e.Kind == "static_calls" && strings.Contains(e.Target, "Bar") {
			t.Fatalf("unexpected static_calls edge for Bar: %+v", e)
		}
	}

	// Bar should persist on calls_raw.
	found := false
	for _, raw := range callsRaw {
		if strings.Contains(raw, "Bar") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Bar not found on calls_raw: %v", callsRaw)
	}
}

func TestPass2bMultimap_PointerOnlyResolves(t *testing.T) {
	pr := ParseResult{
		Methods: []MethodDecl{
			{
				Name:      "Bar",
				ClassName: "*Foo",
				ReceiverAliases: map[string]string{
					"Foo.Bar": "*Foo.Bar",
				},
			},
			{Name: "Sibling", ClassName: "Foo"},
		},
	}

	em := NewEmitter(pr)
	edges, _ := em.Emit()

	var matching []Edge
	for _, e := range edges {
		if e.Kind == "static_calls" && strings.Contains(e.Target, "*Foo.Bar") {
			matching = append(matching, e)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("expected 1 static_calls edge to *Foo.Bar, got %d: %+v", len(matching), matching)
	}
	if !strings.Contains(matching[0].Source, "Sibling") {
		t.Fatalf("expected source to contain Sibling, got %q", matching[0].Source)
	}
}

// ---------------------------------------------------------------------------
// TestPass2dOverrides — overrides edge insertion and cross-file miss
// ---------------------------------------------------------------------------

func TestPass2dOverrides_InsertsEdge(t *testing.T) {
	pr := ParseResult{
		Methods: []MethodDecl{
			{Name: "greet", ClassName: "Greeter", LangMeta: nil},
			{Name: "greet", ClassName: "GreeterImpl", LangMeta: map[string]string{"trait": "Greeter"}},
		},
	}
	methodNodeID := map[string]string{
		"Greeter.greet":     "Greeter.greet",
		"GreeterImpl.greet": "GreeterImpl.greet",
	}

	edges, _ := Pass2dOverrides(pr, methodNodeID)

	if len(edges) != 1 {
		t.Fatalf("expected 1 overrides edge, got %d: %+v", len(edges), edges)
	}
	if edges[0].Kind != "overrides" {
		t.Fatalf("expected kind=overrides, got %q", edges[0].Kind)
	}
	if !strings.Contains(edges[0].Source, "GreeterImpl") {
		t.Fatalf("expected source to contain GreeterImpl, got %q", edges[0].Source)
	}
	if !strings.Contains(edges[0].Target, "Greeter.greet") {
		t.Fatalf("expected target to contain Greeter.greet, got %q", edges[0].Target)
	}
}

func TestPass2dOverrides_CrossFileMissDrops(t *testing.T) {
	pr := ParseResult{
		Methods: []MethodDecl{
			{Name: "greet", ClassName: "GreeterImpl", LangMeta: map[string]string{"trait": "Greeter"}},
		},
	}
	// No Greeter.greet in the methodNodeID map.
	methodNodeID := map[string]string{
		"GreeterImpl.greet": "GreeterImpl.greet",
	}

	edges, attrsJSON := Pass2dOverrides(pr, methodNodeID)

	for _, e := range edges {
		if e.Kind == "overrides" {
			t.Fatalf("unexpected overrides edge: %+v", e)
		}
	}

	// Trait should be preserved in attrs_json.
	raw, ok := attrsJSON["GreeterImpl.greet"]
	if !ok {
		t.Fatalf("expected attrs_json entry for GreeterImpl.greet, got %v", attrsJSON)
	}
	if !strings.Contains(raw, "Greeter") {
		t.Fatalf("expected attrs_json to contain Greeter, got %q", raw)
	}
}

// ---------------------------------------------------------------------------
// TestLangMeta — zero-value LangMeta and ReceiverAliases
// ---------------------------------------------------------------------------

func TestLangMeta_NilDefault(t *testing.T) {
	m := MethodDecl{}
	if m.LangMeta != nil {
		t.Fatal("zero-value MethodDecl.LangMeta should be nil")
	}
	if m.ReceiverAliases != nil {
		t.Fatal("zero-value MethodDecl.ReceiverAliases should be nil")
	}
	// Iterating nil map should yield zero elements.
	count := 0
	for range m.ReceiverAliases {
		count++
	}
	if count != 0 {
		t.Fatalf("expected 0 elements, got %d", count)
	}
}
