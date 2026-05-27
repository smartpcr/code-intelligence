# AST Parser for Additional Languages -- Architecture

> Story: `code-intelligence:AST-PARSER-FOR-ADDIT` -- 13 points
> Companion docs (drafted in parallel): `tech-spec.md`,
> `implementation-plan.md`, `e2e-scenarios.md`. This file owns the
> component, data-model, interface, and sequence contracts. The
> tech spec owns extraction-rule tables and threshold values; the
> implementation plan owns file lists and step order; the e2e
> doc owns the operator-visible scenarios.

## 1. Purpose and Scope

This story extends the Stage 3.2 polyglot AST dispatcher
(`services/agent-memory/internal/repoindexer/ast`) to cover six
additional source languages on top of the existing TypeScript /
JavaScript + Python v1 set:

- C
- C++
- C# (CSharp)
- Go
- Rust
- PowerShell

The dispatcher contract, the canonical-signature scheme, the
two-pass insert protocol, and the `ParseResult` data shape are
already locked by the Stage 3.2 design (`doc.go`, `parser.go`,
`dispatcher.go`). This story does NOT redesign any of those --
it adds six new `LanguageParser` implementations behind the
existing seam, registers their file extensions, maps their
language-specific declaration kinds onto the existing
`ClassDecl` / `MethodDecl` / `Import` envelope, and documents
the support matrix.

### 1.1 In scope vs out of scope

| In scope | Out of scope |
| --- | --- |
| CGO / tree-sitter `LanguageParser` implementations for C, C++, C#, Go, Rust | New `Node.kind` values (everything maps to existing `class` / `method` / `block` / `package`) |
| Mapping per-language declaration kinds onto `ClassDecl` / `MethodDecl` | Cross-file resolver for `extends` / `implements` / `static_calls` targets in another file (still deferred to a later story per `dispatcher.go::emit`) |
| Extension registration in `parsers_cgo.go` and (deliberate per-language) handling in `parsers_nocgo.go` | Schema migrations -- the `attrs_json` blob already absorbs all new per-language metadata |
| PowerShell parser strategy and grammar-acquisition decision (Section 6) | Building a new tree-sitter grammar from scratch for languages NOT in this list |
| Fixture-driven tests per language plus dispatcher routing tests | Performance benchmarking of tree-sitter vs scanner (see `tech-spec.md` ops budget) |
| Documentation of the support matrix in `.claude/context/tests.md` | Editing `.claude/context/architecture.md` -- per-language details belong with the story doc |

### 1.2 Guiding principles inherited from Stage 3.2

These contracts are LOCKED -- every component in this document
must respect them.

- **A1 -- One `ParseResult` shape across all languages.** Every
  parser returns `ParseResult{Classes, Methods, Imports}` with
  the same field semantics. Language-specific richness lives
  in `attrs_json`, never in new top-level fields. (`parser.go`
  lines 60-84.)
- **A2 -- Idempotent canonical signatures.** A class is keyed
  by `<repoURL>::class::<relPath>#<QualifiedName>`; a method by
  `<repoURL>::method::<relPath>#<QualifiedName>(<normalised
  params>)`. Whitespace normalisation (`NormalizeSignature`)
  collapses formatter-only diffs to the same fingerprint.
- **A3 -- Two-pass insert protocol.** Pass 1 inserts every
  Class / Method / Block Node so the local-symbol table is
  fully populated; pass 2 resolves and inserts same-file
  `extends` / `implements` / `static_calls` / `reads` /
  `writes` edges. New parsers MUST produce results in the
  shape pass 1 + pass 2 expect; they do NOT call the writer
  directly.
- **A4 -- Same-file resolution only.** Cross-file targets are
  dropped (and the verbatim names persisted on
  `attrs_json["extends_raw"]` / `attrs_json["calls_raw"]`)
  pending the future cross-file resolver story. Adding new
  languages does NOT change this contract.
- **A5 -- Drop on ambiguity, keep on receiver.** Bare-name
  callees that resolve to multiple same-file methods are
  dropped; receiver-qualified callees (`this.foo` /
  `self.foo` and the language-specific equivalents enumerated
  in Section 5) resolve unambiguously against the enclosing
  class. This is the existing `dispatcher.go::buildCalleeIndex`
  / receiver-qualified pass.
- **A6 -- Parse-errors are file-local.** A panicking or erroring
  parser MUST not abort the worker -- `safeParse` recovers
  panics and `EmitFile` swallows parse errors with a warn log.
  The new parsers MUST be defensive against malformed input
  (truncated source, partial UTF-8) on the same terms.
- **A7 -- Build-tag duality.** Production wiring rides
  tree-sitter under `//go:build cgo`; the `make test` portable
  path on Windows toolchains uses scanner-backed fallbacks
  selected by `//go:build !cgo`. New languages enter BOTH
  tag groups OR an explicit decision is recorded that they
  are CGO-only (Section 7).

## 2. Components and Responsibilities

This story introduces no new packages. All new files live in
`services/agent-memory/internal/repoindexer/ast/`. The component
diagram below names existing pieces (unchanged) and the new
pieces added by this story.

```
                +----------------------------------------+
                |  repoindexer.Worker  (Stage 3.1)        |
                |   - walks file tree                     |
                |   - emits one EmitFileEvent per file    |
                +-------------------+--------------------+
                                    |
                                    v
                +----------------------------------------+
                |  ast.Dispatcher  (Stage 3.2, EXISTING)  |
                |   - selectParser(relPath, hints)        |
                |   - safeParse                           |
                |   - two-pass insert (classes -> methods |
                |     -> blocks, then edges)              |
                |   - emitImportsEdges                    |
                +---+---+---+---+---+---+---+----+-------+
                    |   |   |   |   |   |   |    |
       EXISTING ----+   |   |   |   |   |   |    +----- NEW (this story)
                        |   |   |   |   |   |
        tsTreeSitterParser  |   |   |   |   |
        pyTreeSitterParser  |   |   |   |   |
        tsjsParser (no-CGO) |   |   |   |   |
        pythonParser (no-CGO)   |   |   |   |
                                |   |   |   |
                NEW: cTreeSitterParser   |   |   |
                NEW: cppTreeSitterParser     |   |
                NEW: csharpTreeSitterParser      |
                NEW: goTreeSitterParser          |
                NEW: rustTreeSitterParser        |
                NEW: powershellParser (strategy in Section 6)
```

### 2.1 New parser components

Each new parser is a single Go type that satisfies the existing
`LanguageParser` interface (`parser.go` lines 30-55). Their
responsibilities are uniform:

| Component | File (new) | Build tag | Responsibility |
| --- | --- | --- | --- |
| **C parser** | `parser_treesitter_c.go` | `//go:build cgo` | Walk tree-sitter `c` grammar, emit struct-as-class + free functions, `#include` -> Import, `call_expression` -> bare-name calls. |
| **C++ parser** | `parser_treesitter_cpp.go` | `//go:build cgo` | Walk tree-sitter `cpp` grammar, emit class/struct (with `base_class_clause` -> `Extends`), methods (with receiver-qualified `this->foo()`), free functions, `#include` -> Import. |
| **C# parser** | `parser_treesitter_csharp.go` | `//go:build cgo` | Walk tree-sitter `c_sharp` grammar, emit class/interface/struct/record, methods, base list (extends / implements partitioned by kind lookup), `using` -> Import. |
| **Go parser** | `parser_treesitter_go.go` | `//go:build cgo` | Walk tree-sitter `golang` grammar, emit struct/interface, methods with receiver-qualified `r.foo()`, free functions, `import` -> Import (deduped to module path). |
| **Rust parser** | `parser_treesitter_rust.go` | `//go:build cgo` | Walk tree-sitter `rust` grammar, emit struct/enum/trait + `impl` methods + free functions + `use` -> Import. Trait `impl Trait for Type` produces an `Implements` edge. |
| **PowerShell parser** | `parser_powershell.go` (no CGO) + optional `parser_treesitter_powershell.go` (CGO) | conditional, see Section 6 | Scanner-only in v1 (no smacker binding); functions + dot-sourcing / Import-Module + bare command calls. Classes (`class Foo {}`) extracted via regex; nested-method support if grammar wired. |
| **Extension registry update** | `parsers_cgo.go` (modify) and `parsers_nocgo.go` (modify) | `cgo` / `!cgo` | Add the new parsers to `defaultParsers()`. Per Section 7, the no-CGO build either gets a documented "not supported" list or a thin scanner fallback. |
| **Per-language hint aliases** | `dispatcher.go` (modify `normalizeHints`) | none | Add language aliases `c`, `cxx`, `cs`, `csharp`, `golang`, `rs`, `ps`, `ps1`, etc. so `repo.language_hints[]` can override extension routing. |

