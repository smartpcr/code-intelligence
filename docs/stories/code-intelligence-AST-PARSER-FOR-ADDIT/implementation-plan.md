---
title: "ast parser for additional language"
storyId: "code-intelligence:AST-PARSER-FOR-ADDIT"
---

> Livedoc: tick boxes as PRs land. This plan tracks the
> shipping order for six new `LanguageParser` implementations
> (C, C++, C#, Go, Rust, PowerShell) behind the Stage 3.2
> dispatcher seam at
> `services/agent-memory/internal/repoindexer/ast`. Companion
> docs (parallel): `architecture.md` (component / data-model /
> sequence contracts), `tech-spec.md` (per-language extraction
> rules + thresholds + risks), `e2e-scenarios.md` (operator
> Given/When/Then walk-throughs).
>
> The four operator-pinned decisions (`dot-h-extension-routing`,
> `powershell-grammar-strategy`,
> `go-receiver-pointer-fingerprint`, `rust-trait-overrides-edge`)
> drive the step shapes below. Implementation order follows the
> story description's recommendation: foundations first, then
> Go, then C/C++, then C#, then Rust (carries the new Pass 2d
> overrides emission), then PowerShell, then cross-cutting tests
> and documentation.

# Phase 1: Shared additive surfaces and dispatcher edits

## Dependencies
- _none -- start phase_

## Stage 1.1: Additive parser.go struct surfaces

### Implementation Steps
- [ ] Add `LangMeta map[string]any` field to `ClassDecl` in `services/agent-memory/internal/repoindexer/ast/parser.go`; document that nil means "no per-language attrs" and that it is descriptive, not identifying (architecture C12).
- [ ] Add `LangMeta map[string]any` field to `MethodDecl` in the same file; carry the same nil-is-empty doc comment.
- [ ] Add `LangMeta map[string]any` field to `Import` in the same file with matching doc comment.
- [ ] Add `ReceiverAliases []string` field to `MethodDecl`; doc comment must state that aliases are SECONDARY keys for receiver-qualified call resolution and that the primary key is `QualifiedName` (architecture Section 4.5.1).
- [ ] Export sentinel `ErrParserUnavailable = errors.New("parser: runtime dependency unavailable")` in `parser.go`; doc states it is returned (wrapped via `fmt.Errorf("...: %w (reason=<slug>)", ErrParserUnavailable)`) when a parser's runtime dependency is missing so the dispatcher can log `ast.dispatch.skip` instead of `ast.parse.error`.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: LangMeta nil compiles unchanged -- Given existing TS / Python parsers leave `LangMeta` nil, When `go build ./...` runs, Then it succeeds and no existing test in `parser_typescript_test.go` / `parser_python_test.go` fails.
- [ ] Scenario: ReceiverAliases default nil -- Given a `MethodDecl{}` zero value, When the dispatcher iterates `m.ReceiverAliases`, Then the iteration yields zero elements (no panic on nil slice).
- [ ] Scenario: ErrParserUnavailable identity -- Given a wrapped error `fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ErrParserUnavailable)`, When `errors.Is(err, ErrParserUnavailable)` is evaluated, Then it returns true.

## Stage 1.2: Schema migration for overrides edge kind

### Implementation Steps
- [ ] Create new file `services/agent-memory/migrations/0022_edge_kind_overrides.sql` with a single statement `ALTER TYPE edge_kind ADD VALUE 'overrides';` and a comment header citing the Rust trait shadow rule (architecture Section 9 R4).
- [ ] Verify the existing enum at `services/agent-memory/migrations/0001_enums.sql` lines 28-38 still lists `contains`, `imports`, `static_calls`, `observed_calls`, `extends`, `implements`, `reads`, `writes`, `renamed_to` so that the new value is monotonically appended.
- [ ] Update any local migration runner / golden test in `services/agent-memory/internal/datastore` (if one exists for the enum value set) to expect the new `overrides` value.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Migration applies cleanly -- Given a fresh schema with migrations through `0021_concept_candidate.sql`, When `0022_edge_kind_overrides.sql` is applied, Then it returns no error and `SELECT 'overrides'::edge_kind` succeeds.
- [ ] Scenario: Migration is idempotent on re-run check -- Given the migration runner skips already-applied migrations by filename, When `0022_edge_kind_overrides.sql` runs once, Then a second migration pass does not re-execute it (no `ALTER TYPE ... ADD VALUE` duplicate-error in PostgreSQL).

## Stage 1.3: mergeLangMeta helper and writer attrs integration

### Implementation Steps
- [ ] Add a package-private helper `mergeLangMeta(out map[string]any, in map[string]any)` in `services/agent-memory/internal/repoindexer/ast/dispatcher.go` that iterates `in`, and only writes a key into `out` when `out` does not already hold it (first-class keys win per C11 / architecture Section 4.4.2). Skip when `in == nil`.
- [ ] Call `mergeLangMeta(attrs, c.LangMeta)` at the end of `classAttrs` immediately before `mustJSON`.
- [ ] Call `mergeLangMeta(attrs, m.LangMeta)` at the end of `methodAttrs` immediately before `mustJSON`.
- [ ] Call `mergeLangMeta(attrs, im.LangMeta)` at the end of `importEdgeAttrs` (or the equivalent helper used for the file -> package edge) immediately before `mustJSON`.
- [ ] Document in `doc.go` (existing file) the new `LangMeta` carry-through path so future contributors know which writer hooks fold per-language attrs.

### Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits/stage-additive-parser-go-struct-surfaces

### Test Scenarios
- [ ] Scenario: First-class key wins -- Given a fake parser sets `LangMeta["language"]="bogus"`, When `methodAttrs` runs, Then the persisted `attrs_json["language"]` equals the dispatcher's first-class value (architecture Section 4.4.2 + C11), not `"bogus"`.
- [ ] Scenario: LangMeta nil is a no-op -- Given a TS / Python parser returns `MethodDecl` with `LangMeta=nil`, When `methodAttrs` runs, Then `attrs_json` is byte-identical to its pre-change output (existing `parser_typescript_test.go` / `parser_python_test.go` assertions on attrs continue to pass).
- [ ] Scenario: New LangMeta key flows through -- Given a parser sets `MethodDecl.LangMeta = {"receiver":"r","receiver_ptr":true}`, When `methodAttrs` runs, Then `attrs_json["receiver"] == "r"` and `attrs_json["receiver_ptr"] == true`.

## Stage 1.4: Dispatcher sentinel branch, Pass 2b multimap, Pass 2d overrides

