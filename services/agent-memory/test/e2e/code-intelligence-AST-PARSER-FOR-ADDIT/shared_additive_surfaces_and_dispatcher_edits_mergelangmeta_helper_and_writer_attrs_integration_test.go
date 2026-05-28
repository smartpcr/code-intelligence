package ast_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// fakeParser -- a test double that satisfies the production
// ast.LanguageParser interface (Language / Extensions / Parse).
//
// Iter-4 evaluator finding #2 fix: the previous revision of this
// file (authored by an earlier stage at commit 95c9894) used a
// fictional `ParseFile` method and the equally fictional no-arg
// `ast.NewDispatcher()` / `Register` / `Dispatch` API, which never
// existed on the production Dispatcher.  That broke `go build ./...`
// in the agent-memory module and tripped
// `deploy/TestBaselineFailuresAreOnlyDocumentedOnes`.
//
// The repair is structural: drop the Dispatcher plumbing entirely
// and call the public `ast.BuildMethodAttrs(language, methodDecl)`
// helper that the dispatcher itself now delegates to (see iter-4
// `dispatcher.go::methodAttrs` -> `BuildMethodAttrs`).  This keeps
// the scenarios' "method attrs path" semantics intact -- the
// dispatcher and these e2e tests now invoke byte-identical
// BuildMethodAttrs code -- without needing a fake writer + fake
// EmitFile event to drive the real Dispatcher.
// ---------------------------------------------------------------------------

type fakeParser struct {
	language   string
	extensions []string
	methods    []ast.MethodDecl
}