### 2.2 What the dispatcher does NOT change

The following are explicitly NOT modified by this story:

- `dispatcher.go::emit` -- the two-pass insert protocol is
  language-agnostic; new parsers slot in via `defaultParsers()`.
- `parser.go` -- the `ParseResult` shape and `ClassDecl` /
  `MethodDecl` / `Import` envelopes already absorb every kind
  of construct in the new language set (see Section 4 mapping
  table).
- `block.go::SubdivideMethod` -- the `CountLogicalLines`
  threshold check works on any body source the parser hands
  it; per-language tokenisation rules are out of scope.
- `repoindexer/ast.go` -- the `ASTEmitter` / `EmitFileEvent` /
  `EmitResult` / `TouchedNode` shapes are stable; no new field
  is required.

## 3. Build-Tag Topology

The new parsers participate in the same build-tag scheme the
existing TypeScript / Python pair use (`parsers_cgo.go` /
`parsers_nocgo.go`). Three groups exist after this story:

```
   CGO=1 (production / Linux CI / Mac dev):
       defaultParsers() returns [
         tsTreeSitterParser, pyTreeSitterParser,         # existing
         cTreeSitterParser, cppTreeSitterParser,         # new
         csharpTreeSitterParser, goTreeSitterParser,     # new
         rustTreeSitterParser,                           # new
         powershellParser (scanner) OR
           powershellTreeSitterParser (if grammar wired) # new, Section 6
       ]

   CGO=0 (portable `make test` on stock Windows toolchain):
       defaultParsers() returns [
         tsjsParser, pythonParser,                       # existing
         powershellParser (scanner only)                 # new
       ]
       Files with .c, .cpp, .cs, .go, .rs are SKIPPED by the
       dispatcher (no parser registered -> no_parser log line).
       This is documented in .claude/context/tests.md as the
       "tree-sitter-backed only" set.

   Aliases for repo.language_hints[]:
       c, h           -> "c"
       cc, cxx, hpp   -> "cpp"
       cs, csharp     -> "csharp"
       go, golang     -> "go"
       rs, rust       -> "rust"
       ps, ps1, psm1  -> "powershell"
```

The CGO=0 behaviour for C/C++/C#/Go/Rust is a DELIBERATE
no-op rather than a scanner-fallback. Rationale:

1. Stage 3.2 chose tree-sitter as the canonical parser core
   (`implementation-plan.md` section 3.2 lines 425-427). The
   scanner fallbacks for TS / Python exist because those two
   languages are the v1 release set; reproducing six more
   scanner implementations would (a) duplicate the
   tree-sitter walker logic in a less-correct form, (b)
   inflate the package surface by ~6x without serving a
   production code path, and (c) drift between the two
   implementations would silently flip the dispatcher
   between fingerprints across `CGO_ENABLED` boundaries
   (an A2 violation -- the same source file MUST hash to
   the same canonical signature regardless of build mode).
2. The CGO=0 path's only consumer is the portable Windows
   `make test` target. Files in the new language set are
   covered by the CGO=on tests on Linux CI; the portable
   path proves the dispatcher's "no parser" branch handles
   them safely (no_parser log + clean return, no panic).
3. PowerShell is the one exception -- it ships a SCANNER as
   the primary path (Section 6) because no smacker tree-
   sitter binding exists; both build tag groups therefore
   register the same parser.

## 4. Data Model: Per-Language Mapping

This story emits NO new entity kinds. Every per-language
construct projects onto the existing `ClassDecl` /
`MethodDecl` / `Import` envelope, plus one additive struct
field (`LangMeta`, Section 4.4) used to carry per-language
metadata that the existing envelopes have no field for. The
attrs writers in `dispatcher.go` gain a single merge step to
fold `LangMeta` keys into the persisted `attrs_json` blob.

The per-language tables below are the authoritative mapping;
per-construct extraction rules (the exact tree-sitter node
names walked for each language) are the tech spec's
responsibility.

> **Notation:** when a table cell references
> `attrs_json["<key>"]`, the source of that key is the parser
> writing `LangMeta["<key>"] = ...` and the dispatcher merging
> `LangMeta` into the attrs JSON blob per Section 4.4. The
> dispatcher's existing first-class keys (`language`,
> `decl_kind`, `modifiers`, `params_raw`, `start_line`,
> `end_line`, `enclosing_class`, `calls_raw`, `module`,
> `line`, `symbols`, `alias`, `is_type_only`, `source`,
> `parent_missing`, `members`, `block_kind`, `ordinal`,
> `start_byte`, `end_byte`) are NOT carried in `LangMeta` --
> they remain populated from existing struct fields.

### 4.1 ClassDecl population matrix

Field semantics for `ClassDecl` (`parser.go` lines 94-128)
in each new language:

| Language | What populates `Kind` | What populates `QualifiedName` | What populates `Extends` | What populates `Implements` |
| --- | --- | --- | --- | --- |
| **C** | `"struct"` for `struct_specifier`; `"union"` for `union_specifier`; `"enum"` for `enum_specifier`. No real classes in C; structs are emitted so `contains` edges to functions referring to them stay anchored. | Identifier of the struct/union/enum. Nested structs use parent path joined by `.` (no qualifier chain in C source, so this is empty in practice). | empty | empty |
| **C++** | `"class"` for `class_specifier`; `"struct"` for `struct_specifier`. | Identifier; for nested types the enclosing-class path. Namespace prefix (`ns::Foo`) maps to `ns.Foo` so the `.` separator stays consistent with TypeScript. | Each entry of `base_class_clause` whose access-specifier is NOT none. Templates are stripped via the same `<...>` filter Section 5 lists. | empty (C++ has no `interface` keyword in v1; pure-virtual base classes still go to `Extends` -- the partition is captured in `attrs_json["base_access"]`). |
| **C#** | `"class"` / `"interface"` / `"struct"` / `"record"` / `"enum"` based on tree-sitter node type. | Identifier path; nested types use `.` separators. Namespace info goes to `attrs_json["namespace"]`. | First entry of the base list whose target lookup resolves to a class declaration (same file). | Remaining entries of the base list -- C# syntactic base list mixes class + interface, so the dispatcher's same-file resolver naturally partitions them by what each name resolves to in the file's local symbol table. Verbatim names persist on `attrs_json["base_raw"]`. |
| **Go** | `"struct"` for `type X struct {...}`; `"interface"` for `type X interface {...}`; otherwise `"type_alias"`. | The single type name (Go disallows nesting). | empty (Go has no inheritance; `embedded` types appear in `attrs_json["embeds"]`). | empty (interface satisfaction is structural; no extractor emits an `implements` edge in v1). |
| **Rust** | `"struct"` / `"enum"` / `"trait"` / `"union"` (rare). | Identifier; nested types use `.` separator. | empty for struct/enum. For trait, parent traits in the `: SuperTrait` clause go to `Extends`. | NOT populated by struct/enum; populated for `impl Trait for Type` blocks, which are emitted as a Class-level `Implements` entry on the **Type** class with one entry per trait name (see Section 5 for the `impl` block walker). |
| **PowerShell** | `"class"` for `class Foo {}`. Functions outside a class are free functions (Section 4.2). | Identifier. PowerShell does not allow nested classes in v5; nested unsupported. | The single base type in `class Foo : Bar {}` syntax. | empty (no interface keyword in PowerShell). |

`StartLine` / `EndLine` come from the tree-sitter range for
each declaration (1-based to match the existing TS / Python
parsers).

### 4.2 MethodDecl population matrix

Field semantics for `MethodDecl` (`parser.go` lines 132-226)
in each new language. The `Calls` field is the bare-name
list passed to the dispatcher's bare-name resolver; the
`ReceiverCalls` field is the receiver-qualified list passed
to the receiver-qualified resolver.