### Implementation Steps
- [ ] In `dispatcher.go::EmitFile`, after `safeParse` returns, add an `errors.Is(err, ErrParserUnavailable)` branch that logs `ast.dispatch.skip` with `reason` parsed from the wrapped error (default `"runtime_unavailable"` when the wrapper omits a slug) and returns `EmitResult{}, nil` like the existing `parser == nil` branch.
- [ ] Add a `simpleName(q string) string` helper in `dispatcher.go` near `buildCalleeIndex` that strips a leading `*` then returns `q[LastIndexByte(q, '.')+1:]` (architecture Section 4.5.1).
- [ ] Refactor `dispatcher.go::emit` Pass 2b setup: build `receiverIndex map[string][]string` keyed by `m.EnclosingClass+"."+simpleName(m.QualifiedName)` and append `m.NodeID` for every alias in `m.ReceiverAliases`; replace the existing single-key `methodNodeID[m.EnclosingClass+"."+callee]` lookup with `len(receiverIndex[key]) == 1` resolution and drop-on-collision when `> 1` (A5 rule).
- [ ] Add new Pass 2d ("overrides") that runs AFTER Pass 2c: for each `MethodDecl m` where `m.LangMeta["trait"]` is a non-empty string, look up `methodNodeID[traitName+"."+simpleName(m.QualifiedName)]`; on hit, insert one edge of kind `"overrides"` from `m`'s node id to the trait method id. Cross-file miss -> drop.
- [ ] Wire the new Pass 2d into `emit`'s sequence (after Pass 2c, before `EmitResult` assembly) and ensure `EmitResult.TouchedNodes` is unchanged.

### Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits/stage-additive-parser-go-struct-surfaces
- phase-shared-additive-surfaces-and-dispatcher-edits/stage-schema-migration-for-overrides-edge-kind
- phase-shared-additive-surfaces-and-dispatcher-edits/stage-mergelangmeta-helper-and-writer-attrs-integration

### Test Scenarios
- [ ] Scenario: ErrParserUnavailable skip-log path -- Given a stub parser whose `Parse` returns `fmt.Errorf("test: %w (reason=stub_missing)", ErrParserUnavailable)`, When `EmitFile` processes a file routed to that parser, Then the structured log emits `ast.dispatch.skip` with `reason="stub_missing"`, `EmitFile` returns `(EmitResult{}, nil)`, and the writer receives zero `InsertNode` / `InsertEdge` calls.
- [ ] Scenario: Multimap collision drops -- Given a Go fixture with both `func (r Foo) Bar()` and `func (r *Foo) Bar()` in the same file plus a third method calling `r.Bar()` via receiver, When `emit` runs, Then no `static_calls` edge is emitted for `Bar` (set size `> 1` -> drop per A5) and verbatim `Bar` persists on `calls_raw`.
- [ ] Scenario: Multimap pointer-only resolves -- Given a Go fixture with only `func (r *Foo) Bar()` plus a sibling method calling `r.Bar()` from inside `Foo`'s receiver context, When `emit` runs, Then exactly one `static_calls` edge from the sibling method to `*Foo.Bar` is emitted via the `ReceiverAliases` entry `Foo.Bar`.
- [ ] Scenario: Pass 2d overrides edge -- Given a fake parser result with a trait method `Greeter.greet` (LangMeta nil) and an impl method `GreeterImpl.greet` (LangMeta `{"trait":"Greeter"}`) both in the same file, When Pass 2d runs, Then one edge of kind `"overrides"` is inserted from `GreeterImpl.greet` to `Greeter.greet`.
- [ ] Scenario: Pass 2d cross-file miss drops -- Given an impl method with `LangMeta["trait"]="Greeter"` but no `Greeter.greet` Node in the same file's `methodNodeID`, When Pass 2d runs, Then zero `overrides` edges are inserted and the trait name remains on `attrs_json["trait"]`.

## Stage 1.5: normalizeHints alias expansion

### Implementation Steps
- [ ] Extend `dispatcher.go::normalizeHints` to map `c`, `h` -> `c`; `cc`, `cxx`, `cpp`, `c++`, `hpp` -> `cpp`; `cs`, `csharp`, `c#` -> `csharp`; `go`, `golang` -> `go`; `rs`, `rust` -> `rust`; `ps`, `ps1`, `psm1`, `psd1`, `powershell` -> `powershell` (architecture Section 3, tech spec Section 4.2).
- [ ] Preserve existing TS / Python alias rows verbatim (no regression).
- [ ] Confirm `selectParser` precedence is unchanged (extension-match wins ahead of hints) by reading the current resolution order at `dispatcher.go` lines 231-249.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: normalizeHints resolves new aliases -- Given `LanguageHints=["golang"]`, When `normalizeHints` runs, Then the result contains `"go"`.
- [ ] Scenario: Existing aliases preserved -- Given `LanguageHints=["typescript"]`, When `normalizeHints` runs, Then the result still contains `"typescript"`.
- [ ] Scenario: Extension precedence over hint -- Given a `.h` file with `LanguageHints=["cpp"]`, When `selectParser` runs (with the C parser registered, see Phase 3), Then the returned parser's `Language()` is `"c"` (extension match wins; covered fully by the Phase 7 routing test).

# Phase 2: Go parser

## Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits

## Stage 2.1: goTreeSitterParser implementation

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_treesitter_go.go` with `//go:build cgo`, declaring `goTreeSitterParser` struct, constructor `NewTreeSitterGoParser()`, and `Language()` returning `"go"`, `Extensions()` returning `[]string{".go"}`.
- [ ] Implement `Parse(relPath string, src []byte) (ParseResult, error)` walking the `source_file` root of `github.com/smacker/go-tree-sitter/golang`; declare tree-sitter node-type constants at file top (mirrors `parser_treesitter.go` lines 110-136 pattern).
- [ ] Emit `ClassDecl{Kind:"struct"|"interface"|"type_alias", QualifiedName:<name>, LangMeta:{"embeds":[...]}}` for `type_declaration` -> `type_spec` -> `struct_type` / `interface_type` / `type_identifier` (tech spec Section 5.4 grammar table).
- [ ] Emit `MethodDecl` for `function_declaration` and `method_declaration`; for `method_declaration`, embed `*` in `QualifiedName` for pointer receivers per the pinned `go-receiver-pointer-fingerprint` rule (`Foo.Bar` vs `*Foo.Bar`); keep `EnclosingClass` bare; populate `ReceiverAliases=["Foo.Bar"]` for pointer-receiver methods only; populate `LangMeta={"receiver":<bound name>,"receiver_ptr":<bool>}`.
- [ ] Walk method body for `call_expression`: `identifier` -> `Calls`; `selector_expression` -> `Calls` (rightmost identifier); when the receiver matches the method's bound receiver name -> `ReceiverCalls` instead; use the existing `uniqueStringsInsert` helper for dedupe (C9).
- [ ] Walk method body for `selector_expression` outside any `call_expression` where the receiver matches the bound receiver -> append `MemberAccess{Name:<right>,IsWrite:<LHS of assignment_statement; `:=` does NOT count>}`.
- [ ] Emit `Import` per `import_declaration` / `import_spec`: bare path -> `{Module:<path>}`; alias -> `{Module,Alias}`; dot import -> `{Module,Alias:".",LangMeta:{"dot_import":true}}`; blank import -> `{Module,Alias:"_",LangMeta:{"blank_import":true}}`.
- [ ] Mirror the brace-strip body-span convention of `parser_treesitter.go::handleMethod` (C8): exclude outer `{`/`}` from `BodySource`; keep `BodyStartLine`/`BodyEndLine` on the delimiter lines; `BodyStartByte`/`BodyEndByte` point at first/last interior byte.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Build under CGO=on -- Given `CGO_ENABLED=1`, When `go build ./internal/repoindexer/ast/...` runs from `services/agent-memory`, Then it succeeds (smacker/go-tree-sitter/golang import resolves).
- [ ] Scenario: Pointer receiver canonical -- Given a Go source string containing `func (r *Foo) Bar(s string) {}`, When `goTreeSitterParser.Parse` runs, Then the returned `MethodDecl` has `QualifiedName == "*Foo.Bar"`, `EnclosingClass == "Foo"`, `ReceiverAliases == []string{"Foo.Bar"}`, and `LangMeta["receiver_ptr"] == true`.
- [ ] Scenario: Value receiver canonical -- Given `func (r Foo) Bar() {}`, When parsed, Then `MethodDecl.QualifiedName == "Foo.Bar"`, `ReceiverAliases == nil`, and `LangMeta["receiver_ptr"] == false`.

