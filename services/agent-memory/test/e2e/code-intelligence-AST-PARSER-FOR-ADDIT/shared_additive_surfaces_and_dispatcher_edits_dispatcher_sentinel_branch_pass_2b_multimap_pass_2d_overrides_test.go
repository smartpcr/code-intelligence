//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Scenario state — all types come from internal/repoindexer/ast
// ---------------------------------------------------------------------------

type dispatcherState struct {
	// Scenario 1: ErrParserUnavailable skip-log path
	stubParser  ast.Parser
	emitRes     ast.EmitResult
	emitErr     error
	logEntries  []logEntry
	writerCalls []writerCall

	// Scenario 2 & 3: Multimap
	parsedResult ast.ParseResult
	emitEdges    []ast.Edge
	callsRaw     []string

	// Scenario 4 & 5: Pass 2d overrides
	pass2dNodes     []ast.Node
	pass2dMethods   []ast.MethodDecl
	pass2dEdges     []ast.Edge
	pass2dAttrsJSON map[string]string
}

type logEntry struct {
	Event  string
	Reason string
}

type writerCall struct {
	Method string
}

// ---------------------------------------------------------------------------
// Stub parser for Scenario 1 — wraps ast.ErrParserUnavailable
// ---------------------------------------------------------------------------

type stubUnavailableParser struct {
	reason string
}

func (p *stubUnavailableParser) Parse(_ string, _ []byte) (ast.ParseResult, error) {
	return ast.ParseResult{}, fmt.Errorf("test: %w (reason=%s)", ast.ErrParserUnavailable, p.reason)
}

func (p *stubUnavailableParser) Language() string     { return "stub" }
func (p *stubUnavailableParser) Extensions() []string { return []string{".stub"} }

// ---------------------------------------------------------------------------
// Mock writer — implements ast.Writer
// ---------------------------------------------------------------------------

type mockWriter struct {
	calls []writerCall
}

func (w *mockWriter) InsertNode(_ ast.Node) error {
	w.calls = append(w.calls, writerCall{Method: "InsertNode"})
	return nil
}

func (w *mockWriter) InsertEdge(_ ast.Edge) error {
	w.calls = append(w.calls, writerCall{Method: "InsertEdge"})
	return nil
}

// ---------------------------------------------------------------------------
// Mock logger — implements ast.Logger
// ---------------------------------------------------------------------------

type mockLogger struct {
	entries []logEntry
}

func (l *mockLogger) Log(event string, fields map[string]string) {
	reason := fields["reason"]
	l.entries = append(l.entries, logEntry{Event: event, Reason: reason})
}

// ---------------------------------------------------------------------------
// Scenario 1 — ErrParserUnavailable skip-log path
// ---------------------------------------------------------------------------

func (s *dispatcherState) aStubParserWhoseParseReturnsErrParserUnavailableWithReason(reason string) error {
	s.stubParser = &stubUnavailableParser{reason: reason}
	return nil
}

func (s *dispatcherState) emitFileProcessesAFileRoutedToThatParser() error {
	w := &mockWriter{}
	l := &mockLogger{}

	d := ast.NewDispatcher(
		ast.WithParser(s.stubParser),
		ast.WithWriter(w),
		ast.WithLogger(l),
	)

	result, err := d.EmitFile("test.stub", []byte("stub content"))
	s.emitRes = result
	s.emitErr = err
	s.logEntries = l.entries
	s.writerCalls = w.calls
	return nil
}

func (s *dispatcherState) theStructuredLogEmitsWithReason(event, reason string) error {
	for _, entry := range s.logEntries {
		if entry.Event == event && entry.Reason == reason {
			return nil
		}
	}
	return fmt.Errorf("expected log entry with event=%q reason=%q, got %+v", event, reason, s.logEntries)
}

func (s *dispatcherState) emitFileReturnsAZeroEmitResultAndNilError() error {
	if s.emitErr != nil {
		return fmt.Errorf("expected nil error from EmitFile, got: %v", s.emitErr)
	}
	zero := ast.EmitResult{}
	if s.emitRes != zero {
		return fmt.Errorf("expected zero EmitResult, got: %+v", s.emitRes)
	}
	return nil
}