| Language | What populates `EnclosingClass` (empty = free function) | What populates `ParamSignature` | What is a receiver call? (-> `ReceiverCalls`) | Modifiers |
| --- | --- | --- | --- | --- |
| **C** | empty -- every function is a free function in v1. | text between the outer `(` `)` of the declarator. | empty in v1 -- C has no `this`. | `static`, `inline`, `extern` from the storage-class specifier. |
| **C++** | The `class_specifier` / `struct_specifier` name when the method is declared inside the class body OR when an out-of-line definition uses a `Foo::bar()` qualified declarator. | text between the outer `(` `)`. Reference / pointer markers preserved verbatim; normalised at signature mint time by `NormalizeSignature`. | `this->X(`, `(*this).X(` -- captured separately. Bare `X(` calls inside a method body that refer to a sibling member are recovered by the dispatcher's same-file callee index (`buildCalleeIndex`). | `static`, `virtual`, `inline`, `constexpr`, `noexcept`, `const` (trailing). |
| **C#** | Enclosing class / struct / record name. Out-of-class methods don't exist in C#. | text between the outer `(` `)`. | `this.X(` -- captured separately (mirrors TS). | `static`, `public`, `private`, `protected`, `internal`, `async`, `override`, `virtual`, `sealed`, `abstract`, `readonly`. |
| **Go** | The receiver type name (stripped of pointer marker `*`) when the method has a receiver clause `func (r *Foo) Bar(...)`. Free functions have empty receiver. | text between the outer `(` `)`, PREFIXED with the synthetic receiver tag `recv=Foo; ` or `recv=*Foo; ` per Section 4.5 so pointer / value receivers produce distinct canonical signatures. | The receiver identifier's qualified calls: when the receiver is `r`, every `r.X(` call is a ReceiverCall. The parser tracks the receiver-binding name per method to recognise the right prefix. | empty in v1 (Go has no method modifiers; `LangMeta["receiver"]` and `LangMeta["receiver_ptr"]` capture the bound name and pointer flag). |
| **Rust** | The `impl` block's target type name. Trait-impl methods (`impl Trait for Type`) attach to the Type class -- the trait name lives in `attrs_json["trait"]`. Free `fn name(...)` outside any `impl` -> empty. | text between the outer `(` `)`. | `self.X(` -- captured separately. | `pub`, `pub(crate)`, `pub(super)`, `async`, `unsafe`, `const`, `extern`. |
| **PowerShell** | Enclosing class name when the method is declared inside `class Foo {}`. Top-level `function Bar {...}` -> empty. | text between the outer `(` `)` for `param(...)` blocks; otherwise empty (PowerShell allows parameter blocks inside the body which the scanner does NOT walk in v1). | `$this.X(` inside a class method. | `static`, `hidden` (class methods); none for free functions. |

#### 4.2.1 Body span semantics

`BodySource` / `BodyStartLine` / `BodyEndLine` / `BodyStartByte` /
`BodyEndByte` follow the same brace-stripping convention the TS
tree-sitter parser uses (`parser_treesitter.go::handleMethod`
lines 268-289): the byte / source content excludes the outer
delimiter (`{` / `}` for C/C++/C#/Go/Rust; `{` / `}` for
PowerShell), but the line numbers stay on the delimiter lines
so a span ingestor that observes a stack frame at the `{`
line still matches.

Rust expression-bodied free functions (`fn f() -> T { expr }`)
follow the same rule -- the `block` node child is the body
span. Rust `let` shorthand for closures is NOT emitted as a
free function in v1.

#### 4.2.2 Calls and MemberAccess matrix

| Language | `Calls` extractor rule | `ReceiverCalls` extractor rule | `MemberAccesses` extractor rule |
| --- | --- | --- | --- |
| **C** | tree-sitter `call_expression` whose function is `identifier`; member-call chains are not extracted. | empty | empty |
| **C++** | `call_expression` whose function is `identifier` or rightmost segment of a `qualified_identifier`. Operator calls dropped. | `this->X` / `(*this).X` accesses in `field_expression` whose right child is the call's `function`. | `this->field` / `(*this).field` in `field_expression` outside any call -- `IsWrite` true when the parent is the LHS of `assignment_expression`. |
| **C#** | `invocation_expression` whose expression is `identifier`. | `this.X` calls (`member_access_expression` with `this`). | `this.field` access; `IsWrite` from LHS of `assignment_expression`. |
| **Go** | `call_expression` whose function is `identifier`. Selector calls (`pkg.Func()`) keep the rightmost segment as the bare name. | `<receiver-name>.X` where `<receiver-name>` is the method's bound receiver. | `<receiver-name>.field` access; `IsWrite` from LHS of `assignment_statement` (`:=` does NOT count -- it's a fresh local binding). |
| **Rust** | `call_expression` whose function is `identifier` or rightmost of `scoped_identifier` (`Mod::func()`). | `self.X(...)` -- captured separately. Method-call expressions `obj.X(...)` outside `self.` are NOT receiver calls in v1. | `self.X` field access; `IsWrite` from being the LHS of `assignment_expression`. |
| **PowerShell** | bare-name commands (`Get-Foo`, `Invoke-Bar`) at statement position. Pipeline targets are extracted. | `$this.X` inside class methods. | `$this.X` field touches; `IsWrite` when on LHS of `=` assignment. |

### 4.3 Import population matrix

`Import` (`parser.go` lines 240-263) is filled per language as
follows. The dispatcher renders each non-relative import as a
synthetic external-package Node + `imports` edge
(`dispatcher.go::emitImportsEdges`); the relative-import filter
(`isRelativeImport`) drops paths beginning with `.` or `/`.

| Language | Import construct | `Module` value | `Symbols` value | `Alias` value | `IsTypeOnly` |
| --- | --- | --- | --- | --- | --- |
| **C** | `#include "foo.h"` (relative) -- DROPPED by `isRelativeImport` because it starts with `.` after canonicalisation? No -- C `#include "foo.h"` is plain identifier. The parser CLASSIFIES `#include "..."` as relative-equivalent by prefixing `./` to `Module`, so `isRelativeImport` filters them. `#include <stdio.h>` -> Module=`stdio.h`. | basename or angle-bracket path | empty | empty | false |
| **C++** | same as C, plus `#include <vector>` -> Module=`vector`. C++20 `import std;` -> Module=`std`. `using namespace ns;` is recorded as `attrs_json` on the file via a future extension, NOT a v1 Import. | as above; for `import std` the module name | empty | empty | false |
| **C#** | `using System.Text;` -> Module=`System.Text`. `using static System.Math;` -> Module=`System.Math` plus `attrs_json["is_static"]=true` on the edge. `using A = B.C;` -> Module=`B.C`, Alias=`A`. | empty (C# `using` does not name symbols) | alias name when present | false (no type-only `using` in v1) |
| **Go** | `import "fmt"` -> Module=`fmt`. `import f "fmt"` -> Module=`fmt`, Alias=`f`. `import . "fmt"` -> Module=`fmt`, Alias=`.` (dot imports are flagged via `attrs_json["dot_import"]=true`). `import _ "side"` -> Alias=`_`, `attrs_json["blank_import"]=true`. | module path | empty | alias when present | false |
| **Rust** | `use std::collections::HashMap;` -> Module=`std::collections`, Symbols=`["HashMap"]`. `use std::io::{Read, Write};` -> Module=`std::io`, Symbols=`["Read","Write"]`. `use std::io::Read as MyRead;` -> Module=`std::io`, Symbols=`["Read"]`, Alias=`MyRead`. Glob `use std::io::*;` -> Symbols=`["*"]`. | crate-prefixed path WITHOUT trailing segment when symbols are listed; full path otherwise | listed names | alias when single | false |
| **PowerShell** | `Import-Module Foo` -> Module=`Foo`. `. .\helpers.ps1` dot-source -> Module=`./helpers.ps1` (relative; dropped). `Using module Foo` -> Module=`Foo`. | as listed | empty | empty | false |

The `IsTypeOnly` flag is reserved for TypeScript and stays
false everywhere else in v1.

### 4.4 Per-language metadata: `LangMeta` envelope extension (STRUCT + WRITER CHANGE)

The existing `ClassDecl`, `MethodDecl`, and `Import` envelopes
(`parser.go` lines 60-263) plus the existing attrs writers
(`classAttrs`, `methodAttrs`, `importEdgeAttrs`,
`externalPackageAttrs`, `memberEdgeAttrs` in
`dispatcher.go` lines 832-961) DO NOT have fields that can
carry the per-language metadata six new languages need
(receiver pointer flag, namespace string, trait name, base
access map, dot/blank/static import flags, etc.). This story
extends both layers; the extension is small, additive, and
backward-compatible.

