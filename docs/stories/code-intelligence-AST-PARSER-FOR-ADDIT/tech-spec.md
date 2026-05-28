# AST Parser for Additional Languages -- Tech Spec

> Story: `code-intelligence:AST-PARSER-FOR-ADDIT` -- 13 points
> Companion docs (drafted in parallel): `architecture.md`,
> `implementation-plan.md`, `e2e-scenarios.md`. This file owns
> the problem statement, scope, hard constraints, the
> authoritative per-language extraction-rule tables, the
> ops/threshold values, and the risk register. The architecture
> doc owns components, data-model envelopes, and sequence
> contracts; the implementation plan owns file lists and step
> order; the e2e doc owns operator-visible scenarios.

## 1. Problem Statement

Stage 3.2 of the agent-memory ingest pipeline shipped a
polyglot AST dispatcher (`services/agent-memory/internal/
repoindexer/ast`) with two reference languages:

- **TypeScript / JavaScript** -- `tsTreeSitterParser`
  (CGO=on) plus `tsjsParser` scanner fallback (CGO=off).
- **Python** -- `pyTreeSitterParser` (CGO=on) plus
  `pythonParser` scanner fallback (CGO=off).

The dispatcher and writer contracts are LOCKED -- canonical
signatures, the two-pass insert protocol, the
`ParseResult{Classes, Methods, Imports}` envelope, the
`LanguageParser` interface, the `attrs_json` shape, and the
build-tag duality (`parsers_cgo.go` / `parsers_nocgo.go`)
are stable as of the Stage 3.2 release.

**The gap.** Real production repositories in scope for the
graph index are written in six additional source languages:
**C, C++, C#, Go, Rust, PowerShell**. Today the dispatcher
hits `extMap` misses for every `.c` / `.cpp` / `.cs` / `.go`
/ `.rs` / `.ps1` file and emits `ast.dispatch.skip{reason:
"no_parser"}`. As a result, every Class / Method / Block /
external-package Node and every `contains` / `extends` /
`implements` / `static_calls` / `reads` / `writes` /
`imports` Edge that should anchor those files is silently
absent from the graph -- a measurable coverage gap for any
consumer that asks "which method in this Go repo calls
`formatGreeting`?" or "which Rust trait does `GreeterImpl`
implement?". The agent-memory query layer (`agentapi`)
returns empty answers because no parse-time emission ever
ran for those files.

**What this story delivers.** Six new `LanguageParser`
implementations behind the existing dispatcher seam, plus
the extension registration, plus a minimal additive struct
surface (`LangMeta map[string]any` on `ClassDecl` /
`MethodDecl` / `Import`, plus `ReceiverAliases []string`
on `MethodDecl`) so per-language metadata flows into
`attrs_json` and Go pointer-receiver methods get
disambiguated canonical signatures and receiver-qualified
call resolution. One enum migration appends `overrides` to
`edge_kind` so Rust trait shadowing produces a typed edge.

**What this story does NOT redesign.** Nothing in
`dispatcher.go::emit`, `block.go::SubdivideMethod`, or
`repoindexer.ASTEmitter` is restructured. The new parsers
slot in via `defaultParsers()` registration; the architecture
doc enumerates the exact additive diff to `parser.go` and
the writer helpers in `dispatcher.go` (architecture.md
Section 2.2.1).

**Operator-pinned decisions in effect.** Four operator
answers from prior iterations are honoured verbatim:

| Decision id | Pinned answer | Effect on this spec |
| --- | --- | --- |
| `dot-h-extension-routing` | `.h` files route to C parser unconditionally; no override knob | Section 5.2, Section 8 R6 |
| `powershell-grammar-strategy` | follow in-house `Ast.PowerShell` reference; subprocess to `pwsh` | Section 5.6, Section 8 R5 |
| `go-receiver-pointer-fingerprint` | embed `*` in `QualifiedName` for pointer receivers; `EnclosingClass` stays bare | Section 5.4, Section 8 R2 |
| `rust-trait-overrides-edge` | add `overrides` to `edge_kind` enum; emit edge from impl method to trait default same-file | Section 5.5, Section 8 R4 |

## 2. Scope

### 2.1 In scope

The complete deliverable for this story:

- One `LanguageParser` implementation per new language:
  - C (`parser_treesitter_c.go`, `//go:build cgo`)
  - C++ (`parser_treesitter_cpp.go`, `//go:build cgo`)
  - C# (`parser_treesitter_csharp.go`, `//go:build cgo`)
  - Go (`parser_treesitter_go.go`, `//go:build cgo`)
  - Rust (`parser_treesitter_rust.go`, `//go:build cgo`)
  - PowerShell (`parser_powershell.go`, no build tags; pwsh
    subprocess strategy per Section 5.6)
- Extension registration in `parsers_cgo.go` (all six new
  parsers) and `parsers_nocgo.go` (PowerShell only -- the
  five tree-sitter parsers are CGO-only by design).
- Additive struct surface to `parser.go`: `LangMeta
  map[string]any` on `ClassDecl` / `MethodDecl` / `Import`;
  `ReceiverAliases []string` on `MethodDecl`. Exported
  sentinel `ErrParserUnavailable = errors.New("parser:
  runtime dependency unavailable")`.
- One merge helper in `dispatcher.go`
  (`mergeLangMeta(out, in map[string]any)`) called at the
  end of `classAttrs`, `methodAttrs`, `importEdgeAttrs`.
- One new dispatcher pass (`Pass 2d`) that emits same-file
  `overrides` edges from Rust impl methods to the trait
  default they shadow. No-op for every other language.
- One pre-Pass-2b setup change in `dispatcher.go`: replace
  the single-key `methodNodeID` lookup for
  receiver-qualified resolution with a multimap that honours
  `ReceiverAliases` and applies the existing
  drop-on-collision rule (A5) at set-size > 1.
- New branch in `dispatcher.go::EmitFile` that detects
  `errors.Is(err, ErrParserUnavailable)` after `safeParse`
  and logs `ast.dispatch.skip{reason:"pwsh_not_available"}`
  instead of `ast.parse.error`.
- One new SQL migration:
  `services/agent-memory/migrations/0022_edge_kind_overrides.sql`
  -- `ALTER TYPE edge_kind ADD VALUE 'overrides';` -- to
  support the Rust trait-overrides edge.
- Per-language hint aliases in `normalizeHints` (`c`, `cxx`,
  `cs`, `csharp`, `golang`, `rs`, `ps`, `ps1`, `psm1`).
- Fixture-driven tests per language (one fixture file each
  exercising class/type, method, free function, import,
  same-file static call, and inheritance / interface / trait
  edge where applicable), mirroring the existing
  `TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet`
  pattern in `parser_typescript_test.go` lines 11-100.
- Cross-language dispatcher tests: extension routing,
  unsupported-extension skip behaviour, duplicate
  registration determinism, `.h` -> C pinning even when
  `LanguageHints=["cpp"]`, multimap collision rule for Go
  receiver-qualified calls, `ErrParserUnavailable` skip-log
  path.
- Support-matrix entry appended to
  `.claude/context/tests.md` documenting which languages are
  CGO-on only, which are PowerShell-on-PATH gated, and the
  CGO=off skip semantics for the five tree-sitter-only
  languages.

### 2.2 Out of scope

Explicit non-deliverables for this story:

- **New `Node.kind` values.** Every per-language construct
  maps onto the existing `class` / `method` / `block` /
  `package` kinds. No `enum_value` or `module` kind is added.
- **Table-level schema migrations.** The single enum
  migration (`0022_edge_kind_overrides.sql`) for the Rust
  `overrides` edge value is the ONLY schema change.
  `attrs_json` is `jsonb` end-to-end (migration `0001`),
  so per-language `LangMeta` keys land without any column
  or type change.
- **Cross-file resolver for `extends` / `implements` /
  `static_calls` / `overrides` targets in another file.**
  Same-file resolution only (A4 inherited from Stage 3.2);
  cross-file stitching is a separate workstream. Verbatim
  unresolved names persist on `attrs_json["extends_raw"]` /
  `attrs_json["calls_raw"]` / `LangMeta["trait"]` so the
  future resolver has the data it needs.