## Stage 2.2: Register Go parser in parsers_cgo.go

### Implementation Steps
- [ ] Append `NewTreeSitterGoParser()` to the `defaultParsers()` return slice in `services/agent-memory/internal/repoindexer/ast/parsers_cgo.go`.
- [ ] Leave `parsers_nocgo.go` unchanged for Go (the CGO=off path deliberately drops Go per architecture Section 3 / tech spec Section 4.3).

### Dependencies
- phase-go-parser/stage-gotreesitterparser-implementation

### Test Scenarios
- [ ] Scenario: Extension routing under CGO=on -- Given the dispatcher constructed with `defaultParsers()` under CGO=on, When `selectParser("foo.go", nil)` runs, Then the returned parser's `Language() == "go"`.
- [ ] Scenario: Skip under CGO=off -- Given the dispatcher constructed with `defaultParsers()` under CGO=off, When `EmitFile` processes a `.go` file, Then the structured log emits `ast.dispatch.skip{reason:"no_parser"}` and no Node / Edge is inserted.

## Stage 2.3: Go fixture test

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_treesitter_go_test.go` with `//go:build cgo`; add `TestGoFixture_EmitsExpectedNodeAndEdgeSet` mirroring `TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet` (`parser_typescript_test.go` lines 11-100).
- [ ] Embed a fixture string containing: `type Greeter struct{prefix string}`, method `func (g *Greeter) Greet(name string) string { return formatGreeting(g.prefix, name) }`, free function `func formatGreeting(prefix, name string) string`, and `import "fmt"` (the import wired so the dispatcher emits an `imports` edge to an external `fmt` package node).
- [ ] Assert: 1 class node `Greeter` (`Kind="struct"`); 2 method nodes (`*Greeter.Greet`, `formatGreeting`); 3 contains edges (file -> Greeter, Greeter -> *Greeter.Greet, file -> formatGreeting); 1 static_calls edge (`*Greeter.Greet` -> `formatGreeting`); 1 imports edge (file -> `fmt` package).
- [ ] Add a sub-test `TestGoFixture_PointerReceiverFingerprint` asserting `*Greeter.Greet`'s canonical signature has the `*` prefix in `QualifiedName` (read via the fake writer's captured signature).
- [ ] Add a sub-test `TestGoFixture_MemberAccessWrites` for a fixture method body `g.prefix = name` proving a `writes` edge to a `field` member.

### Dependencies
- phase-go-parser/stage-gotreesitterparser-implementation
- phase-go-parser/stage-register-go-parser-in-parsers-cgo-go

### Test Scenarios
- [ ] Scenario: Go fixture node count -- Given the embedded fixture, When `EmitFile` runs under CGO=on, Then 1 class + 2 method + 1 package nodes and 3 contains + 1 static_calls + 1 imports edges are emitted.
- [ ] Scenario: Pointer receiver QualifiedName has `*` -- Given the same fixture, When the test reads the captured `*Greeter.Greet` method signature, Then it contains the substring `#*Greeter.Greet(`.
- [ ] Scenario: Member write -- Given a fixture method body `g.prefix = name`, When `EmitFile` runs, Then exactly one `writes` edge from the method to a `field` member named `prefix` is emitted.

# Phase 3: C and Cpp parsers

## Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits

## Stage 3.1: cTreeSitterParser implementation

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_treesitter_c.go` with `//go:build cgo`; declare `cTreeSitterParser`, `NewTreeSitterCParser()`, `Language()="c"`, `Extensions()=[]string{".c",".h"}` (per pinned `dot-h-extension-routing`).
- [ ] Walk `translation_unit` -> visit each named child; emit `ClassDecl{Kind:"struct"|"union"|"enum", QualifiedName:<identifier>}` for `struct_specifier` / `union_specifier` / `enum_specifier` with a non-empty body; skip anonymous structs.
- [ ] Emit `MethodDecl{EnclosingClass:"", QualifiedName:<declarator id>, ParamSignature:<between outer parens>, BodySource:<brace-stripped>}` for `function_definition`; skip body-less `function_declarator` prototype declarations (tech spec Section 5.1).
- [ ] Collect modifiers from sibling `storage_class_specifier` / `type_qualifier`: emit lower-case tokens `static`, `inline`, `extern`, `const`.
- [ ] Walk body for `call_expression` with `identifier` function -> append to `Calls` via `uniqueStringsInsert`; skip function-pointer calls and member-access chains in v1.
- [ ] Emit `Import` per `preproc_include`: `<...>` operand -> `{Module:<bracketed path>}`; `"..."` operand -> `{Module:"./"+<quoted path>}` so `isRelativeImport` drops it (C10).
- [ ] Add `Hello` / `Greeter` fixture sourcing data for the Stage 3.4 test (embed it inline in the test file -- documented here for traceability).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Build under CGO=on -- Given `CGO_ENABLED=1`, When `go build ./internal/repoindexer/ast/...` runs from `services/agent-memory` (so the `parser_treesitter_c.go` file participates), Then it succeeds.
- [ ] Scenario: C struct + free function -- Given a C source string with `struct Greeter { int n; };` and `int greet(int n) { return n; }`, When `cTreeSitterParser.Parse` runs, Then `ParseResult.Classes` has one entry `{QualifiedName:"Greeter", Kind:"struct"}` and `ParseResult.Methods` has one entry `{QualifiedName:"greet", ParamSignature:"int n"}`.
- [ ] Scenario: Relative include dropped -- Given `#include "local.h"`, When the dispatcher's Pass 0 runs against the parsed result, Then zero `imports` edges are emitted for that include (the `./local.h` module passes `isRelativeImport`).