#### 4.4.1 Struct fields added by this story

One new field per envelope, all of type `map[string]any`
serialised as JSON. The map is nil by default; the existing TS
/ Python parsers continue to leave it nil. Adding a new key
to the map does NOT require a new struct field.

```go
// parser.go -- ADDED by this story:
type ClassDecl struct {
    // ... existing fields ...
    LangMeta map[string]any // nil-able, per-language attrs
}

type MethodDecl struct {
    // ... existing fields ...
    LangMeta map[string]any // nil-able, per-language attrs
}

type Import struct {
    // ... existing fields ...
    LangMeta map[string]any // nil-able, per-language attrs
}
```

#### 4.4.2 Attrs writer changes (dispatcher.go)

The existing writers gain a final merge step that copies any
`LangMeta` keys into the attrs JSON map. Conflicts with
existing first-class keys (`language`, `decl_kind`,
`modifiers`, `params_raw`, `start_line`, `end_line`,
`enclosing_class`, `calls_raw`, `module`, `line`, `symbols`,
`alias`, `is_type_only`, `source`, `parent_missing`,
`members`, `block_kind`, `ordinal`, `start_byte`, `end_byte`)
are resolved by IGNORING the `LangMeta` value -- first-class
keys win. The merge step is implemented once in a helper
(`mergeLangMeta(out map[string]any, in map[string]any)`) and
called from each existing writer:

```go
// dispatcher.go -- ADDED to existing helpers:
func mergeLangMeta(out, in map[string]any) {
    for k, v := range in {
        if _, exists := out[k]; exists {
            continue // first-class key wins
        }
        out[k] = v
    }
}

// classAttrs(...) -- new last step before mustJSON(m):
mergeLangMeta(m, c.LangMeta)

// methodAttrs(...) -- new last step before mustJSON(out):
mergeLangMeta(out, m.LangMeta)

// importEdgeAttrs(...) -- new last step before mustJSON(m):
mergeLangMeta(m, imp.LangMeta)
```

`externalPackageAttrs`, `memberEdgeAttrs`, and `blockAttrs` do
NOT gain a `LangMeta` source (no per-language richness flows
into those edges in v1; if a future language needs it, the
same pattern applies).

#### 4.4.3 Keys each parser writes into `LangMeta`

This table is the authoritative catalogue of every per-language
attrs key this story introduces; "Field" gives the
fully-qualified path from the envelope.

| Key (in `LangMeta`) | Envelope.Field | Type | Populated by | Purpose |
| --- | --- | --- | --- | --- |
| `decl_kind` extensions (`struct`, `union`, `enum`, `record`, `trait`, `type_alias`) | `ClassDecl.Kind` (existing first-class field; already serialised by `classAttrs`) | string | C / C++ / C# / Go / Rust / PowerShell parsers | Filter without re-parsing the canonical signature. Already an existing first-class key; this story expands the value set. |
| `namespace` | `ClassDecl.LangMeta["namespace"]` | string | C++ / C# parsers | Records the enclosing namespace path. The canonical signature drops it per Section 4.1. |
| `base_access` | `ClassDecl.LangMeta["base_access"]` | map[string]string | C++ parser | Access specifier per base class (`public` / `protected` / `private`). |
| `partial` | `ClassDecl.LangMeta["partial"]` | bool | C# parser | Marks a C# `partial class` so consumers can group cross-file fragments (Section 9 R3). |
| `embeds` | `ClassDecl.LangMeta["embeds"]` | []string | Go parser | Records Go embedded type names. |
| `template_params` | `ClassDecl.LangMeta["template_params"]` | []string | C++ parser | Verbatim template-parameter identifier list. |
| `receiver` | `MethodDecl.LangMeta["receiver"]` | string | Go parser | Bound receiver identifier (e.g. `r`). |
| `receiver_ptr` | `MethodDecl.LangMeta["receiver_ptr"]` | bool | Go parser | True for pointer receivers; complements the canonical-signature `*` prefix described in Section 4.5. |
| `trait` | `MethodDecl.LangMeta["trait"]` | string | Rust parser | When the method came from an `impl Trait for Type`, names the trait. |
| `dot_import` | `Import.LangMeta["dot_import"]` | bool | Go parser | Marks Go `import . "fmt"` dot-imports. |
| `blank_import` | `Import.LangMeta["blank_import"]` | bool | Go parser | Marks Go `import _ "side"` side-effect imports. |
| `is_static` | `Import.LangMeta["is_static"]` | bool | C# parser | Marks C# `using static System.Math;`. |
| `cmdlet_verb` | `Import.LangMeta["cmdlet_verb"]` | string | PowerShell parser | The cmdlet verb (`Get`, `Import`, `Using`, ...) that introduced the import. |
| `module_kind` | `Import.LangMeta["module_kind"]` | string | PowerShell parser | One of `Import-Module`, `using_module`, `dot_source`. |

No schema migration is required -- `attrs_json` is `jsonb`
end-to-end (Stage 2.1 / `migration 0001`).

#### 4.4.4 Backward compatibility

The TS / JS / Python parsers leave `LangMeta` nil. The merge
step `mergeLangMeta(out, nil)` is a no-op, so existing
dispatcher attrs output for those parsers is byte-identical
across this change -- the dispatcher tests in
`dispatcher_test.go` continue to pass unchanged.

### 4.5 Canonical signature disambiguation for Go pointer receivers

Go allows both `func (r Foo) Bar()` and `func (r *Foo) Bar()`
in the same source file -- they are distinct methods at the
language level. The existing canonical-signature scheme
(`<repoURL>::method::<relPath>#<QualifiedName>(<normalised
params>)`) does NOT separate them, because:

- `QualifiedName` is `Foo.Bar` for both.
- `ParamSignature` excludes the receiver clause (it covers
  only the formal parameter list after the function name).

PINNED RULE (this story): the Go parser prepends a synthetic
receiver tag to `ParamSignature` so the canonical signature
differs between the two cases, while keeping `QualifiedName`
and `EnclosingClass` as the bare type name so the existing
dispatcher symbol-table semantics (`methodNodeID[m.QualifiedName]`,
receiver-qualified lookup `methodNodeID[m.EnclosingClass + "." +
callee]`) work uniformly with every other language.

| Source declaration | `EnclosingClass` | `QualifiedName` | `ParamSignature` | Resulting canonical signature |
| --- | --- | --- | --- | --- |
| `func (r Foo) Bar(s string)` | `Foo` | `Foo.Bar` | `recv=Foo; s string` | `<url>::method::<rel>#Foo.Bar(recv=Foo; s string)` |
| `func (r *Foo) Bar(s string)` | `Foo` | `Foo.Bar` | `recv=*Foo; s string` | `<url>::method::<rel>#Foo.Bar(recv=*Foo; s string)` |

The `recv=<type>` prefix is the Go-specific marker. Whitespace
normalisation (`NormalizeSignature`) preserves the semicolons
and the `*` so the two strings hash to distinct fingerprints.
`LangMeta["receiver_ptr"]` carries the boolean form for
consumers; `LangMeta["receiver"]` carries the bound receiver
identifier (e.g. `r`).

Consequences:

- Two distinct canonical signatures, hence two distinct
  fingerprints. A2 is satisfied -- the two methods produce
  two distinct Node rows.
- `methodNodeID[m.QualifiedName]` collides on `Foo.Bar` when
  both forms appear in the same file. The dispatcher's
  pass 1b loop currently overwrites on collision; with two
  declarations the second-inserted method wins the lookup.
  Both methods are persisted (distinct fingerprints), but
  the same-file `static_calls` resolver can only point at
  the second. This is a same-file edge-case the v1 contract
  accepts -- the verbatim `calls_raw` attrs persist intent so
  the future cross-file resolver story can stitch the correct
  target. The case is rare in practice (Go style guides
  discourage value/pointer receiver mixing on the same type).
- Cross-file pointer/value collision is impossible (canonical
  signatures embed `relPath`).