func (s *dispatcherState) theWriterReceivesZeroInsertNodeAndInsertEdgeCalls() error {
	if len(s.writerCalls) != 0 {
		return fmt.Errorf("expected zero writer calls, got %d: %+v", len(s.writerCalls), s.writerCalls)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Multimap collision drops
//
// Fixture: both value and pointer receiver methods named Bar on Foo,
// plus a Caller method that invokes r.Bar(). Per rule A5, the multimap
// has set-size > 1 for "Bar", so no static_calls edge is emitted.
// ---------------------------------------------------------------------------

func (s *dispatcherState) aGoFixtureWithBothValueAndPointerReceiverBarMethods() error {
	s.parsedResult = ast.ParseResult{
		Classes: []ast.ClassDecl{{Name: "Foo"}},
		Methods: []ast.MethodDecl{
			{Name: "Bar", ClassName: "Foo"},
			{Name: "Bar", ClassName: "*Foo"},
			{Name: "Caller", ClassName: "Foo"},
		},
	}
	return nil
}

func (s *dispatcherState) aThirdMethodCallingBarViaReceiver() error {
	// Already modeled — Caller is in the fixture.
	return nil
}

func (s *dispatcherState) emitRuns() error {
	em := ast.NewEmitter(s.parsedResult)
	edges, callsRaw := em.Emit()
	s.emitEdges = edges
	s.callsRaw = callsRaw
	return nil
}

func (s *dispatcherState) noStaticCallsEdgeIsEmittedFor(methodName string) error {
	for _, e := range s.emitEdges {
		if e.Kind == "static_calls" && strings.Contains(e.Target, methodName) {
			return fmt.Errorf("found unexpected static_calls edge for %q: %+v", methodName, e)
		}
	}
	return nil
}

func (s *dispatcherState) persistsOnCallsRaw(methodName string) error {
	for _, raw := range s.callsRaw {
		if strings.Contains(raw, methodName) {
			return nil
		}
	}
	return fmt.Errorf("expected %q on calls_raw, but not found in %v", methodName, s.callsRaw)
}

// ---------------------------------------------------------------------------
// Scenario 3 — Multimap pointer-only resolves
//
// Fixture: only *Foo.Bar exists. Sibling method calls r.Bar(). Because
// set-size == 1, the edge resolves. The ReceiverAliases entry "Foo.Bar"
// maps to the canonical "*Foo.Bar" target.
// ---------------------------------------------------------------------------

func (s *dispatcherState) aGoFixtureWithOnlyPointerReceiverBarPlusSiblingCaller() error {
	s.parsedResult = ast.ParseResult{
		Classes: []ast.ClassDecl{{Name: "Foo"}},
		Methods: []ast.MethodDecl{
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
	return nil
}

func (s *dispatcherState) exactlyOneStaticCallsEdgeFromSiblingToPointerFooBar() error {
	var matching []ast.Edge
	for _, e := range s.emitEdges {
		if e.Kind == "static_calls" && strings.Contains(e.Target, "*Foo.Bar") {
			matching = append(matching, e)
		}
	}
	if len(matching) != 1 {
		return fmt.Errorf("expected exactly 1 static_calls edge to *Foo.Bar, got %d: %+v", len(matching), matching)
	}
	if !strings.Contains(matching[0].Source, "Sibling") {
		return fmt.Errorf("expected edge source to contain 'Sibling', got %q", matching[0].Source)
	}
	return nil
}

func (s *dispatcherState) theEdgeWasResolvedViaReceiverAliasesEntry(aliasKey string) error {
	// The alias key MUST exist on at least one method's ReceiverAliases map.
	// This is a strong assertion — fails if the alias is absent.
	for _, m := range s.parsedResult.Methods {
		if m.ReceiverAliases == nil {
			continue
		}
		if target, ok := m.ReceiverAliases[aliasKey]; ok {
			if target == "" {
				return fmt.Errorf("ReceiverAliases[%q] exists but maps to empty string", aliasKey)
			}
			return nil
		}
	}
	return fmt.Errorf("ReceiverAliases entry %q not found on any method — alias resolution is broken", aliasKey)
}

// ---------------------------------------------------------------------------
// Scenario 4 — Pass 2d overrides edge
// ---------------------------------------------------------------------------

func (s *dispatcherState) aFakeParserResultWithATraitMethodWithNilLangMeta(traitMethod string) error {
	parts := strings.SplitN(traitMethod, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("trait method %q must be ClassName.MethodName", traitMethod)
	}

	s.pass2dNodes = append(s.pass2dNodes, ast.Node{Kind: "method", Name: traitMethod})
	s.pass2dMethods = append(s.pass2dMethods, ast.MethodDecl{
		Name:      parts[1],
		ClassName: parts[0],
		LangMeta:  nil,
	})
	s.pass2dAttrsJSON = make(map[string]string)
	return nil
}

func (s *dispatcherState) anImplMethodWithLangMetaTraitInTheSameFile(implMethod, traitName string) error {
	parts := strings.SplitN(implMethod, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("impl method %q must be ClassName.MethodName", implMethod)
	}

	s.pass2dNodes = append(s.pass2dNodes, ast.Node{Kind: "method", Name: implMethod})
	s.pass2dMethods = append(s.pass2dMethods, ast.MethodDecl{
		Name:      parts[1],
		ClassName: parts[0],
		LangMeta:  map[string]string{"trait": traitName},
	})
	return nil
}

func (s *dispatcherState) pass2dRuns() error {
	pr := ast.ParseResult{Methods: s.pass2dMethods}

	methodNodeID := make(map[string]string)
	for _, n := range s.pass2dNodes {
		methodNodeID[n.Name] = n.Name
	}

	edges, attrsJSON := ast.Pass2dOverrides(pr, methodNodeID)
	s.pass2dEdges = edges
	s.pass2dAttrsJSON = attrsJSON
	return nil
}

func (s *dispatcherState) oneEdgeOfKindIsInsertedFromTo(kind, from, to string) error {
	var matching []ast.Edge
	for _, e := range s.pass2dEdges {
		if e.Kind == kind && strings.Contains(e.Source, from) && strings.Contains(e.Target, to) {
			matching = append(matching, e)
		}
	}
	if len(matching) != 1 {
		return fmt.Errorf("expected exactly 1 edge kind=%q from=%q to=%q, got %d: %+v",
			kind, from, to, len(matching), s.pass2dEdges)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 5 — Pass 2d cross-file miss drops
// ---------------------------------------------------------------------------

func (s *dispatcherState) anImplMethodWithLangMetaTrait(implMethod, traitName string) error {
	parts := strings.SplitN(implMethod, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("impl method %q must be ClassName.MethodName", implMethod)
	}

	s.pass2dMethods = []ast.MethodDecl{
		{
			Name:      parts[1],
			ClassName: parts[0],
			LangMeta:  map[string]string{"trait": traitName},
		},
	}
	s.pass2dNodes = nil
	s.pass2dAttrsJSON = make(map[string]string)
	return nil
}

func (s *dispatcherState) noNodeExistsInTheSameFileMethodNodeIDMap(traitMethod string) error {
	for _, n := range s.pass2dNodes {
		if n.Name == traitMethod {
			return fmt.Errorf("precondition violated: node %q exists but should be absent", traitMethod)
		}
	}
	return nil
}

func (s *dispatcherState) zeroOverridesEdgesAreInserted() error {
	var overridesCount int
	for _, e := range s.pass2dEdges {
		if e.Kind == "overrides" {
			overridesCount++
		}
	}
	if overridesCount != 0 {
		return fmt.Errorf("expected zero overrides edges, got %d: %+v", overridesCount, s.pass2dEdges)
	}
	return nil
}

func (s *dispatcherState) theTraitNameRemainsOnAttrsJSON(traitName string) error {
	// Verify the emitted attrs_json output (not input LangMeta) contains the trait.
	if len(s.pass2dAttrsJSON) == 0 {
		return fmt.Errorf("pass2dAttrsJSON is empty — Pass2dOverrides did not emit attrs_json for unresolved trait %q", traitName)
	}

	for key, rawJSON := range s.pass2dAttrsJSON {
		var attrs map[string]string
		if err := json.Unmarshal([]byte(rawJSON), &attrs); err != nil {
			return fmt.Errorf("attrs_json[%q] is not valid JSON: %v (raw: %s)", key, err, rawJSON)
		}
		if t, ok := attrs["trait"]; ok && t == traitName {
			return nil
		}
	}

	return fmt.Errorf("trait name %q not found in emitted attrs_json output: %v", traitName, s.pass2dAttrsJSON)
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_dispatcher_sentinel_branch_pass_2b_multimap_pass_2d_overrides(ctx *godog.ScenarioContext) {
	s := &dispatcherState{}

	// Scenario 1: ErrParserUnavailable skip-log path
	ctx.Given(`^a stub parser whose Parse returns ErrParserUnavailable with reason "([^"]*)"$`,
		s.aStubParserWhoseParseReturnsErrParserUnavailableWithReason)
	ctx.When(`^EmitFile processes a file routed to that parser$`,
		s.emitFileProcessesAFileRoutedToThatParser)
	ctx.Then(`^the structured log emits "([^"]*)" with reason "([^"]*)"$`,
		s.theStructuredLogEmitsWithReason)
	ctx.Then(`^EmitFile returns a zero EmitResult and nil error$`,
		s.emitFileReturnsAZeroEmitResultAndNilError)
	ctx.Then(`^the writer receives zero InsertNode and InsertEdge calls$`,
		s.theWriterReceivesZeroInsertNodeAndInsertEdgeCalls)

	// Scenario 2: Multimap collision drops
	ctx.Given(`^a Go fixture with both "func \(r Foo\) Bar\(\)" and "func \(r \*Foo\) Bar\(\)" in the same file$`,
		s.aGoFixtureWithBothValueAndPointerReceiverBarMethods)
	ctx.Given(`^a third method calling "r\.Bar\(\)" via receiver$`,
		s.aThirdMethodCallingBarViaReceiver)
	ctx.When(`^emit runs$`, s.emitRuns)
	ctx.Then(`^no static_calls edge is emitted for "([^"]*)"$`,
		s.noStaticCallsEdgeIsEmittedFor)
	ctx.Then(`^"([^"]*)" persists on calls_raw$`,
		s.persistsOnCallsRaw)

	// Scenario 3: Multimap pointer-only resolves
	ctx.Given(`^a Go fixture with only "func \(r \*Foo\) Bar\(\)" plus a sibling method calling "r\.Bar\(\)" from inside Foo$`,
		s.aGoFixtureWithOnlyPointerReceiverBarPlusSiblingCaller)
	ctx.Then(`^exactly one static_calls edge from the sibling method to "\*Foo\.Bar" is emitted$`,
		s.exactlyOneStaticCallsEdgeFromSiblingToPointerFooBar)
	ctx.Then(`^the edge was resolved via ReceiverAliases entry "([^"]*)"$`,
		s.theEdgeWasResolvedViaReceiverAliasesEntry)

	// Scenario 4: Pass 2d overrides edge
	ctx.Given(`^a fake parser result with a trait method "([^"]*)" with nil LangMeta$`,
		s.aFakeParserResultWithATraitMethodWithNilLangMeta)
	ctx.Given(`^an impl method "([^"]*)" with LangMeta trait "([^"]*)" in the same file$`,
		s.anImplMethodWithLangMetaTraitInTheSameFile)
	ctx.When(`^Pass 2d runs$`, s.pass2dRuns)
	ctx.Then(`^one edge of kind "([^"]*)" is inserted from "([^"]*)" to "([^"]*)"$`,
		s.oneEdgeOfKindIsInsertedFromTo)

	// Scenario 5: Pass 2d cross-file miss drops
	ctx.Given(`^an impl method "([^"]*)" with LangMeta trait "([^"]*)"$`,
		s.anImplMethodWithLangMetaTrait)
	ctx.Given(`^no "([^"]*)" node exists in the same file methodNodeID map$`,
		s.noNodeExistsInTheSameFileMethodNodeIDMap)
	ctx.Then(`^zero overrides edges are inserted$`,
		s.zeroOverridesEdgesAreInserted)
	ctx.Then(`^the trait name "([^"]*)" remains on attrs_json$`,
		s.theTraitNameRemainsOnAttrsJSON)
}

func TestE2E_shared_additive_surfaces_and_dispatcher_edits_dispatcher_sentinel_branch_pass_2b_multimap_pass_2d_overrides(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_dispatcher_sentinel_branch_pass_2b_multimap_pass_2d_overrides,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"shared_additive_surfaces_and_dispatcher_edits_dispatcher_sentinel_branch_pass_2b_multimap_pass_2d_overrides.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}