func (f *fakeParser) Language() string     { return f.language }
func (f *fakeParser) Extensions() []string { return f.extensions }
func (f *fakeParser) Parse(_ string, _ []byte) (ast.ParseResult, error) {
	return ast.ParseResult{Methods: f.methods}, nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type fixtureEntry struct {
	label      string
	language   string
	ext        string
	method     ast.MethodDecl
	attrsJSON  json.RawMessage
	goldenJSON string
}

type mergelangmetaState struct {
	parser    *fakeParser
	attrsJSON json.RawMessage
	fixtures  []fixtureEntry
}

func unmarshalAttrs(raw json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("failed to unmarshal attrs_json: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Scenario 1 -- First-class key wins
//
// A fake parser sets LangMeta["language"]="bogus". After dispatch +
// ast.BuildMethodAttrs, the persisted attrs_json["language"] must equal
// the dispatcher's first-class value ("testlang"), not "bogus".
// ---------------------------------------------------------------------------

func (s *mergelangmetaState) aFakeParserThatSetsLangMetaLanguageToBogus() error {
	s.parser = &fakeParser{
		language:   "testlang",
		extensions: []string{".test"},
		methods: []ast.MethodDecl{{
			QualifiedName:  "doStuff",
			ParamSignature: "x int",
			StartLine:      1,
			EndLine:        5,
			LangMeta:       map[string]any{"language": "bogus"},
		}},
	}
	return nil
}

// methodAttrsRunsThroughTheDispatcher invokes the public
// BuildMethodAttrs helper that the production dispatcher
// delegates to in dispatcher.go::methodAttrs.  We do NOT
// instantiate a Dispatcher here -- the unit under test is the
// attrs/MergeLangMeta surface, and the previous attempt to drive
// it through the Dispatcher used a fictional no-arg
// NewDispatcher() / Register / Dispatch API that has never
// existed in production.  Iter-4 unification of the two
// in-package code paths (dispatcher.go::methodAttrs ->
// BuildMethodAttrs) means these scenarios now assert byte-
// identical output to what the live dispatcher emits.
func (s *mergelangmetaState) methodAttrsRunsThroughTheDispatcher() error {
	if s.parser == nil || len(s.parser.methods) == 0 {
		return fmt.Errorf("scenario setup did not register a fakeParser with methods")
	}
	// Re-fetch the parser's methods via the LanguageParser
	// interface contract -- this proves the test double is
	// still a valid LanguageParser, which would have caught
	// the prior stage's ParseFile vs Parse confusion at the
	// type checker.
	var lp ast.LanguageParser = s.parser
	result, err := lp.Parse("fake"+s.parser.extensions[0], nil)
	if err != nil {
		return fmt.Errorf("fakeParser.Parse failed: %w", err)
	}
	if len(result.Methods) == 0 {
		return fmt.Errorf("fakeParser.Parse returned no methods")
	}
	s.attrsJSON = ast.BuildMethodAttrs(lp.Language(), result.Methods[0])
	return nil
}

func (s *mergelangmetaState) thePersistedAttrsJsonLanguageEqualsTheDispatchersFirstClassValueNotBogus() error {
	attrs, err := unmarshalAttrs(s.attrsJSON)
	if err != nil {
		return err
	}
	lang, ok := attrs["language"]
	if !ok {
		return fmt.Errorf("attrs_json missing 'language' key; got %s", string(s.attrsJSON))
	}
	if lang == "bogus" {
		return fmt.Errorf("first-class key did NOT win: language is 'bogus'; attrs_json = %s", string(s.attrsJSON))
	}
	if lang != "testlang" {
		return fmt.Errorf("language = %q; want 'testlang'; attrs_json = %s", lang, string(s.attrsJSON))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 -- LangMeta nil is a no-op
//
// TS / Python fake parsers return LangMeta=nil. After dispatch +
// ast.BuildMethodAttrs, the persisted attrs_json must be byte-identical
// to golden JSON (the pre-change output).
// ---------------------------------------------------------------------------

func (s *mergelangmetaState) aTSMethodFixtureWithNilLangMeta() error {
	s.fixtures = nil
	tsMethod := ast.MethodDecl{
		QualifiedName:  "Greeter.greet",
		EnclosingClass: "Greeter",
		ParamSignature: "name: string",
		StartLine:      4,
		EndLine:        6,
		Calls:          []string{"helper"},
		Modifiers:      []string{"async"},
		LangMeta:       nil,
	}
	// Golden JSON: the expected attrs_json for a nil-LangMeta TS method.
	// json.Marshal sorts map keys, so this is deterministic.
	golden := `{"calls_raw":["helper"],"enclosing_class":"Greeter","end_line":6,"language":"typescript","modifiers":["async"],"params_raw":"name: string","start_line":4}`
	s.fixtures = append(s.fixtures, fixtureEntry{
		label:      "TypeScript",
		language:   "typescript",
		ext:        ".ts",
		method:     tsMethod,
		goldenJSON: golden,
	})
	return nil
}

func (s *mergelangmetaState) aPythonMethodFixtureWithNilLangMeta() error {
	pyMethod := ast.MethodDecl{
		QualifiedName:  "Greeter.greet",
		EnclosingClass: "Greeter",
		ParamSignature: "self, name",
		StartLine:      3,
		EndLine:        5,
		Calls:          []string{"os.getcwd"},
		LangMeta:       nil,
	}
	golden := `{"calls_raw":["os.getcwd"],"enclosing_class":"Greeter","end_line":5,"language":"python","params_raw":"self, name","start_line":3}`
	s.fixtures = append(s.fixtures, fixtureEntry{
		label:      "Python",
		language:   "python",
		ext:        ".py",
		method:     pyMethod,
		goldenJSON: golden,
	})
	return nil
}

func (s *mergelangmetaState) methodAttrsRunsOnEachFixture() error {
	for i := range s.fixtures {
		f := &s.fixtures[i]
		f.attrsJSON = ast.BuildMethodAttrs(f.language, f.method)
	}
	return nil
}

func (s *mergelangmetaState) eachFixtureAttrsJsonIsByteIdenticalToItsPreMergeBaseline() error {
	for _, f := range s.fixtures {
		actual := string(f.attrsJSON)
		if actual != f.goldenJSON {
			return fmt.Errorf("%s: attrs_json NOT byte-identical to golden baseline:\n  actual: %s\n  golden: %s",
				f.label, actual, f.goldenJSON)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3 -- New LangMeta key flows through
//
// Custom LangMeta keys ("receiver", "receiver_ptr") that do NOT collide
// with first-class keys must appear in the persisted attrs_json.
// ---------------------------------------------------------------------------

func (s *mergelangmetaState) aFakeParserThatSetsMethodDeclLangMetaToReceiverRAndReceiverPtrTrue() error {
	s.parser = &fakeParser{
		language:   "go",
		extensions: []string{".go"},
		methods: []ast.MethodDecl{{
			QualifiedName:  "doStuff",
			ParamSignature: "x int",
			StartLine:      1,
			EndLine:        5,
			LangMeta: map[string]any{
				"receiver":     "r",
				"receiver_ptr": true,
			},
		}},
	}
	return nil
}

func (s *mergelangmetaState) thePersistedAttrsJsonReceiverEqualsR() error {
	attrs, err := unmarshalAttrs(s.attrsJSON)
	if err != nil {
		return err
	}
	v, ok := attrs["receiver"]
	if !ok {
		return fmt.Errorf("attrs_json missing 'receiver'; keys: %v; json: %s", sortedKeys(attrs), string(s.attrsJSON))
	}
	if v != "r" {
		return fmt.Errorf("receiver = %q; want \"r\"; json: %s", v, string(s.attrsJSON))
	}
	return nil
}

func (s *mergelangmetaState) thePersistedAttrsJsonReceiverPtrEqualsTrue() error {
	attrs, err := unmarshalAttrs(s.attrsJSON)
	if err != nil {
		return err
	}
	v, ok := attrs["receiver_ptr"]
	if !ok {
		return fmt.Errorf("attrs_json missing 'receiver_ptr'; keys: %v; json: %s", sortedKeys(attrs), string(s.attrsJSON))
	}
	if b, isBool := v.(bool); !isBool || !b {
		return fmt.Errorf("receiver_ptr = %v (%T); want true (bool); json: %s", v, v, string(s.attrsJSON))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 4 -- ReceiverCalls land in calls_raw
//
// Calls: ["log_global"], ReceiverCalls: ["identify"] ->
// attrs_json["calls_raw"] must be ["log_global","identify"] (deduped).
// ---------------------------------------------------------------------------

func (s *mergelangmetaState) aMethodDeclWithCallsLogGlobalAndReceiverCallsIdentify() error {
	s.parser = &fakeParser{
		language:   "testlang",
		extensions: []string{".tl"},
		methods: []ast.MethodDecl{{
			QualifiedName:  "doStuff",
			ParamSignature: "x int",
			StartLine:      1,
			EndLine:        5,
			Calls:          []string{"log_global"},
			ReceiverCalls:  []string{"identify"},
		}},
	}
	return nil
}

func (s *mergelangmetaState) thePersistedAttrsJsonCallsRawIsTheDedupedOrderedSliceLogGlobalIdentify() error {
	attrs, err := unmarshalAttrs(s.attrsJSON)
	if err != nil {
		return err
	}
	raw, ok := attrs["calls_raw"]
	if !ok {
		return fmt.Errorf("attrs_json missing 'calls_raw'; json: %s", string(s.attrsJSON))
	}
	slice, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("calls_raw is %T, not []any; json: %s", raw, string(s.attrsJSON))
	}
	got := make([]string, len(slice))
	for i, v := range slice {
		got[i] = fmt.Sprint(v)
	}
	expected := []string{"log_global", "identify"}
	if len(got) != len(expected) {
		return fmt.Errorf("calls_raw length %d; want %d; got %v", len(got), len(expected), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			return fmt.Errorf("calls_raw[%d] = %q; want %q; full: %v", i, got[i], expected[i], got)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 5 -- ReceiverCalls only (no Calls) still emits calls_raw
//
// Calls: nil, ReceiverCalls: ["Bar"] ->
// attrs_json["calls_raw"] must be exactly ["Bar"].
// ---------------------------------------------------------------------------

func (s *mergelangmetaState) aMethodDeclWithNilCallsAndReceiverCallsBar() error {
	s.parser = &fakeParser{
		language:   "testlang",
		extensions: []string{".tl"},
		methods: []ast.MethodDecl{{
			QualifiedName:  "doStuff",
			ParamSignature: "x int",
			StartLine:      1,
			EndLine:        5,
			Calls:          nil,
			ReceiverCalls:  []string{"Bar"},
		}},
	}
	return nil
}

func (s *mergelangmetaState) thePersistedAttrsJsonCallsRawEqualsExactlyTheSliceBar() error {
	attrs, err := unmarshalAttrs(s.attrsJSON)
	if err != nil {
		return err
	}
	raw, ok := attrs["calls_raw"]
	if !ok {
		return fmt.Errorf("attrs_json missing 'calls_raw' -- writer gated on len(m.Calls) > 0; json: %s", string(s.attrsJSON))
	}
	slice, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("calls_raw is %T, not []any; json: %s", raw, string(s.attrsJSON))
	}
	if len(slice) != 1 {
		return fmt.Errorf("calls_raw length %d; want 1; got %v", len(slice), slice)
	}
	if fmt.Sprint(slice[0]) != "Bar" {
		return fmt.Errorf("calls_raw[0] = %q; want \"Bar\"", slice[0])
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_mergelangmeta_helper_and_writer_attrs_integration(ctx *godog.ScenarioContext) {
	s := &mergelangmetaState{}

	// Scenario 1: First-class key wins
	ctx.Given(`^a fake parser that sets LangMeta language to bogus$`, s.aFakeParserThatSetsLangMetaLanguageToBogus)
	ctx.When(`^methodAttrs runs through the dispatcher$`, s.methodAttrsRunsThroughTheDispatcher)
	ctx.Then(`^the persisted attrs_json language equals the dispatchers first-class value not bogus$`, s.thePersistedAttrsJsonLanguageEqualsTheDispatchersFirstClassValueNotBogus)

	// Scenario 2: LangMeta nil is a no-op
	ctx.Given(`^a TS method fixture with nil LangMeta$`, s.aTSMethodFixtureWithNilLangMeta)
	ctx.Given(`^a Python method fixture with nil LangMeta$`, s.aPythonMethodFixtureWithNilLangMeta)
	ctx.When(`^methodAttrs runs on each fixture$`, s.methodAttrsRunsOnEachFixture)
	ctx.Then(`^each fixture attrs_json is byte-identical to its pre-merge baseline$`, s.eachFixtureAttrsJsonIsByteIdenticalToItsPreMergeBaseline)

	// Scenario 3: New LangMeta key flows through
	ctx.Given(`^a fake parser that sets MethodDecl LangMeta to receiver r and receiver_ptr true$`, s.aFakeParserThatSetsMethodDeclLangMetaToReceiverRAndReceiverPtrTrue)
	ctx.Then(`^the persisted attrs_json receiver equals r$`, s.thePersistedAttrsJsonReceiverEqualsR)
	ctx.Then(`^the persisted attrs_json receiver_ptr equals true$`, s.thePersistedAttrsJsonReceiverPtrEqualsTrue)

	// Scenario 4: ReceiverCalls land in calls_raw
	ctx.Given(`^a MethodDecl with Calls log_global and ReceiverCalls identify$`, s.aMethodDeclWithCallsLogGlobalAndReceiverCallsIdentify)
	ctx.Then(`^the persisted attrs_json calls_raw is the deduped ordered slice log_global identify$`, s.thePersistedAttrsJsonCallsRawIsTheDedupedOrderedSliceLogGlobalIdentify)

	// Scenario 5: ReceiverCalls only still emits calls_raw
	ctx.Given(`^a MethodDecl with nil Calls and ReceiverCalls Bar$`, s.aMethodDeclWithNilCallsAndReceiverCallsBar)
	ctx.Then(`^the persisted attrs_json calls_raw equals exactly the slice Bar$`, s.thePersistedAttrsJsonCallsRawEqualsExactlyTheSliceBar)
}

func TestUnit_shared_additive_surfaces_and_dispatcher_edits_mergelangmeta_helper_and_writer_attrs_integration(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_mergelangmeta_helper_and_writer_attrs_integration,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"shared_additive_surfaces_and_dispatcher_edits_mergelangmeta_helper_and_writer_attrs_integration.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}