- A pure bare-name call `Bar()` inside the same file with
  both receivers present resolves ambiguously and is dropped
  per A5 (the simple-name extraction in `buildCalleeIndex`
  recovers `Bar` from both `Foo.Bar` entries; the resolver
  drops on `len(ids) > 1`).
- No other language in this story has the same problem:
  - C# methods are uniquely keyed by enclosing type + name.
  - Rust trait-impl methods route to the Type class; trait
    identity lives in `LangMeta["trait"]`.
  - C++ `Foo::bar` and `Bar::bar` are already distinct
    because `EnclosingClass` differs.
  - C has no methods.
  - PowerShell methods live in a class body without overload
    semantics.

## 5. Interfaces between Components

### 5.1 LanguageParser (UNCHANGED)

All six new parsers implement the existing interface verbatim:

```go
type LanguageParser interface {
    Language() string
    Extensions() []string
    Parse(relPath string, src []byte) (ParseResult, error)
}
```

Language IDs are the canonical lower-case forms (`c`, `cpp`,
`csharp`, `go`, `rust`, `powershell`). The IDs ride on every
emitted Node's `attrs_json["language"]` so downstream tooling
routes per-language without re-parsing.

### 5.2 Extension claims

| Parser ID | Extensions claimed |
| --- | --- |
| `c` | `.c`, `.h` (header files are parsed as C in v1; see note below) |
| `cpp` | `.cc`, `.cpp`, `.cxx`, `.c++`, `.hpp`, `.hh`, `.hxx`, `.h++` |
| `csharp` | `.cs`, `.csx` |
| `go` | `.go` |
| `rust` | `.rs` |
| `powershell` | `.ps1`, `.psm1`, `.psd1` |

Note on `.h`: C and C++ both use `.h` for headers in practice.
The dispatcher's `extMap` is a 1:1 map, so the extension can
only route to ONE parser by default. **PINNED RULE (this
story): `.h` files route to the C parser unconditionally in
v1; no override mechanism for `.h` -> C++ is shipped.**
Rationale and constraints:

- The existing `selectParser` resolution order (extension
  match first; `LanguageHints` fall-back only when the
  extension does NOT match a registered parser) is unchanged.
  Because `.h` matches the C parser, a `cpp` hint on the
  repo CANNOT redirect `.h` files at lookup time -- the
  extension match wins and the hint never fires. This is by
  design: changing `selectParser` to consult hints BEFORE
  extension would let a repo with one C++-style `.h` file
  break TS / Python routing in the same repo, an A2 /
  A7 regression.
- Repos that mix C and C++ should use the distinct C++ header
  extensions (`.hpp`, `.hh`, `.hxx`, `.h++`) for C++-only
  headers. The C parser DOES accept the C-subset of C++
  header syntax (extern "C" declarations, plain function
  prototypes) without producing spurious nodes, so most
  cross-language `.h` headers degrade gracefully -- they
  emit free-function declarations and `#include` imports
  but skip C++-specific constructs (class / template /
  namespace).
- A follow-up story can introduce a per-repo "extension
  overrides" mechanism (e.g. `repo.extension_overrides[]`
  with entries like `.h:cpp`) that wires into `selectParser`
  ahead of the `extMap` lookup. This is explicitly out of
  scope for this story; the data model and dispatcher
  contract are not extended for it.

### 5.3 Dispatcher hooks (MOSTLY UNCHANGED)

The dispatcher's `selectParser` / `safeParse` / `emit` /
`emitImportsEdges` entry points are unchanged. The edits this
story makes are:

1. `defaultParsers()` in `parsers_cgo.go` -- append the five
   new tree-sitter parsers (Section 3).
2. `defaultParsers()` in `parsers_nocgo.go` -- append the
   PowerShell scanner only.
3. `normalizeHints` -- expanded alias table (Section 3).
4. `classAttrs`, `methodAttrs`, `importEdgeAttrs` -- final
   `mergeLangMeta` call (Section 4.4.2). No other writer
   gains a `LangMeta` source in v1.

`selectParser`'s resolution order does NOT change. Extension
match remains highest precedence; per-event hints fire only
when the extension does not match a registered parser. See
Section 5.2's pinned `.h` rule for the consequence.

### 5.3.1 Updated attrs writer signatures

The merge step is the only writer change. Signatures are
unchanged; the helper `mergeLangMeta` is package-private. The
TouchedNodes contract (`EmitResult.TouchedNodes`) is
unchanged because `LangMeta` keys are NOT part of the
canonical signature -- two files that differ ONLY in
`LangMeta` would still collide on the same fingerprint, and
this is intentional: per-language metadata is descriptive,
not identifying.

### 5.4 Writer interface (UNCHANGED)

The `nodeEdgeWriter` interface (`dispatcher.go` lines 24-27)
is the same: `InsertNode` + `InsertEdge`. New parsers do NOT
hold a writer reference; they return `ParseResult` and the
dispatcher writes.

### 5.5 Embedding publisher hook (UNCHANGED)