- **C# `partial class` fragment unification.** Each partial
  file emits its own `ClassDecl`; the cross-file resolver
  will stitch them via a future `partial_of` edge. v1
  records `LangMeta["partial"]=true` for grouping.
- **C++ template instantiation tracking.** Template
  parameters are recorded in `LangMeta["template_params"]`
  for the class node but no per-instantiation Class /
  Method nodes are emitted -- the canonical signature is
  the template name itself.
- **Go interface satisfaction edges.** Go interfaces are
  structurally satisfied; no static `implements` extractor
  emits an edge in v1. `ClassDecl.Implements` stays empty
  for Go structs.
- **Rust macro expansion.** Tree-sitter sees the macro call
  node, not the expanded code. Methods or types declared
  only via macros (`#[derive(...)]` does not produce
  methods at the syntax level; `lazy_static!` blocks
  contain expressions) are NOT extracted in v1. Macro
  invocations themselves (`macro_invocation` nodes) are
  NOT collected on `MethodDecl.Calls` either -- the macro
  name is not a callable function identifier and the
  bare-name resolver would consistently miss; the
  authoritative rule is Section 5.5 "Macro invocation"
  row.
- **PowerShell tree-sitter grammar binding.** The operator
  pinned the subprocess approach via the official
  `System.Management.Automation.Language.Parser`. Adding a
  community `tree-sitter-powershell` binding is a follow-up
  workstream gated on either subprocess-overhead measurement
  or a deployment requirement to remove the `pwsh`
  dependency.
- **Performance benchmarking** of tree-sitter vs scanner.
  The CGO=off path skips the five tree-sitter languages
  outright (Section 5 build matrix); no comparative
  fingerprint stability tests run because the comparator
  doesn't exist.
- **A scanner fallback for C / C++ / C# / Go / Rust.** See
  Section 4.3 for the rationale (A2 stability + A7 build-tag
  duality + 6x package-surface inflation cost).
- **Editing `.claude/context/architecture.md`.** Per-language
  details belong in the story doc, not the repo-level context
  docs. The `.claude/context/tests.md` table is the ONLY
  context-doc surface this story touches.

### 2.3 Non-goals (will be REFUSED if requested)

Plans that violate the inherited Stage 3.2 contracts are not
acceptable in this story. Specifically:

- Cross-file resolution shortcuts. The dispatcher's
  same-file resolution (A4) MUST NOT be relaxed for any new
  language even if "obvious" cross-file targets (a Go method
  on a struct declared in `types.go` called from
  `service.go`) are present. The verbatim name persists on
  `*_raw` attrs and the future cross-file resolver picks
  them up.
- Per-language edge kinds (e.g. `friend_of`, `partial_of`,
  `derives`, `mod_path`). Every same-file relationship maps
  onto the existing edge set listed in `doc.go` (`contains`,
  `extends`, `implements`, `static_calls`, `reads`,
  `writes`, `imports`) plus the one new `overrides` value
  Section 5.5 introduces.
- Recursive descent into compiled artefacts (`.so`,
  `.dll`, `.pyc`, `.exe`). The Repo Indexer worker (Stage
  3.1) is responsible for source-file selection; this
  story does not extend the worker's enumeration.
- Reformatting the existing parser test suite or the
  dispatcher tests. The acceptance bar is "TS / Python
  tests pass byte-identical" (`LangMeta` nil = no-op merge,
  Section 4.4.4 of architecture); no editorial pass on the
  existing tests is required or accepted.
- A "stripped image without `pwsh`" build tag for the
  PowerShell parser. v1 leaves `parser_powershell.go` build-
  tag free so both `parsers_cgo.go` and `parsers_nocgo.go`
  can register it unconditionally (architecture Section 6.2
  paragraph "Rationale for dropping build tags here").

## 3. Hard Constraints

These constraints come from Stage 3.2 and are LOCKED. Every
component this story introduces MUST respect them; the
acceptance gate is "TS / Python dispatcher tests in
`dispatcher_test.go` pass byte-identical".

| ID | Constraint | Source |
| --- | --- | --- |
| **C1** | Every parser returns `ParseResult{Classes, Methods, Imports}` with field semantics as declared in `parser.go` lines 60-263. | `parser.go` lines 60-263 |
| **C2** | Canonical signatures are `<repoURL>::class::<relPath>#<QualifiedName>` and `<repoURL>::method::<relPath>#<QualifiedName>(<normalised params>)`. Normalisation goes through `NormalizeSignature`; a formatter-only commit MUST yield byte-identical fingerprints. | `doc.go` lines 72-90; `dispatcher.go` `classSignature` (line 811) and `methodSignature` (line 819) |
| **C3** | Two-pass insert protocol: Pass 1 inserts Nodes; Pass 2 resolves and inserts Edges. New Pass 2d (`overrides`) runs after Pass 2c (`reads`/`writes`). New parsers do NOT call the writer directly. | `doc.go` lines 12-21; `dispatcher.go::emit` |
| **C4** | Same-file resolution only. Cross-file `extends` / `implements` / `static_calls` / `overrides` targets are dropped; verbatim names persist on `*_raw` attrs (`extends_raw`, `calls_raw`, `LangMeta["trait"]`). | `doc.go` lines 94-100; `dispatcher.go::resolveBareCalls` |
| **C5** | Drop on ambiguity, keep on receiver. Bare-name callees resolving to multiple same-file methods are dropped; receiver-qualified callees resolve unambiguously through `EnclosingClass` (multimap rule for Go pointer-receiver collisions, Section 5.4). | `dispatcher.go::buildCalleeIndex` |
| **C6** | Parse-errors are file-local. A panic or error from a parser MUST not abort the worker. `safeParse` recovers panics; `EmitFile` swallows parse errors with a warn log; the new `ErrParserUnavailable` sentinel branches to `ast.dispatch.skip` instead of `ast.parse.error`. | `dispatcher.go::safeParse`, `dispatcher.go::EmitFile` |
| **C7** | Build-tag duality. Production rides tree-sitter (`//go:build cgo`); `make test` portable path on stock Windows uses CGO=off. New tree-sitter parsers register CGO-only; PowerShell registers in both because the strategy is subprocess. | `parsers_cgo.go`, `parsers_nocgo.go` |
| **C8** | `MethodDecl.BodySource` excludes the outer `{` and `}` (or language-equivalent delimiters) so `CountLogicalLines` reads the brace-stripped span. `BodyStartLine` / `BodyEndLine` stay on the delimiter lines for span ingestor stack-frame matching. `BodyStartByte` / `BodyEndByte` point at the first / last interior byte. | `parser_treesitter.go::handleMethod` lines 268-289 |
| **C9** | `Calls` and `ReceiverCalls` are deduplicated in insertion order via `uniqueStringsInsert`. New parsers reuse the existing helper; per-call frequency cannot be recovered downstream. | `parser.go` lines 187-211; `parser_treesitter.go` `uniqueStringsInsert` (line 630); call sites at `parser_treesitter.go` lines 285-286, 317, 801-802, 834 |
| **C10** | Imports are filtered by `isRelativeImport`: any module path starting with `.` or `/` is treated as relative and DROPPED from the imports edge set. Verbatim relative imports do NOT persist anywhere in v1. | `dispatcher.go::isRelativeImport` |
| **C11** | `attrs_json["language"]` is set on every Node by the dispatcher to the parser's `Language()` return value (`c`, `cpp`, `csharp`, `go`, `rust`, `powershell`). Parsers MUST NOT set this key in `LangMeta` -- the merge helper's first-class-key wins rule (architecture Section 4.4.2) would silently drop it. | `dispatcher.go::classAttrs`, `dispatcher.go::methodAttrs` |
| **C12** | `LangMeta` is descriptive, not identifying. Two files differing ONLY in `LangMeta` values MUST collide on the same canonical signature. Parsers MUST NOT route language-specific data into `QualifiedName` (the Go `*` prefix is the ONE explicit exception, pinned by operator). | architecture Section 4.4 + Section 4.5 |

