//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// stubLangParser implements ast.Parser for languages whose tree-sitter
// parsers have not yet landed (C, C++, C#, Rust, PowerShell). Returns
// an empty ParseResult so the dispatcher routes successfully.
// ---------------------------------------------------------------------------

type stubLangParser struct {
	lang string
	exts []string
}

func (p *stubLangParser) Parse(_ string, _ []byte) (ast.ParseResult, error) {
	return ast.ParseResult{}, nil
}

func (p *stubLangParser) Language() string     { return p.lang }
func (p *stubLangParser) Extensions() []string { return p.exts }

// ---------------------------------------------------------------------------
// fixtureGoParser returns a pre-built ParseResult for known fixture files.
// This lets the multimap scenarios route through the real
// Dispatcher.EmitFile → Parser.Parse → Emitter.Emit → Writer pipeline
// end-to-end while the actual tree-sitter Go parser is still a stub.
// ---------------------------------------------------------------------------

type fixtureGoParser struct {
	fixtures map[string]ast.ParseResult
}

func (p *fixtureGoParser) Parse(filename string, _ []byte) (ast.ParseResult, error) {
	if pr, ok := p.fixtures[filename]; ok {
		return pr, nil
	}
	return ast.ParseResult{}, nil
}

func (p *fixtureGoParser) Language() string     { return "go" }
func (p *fixtureGoParser) Extensions() []string { return []string{".go"} }

// ---------------------------------------------------------------------------
// unavailableParser wraps ast.ErrParserUnavailable sentinel for scenario 6.
// ---------------------------------------------------------------------------

type unavailableParser struct {
	lang   string
	exts   []string
	reason string
}

func (p *unavailableParser) Parse(_ string, _ []byte) (ast.ParseResult, error) {
	return ast.ParseResult{}, fmt.Errorf("unavailable: %w (reason=%s)", ast.ErrParserUnavailable, p.reason)
}

func (p *unavailableParser) Language() string     { return p.lang }
func (p *unavailableParser) Extensions() []string { return p.exts }

// ---------------------------------------------------------------------------
// recordingWriter captures nodes and edges emitted by the dispatcher.
// ---------------------------------------------------------------------------

type recordingWriter struct {
	nodes []ast.Node
	edges []ast.Edge
}

func (w *recordingWriter) InsertNode(n ast.Node) error {
	w.nodes = append(w.nodes, n)
	return nil
}

func (w *recordingWriter) InsertEdge(e ast.Edge) error {
	w.edges = append(w.edges, e)
	return nil
}

// ---------------------------------------------------------------------------
// Mock logger — captures structured log events from the dispatcher.
// ---------------------------------------------------------------------------

type mockLogger struct {
	entries []logEntry
}

type logEntry struct {
	Event  string
	Reason string
}