`Dispatcher.publishNodeEmbedding` fires for every Method and
Block Node regardless of source language. New parsers MUST
populate `MethodDecl.BodySource` (or leave it empty and the
dispatcher will fall back to signature-only publish per
`dispatcher.go` lines 447-466). C / C++ / C# / Go / Rust
function declarations always have a body -- only interface
methods (C# / Rust trait declarations) hit the signature-only
path.

### 5.6 Per-event language hint integration

`EmitFileEvent.LanguageHints` is the operator-controlled
override path. `normalizeHints` resolves the new aliases to
the canonical parser ID (Section 3). The dispatcher's
`selectParser` already implements the precedence order:

1. File extension exact match (highest priority);
2. per-event `LanguageHints` (from `repo.language_hints[]`);
3. dispatcher-default `WithLanguageHints` (lowest).

This story adds NO new precedence rule -- only new aliases.

## 6. PowerShell Strategy (load-bearing decision)

PowerShell is the one language in this story where the existing
smacker tree-sitter binding set provides NO grammar. Confirmed
by inspecting the vendored module
(`github.com/smacker/go-tree-sitter@v0.0.0-20240827094217...`):
the available bindings are bash, c, cpp, csharp, css, cue,
dockerfile, elixir, elm, golang, groovy, hcl, html, java,
javascript, kotlin, lua, markdown, ocaml, php, protobuf,
python, ruby, rust, scala, sql, svelte, swift, toml, typescript,
yaml. No PowerShell.

Three options were considered for v1:

### 6.1 Option A -- Scanner-only PowerShell (RECOMMENDED for v1)

A new `parser_powershell.go` (NO build tag) using a regex /
scanner approach modeled on `parser_python.go` and
`parser_typescript.go`. Coverage:

- Top-level `function Name { ... }` declarations -> `MethodDecl`
  with empty `EnclosingClass`.
- `class Foo { method bar() {...} }` -> `ClassDecl` + nested
  `MethodDecl` (regex-located by class body brace match, same
  technique `parser_typescript.go` uses).
- `Import-Module`, `Using module`, `. ./helpers.ps1` ->
  `Import` (the dot-source case is filtered by
  `isRelativeImport` because Module is prefixed with `./`).
- Bare cmdlet calls (`Get-Foo`, `Invoke-Bar`) inside a function
  body -> `MethodDecl.Calls`.
- `$this.X(` inside class methods -> `ReceiverCalls`.
- No tree-sitter dependency, no new module, no C toolchain
  requirement; ships in both CGO=on and CGO=off builds.

Strengths: zero new dependency surface; uniform across build
tags so the canonical signature is identical; consistent with
the existing Python / TS scanner-fallback codebase.

Weaknesses: same accuracy ceiling as the Python / TS scanners
-- string-literal masking edge cases, comment-only files in
PSD1 manifests, etc. The v1 fixture set is sized so these
edges are not exercised.

### 6.2 Option B -- Add an external tree-sitter PowerShell grammar

There IS an upstream community grammar (`tree-sitter-powershell`)
maintained under various GitHub forks. Wiring it would require:

1. Adding the grammar's Go binding directory under
   `github.com/<fork>/tree-sitter-powershell` to `go.mod`.
2. Cross-checking the C source is CGO-friendly on Windows /
   Linux / Mac toolchains.
3. Implementing `parser_treesitter_powershell.go` mirroring the
   existing TypeScript / Python tree-sitter walkers.

Strengths: production-quality coverage; matches the canonical
"tree-sitter is the parser core" mandate from
`implementation-plan.md` section 3.2.

Weaknesses: introduces a new third-party C dependency; the
build matrix grows by one (CGO=off Windows must still ship the
scanner fallback, so both implementations exist); upstream
maintenance of the PowerShell grammar is not first-party.

### 6.3 Option C -- Vendor a binding directly

Same as Option B but with the grammar pinned in
`internal/repoindexer/ast/grammars/powershell/` and the Go
binding written in-tree. Highest control, highest maintenance
cost. NOT recommended for v1.

### 6.4 v1 decision (PINNED)

**Ship Option A (scanner-only).** This is the v1 commitment;
no operator decision is pending. The scanner approach is the
lowest-risk landing for the 13-point budget; wiring a new C
dependency in the same story risks blowing the budget on
toolchain debugging. The decision is recorded here in-doc
rather than as an open question.

Promotion path to Option B (or C) is a follow-up workstream:
add a `parser_treesitter_powershell.go` behind `//go:build
cgo`, list the smacker-style binding under `go.mod`, and
extend `defaultParsers()` in `parsers_cgo.go` to prefer the
tree-sitter parser when CGO is on while keeping the scanner
as the CGO=off path. The follow-up is in scope for a new
story; this architecture document does NOT extend its scope.

## 7. End-to-End Sequence Flows

The dispatcher's existing `EmitFile` sequence is unchanged.
This section walks the three primary scenarios for the new
languages, calling out where each parser's per-language
behaviour shows up.

### 7.1 Scenario: Go source file with method, free function, import, and same-file call

Setup: a Go file `internal/foo/foo.go` declaring `type Greeter
struct{...}`, method `func (g *Greeter) Greet(...) string`,
free function `func formatGreeting(...) string` called from
`Greet`, and `import "fmt"`.

```
Worker walks repo  ->  EmitFileEvent {RepoID, RepoURL, SHA,
                       FileNodeID, RepoNodeID,
                       RelPath="internal/foo/foo.go",
                       LanguageHints=[],
                       Open=...}
       v
Dispatcher.EmitFile
       v
selectParser("internal/foo/foo.go", [])
   extMap[".go"]  ->  goTreeSitterParser  (CGO=on path)
       v
readEvent(ev)  ->  src=[]byte of the file
       v
safeParse(goTreeSitterParser, "internal/foo/foo.go", src)
       v
goTreeSitterParser.Parse:
   - parses with smacker/go-tree-sitter "golang" grammar
   - walks "source_file" -> "type_declaration" -> "type_spec"
     -> "struct_type"     :: emits ClassDecl{
         QualifiedName="Greeter", Kind="struct",
         StartLine=N, EndLine=N+K }
   - walks "method_declaration" with receiver "(g *Greeter)"
     :: emits MethodDecl{
         QualifiedName="Greeter.Greet",
         EnclosingClass="Greeter",
         ParamSignature="recv=*Greeter; name string",  # per Section 4.5
         BodySource=stripped braces,
         Calls=["formatGreeting"],
         ReceiverCalls=[],          # no g.X calls in this body
         MemberAccesses=[],
         Modifiers=[],
         LangMeta={"receiver":"g", "receiver_ptr":true} }
   - walks "function_declaration" `formatGreeting`
     :: emits MethodDecl{
         QualifiedName="formatGreeting",
         EnclosingClass="",
         ParamSignature="prefix string, name string",
         BodySource=..., Calls=[], ReceiverCalls=[],
         Modifiers=[] }
   - walks "import_declaration" with `"fmt"`
     :: emits Import{Module="fmt", Symbols=[], Alias="",
                     Line=2, IsTypeOnly=false}
       v
ParseResult returned to dispatcher
       v
Dispatcher.emit -- two-pass insert:
   Pass 0: emitImportsEdges
       fmt is not isRelativeImport -> insert synthetic package
         Node sig = repoURL+"::package::ext::fmt",
                    AttrsJSON {language:"go", module:"fmt",
                               source:"external"}
       insert imports edge file -> fmt package
   Pass 1a: insert ClassDecl Greeter
       sig = repoURL+"::class::internal/foo/foo.go#Greeter"
       AttrsJSON {language:"go", decl_kind:"struct", ...}
       insert contains edge file -> Greeter class
   Pass 1b: insert MethodDecl Greeter.Greet
       parentID = Greeter class node
       sig = repoURL+"::method::internal/foo/foo.go#Greeter.Greet
             (recv=*Greeter; name string)"   # per Section 4.5
       AttrsJSON {language:"go", enclosing_class:"Greeter",
                  receiver:"g", receiver_ptr:true,
                  params_raw:"recv=*Greeter; name string", ...}
       (receiver / receiver_ptr came from LangMeta via
        mergeLangMeta in methodAttrs; params_raw is the
        existing first-class key carrying ParamSignature.)
       insert contains edge class -> method
       publishNodeEmbedding (Method)
       SubdivideMethod (body has <80 logical lines) -> no Blocks
   Pass 1b: insert MethodDecl formatGreeting (free function)
       parentID = FileNodeID
       sig = repoURL+"::method::internal/foo/foo.go#formatGreeting
             (prefix string, name string)"
       AttrsJSON {language:"go", ...}
       insert contains edge file -> method
       publishNodeEmbedding (Method)
   Pass 2a: extends / implements
       Greeter has no Extends/Implements -> no edges emitted
   Pass 2b: static_calls
       calleeIndex (built from methodNodeID, sorted, unique
                    simple names) = {
         "Greet":            <Greeter.Greet node>,
         "formatGreeting":   <formatGreeting node>
       }
       Greet.Calls = ["formatGreeting"]
         -> resolves to formatGreeting node
         -> insert static_calls Greet -> formatGreeting
       Greet.ReceiverCalls = []  (no g.X calls)
   Pass 2c: reads / writes
       Greet.MemberAccesses = []  -> no edge emitted
       v
EmitFile returns EmitResult{TouchedNodes=[Greeter class,
                                          Greeter.Greet method,
                                          formatGreeting method,
                                          fmt package]}
```

### 7.2 Scenario: Rust trait impl with same-file static call and `use` import

Setup: a Rust file `src/lib.rs` declaring `trait Greeter
{...}`, `struct GreeterImpl;`, `impl Greeter for GreeterImpl
{ fn greet(&self,...) {...} }`, free function `fn
format_greeting(...)`, and `use std::fmt::Display;`.

```
selectParser("src/lib.rs", [])
   extMap[".rs"]  ->  rustTreeSitterParser
       v
rustTreeSitterParser.Parse:
   - "trait_item"  ::  ClassDecl{
       QualifiedName="Greeter", Kind="trait", Extends=[],
       Implements=[]}
   - "struct_item" GreeterImpl  ::  ClassDecl{
       QualifiedName="GreeterImpl", Kind="struct"}
   - "impl_item" with trait `Greeter` for `GreeterImpl`  ::
       (i) appends "Greeter" to GreeterImpl.Implements
       (ii) for each `function_item` in the impl body:
            emit MethodDecl{
              QualifiedName="GreeterImpl.greet",
              EnclosingClass="GreeterImpl",
              attrs_json["trait"]="Greeter" }
              ParamSignature="&self, name: &str",
              BodySource=...,
              Calls=["format_greeting"],
              ReceiverCalls=[]
   - "function_item" format_greeting at file scope  ::
       MethodDecl{QualifiedName="format_greeting", ...,
                  Modifiers=["pub"]}
   - "use_declaration" std::fmt::Display
       :: Import{Module="std::fmt", Symbols=["Display"]}
       v
Dispatcher pass 1: Greeter class, GreeterImpl class,
                   greet method (parent=GreeterImpl),
                   format_greeting method (parent=file)
Dispatcher pass 2a:
   GreeterImpl.Extends=[]      -> no edges
   GreeterImpl.Implements=["Greeter"]
     -> classNodeID["Greeter"] resolves -> insert implements
        GreeterImpl -> Greeter
Dispatcher pass 2b:
   GreeterImpl.greet.Calls=["format_greeting"]
     -> resolves -> static_calls edge
Dispatcher pass 0:
   std::fmt is non-relative -> external package node
   imports edge file -> std::fmt with attrs.symbols=["Display"]
```

### 7.3 Scenario: C++ class with inheritance, `this->` call, and `#include`

Setup: a C++ file `src/greeter.cpp` declaring
`class Greeter : public Base { void greet(); };` and
`void Greeter::greet() { this->log(); log_global(); }`,
plus `#include "base.h"` and `#include <string>`.

```
selectParser("src/greeter.cpp", [])  ->  cppTreeSitterParser
       v
cppTreeSitterParser.Parse:
   - class_specifier `Greeter` :: ClassDecl{
       QualifiedName="Greeter", Kind="class",
       Extends=["Base"], Implements=[],
       attrs_json["base_access"]={"Base":"public"}}
   - in-class method declaration `void greet();` (no body)
     :: MethodDecl{QualifiedName="Greeter.greet",
                   EnclosingClass="Greeter",
                   BodySource="", BodyStartLine=0, ...}
   - out-of-line definition `void Greeter::greet() {...}`
     :: MethodDecl{QualifiedName="Greeter.greet",
                   EnclosingClass="Greeter",
                   BodySource="this->log(); log_global();",
                   ReceiverCalls=["log"],
                   Calls=["log_global"], ...}
   NOTE: the dispatcher's two-pass insert MUST not double-
   insert "Greeter.greet". The C++ parser de-duplicates by
   QualifiedName before returning; the version with a non-
   empty body wins. See the open-question block on
   declaration/definition handling.
   - free function log_global :: MethodDecl{
       QualifiedName="log_global", EnclosingClass="",
       BodySource=...}
   - `#include "base.h"`  ::  Module="./base.h"
        -> filtered by isRelativeImport (treated relative)
   - `#include <string>`  ::  Module="string"
       v
Dispatcher pass 0:
   "string" -> external package node + imports edge
   "./base.h" -> dropped (relative)
Dispatcher pass 1:
   Greeter class + contains edge
   Greeter.greet method (body version) + contains edge
   log_global method + contains edge
Dispatcher pass 2a:
   Greeter.Extends=["Base"]  -> classNodeID["Base"] missing
     (Base lives in another file) -> edge dropped
     extends_raw=["Base"] persists on the Greeter class
     attrs (existing dispatcher.classAttrs path)
Dispatcher pass 2b:
   Greeter.greet.ReceiverCalls=["log"]:
     - EnclosingClass="Greeter"
     - lookup methodNodeID["Greeter.log"] -> missing
       (log is declared in Base, cross-file) -> no edge
       (drop on missing -- consistent with A4 / A5)
   Greeter.greet.Calls=["log_global"]:
     - calleeIndex["log_global"] -> log_global node
     - insert static_calls Greeter.greet -> log_global
Dispatcher pass 2c:
   Greeter.greet.MemberAccesses=[]  -> no edge
```

### 7.4 Failure scenarios (inherited contract)

The contract is unchanged from Stage 3.2:

- **Parse panic** -- `safeParse` recovers, returns error,
  `EmitFile` logs `ast.parse.error` at warn and returns nil.
  Worker continues with the next file.
- **Tree-sitter grammar mismatch** (e.g. PowerShell file when
  no PowerShell parser is registered) -- `extMap` lookup
  misses, `LanguageHints` does not name a registered parser,
  `selectParser` returns nil, `EmitFile` logs
  `ast.dispatch.skip` at debug. No Node / Edge writes.
- **Embedding publisher recorded-failure** -- per existing
  `publishNodeEmbedding` policy, log warn and continue. No
  language-specific change.
- **Writer error** -- propagates up through `EmitFile` so the
  Stage 3.4 delta handler sees a non-nil error and the ingest
  job records `status='failed'`. Same as today.

## 8. Cross-Component Concerns

### 8.1 Determinism

`ParseResult` ordering is the same source-order contract the
existing parsers honour:

- `Classes` -- source order top to bottom.
- `Methods` -- source order; a method that appears inside a
  class declaration is emitted AFTER the class.
- `Imports` -- source order.

Within a method, `Calls` / `ReceiverCalls` are deduplicated
in insertion order (per `parser.go` lines 187-211). New
parsers MUST use the same `uniqueStringsInsert` helper.

### 8.2 Canonical signature stability across CGO=on / CGO=off

For C / C++ / C# / Go / Rust this constraint does not apply
because the CGO=off path does NOT register those parsers
(Section 3 -- no scanner fallback). For PowerShell, the
scanner is the single implementation, so the constraint is
trivially satisfied.

### 8.3 Logging

The existing structured-log keys are reused verbatim:

- `ast.dispatch.skip` with `reason=no_parser` -- emitted on
  language miss (most likely for `.c` / `.cpp` / `.cs` / `.go`
  / `.rs` files under CGO=0 builds).
- `ast.parse.error` -- emitted on per-file parse failure.
- `ast.parse.panic` -- emitted on recovered panic.
- `ast.dispatch.ok` -- emitted on success with the
  `language` field set to the new parser's ID.
- `ast.imports.skip_relative` -- emitted for any relative
  import (C `#include "..."`, PowerShell dot-source,
  Rust local `mod` declarations -- which are NOT emitted as
  Imports at all in v1).