## 4. Build-Tag Topology and Registration

Three groups of `defaultParsers()` returns exist after this
story. The diagram is ASCII art (no box-drawing glyphs).

```
+-------------------------+ CGO=1 (production / Linux CI)   |
| parsers_cgo.go          |                                  |
| defaultParsers() = [    |                                  |
|   tsTreeSitterParser    | <- existing                      |
|   pyTreeSitterParser    | <- existing                      |
|   cTreeSitterParser     | <- NEW                           |
|   cppTreeSitterParser   | <- NEW                           |
|   csharpTreeSitterParser| <- NEW                           |
|   goTreeSitterParser    | <- NEW                           |
|   rustTreeSitterParser  | <- NEW                           |
|   powershellParser      | <- NEW (pwsh subprocess)         |
| ]                       |                                  |
+-------------------------+

+-------------------------+ CGO=0 (portable make test, Windows)
| parsers_nocgo.go        |                                  |
| defaultParsers() = [    |                                  |
|   tsjsParser            | <- existing                      |
|   pythonParser          | <- existing                      |
|   powershellParser      | <- NEW (same impl)               |
| ]                       |                                  |
|                         | .c / .cpp / .cs / .go / .rs ->   |
|                         |   no_parser SKIP (Section 5.1)   |
+-------------------------+
```

### 4.1 Extension claims (authoritative)

| Parser ID | `Extensions()` return |
| --- | --- |
| `c` | `.c`, `.h` |
| `cpp` | `.cc`, `.cpp`, `.cxx`, `.c++`, `.hpp`, `.hh`, `.hxx`, `.h++` |
| `csharp` | `.cs`, `.csx` |
| `go` | `.go` |
| `rust` | `.rs` |
| `powershell` | `.ps1`, `.psm1`, `.psd1` |

`.h` routes to C unconditionally per the pinned operator
decision (Section 8 R6). Repos that need C++ headers must
use `.hpp` / `.hh` / `.hxx` / `.h++`.

### 4.2 Hint aliases registered in `normalizeHints`

| Alias | Canonical parser ID |
| --- | --- |
| `c`, `h` | `c` |
| `cc`, `cxx`, `cpp`, `c++`, `hpp` | `cpp` |
| `cs`, `csharp`, `c#` | `csharp` |
| `go`, `golang` | `go` |
| `rs`, `rust` | `rust` |
| `ps`, `ps1`, `psm1`, `psd1`, `powershell` | `powershell` |

`selectParser` precedence is unchanged: extension match
first, then per-event `LanguageHints`, then dispatcher
default. A `cpp` hint on a `.h` file therefore does NOT
re-route to C++ (extension match wins).

### 4.3 Why no scanner fallback for the five tree-sitter languages

The CGO=off path INTENTIONALLY does not register C / C++ /
C# / Go / Rust. Reasoning:

1. **A2 / C2 stability.** Reproducing six more
   regex-or-state-machine scanners would inevitably emit
   different `QualifiedName` / `ParamSignature` strings on
   edge cases (multi-line declarators, raw string literals,
   nested generics) than the tree-sitter walkers. Two
   different fingerprints for the same source file across
   `CGO_ENABLED` boundaries violate the C2 byte-identical
   normalisation guarantee.
2. **Package surface.** Six scanner implementations would
   add ~6x the existing TS / Python scanner code -- ~15k
   lines for code paths that no production build runs.
3. **The CGO=off consumer is portable `make test` on
   Windows.** Files in the new language set are covered by
   CGO=on tests on Linux CI; the portable path proves the
   dispatcher's "no parser" branch handles them safely
   (`ast.dispatch.skip{reason:"no_parser"}` + clean
   return, no panic).

PowerShell is the explicit exception: the subprocess strategy
is build-tag agnostic, so a single implementation registers
in both groups.

## 5. v1 Extraction Scope per Language

This section is the authoritative source for what each
parser walks, what tree-sitter node types it visits, and how
each language-specific construct projects onto the
`ClassDecl` / `MethodDecl` / `Import` envelope. The
architecture doc (Section 4) gives the per-language ENVELOPE
field semantics; this section gives the GRAMMAR-level rules
that produce those fields.

Conventions:
- Tree-sitter node names below are quoted in backticks
  exactly as they appear in the smacker/go-tree-sitter
  grammar bindings.
- "Walk" means: visit named children with the existing
  `TreeCursor` pattern from `parser_treesitter.go`. The
  parser is a single struct with `walkTop` / `visitTopLevel`
  / `handle*` methods mirroring `tsTreeSitterParser`.
- A grammar node listed here that is absent or renamed in
  the smacker binding triggers a build error at the parser
  file, which is the correct early failure (no silent miss).
- Modifier strings are persisted on
  `MethodDecl.Modifiers`; the dispatcher's existing
  `methodAttrs` writes them to
  `attrs_json["modifiers"]`. Parsers MUST emit lower-case
  modifier tokens to match the existing TS / Python
  convention.

### 5.0 Build matrix (recap)

| Language | CGO=on parser | CGO=off behaviour | Grammar source |
| --- | --- | --- | --- |
| C | `cTreeSitterParser` | skipped (no_parser) | `github.com/smacker/go-tree-sitter/c` |
| C++ | `cppTreeSitterParser` | skipped (no_parser) | `github.com/smacker/go-tree-sitter/cpp` |
| C# | `csharpTreeSitterParser` | skipped (no_parser) | `github.com/smacker/go-tree-sitter/csharp` |
| Go | `goTreeSitterParser` | skipped (no_parser) | `github.com/smacker/go-tree-sitter/golang` |
| Rust | `rustTreeSitterParser` | skipped (no_parser) | `github.com/smacker/go-tree-sitter/rust` |
| PowerShell | `powershellParser` (subprocess) | same impl (subprocess) | `pwsh` System.Management.Automation.Language |

### 5.1 C parser (`parser_treesitter_c.go`)

**Grammar:** `github.com/smacker/go-tree-sitter/c`.

**Walk roots:** `translation_unit` -> visit each named child.

| Construct | Tree-sitter node | Envelope effect |
| --- | --- | --- |
| Struct declaration | `struct_specifier` with a non-empty body | `ClassDecl{Kind="struct", QualifiedName=<identifier>}`. Anonymous struct (no identifier) is SKIPPED. |
| Union declaration | `union_specifier` with non-empty body | `ClassDecl{Kind="union", ...}` (same shape as struct) |
| Enum declaration | `enum_specifier` with non-empty body | `ClassDecl{Kind="enum", ...}`; enumerators are NOT emitted as Methods |
| Function definition (top-level) | `function_definition` | `MethodDecl{EnclosingClass="", QualifiedName=<declarator identifier>, ParamSignature=<text between outer parens of declarator>, BodySource=<brace-stripped function body>}` |
| Function declaration (prototype, no body) | `declaration` containing a `function_declarator` | SKIPPED in v1 (no body -> no MethodDecl); a future story may emit as a "declaration-only" Method |
| Include (system) | `preproc_include` with `<...>` operand | `Import{Module=<bracketed path>}`. Example: `#include <stdio.h>` -> Module=`stdio.h` |
| Include (local) | `preproc_include` with `"..."` operand | `Import{Module="./"+<quoted path>}`. The `./` prefix forces `isRelativeImport` to drop the edge (per architecture Section 4.3); verbatim path NOT persisted in v1 |
| Call site inside function body | `call_expression` whose `function` field is an `identifier` | append the identifier to `MethodDecl.Calls` (dedupe via `uniqueStringsInsert`) |
| Function-pointer call | `call_expression` whose `function` is a parenthesised expression | NOT collected (target name is not resolvable at parse time) |

**Modifiers:** scan `storage_class_specifier` and
`type_qualifier` siblings of the declarator: emit `static`,
`inline`, `extern`, `const` as lower-case tokens.

**ReceiverCalls / MemberAccesses:** empty in v1 (C has no
`this`).

**LangMeta keys written:** none in v1 (decl kind already
flows through `ClassDecl.Kind`).

### 5.2 C++ parser (`parser_treesitter_cpp.go`)

