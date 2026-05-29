//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Scenario 1 helpers — spy Writer for capturing EmitFile output
// ---------------------------------------------------------------------------

// spyWriter records all nodes and edges emitted by the Dispatcher.
type spyWriter struct {
	nodes []ast.Node
	edges []ast.Edge
}

func (w *spyWriter) InsertNode(n ast.Node) error {
	w.nodes = append(w.nodes, n)
	return nil
}

func (w *spyWriter) InsertEdge(e ast.Edge) error {
	w.edges = append(w.edges, e)
	return nil
}

func (w *spyWriter) nodeCounts() map[string]int {
	counts := make(map[string]int)
	for _, n := range w.nodes {
		counts[n.Kind]++
	}
	return counts
}

func (w *spyWriter) edgeCounts() map[string]int {
	counts := make(map[string]int)
	for _, e := range w.edges {
		counts[e.Kind]++
	}
	return counts
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type rustFixtureState struct {
	// Scenario 1: fixture node+edge count via real Dispatcher.EmitFile
	fixtureSrc string
	spy        *spyWriter

	// Scenarios 2 & 3: Pass 2d overrides (direct call to ast.Pass2dOverrides)
	pass2dPR      ast.ParseResult
	pass2dNodeMap map[string]string
	pass2dEdges   []ast.Edge
	pass2dAttrs   map[string]string
}

// ---------------------------------------------------------------------------
// Scenario 1 — Rust fixture node + edge count
// ---------------------------------------------------------------------------

func (s *rustFixtureState) theRustFixtureSource(src *godog.DocString) error {
	s.fixtureSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *rustFixtureState) emitFileRunsUnderCGOOn() error {
	s.spy = &spyWriter{}
	d := ast.NewDispatcher(
		ast.WithParser(ast.NewRustParser()),
		ast.WithWriter(s.spy),
	)
	_, err := d.EmitFile("fixture.rs", []byte(s.fixtureSrc))
	if err != nil {
		if errors.Is(err, ast.ErrParserUnavailable) {
			return fmt.Errorf("Rust tree-sitter parser requires CGO_ENABLED=1: %w", err)
		}
		return err
	}
	return nil
}

func (s *rustFixtureState) nClassNodesAreEmitted(expected int) error {
	actual := s.spy.nodeCounts()["class"]
	if actual != expected {
		return fmt.Errorf("expected %d class nodes, got %d (all: %v)",
			expected, actual, s.spy.nodeCounts())
	}
	return nil
}

func (s *rustFixtureState) nMethodNodesAreEmitted(expected int) error {
	actual := s.spy.nodeCounts()["method"]
	if actual != expected {
		return fmt.Errorf("expected %d method nodes, got %d (all: %v)",
			expected, actual, s.spy.nodeCounts())
	}
	return nil
}

func (s *rustFixtureState) nPackageNodesAreEmitted(expected int) error {
	actual := s.spy.nodeCounts()["package"]
	if actual != expected {
		return fmt.Errorf("expected %d package nodes, got %d (all: %v)",
			expected, actual, s.spy.nodeCounts())
	}
	return nil
}

func (s *rustFixtureState) nEdgesOfKindAreEmitted(expected int, kind string) error {
	actual := s.spy.edgeCounts()[kind]
	if actual != expected {
		return fmt.Errorf("expected %d %s edges, got %d (all: %v)",
			expected, kind, actual, s.spy.edgeCounts())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Pass 2d overrides same-file emission
// ---------------------------------------------------------------------------

func (s *rustFixtureState) aFakeParserResultWithATraitMethodWithNilLangMeta(traitMethod string) error {
	parts := strings.SplitN(traitMethod, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("trait method %q must be ClassName.MethodName", traitMethod)
	}

	s.pass2dPR = ast.ParseResult{
		Methods: []ast.MethodDecl{
			{Name: parts[1], ClassName: parts[0], LangMeta: nil},
		},
	}
	s.pass2dNodeMap = map[string]string{
		traitMethod: traitMethod,
	}
	return nil
}

func (s *rustFixtureState) anImplMethodWithLangMetaTraitInTheSameFile(implMethod, traitName string) error {
	parts := strings.SplitN(implMethod, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("impl method %q must be ClassName.MethodName", implMethod)
	}

	s.pass2dPR.Methods = append(s.pass2dPR.Methods, ast.MethodDecl{
		Name:      parts[1],
		ClassName: parts[0],
		LangMeta:  map[string]string{"trait": traitName},
	})
	s.pass2dNodeMap[implMethod] = implMethod
	return nil
}

func (s *rustFixtureState) pass2dRuns() error {
	s.pass2dEdges, s.pass2dAttrs = ast.Pass2dOverrides(s.pass2dPR, s.pass2dNodeMap)
	return nil
}

func (s *rustFixtureState) exactlyOneEdgeOfKindFromTo(kind, from, to string) error {
	var matching []ast.Edge
	for _, e := range s.pass2dEdges {
		if e.Kind == kind && e.Source == from && e.Target == to {
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
// Scenario 3 — Cross-file overrides miss is silent
// ---------------------------------------------------------------------------

func (s *rustFixtureState) anImplMethodWithLangMetaTrait(implMethod, traitName string) error {
	parts := strings.SplitN(implMethod, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("impl method %q must be ClassName.MethodName", implMethod)
	}

	s.pass2dPR = ast.ParseResult{
		Methods: []ast.MethodDecl{
			{
				Name:      parts[1],
				ClassName: parts[0],
				LangMeta:  map[string]string{"trait": traitName},
			},
		},
	}
	// impl method exists in the file's methodNodeID, but the trait method does not
	s.pass2dNodeMap = map[string]string{
		implMethod: implMethod,
	}
	return nil
}

func (s *rustFixtureState) noNodeExistsInTheSameFileMethodNodeIDMap(traitMethod string) error {
	if _, exists := s.pass2dNodeMap[traitMethod]; exists {
		return fmt.Errorf("precondition violated: node %q exists but should be absent", traitMethod)
	}
	return nil
}

func (s *rustFixtureState) zeroOverridesEdgesAreEmitted() error {
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

func (s *rustFixtureState) attrsJSONForContainsTrait(implMethod, traitName string) error {
	rawJSON, ok := s.pass2dAttrs[implMethod]
	if !ok {
		return fmt.Errorf("no attrs_json entry for %q; map: %v", implMethod, s.pass2dAttrs)
	}
	var attrs map[string]string
	if err := json.Unmarshal([]byte(rawJSON), &attrs); err != nil {
		return fmt.Errorf("attrs_json[%q] is not valid JSON: %v (raw: %s)", implMethod, err, rawJSON)
	}
	if t, ok := attrs["trait"]; !ok || t != traitName {
		return fmt.Errorf("expected trait=%q in attrs_json[%q], got %v", traitName, implMethod, attrs)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_rust_parser_rust_fixture_test_including_pass_2d_overrides(ctx *godog.ScenarioContext) {
	s := &rustFixtureState{}

	// Scenario 1: Rust fixture node + edge count
	ctx.Given(`^the Rust fixture source:$`, s.theRustFixtureSource)
	ctx.When(`^EmitFile runs under CGO on$`, s.emitFileRunsUnderCGOOn)
	ctx.Then(`^(\d+) class nodes are emitted$`, s.nClassNodesAreEmitted)
	ctx.Then(`^(\d+) method nodes are emitted$`, s.nMethodNodesAreEmitted)
	ctx.Then(`^(\d+) package node(?:s)? (?:is|are) emitted$`, s.nPackageNodesAreEmitted)
	ctx.Then(`^(\d+) implements edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "implements")
	})
	ctx.Then(`^(\d+) static_calls edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "static_calls")
	})
	ctx.Then(`^(\d+) imports edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "imports")
	})
	ctx.Then(`^(\d+) overrides edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "overrides")
	})

	// Scenario 2: Pass 2d overrides same-file emission
	ctx.Given(`^a fake parser result with a trait method "([^"]*)" with nil LangMeta$`,
		s.aFakeParserResultWithATraitMethodWithNilLangMeta)
	ctx.Given(`^an impl method "([^"]*)" with LangMeta trait "([^"]*)" in the same file$`,
		s.anImplMethodWithLangMetaTraitInTheSameFile)
	ctx.When(`^Pass 2d runs$`, s.pass2dRuns)
	ctx.Then(`^exactly one edge of kind "([^"]*)" from "([^"]*)" to "([^"]*)" is emitted$`,
		s.exactlyOneEdgeOfKindFromTo)

	// Scenario 3: Cross-file overrides miss is silent
	ctx.Given(`^an impl method "([^"]*)" with LangMeta trait "([^"]*)"$`,
		s.anImplMethodWithLangMetaTrait)
	ctx.Given(`^no "([^"]*)" node exists in the same file methodNodeID map$`,
		s.noNodeExistsInTheSameFileMethodNodeIDMap)
	ctx.Then(`^zero overrides edges are emitted$`,
		s.zeroOverridesEdgesAreEmitted)
	ctx.Then(`^attrs_json for "([^"]*)" contains trait "([^"]*)"$`,
		s.attrsJSONForContainsTrait)
}

func TestE2E_rust_parser_rust_fixture_test_including_pass_2d_overrides(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_rust_parser_rust_fixture_test_including_pass_2d_overrides,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"rust_parser_rust_fixture_test_including_pass_2d_overrides.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}