### 8.4 Test surface

Per the story description's tests section:

- Each language gets a fixture-driven test in
  `parser_treesitter_<lang>_test.go` (CGO=on) plus
  `parser_<lang>_test.go` for PowerShell (no CGO).
- Cross-language dispatcher tests in `dispatcher_test.go`
  cover routing (extension -> parser), miss handling
  (unknown extension -> nil parser -> skip log), and
  duplicate registration determinism.
- The `.h` vs `.hpp` rule is exercised by a routing test that
  asserts `selectParser("a.h", nil).Language() == "c"` and
  `selectParser("a.hpp", nil).Language() == "cpp"`, plus a
  test confirming the pinned Section 5.2 rule: even when
  `LanguageHints` is `["cpp"]`, `selectParser("a.h",
  []string{"cpp"})` returns the C parser because extension
  match is highest precedence. (The test documents the
  follow-up workstream that would add an extension-override
  knob.)

The implementation plan owns the file list; the tech spec
owns the fixture contents.

### 8.5 Documentation deliverable

This story updates `.claude/context/tests.md` (NOT
`.claude/context/architecture.md`) with the support matrix:

| Language | CGO=on (production) | CGO=off (`make test` portable) | Notes |
| --- | --- | --- | --- |
| TypeScript / JavaScript | tree-sitter | scanner fallback | unchanged |
| Python | tree-sitter | scanner fallback | unchanged |
| C | tree-sitter | NOT supported | files skipped with `ast.dispatch.skip` |
| C++ | tree-sitter | NOT supported | files skipped |
| C# | tree-sitter | NOT supported | files skipped |
| Go | tree-sitter | NOT supported | files skipped |
| Rust | tree-sitter | NOT supported | files skipped |
| PowerShell | scanner | scanner | grammar binding decision deferred (Section 6) |

The "tree-sitter-backed only" caveat and the PowerShell
grammar-acquisition note are quoted in the table.