**Grammar:** `github.com/smacker/go-tree-sitter/cpp`.

**Walk roots:** `translation_unit` -> visit each named
child; recurse into `namespace_definition` while accumulating
the namespace path in a `container` argument (mirrors the TS
walker's `outer` argument in `parser_treesitter.go::visitTopLevel`).

| Construct | Tree-sitter node | Envelope effect |
| --- | --- | --- |
| Class declaration | `class_specifier` | `ClassDecl{Kind="class", QualifiedName=<class name with namespace path joined by "."> }`. Nested types use parent path joined by `.`. |
| Struct declaration | `struct_specifier` (top-level or namespaced) | `ClassDecl{Kind="struct", ...}` -- distinguished from C by C++ build tag wiring |
| Base class list | `base_class_clause` -> `type_identifier` | append each to `ClassDecl.Extends`; populate `LangMeta["base_access"][baseName] = <access specifier>` (`public` / `protected` / `private`; `public` default for `struct`, `private` default for `class`) |
| Template parameter list | `template_declaration` -> `template_parameter_list` | record identifiers in `LangMeta["template_params"]` on the inner `ClassDecl` |
| In-class method declaration | `function_definition` inside class body | `MethodDecl{EnclosingClass=<class qualified name>, QualifiedName=<class>.<method>, ParamSignature=<between outer parens>, BodySource=<brace-stripped>}` |
| Out-of-line method definition | `function_definition` at namespace scope whose declarator is a `qualified_identifier` | parse `Foo::bar` into enclosing=`Foo` + method=`bar`; merge with any in-class declaration of the same `QualifiedName` (Section 8 R1: definition with body wins, dedupe before return) |
| Free function | `function_definition` at namespace scope with a plain `identifier` declarator | `MethodDecl{EnclosingClass="", QualifiedName=<name>}` |
| Include (system) | `preproc_include` `<...>` | `Import{Module=<bracketed path>}` |
| Include (local) | `preproc_include` `"..."` | `Import{Module="./"+<path>}` -- dropped by `isRelativeImport` |
| Module import (C++20) | `import_declaration` with module name | `Import{Module=<module name>}` |
| Call site | `call_expression` whose `function` is `identifier` or rightmost segment of `qualified_identifier` | append to `MethodDecl.Calls` |
| Receiver call | `call_expression` whose `function` is `field_expression` with `this` or `(*this)` as receiver | append the field name to `MethodDecl.ReceiverCalls` |
| Member access | `field_expression` whose receiver is `this` / `(*this)`, NOT under a `call_expression` | append `MemberAccess{Name, IsWrite=<LHS of assignment_expression>}` |
| Operator call | `operator_name` inside call | DROPPED (not a callable identifier in v1) |

**Modifiers:** `static`, `virtual`, `inline`, `constexpr`,
`noexcept`, `const` (trailing `const` on member functions);
collected from the children listed before the declarator
plus the trailing `type_qualifier`.

**LangMeta keys:** `namespace` (string),
`base_access` (map[string]string), `template_params`
([]string).

**Deduplication:** the parser walks the file once, then
runs a final pass over `Methods` that collapses any pair of
entries with the same `QualifiedName`: the one with a
non-empty `BodySource` wins; if both have a body the LATER
(source order) wins. Same-file pairs without conflict pass
through unchanged. This is the C++-only behavioural fix
required by Section 8 R1.

### 5.3 C# parser (`parser_treesitter_csharp.go`)

**Grammar:** `github.com/smacker/go-tree-sitter/csharp`.

**Walk roots:** `compilation_unit` -> recurse into
`namespace_declaration` and `file_scoped_namespace_declaration`,
accumulating the namespace into `LangMeta["namespace"]`.

| Construct | Tree-sitter node | Envelope effect |
| --- | --- | --- |
| Class | `class_declaration` | `ClassDecl{Kind="class", QualifiedName=<simple name>}`. Namespace persisted on `LangMeta["namespace"]`. |
| Interface | `interface_declaration` | `ClassDecl{Kind="interface", ...}` |
| Struct | `struct_declaration` | `ClassDecl{Kind="struct", ...}` |
| Record | `record_declaration` | `ClassDecl{Kind="record", ...}` |
| Enum | `enum_declaration` | `ClassDecl{Kind="enum", ...}`; enumerators NOT emitted as Methods |
| Partial flag | `modifier` = `partial` on any of the above | `LangMeta["partial"]=true` |
| Base list | `base_list` -> child `identifier` or `qualified_name` | partitioned at PARSE TIME via the parser's own same-file two-pass scan (see "C# base-list partition rule" below). Verbatim raw list persisted on `LangMeta["base_raw"]` regardless. |
| Method declaration | `method_declaration` inside type body | `MethodDecl{EnclosingClass=<simple class name>, QualifiedName=<class>.<method>, ParamSignature=<text between outer parens>, BodySource=<brace-stripped body when present; signature-only methods (`abstract`, interface members) leave BodySource="">}` |
| Constructor | `constructor_declaration` | `MethodDecl{QualifiedName=<class>.<class>}` (mirrors the existing TS constructor convention) |
| Property accessor | `accessor_declaration` (get/set) | NOT emitted as Methods in v1; property names ARE recorded for `MemberAccesses` resolution on the enclosing type |
| Using directive (plain) | `using_directive` with `name` child | `Import{Module=<qualified name>}` |
| Using static | `using_directive` with `static` keyword | `Import{Module=<qualified name>, LangMeta={"is_static": true}}` |
| Using alias | `using_directive` with `name_equals` | `Import{Module=<right-hand qualified name>, Alias=<left-hand identifier>}` |
| Call site | `invocation_expression` whose `function` is `identifier` | append to `Calls` |
| Receiver call | `invocation_expression` whose `function` is `member_access_expression` with `this` as receiver | append the member name to `ReceiverCalls` |
| Member access | `member_access_expression` with `this` as receiver, outside any `invocation_expression` | append `MemberAccess` with `IsWrite` from `assignment_expression` LHS |

**Modifiers:** `public`, `private`, `protected`, `internal`,
`static`, `async`, `override`, `virtual`, `sealed`,
`abstract`, `readonly`, `extern`, `partial`, `unsafe`.

**LangMeta keys:** `namespace` (string), `partial` (bool),
`base_raw` ([]string), `is_static` (bool on `Import`).

**C# base-list partition rule (same-file local symbol-table
resolver, executed at parse time).** The C# `base_list`
mixes class + interface entries with no syntactic
distinction (`class Foo : Bar, IBaz, IQux`). Per the
companion `architecture.md` Section 4.1 C# row, the
partition is "by what each name resolves to in the file's
local symbol table". This spec realises that contract by
locating the resolver inside the C# parser: the parser
walks the file twice, builds the local symbol table on the
first walk, and emits `ClassDecl.Extends` /
`ClassDecl.Implements` already partitioned on the second
walk. The dispatcher's Pass 2a (`dispatcher.go` lines
533-567) then iterates `c.Extends` / `c.Implements`
unchanged -- no new `classKind` map is added to the
dispatcher because the resolution is complete before the
`ParseResult` returns. This satisfies the architecture
contract while staying compatible with the current
dispatcher data structures (`classNodeID map[string]string`
at `dispatcher.go` line 332 has no kind metadata; the
parser supplies the kind from its own walk).

Two-pass walker contract:

1. **Pass A (declarations).** Walk every `class_declaration`,
   `interface_declaration`, `struct_declaration`,
   `record_declaration` in the file. Build
   `localKind map[string]string` with values `"class"`,
   `"interface"`, `"struct"`, `"record"` keyed by simple
   name (`Foo` -- namespace tracked separately on
   `LangMeta["namespace"]`). This map IS the "file's local
   symbol table" the architecture references.