## Stage 3.2: cppTreeSitterParser implementation

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_treesitter_cpp.go` with `//go:build cgo`; declare `cppTreeSitterParser`, `NewTreeSitterCppParser()`, `Language()="cpp"`, `Extensions()=[]string{".cc",".cpp",".cxx",".c++",".hpp",".hh",".hxx",".h++"}`.
- [ ] Walk `translation_unit`; recurse into `namespace_definition` accumulating the namespace path via a `container` parameter (mirror the TS walker's `outer` argument).
- [ ] Emit `ClassDecl` for `class_specifier` / `struct_specifier`: `QualifiedName=<namespace+"."+name>`; capture `base_class_clause` entries into `Extends` and populate `LangMeta["base_access"][baseName]=<access specifier>` and `LangMeta["template_params"]=[...]` for templated classes.
- [ ] Emit in-class `MethodDecl{EnclosingClass:<class qualified name>, QualifiedName:<class>.<method>}` for `function_definition` inside the class body; emit out-of-line definitions for `function_definition` at namespace scope whose declarator is a `qualified_identifier`.
- [ ] Add a final dedupe pass within `Parse` that collapses pairs of `MethodDecl` with the same `QualifiedName`: body-present wins; later source order wins on tie (tech spec Section 5.2 / Section 9 R1).
- [ ] Walk body for `call_expression` with `identifier` or rightmost segment of `qualified_identifier` function -> `Calls`; `field_expression` with `this` or `(*this)` receiver -> `ReceiverCalls`; `field_expression` with `this` outside any call -> `MemberAccesses` with `IsWrite` from LHS of `assignment_expression`; drop `operator_name` calls.
- [ ] Emit `Import` per `preproc_include`: `<...>` -> `{Module:<bracketed path>}`; `"..."` -> `{Module:"./"+<path>}`; emit `Import` per C++20 `import_declaration` with `{Module:<module name>}`.
- [ ] Collect modifiers `static`, `virtual`, `inline`, `constexpr`, `noexcept`, `const` (trailing) from children listed before the declarator plus the trailing `type_qualifier`.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Build under CGO=on -- Given `CGO_ENABLED=1`, When `go build ./internal/repoindexer/ast/...` runs from `services/agent-memory` (so the `parser_treesitter_cpp.go` file participates), Then it succeeds.
- [ ] Scenario: Class + base + in-class method -- Given `class Greeter : public Base { void greet() {} };`, When parsed, Then one `ClassDecl{QualifiedName:"Greeter", Extends:["Base"], LangMeta:{"base_access":{"Base":"public"}}}` and one `MethodDecl{QualifiedName:"Greeter.greet", EnclosingClass:"Greeter"}` are emitted.
- [ ] Scenario: In-class declaration + out-of-line definition dedupe -- Given `class Foo { void bar(); }; void Foo::bar() { log(); }`, When parsed, Then `ParseResult.Methods` contains exactly one `Foo.bar` entry and that entry's `BodySource` is non-empty.

## Stage 3.3: Register C and Cpp parsers in parsers_cgo.go

### Implementation Steps
- [ ] Append `NewTreeSitterCParser()` and `NewTreeSitterCppParser()` to the `defaultParsers()` return slice in `parsers_cgo.go` (in the documented order so the test `TestDispatcher_DotHRoutesToC_EvenWithCppHint` is deterministic).
- [ ] Leave `parsers_nocgo.go` unchanged for C / C++ (intentional no_parser skip under CGO=off per architecture Section 3).

### Dependencies
- phase-c-and-cpp-parsers/stage-ctreesitterparser-implementation
- phase-c-and-cpp-parsers/stage-cpptreesitterparser-implementation

### Test Scenarios
- [ ] Scenario: `.c` routes to C -- Given the dispatcher under CGO=on, When `selectParser("foo.c", nil)` runs, Then the returned parser's `Language() == "c"`.
- [ ] Scenario: `.cpp` routes to C++ -- Given the same dispatcher, When `selectParser("foo.cpp", nil)` runs, Then the returned parser's `Language() == "cpp"`.
- [ ] Scenario: `.h` routes to C unconditionally -- Given `selectParser("foo.h", nil)` and `selectParser("foo.h", []string{"cpp"})`, When each call runs, Then both return the C parser (pinned `dot-h-extension-routing`).

## Stage 3.4: C fixture test

### Implementation Steps
- [ ] Create `parser_treesitter_c_test.go` with `//go:build cgo`; add `TestCFixture_EmitsExpectedNodeAndEdgeSet`.
- [ ] Embed a fixture string with: `struct Greeter { int n; };`, free function `int greet(int n) { return format_greeting(n); }`, free function `int format_greeting(int n) { return n + 1; }`, `#include <stdio.h>`, and `#include "local.h"`.
- [ ] Assert: 1 class node `Greeter` (`Kind="struct"`); 2 method nodes (`greet`, `format_greeting`); 3 contains edges (file -> Greeter, file -> greet, file -> format_greeting); 1 static_calls edge (`greet` -> `format_greeting`); 1 imports edge (file -> `stdio.h` package); 0 imports edges for `./local.h`.

### Dependencies
- phase-c-and-cpp-parsers/stage-register-c-and-cpp-parsers-in-parsers-cgo-go

### Test Scenarios
- [ ] Scenario: C fixture node + edge count -- Given the embedded C fixture, When `EmitFile` runs under CGO=on, Then 1 class + 2 method + 1 package nodes and 3 contains + 1 static_calls + 1 imports edges are emitted.
- [ ] Scenario: Relative include dropped at dispatcher -- Given the same fixture, When `EmitFile` runs, Then zero `imports` edges target a package node whose module starts with `./`.

## Stage 3.5: Cpp fixture test

### Implementation Steps
- [ ] Create `parser_treesitter_cpp_test.go` with `//go:build cgo`; add `TestCppFixture_EmitsExpectedNodeAndEdgeSet`.
- [ ] Embed a fixture: `class Base { public: void identify() {} }; class Greeter : public Base { public: void greet() { this->identify(); log_global(); } }; void log_global() {}` plus `#include <string>` and `#include "base.h"`.
- [ ] Assert: 2 class nodes (`Base`, `Greeter`); 3 method nodes (`Base.identify`, `Greeter.greet`, `log_global`); 1 extends edge (`Greeter` -> `Base`); exactly 1 `static_calls` edge (`Greeter.greet` -> `log_global`). The `this->identify()` call resolves through the Pass 2b receiver-qualified path against `methodNodeID["Greeter.identify"]`; because `identify` is declared only on `Base` (different class in the same file), the lookup misses and the edge is correctly dropped per A4. The verbatim name persists on `attrs_json["calls_raw"]` for the future cross-file resolver.
- [ ] Add a sub-test for the dedupe rule: fixture `class Foo { void bar(); }; void Foo::bar() { log_global(); }` -> exactly one `Foo.bar` method node whose `static_calls` edge targets `log_global`.
- [ ] Add a sub-test for `LangMeta["base_access"]={"Base":"public"}` on the `Greeter` class node's `attrs_json`.

### Dependencies
- phase-c-and-cpp-parsers/stage-register-c-and-cpp-parsers-in-parsers-cgo-go

### Test Scenarios
- [ ] Scenario: C++ fixture node + edge baseline -- Given the embedded C++ fixture, When `EmitFile` runs under CGO=on, Then 2 class + 3 method + 1 package nodes and 5 contains + 1 extends + 1 static_calls + 1 imports edges are emitted (relative include dropped).
- [ ] Scenario: Dedupe collapses declaration + definition -- Given the embedded dedupe fixture, When `EmitFile` runs, Then exactly one `Foo.bar` method node exists with non-empty `BodySource` and exactly one `static_calls` edge to `log_global`.
- [ ] Scenario: base_access attrs persist -- Given the inheritance fixture, When `EmitFile` runs, Then the `Greeter` class node's `attrs_json["base_access"]["Base"] == "public"`.

# Phase 4: CSharp parser

## Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits

## Stage 4.1: csharpTreeSitterParser implementation

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_treesitter_csharp.go` with `//go:build cgo`; declare `csharpTreeSitterParser`, `NewTreeSitterCSharpParser()`, `Language()="csharp"`, `Extensions()=[]string{".cs",".csx"}`.
- [ ] Walk `compilation_unit`; recurse into `namespace_declaration` / `file_scoped_namespace_declaration` accumulating the namespace into `LangMeta["namespace"]`.
- [ ] Emit `ClassDecl{Kind:<class|interface|struct|record|enum>}` for the matching `*_declaration` nodes; populate `LangMeta["partial"]=true` when a `modifier` child equals `partial`.
- [ ] Implement the same-file two-pass base-list partition described in tech spec Section 5.3: Pass A walks the file to build `localKind map[string]string` keyed by simple type name; Pass B partitions each `base_list` per the decision matrix (`class` declaring kind splits position 0 by `localKind[entry]`; `interface` declaring kind sends every entry to `Extends`; `struct` / `record` declaring kind sends every entry to `Implements`). Verbatim raw list always persists on `LangMeta["base_raw"]`.
- [ ] Emit `MethodDecl` for `method_declaration` inside type body and `constructor_declaration` (`QualifiedName=<class>.<class>`); skip `accessor_declaration` but record property names so member-access resolution can recognise them.
- [ ] Walk body for `invocation_expression` with `identifier` function -> `Calls`; `member_access_expression` with `this` -> `ReceiverCalls`; `member_access_expression` with `this` outside a call -> `MemberAccesses` (LHS of `assignment_expression` -> `IsWrite`).
- [ ] Emit `Import` for `using_directive`: plain -> `{Module:<qualified name>}`; `using static` -> `{Module,LangMeta:{"is_static":true}}`; alias -> `{Module:<rhs>,Alias:<lhs>}`.
- [ ] Collect modifiers `public`, `private`, `protected`, `internal`, `static`, `async`, `override`, `virtual`, `sealed`, `abstract`, `readonly`, `extern`, `partial`, `unsafe`.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Build under CGO=on -- Given `CGO_ENABLED=1`, When `go build ./internal/repoindexer/ast/...` runs from `services/agent-memory` (so the `parser_treesitter_csharp.go` file participates), Then it succeeds.
- [ ] Scenario: Class with same-file interface implements -- Given `interface IFoo {} class Foo : IFoo {}`, When parsed, Then the `Foo` ClassDecl has `Extends=[]`, `Implements=["IFoo"]`, and `LangMeta["base_raw"]=["IFoo"]`.
- [ ] Scenario: Class with same-file class extends -- Given `class Bar {} class Foo : Bar {}`, When parsed, Then the `Foo` ClassDecl has `Extends=["Bar"]`, `Implements=[]`, and `LangMeta["base_raw"]=["Bar"]`.
- [ ] Scenario: Mixed same-file partition -- Given `class Bar {} interface IBaz {} class Foo : Bar, IBaz {}`, When parsed, Then `Foo.Extends=["Bar"]` and `Foo.Implements=["IBaz"]`.

## Stage 4.2: Register CSharp parser in parsers_cgo.go

### Implementation Steps
- [ ] Append `NewTreeSitterCSharpParser()` to the `defaultParsers()` slice in `parsers_cgo.go`.
- [ ] Leave `parsers_nocgo.go` unchanged for C# (no_parser skip under CGO=off).

### Dependencies
- phase-csharp-parser/stage-csharptreesitterparser-implementation

### Test Scenarios
- [ ] Scenario: `.cs` routes to C# -- Given the dispatcher under CGO=on, When `selectParser("foo.cs", nil)` runs, Then `Language() == "csharp"`.
- [ ] Scenario: `.csx` script routes to C# -- Given the dispatcher under CGO=on, When `selectParser("foo.csx", nil)` runs, Then `Language() == "csharp"`.

## Stage 4.3: CSharp fixture test

### Implementation Steps
- [ ] Create `parser_treesitter_csharp_test.go` with `//go:build cgo`; add `TestCSharpFixture_EmitsExpectedNodeAndEdgeSet`.
- [ ] Embed a fixture: `namespace Demo; interface IGreeter { string Greet(string name); } class Base { public string Identify() => "base"; } class HelloWorld : Base, IGreeter { public string Greet(string name) { return FormatGreeting(this.prefix, name); } private static string FormatGreeting(string prefix, string name) => prefix + name; private string prefix = "hi"; }` plus `using System;`.
- [ ] Assert: 3 class/interface nodes (`IGreeter`, `Base`, `HelloWorld`); 4 method nodes (`IGreeter.Greet`, `Base.Identify`, `HelloWorld.Greet`, `HelloWorld.FormatGreeting`); 1 extends edge (`HelloWorld` -> `Base`); 1 implements edge (`HelloWorld` -> `IGreeter`); 1 static_calls edge (`HelloWorld.Greet` -> `HelloWorld.FormatGreeting`); 1 imports edge (file -> `System` package).
- [ ] Add `TestCSharpFixture_BaseListPartitionsByLocalKind` covering the six rows of tech spec Section 5.3's decision matrix (single-class, single-interface, mixed, interface-only, cross-file class, mixed cross-file).
- [ ] Add a sub-test asserting `LangMeta["namespace"]=="Demo"` and `LangMeta["partial"]==true` for a `partial class Foo` fragment.

### Dependencies
- phase-csharp-parser/stage-register-csharp-parser-in-parsers-cgo-go

### Test Scenarios
- [ ] Scenario: C# fixture node + edge count -- Given the fixture, When `EmitFile` runs under CGO=on, Then 3 class + 4 method + 1 package nodes are emitted along with 1 extends + 1 implements + 1 static_calls + 1 imports edge.
- [ ] Scenario: Base-list partition decision matrix -- Given the six fixtures from tech spec Section 5.3, When each parses, Then the resulting `Extends` / `Implements` lists match the table verbatim.
- [ ] Scenario: Partial class flag -- Given `partial class Foo {}`, When parsed, Then `ClassDecl.LangMeta["partial"] == true`.

# Phase 5: Rust parser

## Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits

## Stage 5.1: rustTreeSitterParser implementation

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_treesitter_rust.go` with `//go:build cgo`; declare `rustTreeSitterParser`, `NewTreeSitterRustParser()`, `Language()="rust"`, `Extensions()=[]string{".rs"}`.
- [ ] Walk `source_file`; recurse into `mod_item` (in-file modules) without propagating the module name into `QualifiedName` in v1.
- [ ] Emit `ClassDecl` for `struct_item` (Kind="struct"), `enum_item` (Kind="enum"), `trait_item` (Kind="trait"; supertrait clause `: A + B` populates `Extends`); skip enum variants as Methods.
- [ ] Emit `MethodDecl` for `function_item` inside `trait_item` body (`LangMeta["trait_default"]=true`) and for `function_signature_item` inside `trait_item` (body empty); emit `MethodDecl` for `function_item` inside `impl_item` with NO trait clause (`EnclosingClass=<Type>`); emit `MethodDecl` for `function_item` inside `impl_item` WITH a trait clause (`EnclosingClass=<Type>, LangMeta["trait"]=<TraitName>`) AND append `TraitName` to the Type class's `Implements`.
- [ ] Emit `MethodDecl{EnclosingClass:""}` for free `function_item` at file scope.
- [ ] Walk body for `call_expression`: `identifier` or rightmost of `scoped_identifier` -> `Calls`; `field_expression` with `self` -> `ReceiverCalls`; non-`self` method-call expressions are NOT collected in v1 (receiver type not statically known); `field_expression` with `self` outside `call_expression` -> `MemberAccesses`.
- [ ] Emit `Import` per `use_declaration`: single -> `{Module:<path WITHOUT last segment>, Symbols:[<last>]}`; list -> one or more Imports same Module distinct Symbols; alias via `use_as_clause` -> `Alias`; glob `*` -> `Symbols:["*"]`.
- [ ] Collect modifiers `pub`, `pub(crate)`, `pub(super)`, `async`, `unsafe`, `const`, `extern` (lower-case tokens); skip `macro_invocation` (tech spec Section 5.5 explicit non-goal).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Build under CGO=on -- Given `CGO_ENABLED=1`, When `go build ./internal/repoindexer/ast/...` runs from `services/agent-memory` (so the `parser_treesitter_rust.go` file participates), Then it succeeds.
- [ ] Scenario: Trait + impl method emit -- Given `trait Greeter { fn greet(&self) -> String { String::new() } } struct G; impl Greeter for G { fn greet(&self) -> String { String::from("hi") } }`, When parsed, Then `Methods` contains both `Greeter.greet` (`LangMeta["trait_default"]==true`) and `G.greet` (`LangMeta["trait"]=="Greeter"`); `G.Implements==["Greeter"]`.
- [ ] Scenario: Supertrait extends -- Given `trait A {} trait B: A {}`, When parsed, Then the `B` ClassDecl has `Extends==["A"]`.

## Stage 5.2: Register Rust parser in parsers_cgo.go

### Implementation Steps
- [ ] Append `NewTreeSitterRustParser()` to the `defaultParsers()` slice in `parsers_cgo.go`.
- [ ] Leave `parsers_nocgo.go` unchanged for Rust (no_parser skip under CGO=off).

### Dependencies
- phase-rust-parser/stage-rusttreesitterparser-implementation

### Test Scenarios
- [ ] Scenario: `.rs` routes to Rust -- Given the dispatcher under CGO=on, When `selectParser("foo.rs", nil)` runs, Then `Language() == "rust"`.
- [ ] Scenario: `.rs` skipped under CGO=off -- Given the dispatcher constructed via `defaultParsers()` under CGO=off, When `EmitFile` processes a `.rs` file, Then `ast.dispatch.skip{reason:"no_parser"}` fires and no Nodes / Edges are written.

## Stage 5.3: Rust fixture test including Pass 2d overrides

### Implementation Steps
- [ ] Create `parser_treesitter_rust_test.go` with `//go:build cgo`; add `TestRustFixture_EmitsExpectedNodeAndEdgeSet`.
- [ ] Embed a fixture: `use std::fmt::Display; trait Greeter { fn greet(&self, name: &str) -> String { String::new() } } struct GreeterImpl; impl Greeter for GreeterImpl { fn greet(&self, name: &str) -> String { format_greeting(name) } } pub fn format_greeting(name: &str) -> String { String::from(name) }`.
- [ ] Assert: 2 class nodes (`Greeter` trait, `GreeterImpl` struct); 3 method nodes (`Greeter.greet`, `GreeterImpl.greet`, `format_greeting`); 1 implements edge (`GreeterImpl` -> `Greeter`); 1 static_calls edge (`GreeterImpl.greet` -> `format_greeting`); 1 imports edge (file -> `std::fmt` package with `Symbols=["Display"]`).
- [ ] Add `TestRustFixture_OverridesEdgeFromImplToTraitDefault` asserting exactly one `overrides` edge from `GreeterImpl.greet` to `Greeter.greet` (Pass 2d emission per architecture Section 7.2).
- [ ] Add a sub-test that the cross-file miss path is silent: fixture has only the impl (no trait declaration in the same file) and `LangMeta["trait"]="Greeter"` -> zero `overrides` edges emitted.

### Dependencies
- phase-rust-parser/stage-rusttreesitterparser-implementation
- phase-rust-parser/stage-register-rust-parser-in-parsers-cgo-go

### Test Scenarios
- [ ] Scenario: Rust fixture node + edge count -- Given the fixture, When `EmitFile` runs under CGO=on, Then 2 class + 3 method + 1 package nodes are emitted with 1 implements + 1 static_calls + 1 imports + 1 overrides edge.
- [ ] Scenario: Pass 2d overrides same-file emission -- Given the trait + impl fixture, When Pass 2d runs, Then exactly one edge of kind `"overrides"` from `GreeterImpl.greet` to `Greeter.greet` is emitted.
- [ ] Scenario: Cross-file overrides miss is silent -- Given an impl-only fixture with `LangMeta["trait"]="Greeter"` but no `Greeter.greet` in the same file's `methodNodeID`, When Pass 2d runs, Then zero `overrides` edges are emitted and `attrs_json["trait"]=="Greeter"` persists on the impl method node.

# Phase 6: PowerShell parser

## Dependencies
- phase-shared-additive-surfaces-and-dispatcher-edits

## Stage 6.1: powershellParser subprocess implementation

### Implementation Steps
- [ ] Create `services/agent-memory/internal/repoindexer/ast/parser_powershell.go` with NO build tags; declare `powershellParser{pwshBin string}`, constructor `NewPowerShellParser()` that resolves `pwshBin` via `exec.LookPath("pwsh")`, `Language()="powershell"`, `Extensions()=[]string{".ps1",".psm1",".psd1"}`.
- [ ] When `pwshBin == ""`, return `fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ErrParserUnavailable)` from `Parse` so the dispatcher Pass-in-EmitFile branch logs `ast.dispatch.skip{reason:"pwsh_not_available"}` (Phase 1 Stage 1.4).
- [ ] Embed the extraction script (multi-line raw string) modelled on `Ast.PowerShell.PowerShellAstParser.ExtractNodes`: call `[System.Management.Automation.Language.Parser]::ParseInput`, walk `FunctionDefinitionAst`, `TypeDefinitionAst`, `ParamBlockAst`, `ScriptBlockAst`, plus `PowerShellHyperedgeExtractor`-style import constructs, emitting JSON `{"functions":[...],"types":[...],"imports":[...]}` to stdout.
- [ ] Invoke `pwsh -NoProfile -NonInteractive -Command -` via `os/exec` piping `src` to stdin under a `context.WithTimeout(parentCtx, 10*time.Second)`; unmarshal stdout into the documented JSON shape; on non-zero exit or timeout, return the unwrapped error so `safeParse` logs `ast.parse.error`.
- [ ] Map JSON: `types[]` (class) -> `ClassDecl{Kind:"class", QualifiedName:<name>, Extends:BaseTypes[0:1] when non-empty}`; `types[]` with `IsEnum=true` -> `ClassDecl{Kind:"enum"}`; `types[].Methods[]` -> `MethodDecl{EnclosingClass:<class>, QualifiedName:<class>.<method>}`; `functions[]` -> `MethodDecl{EnclosingClass:""}`.
- [ ] Map JSON imports: `Import-Module Foo` -> `{Module:"Foo", LangMeta:{"cmdlet_verb":"Import","module_kind":"Import-Module"}}`; `using module Foo` -> `{Module:"Foo", LangMeta:{"cmdlet_verb":"using","module_kind":"using_module"}}`; `. ./helpers.ps1` -> `{Module:"./helpers.ps1", LangMeta:{"cmdlet_verb":".","module_kind":"dot_source"}}` (relative -> dropped by `isRelativeImport`).
- [ ] Walk the `functions[]` / class method bodies for command-name calls (`Get-Foo`, `Invoke-Bar`) at statement position -> `Calls`; `$this.X(...)` inside class methods -> `ReceiverCalls`; `$this.X` outside calls -> `MemberAccesses` (`IsWrite` from LHS of `=`).
- [ ] Emit `static` / `hidden` modifiers for class methods when present; top-level functions emit no modifiers.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Build under both CGO=on and CGO=off -- Given the file has no build tags, When `go build ./internal/repoindexer/ast/...` runs from `services/agent-memory` under both `CGO_ENABLED=1` and `CGO_ENABLED=0`, Then both succeed.
- [ ] Scenario: pwsh missing returns sentinel -- Given a `powershellParser{pwshBin:""}`, When `Parse("foo.ps1", []byte("function Foo {}"))` runs, Then it returns `(ParseResult{}, err)` where `errors.Is(err, ErrParserUnavailable)` is true.
- [ ] Scenario: pwsh timeout returns error not sentinel -- Given a fake `pwshBin` that sleeps longer than the timeout, When `Parse` runs, Then it returns a non-nil error and `errors.Is(err, ErrParserUnavailable)` is false (so `safeParse` logs `ast.parse.error` per Section 6.4).

## Stage 6.2: Register PowerShell parser in both build tag files

### Implementation Steps
- [ ] Append `NewPowerShellParser()` to the `defaultParsers()` return slice in `parsers_cgo.go`.
- [ ] Append `NewPowerShellParser()` to the `defaultParsers()` return slice in `parsers_nocgo.go` (PowerShell is the one language that registers under BOTH build tags because the subprocess approach is build-tag agnostic).

### Dependencies
- phase-powershell-parser/stage-powershellparser-subprocess-implementation

### Test Scenarios
- [ ] Scenario: `.ps1` routes to PowerShell under CGO=on -- Given the dispatcher constructed via `defaultParsers()` under CGO=on, When `selectParser("foo.ps1", nil)` runs, Then `Language() == "powershell"`.
- [ ] Scenario: `.ps1` routes to PowerShell under CGO=off -- Given the dispatcher constructed via `defaultParsers()` under CGO=off, When `selectParser("foo.ps1", nil)` runs, Then `Language() == "powershell"`.
- [ ] Scenario: pwsh-not-available logs skip-not-error -- Given a host without `pwsh` on PATH, When `EmitFile` processes a `.ps1` file, Then `ast.dispatch.skip{reason:"pwsh_not_available"}` fires (not `ast.parse.error`) and `EmitFile` returns `(EmitResult{}, nil)`.

## Stage 6.3: PowerShell fixture test

### Implementation Steps
- [ ] Create `parser_powershell_test.go` (no build tags); the top of each test calls `if _, err := exec.LookPath("pwsh"); err != nil { t.Skip("pwsh not on PATH") }` so PowerShell-less CI stays green.
- [ ] Add `TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet`: embed a fixture `class Greeter { [string] $Prefix; [string] Format([string]$name) { return "$($this.Prefix) $name" } [string] Greet([string]$name) { return $this.Format($name) } } function Format-Hello { param([string]$Name) return "hi $Name" } Import-Module Foo` and assert: 1 class node (`Greeter`); 3 method nodes (`Greeter.Format`, `Greeter.Greet`, `Format-Hello`); 1 contains edge per node + file; 1 imports edge to `Foo`; `Greeter.Greet`'s `static_calls` to `Greeter.Format` resolves through the `$this.Format(...)` receiver-qualified path covered by the Stage 6.1 `$this.X(...)` extractor (tech-spec Section 5.6). Note: `[Greeter]::Format(...)` static class invocations are explicitly out of v1 scope; the fixture uses an instance receiver call instead.
- [ ] Add `TestPowerShellParser_NoPwsh_ReturnsSentinel` that constructs `&powershellParser{pwshBin:""}` directly and asserts `errors.Is(err, ErrParserUnavailable)` is true.
- [ ] Add `TestPowerShellFixture_DotSourceDropped` asserting `. ./helpers.ps1` produces zero `imports` edges (the `./helpers.ps1` module trips `isRelativeImport`) but `LangMeta["module_kind"]=="dot_source"` flows into the parser's emission.

### Dependencies
- phase-powershell-parser/stage-powershellparser-subprocess-implementation
- phase-powershell-parser/stage-register-powershell-parser-in-both-build-tag-files

### Test Scenarios
- [ ] Scenario: pwsh-present fixture parses -- Given a host with `pwsh` on PATH, When the fixture runs through `EmitFile`, Then 1 class + 3 method + 1 package nodes are emitted along with the expected contains / static_calls / imports edges.
- [ ] Scenario: pwsh-absent fixture is skipped -- Given a host without `pwsh` on PATH, When `TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet` runs, Then it calls `t.Skip` and reports no failure.
- [ ] Scenario: Sentinel-returning parser -- Given `&powershellParser{pwshBin:""}`, When `Parse` runs against any input, Then `errors.Is(err, ErrParserUnavailable) == true`.

# Phase 7: Cross-cutting tests, documentation, validation

## Dependencies
- phase-go-parser
- phase-c-and-cpp-parsers
- phase-csharp-parser
- phase-rust-parser
- phase-powershell-parser

## Stage 7.1: Cross-language dispatcher tests

### Implementation Steps
- [ ] Extend `services/agent-memory/internal/repoindexer/ast/dispatcher_test.go` (under `//go:build cgo` if it references CGO-only parsers, otherwise unconditional) with `TestDispatcher_RoutesByExtension` asserting `selectParser` returns the correct `Language()` for `.c`, `.h`, `.cpp`, `.cs`, `.go`, `.rs`, `.ps1`, `.psm1`.
- [ ] Add `TestDispatcher_DotHRoutesToC_EvenWithCppHint` asserting `selectParser("a.h", []string{"cpp"}).Language() == "c"` (pinned per `dot-h-extension-routing`).
- [ ] Add `TestDispatcher_NoParserForUnknown` asserting `.foo` returns `nil` from `selectParser` and that `EmitFile` logs `ast.dispatch.skip{reason:"no_parser"}` with zero writer calls.
- [ ] Add `TestDispatcher_DuplicateExtensionLastWins` constructing a dispatcher via `WithParsers(parserA, parserB)` where both claim `.go` and asserting the returned parser is `parserB` (matches `buildExtMap` last-write-wins contract).
- [ ] Add `TestDispatcher_GoMultimapDropsOnReceiverCollision` and `TestDispatcher_GoMultimapResolvesPointerReceiverAlone` (test cases match the scenarios listed in Phase 1 Stage 1.4 but run end-to-end through `EmitFile` against a Go fixture).
- [ ] Add `TestDispatcher_ErrParserUnavailable_LogsSkip` using a mock parser whose `Parse` always returns the wrapped sentinel; assert the logged `reason` and that `EmitFile` returns `(EmitResult{}, nil)`.
- [ ] Add `TestDispatcher_LangMetaMergePreservesFirstClassKeys` ensuring a parser populating `LangMeta["language"]="bogus"` does NOT override the dispatcher's first-class `language` attr (C11).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Every new extension routes to its parser -- Given the dispatcher under CGO=on, When `selectParser` runs for each of `.c`, `.h`, `.cpp`, `.cxx`, `.cs`, `.go`, `.rs`, `.ps1`, `.psm1`, `.psd1`, Then it returns a non-nil parser with the expected `Language()` value.
- [ ] Scenario: `.h` pinning under CGO=on -- Given the dispatcher under CGO=on, When `selectParser("foo.h", []string{"cpp"})` runs, Then the returned parser's `Language() == "c"`.
- [ ] Scenario: Duplicate registration last-wins -- Given two stub parsers both claiming `.go`, When `selectParser("x.go", nil)` runs against a dispatcher built with `WithParsers(parserA, parserB)`, Then the returned parser is `parserB`.
- [ ] Scenario: Multimap collision drops end-to-end -- Given a Go fixture with `func (r Foo) Bar()` and `func (r *Foo) Bar()` both invoked via `r.Bar()` from a sibling, When `EmitFile` runs, Then zero `static_calls` edges target `Bar` and verbatim `Bar` persists on `calls_raw`.
- [ ] Scenario: Multimap pointer-only resolves end-to-end -- Given a Go fixture with only the pointer-receiver method, When `EmitFile` runs, Then exactly one `static_calls` edge from the sibling to `*Foo.Bar` is emitted.
- [ ] Scenario: ErrParserUnavailable surfaces as skip-not-error -- Given a stub parser returning the wrapped sentinel for `.xx`, When `EmitFile` processes an `.xx` file, Then the log key is `ast.dispatch.skip` with `reason="<stub slug>"` (NOT `ast.parse.error`).
- [ ] Scenario: First-class attr key cannot be overridden -- Given a parser populates `LangMeta["language"]="bogus"`, When `methodAttrs` writes the result, Then `attrs_json["language"]` equals the dispatcher's first-class value.

## Stage 7.2: Documentation -- support matrix update

### Implementation Steps
- [ ] Update `.claude/context/tests.md` adding a support-matrix table per architecture Section 8.5: rows for TypeScript / JavaScript, Python, C, C++, C#, Go, Rust, PowerShell; columns `CGO=on (production)`, `CGO=off (make test portable)`, `Notes`.
- [ ] Document explicitly that C / C++ / C# / Go / Rust require CGO and skip with `ast.dispatch.skip{reason:"no_parser"}` under CGO=off.
- [ ] Document that PowerShell requires `pwsh` on PATH (either build tag) and skips with `ast.dispatch.skip{reason:"pwsh_not_available"}` when absent.
- [ ] Document the `.h` -> C unconditional routing rule and point to the future per-repo extension-override workstream (architecture Section 5.2 / 10).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: tests.md has the support matrix -- Given the edited `.claude/context/tests.md`, When a grep for `Language | CGO=on` is run against the file, Then it matches a non-empty line in the new matrix.
- [ ] Scenario: tests.md lists the pwsh-not-available skip key -- Given the edited file, When grep for `pwsh_not_available` runs, Then it matches at least one line.

## Stage 7.3: Validation -- targeted and full service suite

### Implementation Steps
- [ ] Run `go test ./internal/repoindexer/ast -count=1` from `services/agent-memory` under `CGO_ENABLED=1`; assert all new parser fixture tests + cross-language dispatcher tests pass.
- [ ] Run `go test ./internal/repoindexer/ast -count=1` from `services/agent-memory` under `CGO_ENABLED=0`; assert the existing TS / Python fallback tests pass and the new C / C++ / C# / Go / Rust files fall through to the `no_parser` skip path without panic.
- [ ] Run `go test ./...` from `services/agent-memory` under `CGO_ENABLED=1`; assert the full service suite passes.
- [ ] Run `make lint` from `services/agent-memory` (existing target) and resolve any lint findings introduced by the new files.

### Dependencies
- phase-cross-cutting-tests-documentation-validation/stage-cross-language-dispatcher-tests
- phase-cross-cutting-tests-documentation-validation/stage-documentation-support-matrix-update

### Test Scenarios
- [ ] Scenario: Targeted AST tests pass under CGO=on -- Given `CGO_ENABLED=1`, When `go test ./internal/repoindexer/ast -count=1` runs, Then the exit code is 0.
- [ ] Scenario: Targeted AST tests pass under CGO=off -- Given `CGO_ENABLED=0`, When `go test ./internal/repoindexer/ast -count=1` runs, Then the exit code is 0 and the new tree-sitter parser tests are excluded by build tags.
- [ ] Scenario: Full service suite passes -- Given `CGO_ENABLED=1`, When `go test ./...` runs from `services/agent-memory`, Then the exit code is 0.
- [ ] Scenario: Lint clean -- Given the edited tree, When `make lint` runs from `services/agent-memory`, Then it exits 0.