## 9. Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| **R1** -- C++ declaration vs definition double-count. A class member function declared in a header (no body) and defined in a `.cpp` file (with body) would create two `MethodDecl` entries with the same QualifiedName. | The C++ parser deduplicates by QualifiedName within a single file at the end of `Parse`. Cross-file duplicates (header + cpp in separate files) are a future cross-file resolver problem and stay as two `method` nodes whose canonical signatures differ in `relPath` -- correct per A2. |
| **R2** -- Go method receiver pointer / value variants. `func (r *Foo) Bar()` and `func (r Foo) Bar()` are distinct methods at the language level. The receiver clause is OUTSIDE the formal `ParamSignature`, so without intervention the two methods collide on `<rel>::method#Foo.Bar()`. | **PINNED RULE per Section 4.5**: the Go parser prepends a synthetic `recv=Foo;` or `recv=*Foo;` tag to `ParamSignature` so canonical signatures differ -> distinct fingerprints. `QualifiedName` and `EnclosingClass` stay as the bare type name `Foo` so the dispatcher's local symbol-table semantics work uniformly with other languages. `LangMeta["receiver_ptr"]` carries the boolean. Same-file `methodNodeID[Foo.Bar]` collision is accepted as an A5 corner case -- both nodes are persisted, only one wins the local lookup; `calls_raw` preserves intent. |
| **R3** -- C# `partial class` declarations across multiple files. v1 treats each partial file as its own ClassDecl with the same QualifiedName but different relPath -> different canonical signatures, hence distinct nodes. The cross-file resolver will stitch them via a future `partial_of` edge. | Documented as a future story; no v1 work required. The `LangMeta["partial"]=true` flag is set on each partial class so the consumer can group by name. |
| **R4** -- Rust trait method default impl. A trait declaration with a default-bodied method produces both a MethodDecl on the trait class (Kind="trait") AND, separately, an `impl Trait for Type` method shadows it on the Type class. | **PINNED RULE**: v1 does NOT emit an `overrides` edge from Type.method -> Trait.method. The v1 edge scope in `doc.go` lists `contains`, `extends`, `implements`, `static_calls`, `imports`, `reads`, `writes`; adding `overrides` is a schema / migration question that exceeds this story. The trait identity is recorded on `LangMeta["trait"]` of the impl method; a future workstream can derive the `overrides` edge by joining trait method nodes to impl method nodes via that key. The same-file ambiguity case is handled by `buildCalleeIndex`'s drop-on-collision rule (A5). |
| **R5** -- PowerShell scanner accuracy on heredocs / nested strings. | Same risk class the Python / TS scanners already have; scope-limited per Section 6. Fixture set sized so heredoc edge cases are not exercised. |
| **R6** -- `.h` ambiguity. C / C++ both use `.h`; the v1 routing sends `.h` to the C parser. | **PINNED PER SECTION 5.2**: `.h` routes to C unconditionally in v1; no hint-override mechanism is shipped. Repos with C++-only headers must use `.hpp` / `.hh` / `.hxx` / `.h++`. A follow-up story can add a per-repo extension-override knob that fires ahead of `extMap` in `selectParser`. |

## 10. Pinned Decisions and Out-of-Story Workstreams

This story makes the following v1 decisions in-doc (no
operator question is pending on any of them):

- **`.h` routing** -- PINNED: `.h` files route to the C parser
  unconditionally; no hint-override mechanism is shipped. See
  Section 5.2 for the rationale and Section 9 R6 for the risk
  acknowledgement. A follow-up story may add a per-repo
  extension-override knob.
- **PowerShell grammar acquisition** -- PINNED: ship Option A
  (scanner-only) in v1. See Section 6.4. Promotion to a
  tree-sitter binding is a follow-up workstream.
- **Go pointer-receiver fingerprint disambiguation** -- PINNED:
  Go parser prepends a synthetic `recv=Foo;` or `recv=*Foo;`
  prefix to `ParamSignature` so canonical signatures differ
  between value and pointer receivers. `QualifiedName` and
  `EnclosingClass` stay as the bare type name so dispatcher
  semantics work uniformly with other languages. See Section
  4.5 and Section 9 R2.
- **Rust trait `overrides` edge** -- PINNED: v1 does NOT
  emit an `overrides` edge. Trait identity is recorded on
  `LangMeta["trait"]` of the impl method; a future
  workstream may derive the edge by joining trait method
  nodes to impl method nodes via that key. See Section 9 R4.

The following workstreams are explicitly out of scope and
will be picked up by future stories:

- **Cross-file resolver** -- still deferred per A4 / A5.
  Cross-file `extends` for C++ `Base`, cross-file
  `static_calls` for Rust `mod` references, cross-file
  `using_module` for PowerShell -- all dropped today, all
  carry verbatim names on `LangMeta` and the existing
  `extends_raw` / `calls_raw` attrs so the future resolver
  can re-emit them.
- **Go interface satisfaction (`implements`)** -- Go's
  structural typing means a `struct` implements an
  `interface` without any syntactic marker. Inferring it
  requires whole-program analysis; explicitly out of scope
  for v1.
- **C++ namespace + template handling beyond name extraction**
  -- the `<...>` strip rule loses template-parameter
  identity. `LangMeta["template_params"]` records the
  verbatim list but it is not used for resolution.
- **Extension-override knob for `.h` -> C++ routing** -- the
  data model change required (a new `repo` column or a new
  dispatcher option) exceeds this story's scope.
- **PowerShell tree-sitter grammar binding** -- promotion
  from Option A to Option B / C is a separate workstream.

## Iteration Summary

- Path: `docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md`
- Covers from the story description: (1) v1 extraction scope
  per language (Section 4 matrices), (2) parser library
  coverage for C / C++ / C# / Go / Rust and the PowerShell
  pinned-Option-A decision (Section 6), (3) new parser file
  plan (Section 2.1 table), (4) extension registration in
  `parsers_cgo.go` / `parsers_nocgo.go` (Section 3), (5)
  mapping of language constructs into existing graph model
  including the explicit struct + writer extension for
  `LangMeta` (Sections 4.1 - 4.5), (6) note on fixture-
  driven tests (Section 8.4), (7) cross-language dispatcher
  tests (Section 8.4), (8) support matrix documentation
  hand-off to `.claude/context/tests.md` (Section 8.5), (9)
  end-to-end sequences for the primary language scenarios
  (Section 7.1 / 7.2 / 7.3) and the failure case (Section
  7.4).
- Not covered (deliberately, owned by sibling docs): per-step
  file creation order (implementation-plan.md), fixture text
  bodies and exact regex patterns for the scanner-mode
  PowerShell parser (tech-spec.md), and operator-visible
  scenario walk-throughs framed as Given/When/Then
  (e2e-scenarios.md).

### Prior feedback resolution

- [x] 1. FIXED -- Sections 5.2, 6.4, 4.5, 9 R2 / R4 / R6, and
      Section 10 -- all four open operator questions are now
      pinned in-doc as v1 decisions (`.h` -> C unconditionally;
      PowerShell scanner-only; Go receiver-prefix in
      ParamSignature; Rust `overrides` edge NOT emitted in
      v1). The `## Iteration 1 open questions for the
      operator` block at the end of the file has been
      removed and the open-questions JSON fenced block is
      omitted from this reply.
      Verification:
      ```
      $ grep -nF "Iteration 1 open questions for the operator" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "See open-questions block" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "operator may pin" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
- [x] 2. FIXED -- Section 4.4 -- replaced the misleading
      "without changing the attrs writers themselves" sentence
      with an explicit struct + writer extension. New
      Sections 4.4.1 (struct fields: `LangMeta map[string]any`
      added to `ClassDecl`, `MethodDecl`, `Import`), 4.4.2
      (writer changes: `mergeLangMeta` helper called from
      `classAttrs`, `methodAttrs`, `importEdgeAttrs`), 4.4.3
      (the authoritative key catalogue moved from "added
      without changes" to "carried via `LangMeta`"), and
      4.4.4 (backward compatibility -- TS / Python parsers
      leave LangMeta nil so existing tests are byte-identical
      across the change). The Section 4 preface adds a
      notation note that `attrs_json["<key>"]` references in
      the per-language matrices represent the persisted form
      populated via `LangMeta`.
      Verification:
      ```
      $ grep -nF "without changing the attrs writers themselves" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "This story adds the following keys without changing" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
- [x] 3. FIXED -- Section 5.2 -- the misleading paragraph
      claiming `repo.language_hints[]` can override `.h` from
      C to C++ has been replaced with a pinned rule that
      `.h` files route to the C parser unconditionally in v1
      and no override mechanism is shipped. Section 9 R6 is
      aligned. Section 8.4 test-surface description is
      aligned. Section 10 records the extension-override knob
      as a follow-up.
      Verification:
      ```
      $ grep -nF "Repos with C++ projects that use `.h` for headers can override via" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "operators with C++ `.h` projects override via" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
- [x] 4. FIXED -- Section 4.5 (new section) and Section 9 R2
      pin the Go pointer-receiver fingerprint rule: the
      parser prepends `recv=Foo; ` or `recv=*Foo; ` to
      `ParamSignature` so canonical signatures differ; the
      `QualifiedName` and `EnclosingClass` stay as the bare
      type name `Foo` so dispatcher symbol-table semantics
      work uniformly with every other language. The Section
      7.1 Go sequence trace updates the emitted ParamSignature
      and the canonical signature line accordingly. The
      Section 4.2 MethodDecl Go row mentions the receiver
      prefix.
      Verification:
      ```
      $ grep -nF "canonical signature includes the parameter list verbatim, so `(g *Greeter)` vs `(g Greeter)`" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "ParamSignature is empty (Go puts the receiver in its own clause)" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "ParamSignature=\"name string\"" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