2. **Pass B (base-list assignment).** For each declaration
   with a non-empty `base_list`, partition by declaring
   kind and per-entry resolved kind:
   - **Declaring kind = `class`:** the entry at position 0
     is dispatched by `localKind[entry]`:
     - `"class"` -> `ClassDecl.Extends` (the base class).
     - `"interface"` -> `ClassDecl.Implements` (covers
       `class Foo : IFoo` where `IFoo` is declared in the
       same file -- there is no base class in this case).
     - `"struct"` / `"record"` / `"enum"` -> dropped from
       both partitions; the verbatim entry persists on
       `LangMeta["base_raw"]` only. (A class cannot extend
       a struct/record/enum in C#; the parser does not
       emit an edge that would mis-type the relationship.)
     - unresolved (no `localKind` entry; target lives in
       another file) -> `ClassDecl.Extends`. C#'s language
       rule restricts a class to at most one base class
       which must appear first when present, so an
       unresolved position-0 name is most likely the
       cross-file base class. The dispatcher's Pass 2a
       drops the edge per C4 when `classNodeID[entry]`
       misses, so a mis-classified cross-file interface
       produces no false edge in the graph. The verbatim
       name persists on `LangMeta["base_raw"]` for the
       future cross-file resolver to re-partition with
       project-wide kind info.
     - Entries at position 1 or later are dispatched the
       same way, except: a "class" or unresolved entry at
       position 1+ is invalid C# and goes to
       `ClassDecl.Implements` defensively (a class has
       exactly one base class in C#). The verbatim entry
       always persists on `LangMeta["base_raw"]`.
   - **Declaring kind = `interface`:** every entry goes to
     `Extends` (an interface's base list is all
     super-interfaces). The dispatcher's Pass 2a emits
     `extends` between interface nodes -- consistent with
     the existing TS pattern where `interface Greeter
     extends BaseGreeter` produces `extends` (not
     `implements`).
   - **Declaring kind = `struct` / `record`:** every entry
     goes to `Implements` (structs / records have no base
     class in C#; the entire base list is the implemented
     interface set).
3. **Verbatim raw retention.** Regardless of partition,
   `LangMeta["base_raw"]` records the verbatim list in
   source order so the future cross-file resolver can
   re-partition with full project-wide kind information.

The decision matrix for the most common base-list shapes
(declaring kind = `class`):

| Source | Position 0 | `localKind[pos0]` | -> | Outcome |
| --- | --- | --- | --- | --- |
| `class Foo : Bar` (Bar is same-file class) | `Bar` | `"class"` | -> | `Extends=["Bar"]`, `Implements=[]` |
| `class Foo : IBar` (IBar is same-file interface) | `IBar` | `"interface"` | -> | `Extends=[]`, `Implements=["IBar"]` |
| `class Foo : Bar, IBaz` (both same-file) | `Bar`/`IBaz` | class/interface | -> | `Extends=["Bar"]`, `Implements=["IBaz"]` |
| `class Foo : IBaz, IQux` (interface-only, same-file) | `IBaz`/`IQux` | interface/interface | -> | `Extends=[]`, `Implements=["IBaz","IQux"]` |
| `class Foo : Bar` (Bar is cross-file) | `Bar` | unresolved | -> | `Extends=["Bar"]`; dispatcher Pass 2a drops the edge on `classNodeID` miss per C4; `LangMeta["base_raw"]=["Bar"]` retained |
| `class Foo : Bar, IBaz` (Bar cross-file, IBaz same-file interface) | `Bar`/`IBaz` | unresolved/interface | -> | `Extends=["Bar"]`, `Implements=["IBaz"]`; cross-file Bar dropped by dispatcher |

This rule keeps the existing dispatcher Pass 2a code path
in `dispatcher.go` lines 533-567 unchanged -- the `for _,
target := range c.Extends` loop simply receives the
partitioned `Extends`; the `c.Implements` loop receives
the partitioned `Implements`. No new `classKind` map is
added to the dispatcher. No new `language=="csharp"`
sub-step is added. The architecture's `Extends = First
entry... whose target lookup resolves to a class
declaration (same file)` rule is preserved -- it is
executed by the parser using its own same-file
`localKind` table rather than by a downstream dispatcher
pass, but the observable `ClassDecl` shape is identical.

### 5.4 Go parser (`parser_treesitter_go.go`)

**Grammar:** `github.com/smacker/go-tree-sitter/golang`.

**Walk roots:** `source_file` -> visit each top-level
declaration.

| Construct | Tree-sitter node | Envelope effect |
| --- | --- | --- |
| Type alias | `type_declaration` -> `type_spec` with `type_identifier` body | `ClassDecl{Kind="type_alias", QualifiedName=<name>}` |
| Struct type | `type_spec` -> `struct_type` body | `ClassDecl{Kind="struct", QualifiedName=<name>}`. Embedded type names captured as `LangMeta["embeds"]`. |
| Interface type | `type_spec` -> `interface_type` body | `ClassDecl{Kind="interface", QualifiedName=<name>}` |
| Free function | `function_declaration` (no receiver) | `MethodDecl{EnclosingClass="", QualifiedName=<name>, ParamSignature=<text between outer parens>, BodySource=<brace-stripped>}` |
| Method with value receiver | `method_declaration` with `parameter_declaration` of plain `type_identifier` | `MethodDecl{EnclosingClass=<type name>, QualifiedName=<type>.<name>, ReceiverAliases=nil, LangMeta={"receiver":<bound name>, "receiver_ptr":false}}` |
| Method with pointer receiver | `method_declaration` with `parameter_declaration` of `pointer_type` -> `type_identifier` | `MethodDecl{EnclosingClass=<type name>, QualifiedName="*"+<type>+"."+<name>, ReceiverAliases=[<type>.<name>], LangMeta={"receiver":<bound name>, "receiver_ptr":true}}` -- per pinned `go-receiver-pointer-fingerprint` |
| Import (single) | `import_declaration` -> single `import_spec` with `interpreted_string_literal` | `Import{Module=<unquoted path>}` |
| Import with alias | `import_spec` with `package_identifier` | `Import{Module=<path>, Alias=<alias>}` |
| Dot import | `import_spec` with `.` package | `Import{Module=<path>, Alias=".", LangMeta={"dot_import": true}}` |
| Blank import | `import_spec` with `_` package | `Import{Module=<path>, Alias="_", LangMeta={"blank_import": true}}` |
| Import group | `import_declaration` with `import_spec_list` | one `Import` per inner `import_spec` |
| Call site | `call_expression` whose `function` is `identifier` | append to `Calls` |
| Selector call | `call_expression` whose `function` is `selector_expression` (`pkg.Func()`) | append the RIGHTMOST identifier (`Func`) to `Calls` -- bare-name collision with same-file declarations triggers the A5 drop rule via `buildCalleeIndex` |
| Receiver call | `call_expression` whose `function` is `selector_expression` whose receiver matches the method's bound receiver name | append the RIGHTMOST identifier to `ReceiverCalls` |
| Member access | `selector_expression` whose receiver is the bound receiver, OUTSIDE any `call_expression` | append `MemberAccess{Name=<right>, IsWrite=<LHS of assignment_statement; `:=` does NOT count -- it is a fresh binding>}` |

**Modifiers:** empty in v1 (Go has no `static` /
`public` / `async` keywords; visibility comes from
identifier case but is not stored as a modifier in this
story).

**LangMeta keys:** `embeds` ([]string on `ClassDecl`),
`receiver` (string), `receiver_ptr` (bool), `dot_import`
(bool), `blank_import` (bool).

**ReceiverAliases population rule:** the alias is
populated ONLY for pointer-receiver methods; the value-
receiver method's `QualifiedName` already equals the
receiver-qualified lookup key. See architecture Section
4.5.1 for the dispatcher-side multimap.

### 5.5 Rust parser (`parser_treesitter_rust.go`)

**Grammar:** `github.com/smacker/go-tree-sitter/rust`.

**Walk roots:** `source_file` -> visit each top-level item.
Recurse into `mod_item` while accumulating the module path
in a `container` argument (top-level `mod_item` blocks
declare in-file modules; their bodies are walked but the
module name is NOT propagated into the `QualifiedName`
in v1 -- only nested type names follow the dotted path).

| Construct | Tree-sitter node | Envelope effect |
| --- | --- | --- |
| Struct | `struct_item` | `ClassDecl{Kind="struct", QualifiedName=<name>}` |
| Enum | `enum_item` | `ClassDecl{Kind="enum", ...}`; variants NOT emitted as Methods |
| Trait | `trait_item` | `ClassDecl{Kind="trait", QualifiedName=<name>}`. Parent traits in the supertrait clause (`trait X: A + B`) go to `Extends`. |
| Trait default method | `function_item` inside a `trait_item` body with a `block` body | `MethodDecl{EnclosingClass=<trait name>, QualifiedName=<trait>.<name>, LangMeta={"trait_default": true}}` |
| Trait method declaration (no body) | `function_signature_item` inside `trait_item` | `MethodDecl{EnclosingClass=<trait name>, QualifiedName=<trait>.<name>, BodySource="", BodyStartByte=0, BodyEndByte=0}` |
| Inherent impl block | `impl_item` with NO `trait` clause | walk inner `function_item` children, emit `MethodDecl{EnclosingClass=<Type>, QualifiedName=<Type>.<name>}` |
| Trait impl block | `impl_item` with a `trait` clause (`impl Trait for Type`) | append `Trait` to the Type class's `Implements` list; for each inner `function_item`, emit `MethodDecl{EnclosingClass=<Type>, QualifiedName=<Type>.<name>, LangMeta={"trait":<TraitName>}}` |
| Free function | `function_item` at file scope | `MethodDecl{EnclosingClass="", QualifiedName=<name>}` |
| `use` (single) | `use_declaration` with `scoped_identifier` ending in identifier | `Import{Module=<path WITHOUT last segment>, Symbols=[<last segment>]}` |
| `use` (list) | `use_declaration` with `use_list` | one or more Imports; same Module, distinct Symbols |
| `use` (alias) | `use_declaration` with `use_as_clause` | `Import{Module=<path>, Symbols=[<original>], Alias=<alias>}` |
| `use` (glob) | `use_declaration` with `*` | `Import{Module=<path>, Symbols=["*"]}` |
| Call site | `call_expression` whose `function` is `identifier` or rightmost of `scoped_identifier` (`Mod::func()`) | append to `Calls` |
| Receiver call | `call_expression` whose `function` is `field_expression` with `self` as receiver | append the field name to `ReceiverCalls` |
| Method-call (non-self) | `call_expression` -> `field_expression` whose receiver is NOT `self` | NOT collected in v1 -- the receiver type is not statically known at parse time |
| Member access | `field_expression` with `self` receiver, outside `call_expression` | `MemberAccess{Name, IsWrite=<LHS of assignment_expression>}` |
| Macro invocation | `macro_invocation` whose `macro` is `identifier` | NOT collected in v1 (macro semantics are unknown to the syntax-only parser) |

**Modifiers:** `pub`, `pub(crate)`, `pub(super)`, `async`,
`unsafe`, `const`, `extern`. Visibility tokens are emitted as
lower-case strings.

**LangMeta keys:** `trait` (string on `MethodDecl`),
`trait_default` (bool on `MethodDecl`).

**Pass 2d (overrides) emission contract:** for each
`MethodDecl` whose `LangMeta["trait"]` is non-empty, the
dispatcher looks up `methodNodeID[traitName + "." +
simpleName(m.QualifiedName)]`. On a same-file hit the
dispatcher emits a single `overrides` edge from the impl
method's node id to the trait method's node id. On a
cross-file miss the edge is dropped; verbatim trait name
persists on `LangMeta["trait"]`. The `simpleName` helper is
the existing dispatcher utility (`buildCalleeIndex`) -- it
strips a leading `*` then returns the dotted last segment.

### 5.6 PowerShell parser (`parser_powershell.go`)

**Strategy:** subprocess to `pwsh -NoProfile -NonInteractive
-Command -` running an embedded extraction script. NOT a
tree-sitter binding. No CGO dependency. Pinned by operator
answer `powershell-grammar-strategy`; reference
implementation at `E:\work\github\crp\workflow\src\ast\
Ast.PowerShell` uses the same official AST API.

**Subprocess contract:**

| Aspect | Value |
| --- | --- |
| Invocation | `pwsh -NoProfile -NonInteractive -Command -` |
| Stdin | The source bytes followed by EOF |
| Stdout | A JSON document `{"functions":[...], "types":[...], "imports":[...]}` |
| Timeout | 10 seconds per file via `context.WithTimeout` (Section 6) |
| pwsh missing | Constructor sets `pwshBin=""`; every `Parse` returns `ErrParserUnavailable` |
| pwsh runtime error | `Parse` returns the unwrapped error -> `safeParse` -> `ast.parse.error` log; worker continues |

**Embedded extraction script** mirrors `Ast.PowerShell.
PowerShellAstParser.ExtractNodes`. It calls
`[System.Management.Automation.Language.Parser]::ParseInput`
and walks `FunctionDefinitionAst`, `TypeDefinitionAst`,
`ParamBlockAst`, `ScriptBlockAst`, plus the import-like
constructs identified by `PowerShellHyperedgeExtractor`.

| .NET AST kind | JSON output field | Envelope effect |
| --- | --- | --- |
| `TypeDefinitionAst` (class) | `types[*]` with `Kind="class"`, `BaseTypes=[]`, `Methods=[]`, `Properties=[]` | `ClassDecl{Kind="class", QualifiedName=<name>, Extends=BaseTypes[0:1] when BaseTypes non-empty}`. PowerShell does not allow nested classes in v5; nested skipped. |
| `TypeDefinitionAst` (enum) | `types[*]` with `IsEnum=true` | `ClassDecl{Kind="enum", QualifiedName=<name>}` |
| `FunctionMemberAst` (inside class) | `types[*].Methods[*]` | `MethodDecl{EnclosingClass=<class>, QualifiedName=<class>.<method>, ParamSignature=<joined param list>}` |
| `FunctionDefinitionAst` (top-level) | `functions[*]` | `MethodDecl{EnclosingClass="", QualifiedName=<name>}` |
| `ParamBlockAst` (inside function/method) | folded into ParamSignature | NOT a separate Node; the joined parameter list becomes `ParamSignature` |
| `ScriptBlockAst` (top-level / anonymous) | not emitted as Class/Method | only its TOP-LEVEL import-like statements are walked |
| `Import-Module Foo` | `imports[*]` | `Import{Module="Foo", LangMeta={"cmdlet_verb":"Import","module_kind":"Import-Module"}}` |
| `using module Foo` | `imports[*]` | `Import{Module="Foo", LangMeta={"cmdlet_verb":"using","module_kind":"using_module"}}` |
| `. ./helpers.ps1` (dot-source) | `imports[*]` | `Import{Module="./helpers.ps1", LangMeta={"cmdlet_verb":".","module_kind":"dot_source"}}` -- dropped by `isRelativeImport` |
| Call site (bare cmdlet) | walked from inside function body | append `Get-Foo` / `Invoke-Bar` to `Calls`; only command names at statement position are extracted in v1 |
| `$this.X(...)` inside class method | walked from inside `FunctionMemberAst` body | append the field name to `ReceiverCalls` |
| `$this.X` outside a call | as above | append `MemberAccess{Name, IsWrite=<LHS of `=`>}` |

**Modifiers:** class methods emit `static` / `hidden` when
present; top-level functions emit nothing.

**LangMeta keys:** `cmdlet_verb` (string on `Import`),
`module_kind` (string on `Import`).

**Failure modes (full table in Section 6.4 of
architecture):**

- `pwsh` not on PATH -> `ErrParserUnavailable` ->
  `ast.dispatch.skip{reason:"pwsh_not_available"}` per
  file.
- `pwsh` exits non-zero -> error returned -> `safeParse`
  catches it -> `ast.parse.error`.
- `pwsh` hangs -> 10-second context timeout fires ->
  `safeParse` -> `ast.parse.error`.

## 6. Threshold Values and Ops Budgets

This story does NOT change the Stage 3.2 thresholds but
records the relevant ones plus one new value (the pwsh
subprocess timeout).

| Threshold | Value | Source / scope |
| --- | --- | --- |
| Method body logical-line threshold for Block subdivision | 80 logical lines | `block.go::SubdivideMethod`; UNCHANGED -- applies to every language including the six new ones |
| Block subdivision minimum count when threshold tripped | 2 (entry + exit) | `block.go`; UNCHANGED |
| Max file size the dispatcher attempts | governed by `repoindexer.WorkerOptions.MaxFileBytes` (Stage 3.1) | UNCHANGED -- this story does not extend the worker |
| Tree-sitter parse timeout | none enforced in v1 (smacker's `ParseCtx` accepts a `context.Context` but the dispatcher passes `context.Background()` in the existing parsers) | UNCHANGED |
| pwsh subprocess per-file timeout | 10 seconds | NEW -- `parser_powershell.go` uses `context.WithTimeout(parentCtx, 10*time.Second)` |
| pwsh subprocess invocation strategy | one process per file in v1 | NEW -- a future story may switch to a long-lived host process |
| `Imports.Module` relative-prefix filter | any path starting with `.` or `/` | `dispatcher.go::isRelativeImport`; UNCHANGED -- the new parsers obey by prefixing C `#include "..."`, PowerShell `. ./X.ps1`, and `./Local.psm1` paths with `./` so the filter catches them |
| `Calls` / `ReceiverCalls` dedupe order | insertion order, first occurrence wins | `parser.go` lines 187-211 + `uniqueStringsInsert`; UNCHANGED |
| `attrs_json` size cap | none enforced; `jsonb` accepts anything PostgreSQL accepts | UNCHANGED -- `LangMeta` typical payload is < 1 KB per node |
| Maximum receiver-qualified call set size before drop | > 1 (drop on collision per A5) | NEW behaviour gated by the Pass 2b multimap (architecture Section 4.5.1); UNCHANGED A5 rule |

The block subdivision threshold is the operationally
visible one. For C functions, C# methods, Go methods, and
Rust impl methods the same 80-line cap applies; methods
that exceed it get the minimal {entry, exit} Block pair
and the existing per-block `attrs_json` shape.

### 6.1 Logging keys touched

This story adds ONE new logging key value and reuses every
other existing key:

| Log event | Key=value addition | Source |
| --- | --- | --- |
| `ast.dispatch.skip` | `reason="pwsh_not_available"` | NEW -- Section 2.2.1 of architecture; fires from the new EmitFile branch on `errors.Is(err, ErrParserUnavailable)` |
| `ast.dispatch.skip` | `reason="no_parser"` | UNCHANGED -- fires for `.c` / `.cpp` / `.cs` / `.go` / `.rs` files under CGO=off |
| `ast.parse.error` | unchanged | fires for tree-sitter parse failures and for pwsh non-zero exit / timeout |
| `ast.parse.panic` | unchanged | fires from `safeParse` recovery |
| `ast.dispatch.ok` | `language=<new id>` | UNCHANGED -- the `language` value set comes from the parser's `Language()` return |
| `ast.imports.skip_relative` | unchanged | fires for C `#include "..."`, PowerShell `. ./X.ps1`, `./Local.psm1`, and Rust `mod` declarations (which v1 does not emit as Imports at all) |

## 7. Test Surface Requirements

Each new parser ships a fixture-driven test mirroring
`TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet`
(`parser_typescript_test.go` lines 11-100). The fixture
file is a single embedded source string that exercises:

| Construct in fixture | Why it is there |
| --- | --- |
| One type/class with a same-file base (where the language has inheritance) | proves Pass 2a `extends` edge emission |
| One interface / trait / abstract type in the same file (where applicable) | proves Pass 2a `implements` edge emission for **C# and Rust only**. PowerShell has no interface keyword (companion `architecture.md` Section 4.1 line 304; this spec Section 5.6 emits only `Extends`); the PowerShell fixture has NO `implements` assertion. C and Go have no `implements` either (C has no class, Go interfaces are structurally satisfied per Section 2.2 "Go interface satisfaction edges"). The Rust row also covers the trait-default override case. |
| One method on the type with a body | proves Pass 1 method node insert + `contains` edge + brace-strip body span (C8) |
| One free function | proves the free-function `EnclosingClass=""` path |
| One same-file static call from the method body to the free function | proves Pass 2b `static_calls` edge emission |
| One receiver-qualified call (where applicable) | proves Pass 2b receiver-qualified path -- Go and Rust use `r.X()` / `self.X()`; C++ uses `this->X()`; C# uses `this.X()`; PowerShell uses `$this.X()` |
| One member access (where applicable) | proves Pass 2c `reads` / `writes` edge emission and the `members` attr |
| One import / `#include` / `use` / `using` / `Import-Module` | proves Pass 0 `imports` edge emission and the per-language `LangMeta` keys |
| One language-specific edge (Rust only) | proves Pass 2d `overrides` edge emission for the trait-default shadowed case |

Cross-language dispatcher tests (added to
`dispatcher_test.go`):

| Test | Assertion |
| --- | --- |
| `TestDispatcher_RoutesByExtension` | `selectParser("a.cs", nil).Language() == "csharp"` for each new extension |
| `TestDispatcher_DotHRoutesToC_EvenWithCppHint` | `selectParser("a.h", []string{"cpp"}).Language() == "c"` per Section 4.1 / Section 8 R6 |
| `TestDispatcher_NoParserForUnknown` | `.foo` returns nil parser and logs `ast.dispatch.skip{reason:"no_parser"}` |
| `TestDispatcher_DuplicateExtensionLastWins` | Registering two parsers that both claim `.go` results in the LATER parser winning for that extension. The actual contract is `buildExtMap` in `dispatcher.go` lines 155-161: it iterates `parsers` in slice order and writes each `Extensions()` value into the `map[string]LanguageParser` -- a duplicate key silently overwrites the earlier entry (Go's standard map assignment). No `Register` method exists; there is no panic and no error return. The test constructs a dispatcher with `WithParsers(parserA, parserB)` where both claim `.go`, calls `selectParser("x.go", nil)`, and asserts the returned parser is `parserB`. This pins the deterministic last-wins behaviour and catches a regression that would either introduce a panic or change ordering. |
| `TestDispatcher_GoMultimapDropsOnReceiverCollision` | a file with `func (r Foo) Bar()` and `func (r *Foo) Bar()` both calling `r.Bar()` from a third method emits NO `static_calls` edge for that callee; verbatim `calls_raw` retains `Bar` |
| `TestDispatcher_GoMultimapResolvesPointerReceiverAlone` | a file with ONLY `func (r *Foo) Bar()` and a sibling method calling `r.Bar()` emits ONE `static_calls` edge to the `*Foo.Bar` node, via the `ReceiverAliases` entry |
| `TestDispatcher_ErrParserUnavailable_LogsSkip` | a mock parser returning `ErrParserUnavailable` causes EmitFile to log `ast.dispatch.skip{reason:<sentinel reason>}` and return `EmitResult{}, nil` (NOT `ast.parse.error`) |
| `TestDispatcher_LangMetaMergePreservesFirstClassKeys` | a fake parser populating `LangMeta["language"]="bogus"` results in `attrs_json["language"]=<dispatcher value>` -- first-class key wins per C11 |

PowerShell tests use `t.Skip` when `exec.LookPath("pwsh")`
fails so the suite stays green on PowerShell-less CI hosts.
A separate `TestPowerShellParser_NoPwsh_ReturnsSentinel`
test forces `pwshBin=""` and asserts
`errors.Is(err, ErrParserUnavailable)`.

## 8. Risks

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| **R1** -- C++ declaration vs definition double-count. A class member function declared in the header (no body) and defined in a `.cpp` file (with body) creates two `MethodDecl` with the same `QualifiedName`; without dedupe, Pass 1 inserts both and Pass 2a / 2b resolve against an ambiguous map. | high (any non-trivial C++ project) | medium (silent duplicate Method nodes; ambiguous calls dropped per A5) | C++ parser deduplicates by `QualifiedName` within a single file at the end of `Parse`: body-present wins; later-source-order wins on tie. Cross-file (header in `foo.h`, body in `foo.cpp`) is a future cross-file resolver story; the two distinct `relPath` values produce two distinct canonical signatures per C2, which is correct for v1. |
| **R2** -- Go pointer-receiver vs value-receiver method-fingerprint collision. `func (r Foo) Bar()` and `func (r *Foo) Bar()` are distinct at the language level but the receiver clause is OUTSIDE `ParamSignature`, so naive emission collides on `<rel>::method#Foo.Bar()`. | low (uncommon but legal) | high (silent merge of two methods into one Node; receiver-qualified calls misresolve) | Pinned per `go-receiver-pointer-fingerprint`: pointer-receiver methods key as `*Foo.Bar` in `QualifiedName`; `EnclosingClass` stays `Foo` for class attachment; `LangMeta["receiver_ptr"]=true` for filtering. Receiver-qualified calls resolve via the new Pass 2b multimap (Section 5.4 / architecture Section 4.5.1); collisions drop per A5; single-form lookups succeed via `ReceiverAliases`. |
| **R3** -- C# `partial class` declarations across multiple files. v1 emits one `ClassDecl` per partial file at distinct `relPath` -> distinct canonical signatures -> distinct Nodes. Consumers asking "all members of `Foo`" see a fragmented answer. | medium (any non-trivial Razor / EF / WPF project) | low (consumer-visible, but the data is present in fragments) | `LangMeta["partial"]=true` set on each partial fragment so a consumer can group by qualified name. Cross-file stitching via a future `partial_of` edge is out of scope. |
| **R4** -- Rust trait default-impl shadowing. A trait with a default-bodied method plus an impl block that overrides it must emit a typed edge from the impl method to the trait default; the current `edge_kind` enum (`0001_enums.sql` lines 28-38) does NOT list `overrides`. | medium (idiomatic Rust pattern) | high (without a typed edge, the override relationship is silently absent) | Pinned per `rust-trait-overrides-edge`: new migration `0022_edge_kind_overrides.sql` issues `ALTER TYPE edge_kind ADD VALUE 'overrides';`. New Pass 2d emits the edge when both the trait default-bodied method AND the impl method live in the same file (resolved via `methodNodeID[traitName + "." + simpleName(implMethod)]`). Cross-file pairs are dropped per A4; verbatim trait identity persists on `LangMeta["trait"]`. The enum migration is non-transactional but additive-safe per PostgreSQL semantics; no rollback path is needed because the enum value is monotonically added. |
| **R5** -- PowerShell `pwsh` subprocess overhead and host-availability. One process per file is slow for large `.ps1`-heavy repos; absent `pwsh` on PATH means no PS coverage at all. | medium (CI hosts may not have pwsh; large repos with thousands of scripts pay subprocess overhead) | medium (degraded coverage, not silent corruption) | One process per file is acceptable for v1's per-file emission cadence (the dispatcher already runs per-file in `EmitFile`). The 10-second timeout caps worst-case latency. Tests `t.Skip` when pwsh is missing. The dispatcher's new `ast.dispatch.skip{reason:"pwsh_not_available"}` log per file makes the coverage gap operationally visible. Future workstream batches via long-lived `pwsh` host on overhead measurement. |
| **R6** -- `.h` ambiguity. C and C++ both use `.h` for headers. v1 routes `.h` to the C parser, which means a C++-only header (with classes / templates / namespaces) parses as C and emits NO class / namespace nodes, only free-function declarations and includes. | medium (mixed C / C++ repos are common) | medium (silent under-coverage of C++ headers using `.h`) | Pinned per `dot-h-extension-routing`: `.h` -> C unconditionally; repos with C++ headers must use `.hpp` / `.hh` / `.hxx` / `.h++`. The C parser degrades gracefully on `extern "C"` declarations and plain function prototypes (the C-subset of C++ header syntax). A follow-up story may add a per-repo `extension_overrides[]` knob that fires ahead of `extMap`; that knob is OUT of v1. |
| **R7** -- Tree-sitter grammar churn / smacker binding lag. A future smacker module update may rename node types (e.g. `class_declaration` -> `class_decl_node`), breaking the parsers. | low (smacker stable since 2024) | high (silent miss-emission of every Class / Method) | All grammar-specific node-type strings are named constants in the parser file (per the existing TS / Python pattern at `parser_treesitter.go` lines 110-136). A bump pins the breaking change to a single file per language. Fixture tests catch silent breakage on the first CI run after a smacker bump. |
| **R8** -- `LangMeta` key drift across parsers. Two parsers emitting the same `LangMeta` key with different semantics (e.g. `"receiver"` meaning "Go bound name" vs "C# `this` shadow") confuse downstream consumers. | low (only one parser populates each documented key in v1) | low (downstream filters can branch on `language`) | The authoritative key table is Section 4.4.3 of architecture (and reflected per-language in Sections 5.1-5.6 above). Each key is documented to one parser; future parsers must extend the table, not silently overload a key. |
| **R9** -- CGO=off coverage gap discovered late. A developer running `make test` on a Windows box sees zero failures for C / C++ / C# / Go / Rust because the dispatcher's no_parser path is "silent skip" by design. | medium (developers may assume code paths are tested when they're not) | low (CI covers CGO=on; developer's local belief is the only loss) | The new dispatcher behaviour is documented in `.claude/context/tests.md` per Section 2.1 of architecture. The `tests.md` table makes the "tree-sitter-backed only" caveat explicit. A debug-level log line at startup enumerates the registered parsers; future work could elevate this to info when CGO=off is the active build. |
| **R10** -- Pass 2d (overrides) emission ordering. Pass 2d depends on Pass 1 having populated `methodNodeID` for both the trait method and the impl method. If the dispatcher reordering ever moves Pass 2d before Pass 1 completes, the lookups silently miss. | very low (Pass 1 / 2 ordering is invariant) | high (silent loss of every overrides edge) | Pass 2d is implemented as a new method on `Dispatcher` called explicitly from `emit` AFTER Pass 2c. The dispatcher test `TestDispatcher_Rust_TraitOverrides_SameFile` asserts the edge is emitted; any reordering regression fails the test. |
| **R11** -- C# base-list partition error. The C# `base_list` mixes class + interface entries with no syntactic distinction (`class Foo : Bar, IBaz, IQux`). A mis-partition would emit `extends` where `implements` is correct (or vice versa). The case that breaks a naive "position 0 = base class" heuristic is `class Foo : IFoo` -- IFoo is the SOLE entry, sits at position 0, but is an interface, so the correct partition is `Extends=[], Implements=["IFoo"]`. | low (the C# language rules around base-class position and per-kind dispatch are unambiguous and codified in the Section 5.3 decision matrix) | medium (mis-typed edges: a class would receive `extends` to an interface it should `implements`) | Partition runs at PARSE TIME inside the C# parser via the same-file two-pass walk documented in Section 5.3 ("C# base-list partition rule"). Pass A collects `localKind` from every type declaration in the file (the "file's local symbol table" the architecture references). Pass B partitions by declaring kind AND per-entry `localKind` lookup: `class Foo : IFoo` resolves IFoo via `localKind["IFoo"]=="interface"` -> Implements, NOT Extends. Cross-file targets at position 0 of a `class` default to Extends and the dispatcher's Pass 2a drops the edge per C4 if `classNodeID` misses; verbatim names persist on `LangMeta["base_raw"]` for the future cross-file resolver. The dispatcher's existing Pass 2a code at `dispatcher.go` lines 533-567 reads `c.Extends` and `c.Implements` UNCHANGED; no new `classKind` map or `language=="csharp"` sub-step is added. Test `TestCSharp_BaseListPartitionsByLocalKind` covers the six rows of the Section 5.3 decision matrix (single-class, single-interface, mixed, interface-only, cross-file class, mixed cross-file). |

## 9. Open Questions

None. All four operator pinnings (`dot-h-extension-routing`,
`powershell-grammar-strategy`,
`go-receiver-pointer-fingerprint`,
`rust-trait-overrides-edge`) are honoured in Sections 1, 4,
5, and 8. If a sibling iteration on `architecture.md` or
`implementation-plan.md` surfaces a new pinning need, this
file will re-emit a structured open-questions block at the
top of Section 9.