func (l *mockLogger) Log(event string, fields map[string]string) {
	l.entries = append(l.entries, logEntry{Event: event, Reason: fields["reason"]})
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type crossLangState struct {
	// Shared dispatcher
	dispatcher *ast.Dispatcher

	// Extension routing (scenarios 1-3) — uses SelectParser
	routingResult map[string]string // ext → language
	routingErr    map[string]error
	selectedLang  string // single-extension SelectParser result

	// Duplicate registration (scenario 3)
	parserA ast.Parser
	parserB ast.Parser

	// Multimap (scenarios 4-5) — uses EmitFile end-to-end
	fixturePath string
	fixtureSrc  []byte
	recorder    *recordingWriter
	emitRes     ast.EmitResult
	emitErr     error

	// ErrParserUnavailable (scenario 6)
	logEntries []logEntry

	// First-class attr key (scenario 7) — uses real ast.MergeLangMeta
	parserLangMeta map[string]string
	mergedAttrs    map[string]string
}

// ---------------------------------------------------------------------------
// Scenario 1: Every new extension routes to its parser
//
// Builds a Dispatcher with real ast.NewGoParser() for .go and stub parsers
// for languages whose tree-sitter bindings have not yet landed.
// Calls d.SelectParser() — the real routing function — for each extension.
// ---------------------------------------------------------------------------

func (s *crossLangState) theDispatcherIsConfiguredWithParsersForAllNewLanguages() error {
	s.dispatcher = ast.NewDispatcher(
		ast.WithWriter(&recordingWriter{}),
		ast.WithParser(ast.NewGoParser()),
		ast.WithParser(&stubLangParser{lang: "c", exts: []string{".c", ".h"}}),
		ast.WithParser(&stubLangParser{lang: "cpp", exts: []string{".cpp", ".cxx"}}),
		ast.WithParser(&stubLangParser{lang: "csharp", exts: []string{".cs"}}),
		ast.WithParser(&stubLangParser{lang: "rust", exts: []string{".rs"}}),
		ast.WithParser(&stubLangParser{lang: "powershell", exts: []string{".ps1", ".psm1", ".psd1"}}),
	)
	return nil
}

func (s *crossLangState) selectParserRunsForEachExtension(extList string) error {
	exts := parseQuotedList(extList)
	s.routingResult = make(map[string]string)
	s.routingErr = make(map[string]error)

	for _, ext := range exts {
		p := s.dispatcher.SelectParser("test"+ext, nil)
		if p == nil {
			s.routingErr[ext] = fmt.Errorf("SelectParser returned nil for %q", ext)
			continue
		}
		s.routingResult[ext] = p.Language()
	}
	return nil
}

func (s *crossLangState) eachExtensionReturnsANonNilParserWithTheExpectedLanguageValue() error {
	expected := map[string]string{
		".c": "c", ".h": "c",
		".cpp": "cpp", ".cxx": "cpp",
		".cs":  "csharp",
		".go":  "go",
		".rs":  "rust",
		".ps1": "powershell", ".psm1": "powershell", ".psd1": "powershell",
	}

	for ext, wantLang := range expected {
		if err, ok := s.routingErr[ext]; ok {
			return fmt.Errorf("extension %q: %v", ext, err)
		}
		gotLang, ok := s.routingResult[ext]
		if !ok {
			return fmt.Errorf("extension %q was not routed to any parser", ext)
		}
		if gotLang != wantLang {
			return fmt.Errorf("extension %q routed to %q, want %q", ext, gotLang, wantLang)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2: .h pinning under CGO=on
//
// Calls SelectParser("foo.h", []string{"cpp"}) — with the cpp hint —
// and verifies the returned parser's Language() is still "c".
// ---------------------------------------------------------------------------

func (s *crossLangState) theDispatcherHasACParserClaimingCAndH() error {
	// Partial setup — completed when the C++ parser step fires.
	return nil
}

func (s *crossLangState) theDispatcherHasACppParserClaimingCppAndCxx() error {
	s.dispatcher = ast.NewDispatcher(
		ast.WithWriter(&recordingWriter{}),
		ast.WithParser(&stubLangParser{lang: "c", exts: []string{".c", ".h"}}),
		ast.WithParser(&stubLangParser{lang: "cpp", exts: []string{".cpp", ".cxx"}}),
	)
	return nil
}

func (s *crossLangState) selectParserRunsForFileWithHints(filename, hint string) error {
	hints := []string{hint}
	p := s.dispatcher.SelectParser(filename, hints)
	if p == nil {
		return fmt.Errorf("SelectParser(%q, %v) returned nil", filename, hints)
	}
	s.selectedLang = p.Language()
	return nil
}

func (s *crossLangState) theReturnedParserLanguageIs(expectedLang string) error {
	if s.selectedLang != expectedLang {
		return fmt.Errorf("expected parser language %q, got %q", expectedLang, s.selectedLang)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3: Duplicate registration last-wins
//
// Uses ast.WithParsers(parserA, parserB) — the variadic registration —
// and calls SelectParser to verify parserB wins.
// ---------------------------------------------------------------------------

func (s *crossLangState) twoStubParsersBothClaimingGoRegisteredViaWithParsers() error {
	s.parserA = &stubLangParser{lang: "go-first", exts: []string{".go"}}
	s.parserB = &stubLangParser{lang: "go-second", exts: []string{".go"}}

	s.dispatcher = ast.NewDispatcher(
		ast.WithWriter(&recordingWriter{}),
		ast.WithParsers(s.parserA, s.parserB),
	)
	return nil
}

func (s *crossLangState) selectParserRunsForXGo() error {
	p := s.dispatcher.SelectParser("x.go", nil)
	if p == nil {
		return fmt.Errorf("SelectParser(x.go, nil) returned nil")
	}
	s.selectedLang = p.Language()
	return nil
}

func (s *crossLangState) theReturnedParserIsTheSecondRegisteredParser() error {
	if s.selectedLang != "go-second" {
		return fmt.Errorf("expected second parser (go-second), got %q — last-wins violated", s.selectedLang)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 4: Multimap collision drops end-to-end
//
// Reads a real Go fixture file from disk. Builds a fixtureGoParser that
// returns the expected ParseResult for that file. Routes through the real
// Dispatcher.EmitFile → fixtureGoParser.Parse → Emitter.Emit → Writer
// pipeline. Asserts on the edges captured by the recording writer.
// ---------------------------------------------------------------------------

func (s *crossLangState) aGoFixtureFileWithBothReceiverVariantsAndSiblingCaller(fixturePath string) error {
	absPath := filepath.FromSlash(fixturePath)
	src, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("failed to read fixture %s: %v", absPath, err)
	}
	s.fixturePath = absPath
	s.fixtureSrc = src

	parser := &fixtureGoParser{
		fixtures: map[string]ast.ParseResult{
			absPath: {
				Classes: []ast.ClassDecl{{Name: "Foo"}},
				Methods: []ast.MethodDecl{
					{Name: "Bar", ClassName: "Foo"},
					{Name: "Bar", ClassName: "*Foo"},
					{Name: "Caller", ClassName: "Foo"},
				},
			},
		},
	}

	s.recorder = &recordingWriter{}
	s.dispatcher = ast.NewDispatcher(
		ast.WithParser(parser),
		ast.WithWriter(s.recorder),
	)
	return nil
}

func (s *crossLangState) emitFileRunsOnTheGoFixture() error {
	res, err := s.dispatcher.EmitFile(s.fixturePath, s.fixtureSrc)
	if err != nil {
		return fmt.Errorf("EmitFile error: %v", err)
	}
	s.emitRes = res
	return nil
}

func (s *crossLangState) zeroStaticCallsEdgesTargetBar(methodName string) error {
	for _, e := range s.recorder.edges {
		if e.Kind == "static_calls" && strings.Contains(e.Target, methodName) {
			return fmt.Errorf("unexpected static_calls edge targeting %q: %+v", methodName, e)
		}
	}
	return nil
}

func (s *crossLangState) verbatimPersistsOnCallsRaw(methodName string) error {
	for _, raw := range s.emitRes.CallsRaw {
		if strings.Contains(raw, methodName) {
			return nil
		}
	}
	return fmt.Errorf("%q not found in CallsRaw: %v", methodName, s.emitRes.CallsRaw)
}

// ---------------------------------------------------------------------------
// Scenario 5: Multimap pointer-only resolves end-to-end
//
// Same end-to-end EmitFile path, but with only the pointer-receiver method.
// ---------------------------------------------------------------------------

func (s *crossLangState) aGoFixtureFileWithOnlyPointerReceiverMethod(fixturePath string) error {
	absPath := filepath.FromSlash(fixturePath)
	src, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("failed to read fixture %s: %v", absPath, err)
	}
	s.fixturePath = absPath
	s.fixtureSrc = src
	return nil
}

func (s *crossLangState) theFixtureParserMapsPointerReceiverMethodWithAlias(methodName, className, aliasKey, aliasTarget string) error {
	parser := &fixtureGoParser{
		fixtures: map[string]ast.ParseResult{
			s.fixturePath: {
				Classes: []ast.ClassDecl{{Name: "Foo"}},
				Methods: []ast.MethodDecl{
					{
						Name:      methodName,
						ClassName: className,
						ReceiverAliases: map[string]string{
							aliasKey: aliasTarget,
						},
					},
					{Name: "Sibling", ClassName: "Foo"},
				},
			},
		},
	}

	s.recorder = &recordingWriter{}
	s.dispatcher = ast.NewDispatcher(
		ast.WithParser(parser),
		ast.WithWriter(s.recorder),
	)
	return nil
}

func (s *crossLangState) exactlyOneStaticCallsEdgeFromSiblingToStarFooBar() error {
	var matching []ast.Edge
	for _, e := range s.recorder.edges {
		if e.Kind == "static_calls" && strings.Contains(e.Target, "*Foo.Bar") {
			matching = append(matching, e)
		}
	}
	if len(matching) != 1 {
		return fmt.Errorf("expected 1 static_calls edge to *Foo.Bar, got %d: %+v", len(matching), matching)
	}
	if !strings.Contains(matching[0].Source, "Sibling") {
		return fmt.Errorf("expected edge source containing 'Sibling', got %q", matching[0].Source)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 6: ErrParserUnavailable surfaces as skip-not-error
//
// Uses the real Dispatcher.EmitFile + ErrParserUnavailable sentinel.
// ---------------------------------------------------------------------------

func (s *crossLangState) aStubParserReturningErrParserUnavailableWithReason(reason, ext string) error {
	// Setup deferred to the When step so the dispatcher is built fresh.
	return nil
}

func (s *crossLangState) emitFileProcessesAnExtFile(ext string) error {
	logger := &mockLogger{}
	stub := &unavailableParser{
		lang:   "stub",
		exts:   []string{ext},
		reason: "test_unavailable",
	}
	s.recorder = &recordingWriter{}
	s.dispatcher = ast.NewDispatcher(
		ast.WithParser(stub),
		ast.WithWriter(s.recorder),
		ast.WithLogger(logger),
	)

	res, err := s.dispatcher.EmitFile("file"+ext, []byte("// test"))
	s.emitRes = res
	s.emitErr = err
	s.logEntries = logger.entries
	return nil
}

func (s *crossLangState) theLogKeyIsWithReason(event, reason string) error {
	for _, entry := range s.logEntries {
		if entry.Event == event && entry.Reason == reason {
			return nil
		}
	}
	return fmt.Errorf("expected log entry event=%q reason=%q, got %+v", event, reason, s.logEntries)
}

func (s *crossLangState) emitFileReturnsZeroEmitResultAndNilError() error {
	if s.emitErr != nil {
		return fmt.Errorf("expected nil error, got: %v", s.emitErr)
	}
	if s.emitRes.NodeCount != 0 || s.emitRes.EdgeCount != 0 || len(s.emitRes.CallsRaw) != 0 {
		return fmt.Errorf("expected zero EmitResult, got: %+v", s.emitRes)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 7: First-class attr key cannot be overridden
//
// Exercises the real ast.MergeLangMeta function.
// ---------------------------------------------------------------------------

func (s *crossLangState) aParserPopulatesLangMetaWithLanguageSetTo(bogusLang string) error {
	s.parserLangMeta = map[string]string{
		"language": bogusLang,
		"custom":   "value",
	}
	return nil
}

func (s *crossLangState) methodAttrsWritesTheResultWithDispatcherLanguage(dispatcherLang string) error {
	s.mergedAttrs = ast.MergeLangMeta(s.parserLangMeta, dispatcherLang)
	return nil
}

func (s *crossLangState) attrsJSONLanguageEquals(expectedLang string) error {
	raw, err := json.Marshal(s.mergedAttrs)
	if err != nil {
		return fmt.Errorf("json.Marshal failed: %v", err)
	}
	var attrs map[string]string
	if err := json.Unmarshal(raw, &attrs); err != nil {
		return fmt.Errorf("failed to unmarshal attrs_json: %v", err)
	}
	if attrs["language"] != expectedLang {
		return fmt.Errorf("attrs_json[\"language\"] = %q, want %q", attrs["language"], expectedLang)
	}
	if attrs["custom"] != "value" {
		return fmt.Errorf("attrs_json[\"custom\"] = %q, want %q — merge dropped non-reserved keys", attrs["custom"], "value")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseQuotedList(s string) []string {
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"")
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_cross_cutting_tests_documentation_validation_cross_language_dispatcher_tests(ctx *godog.ScenarioContext) {
	s := &crossLangState{}

	// Scenario 1: Every new extension routes to its parser
	ctx.Given(`^the dispatcher is configured with parsers for all new languages$`,
		s.theDispatcherIsConfiguredWithParsersForAllNewLanguages)
	ctx.When(`^selectParser runs for each of (.+)$`,
		s.selectParserRunsForEachExtension)
	ctx.Then(`^each extension returns a non-nil parser with the expected Language value$`,
		s.eachExtensionReturnsANonNilParserWithTheExpectedLanguageValue)

	// Scenario 2: .h pinning under CGO=on
	ctx.Given(`^the dispatcher has a C parser claiming "\.c" and "\.h"$`,
		s.theDispatcherHasACParserClaimingCAndH)
	ctx.Given(`^the dispatcher has a C\+\+ parser claiming "\.cpp" and "\.cxx"$`,
		s.theDispatcherHasACppParserClaimingCppAndCxx)
	ctx.When(`^selectParser runs for "([^"]*)" with hints "([^"]*)"$`,
		s.selectParserRunsForFileWithHints)
	ctx.Then(`^the returned parser Language is "([^"]*)"$`,
		s.theReturnedParserLanguageIs)

	// Scenario 3: Duplicate registration last-wins (WithParsers)
	ctx.Given(`^two stub parsers both claiming "\.go" registered via WithParsers$`,
		s.twoStubParsersBothClaimingGoRegisteredViaWithParsers)
	ctx.When(`^selectParser runs for "x\.go"$`,
		s.selectParserRunsForXGo)
	ctx.Then(`^the returned parser is the second registered parser$`,
		s.theReturnedParserIsTheSecondRegisteredParser)

	// Scenario 4: Multimap collision drops end-to-end (EmitFile)
	ctx.Given(`^a Go fixture file "([^"]*)" with both receiver variants of "([^"]*)" and a sibling caller$`,
		s.aGoFixtureFileWithBothReceiverVariantsAndSiblingCaller)
	ctx.When(`^EmitFile runs on the Go fixture$`, s.emitFileRunsOnTheGoFixture)
	ctx.Then(`^zero static_calls edges target "([^"]*)"$`,
		s.zeroStaticCallsEdgesTargetBar)
	ctx.Then(`^verbatim "([^"]*)" persists on calls_raw$`,
		s.verbatimPersistsOnCallsRaw)

	// Scenario 5: Multimap pointer-only resolves end-to-end (EmitFile)
	ctx.Given(`^a Go fixture file "([^"]*)" with only the pointer-receiver method$`,
		s.aGoFixtureFileWithOnlyPointerReceiverMethod)
	ctx.Given(`^the fixture parser maps the pointer-receiver method "([^"]*)" on "([^"]*)" with alias "([^"]*)" to "([^"]*)"$`,
		s.theFixtureParserMapsPointerReceiverMethodWithAlias)
	ctx.Then(`^exactly one static_calls edge from the sibling to "\*Foo\.Bar" is emitted$`,
		s.exactlyOneStaticCallsEdgeFromSiblingToStarFooBar)

	// Scenario 6: ErrParserUnavailable surfaces as skip-not-error
	ctx.Given(`^a stub parser returning ErrParserUnavailable with reason "([^"]*)" for "([^"]*)"$`,
		s.aStubParserReturningErrParserUnavailableWithReason)
	ctx.When(`^EmitFile processes an "([^"]*)" file$`,
		s.emitFileProcessesAnExtFile)
	ctx.Then(`^the log key is "([^"]*)" with reason "([^"]*)"$`,
		s.theLogKeyIsWithReason)
	ctx.Then(`^EmitFile returns a zero EmitResult and nil error$`,
		s.emitFileReturnsZeroEmitResultAndNilError)

	// Scenario 7: First-class attr key cannot be overridden
	ctx.Given(`^a parser populates LangMeta with "language" set to "([^"]*)"$`,
		s.aParserPopulatesLangMetaWithLanguageSetTo)
	ctx.When(`^methodAttrs writes the result with dispatcher language "([^"]*)"$`,
		s.methodAttrsWritesTheResultWithDispatcherLanguage)
	ctx.Then(`^attrs_json "language" equals "([^"]*)"$`,
		s.attrsJSONLanguageEquals)
}

func TestE2E_cross_cutting_tests_documentation_validation_cross_language_dispatcher_tests(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_cross_cutting_tests_documentation_validation_cross_language_dispatcher_tests,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"cross_cutting_tests_documentation_validation_cross_language_dispatcher_tests.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}