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
  primarily in `attrs_json`. This story makes TWO additive,
  nilable struct surfaces explicit (and only those two): a
  `LangMeta map[string]any` field on each envelope (Section
  4.4) and a `ReceiverAliases []string` field on `MethodDecl`
  (Section 4.5.1). No other top-level fields are added.
  Pre-existing parsers (TS / JS / Python) leave both new
  fields nil; their dispatcher output is byte-identical.
  (`parser.go` lines 60-84 today; the diff is enumerated in
  Section 2.2.)
- **A2 -- Idempotent canonical signatures.** A class is keyed
  by `<repoURL>::class::<relPath>#<QualifiedName>`; a method by
  `<repoURL>::method::<relPath>#<QualifiedName>(<normalised
  params>)`. Whitespace normalisation (`NormalizeSignature`)
  collapses formatter-only diffs to the same fingerprint.
- **A3 -- Two-pass insert protocol.** Pass 1 inserts every
  Class / Method / Block Node so the local-symbol table is
  fully populated; pass 2 resolves and inserts same-file
  `extends` / `implements` / `static_calls` / `reads` /
  `writes` edges. This story adds ONE new pass (2d) that
  emits same-file `overrides` edges from Rust impl-block
  methods to the trait default they shadow (Section 7.2 and
  Section 9 R4). The new pass runs after Pass 2c and is a
  no-op for every other language. New parsers MUST produce
  results in the shape pass 1 + pass 2 expect; they do NOT
  call the writer directly.
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
| **PowerShell parser** | `parser_powershell.go` (no CGO; subprocess invocation of `pwsh`) | none (subprocess approach is build-tag agnostic) | v1 follows the in-house example at `E:\work\github\crp\workflow\src\ast\Ast.PowerShell` (Section 6). Invokes `pwsh -NoProfile` to obtain the official `System.Management.Automation.Language` AST and maps `FunctionDefinitionAst` / `TypeDefinitionAst` / `ParamBlockAst` / `ScriptBlockAst` onto our envelope. Files skipped with a WARN log when `pwsh` is not on PATH. |
| **Extension registry update** | `parsers_cgo.go` (modify) and `parsers_nocgo.go` (modify) | `cgo` / `!cgo` | Add the new parsers to `defaultParsers()`. Per Section 7, the no-CGO build skips C / C++ / C# / Go / Rust files and registers only the existing TS / Python scanner pair plus the new `pwsh`-subprocess PowerShell parser. |
| **Per-language hint aliases** | `dispatcher.go` (modify `normalizeHints`) | none | Add language aliases `c`, `cxx`, `cs`, `csharp`, `golang`, `rs`, `ps`, `ps1`, etc. so `repo.language_hints[]` can resolve files whose extension is NOT registered in `extMap` (e.g. an extensionless shell-prefixed script, a fragment passed through `LanguageHints` by a wrapping tool). Per Section 5.2 the extension-first match in `selectParser` always wins when present, so `.h` remains C-routed even when a `cpp` hint is supplied. |

### 2.2 What the dispatcher does NOT change

The following are explicitly NOT modified by this story:

- `dispatcher.go::emit` -- the two-pass insert protocol is
  language-agnostic; new parsers slot in via `defaultParsers()`.
  An additional Pass 2d for `overrides` edges is layered on
  (Section 7.2 and Section 9 R4); the existing Pass 0 / 1 /
  2a / 2b / 2c are untouched.
- `block.go::SubdivideMethod` -- the `CountLogicalLines`
  threshold check works on any body source the parser hands
  it; per-language tokenisation rules are out of scope.
- `repoindexer/ast.go` -- the `ASTEmitter` / `EmitFileEvent` /
  `EmitResult` / `TouchedNode` shapes are stable; no new field
  is required.

### 2.2.1 What this story DOES change in `parser.go` and writers

Two additive struct fields and one writer hook -- enumerated
here so the next iteration of the tech-spec / implementation
plan can reference an exact diff surface:

| File | Change | Justification |
| --- | --- | --- |
| `parser.go` | Add `LangMeta map[string]any` to `ClassDecl`, `MethodDecl`, `Import`. | Section 4.4 -- per-language attrs flow into the writer through a single merge step. |
| `parser.go` | Add `ReceiverAliases []string` to `MethodDecl`. | Section 4.5.1 -- Go pointer-receiver methods register a secondary key (`Foo.Bar`) for receiver-qualified call resolution while the primary key keeps the `*` prefix. |
| `dispatcher.go` -- `classAttrs`, `methodAttrs`, `importEdgeAttrs` | Add a final `mergeLangMeta(out, m.LangMeta)` call before `mustJSON`. | Section 4.4.2 -- merges per-language keys into the existing attrs map. |
| `dispatcher.go` -- receiver-qualified resolver | Consult `MethodDecl.ReceiverAliases` after the primary `methodNodeID[m.EnclosingClass+"."+callee]` lookup. | Section 4.5.1 -- restores `self.X` / `r.X` resolution for Go pointer-receiver targets without affecting other languages. |

All four edits are additive; existing TS / JS / Python
parsers populate neither field and their writer output is
byte-identical (Section 4.4.4). The dispatcher tests in
`dispatcher_test.go` continue to pass unchanged.

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
         powershellParser (pwsh subprocess, Section 6)   # new
       ]

   CGO=0 (portable `make test` on stock Windows toolchain):
       defaultParsers() returns [
         tsjsParser, pythonParser,                       # existing
         powershellParser (pwsh subprocess, Section 6)   # new -- same impl
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
3. PowerShell is the one exception -- it ships the `pwsh`
   subprocess parser (Section 6) as the SINGLE implementation
   across both CGO tags, because no smacker tree-
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
| **Go** | The receiver type name (stripped of pointer marker `*`) when the method has a receiver clause `func (r *Foo) Bar(...)`. Free functions have empty receiver. | text between the outer `(` `)`. The pointer-receiver disambiguation lives in `QualifiedName` (`*Foo.Bar`), NOT in `ParamSignature` (Section 4.5). | The receiver identifier's qualified calls: when the receiver is `r`, every `r.X(` call is a ReceiverCall. The parser tracks the receiver-binding name per method to recognise the right prefix. Receiver-qualified resolution for pointer-receiver methods uses `ReceiverAliases` (Section 4.5.1). | empty in v1 (Go has no method modifiers; `LangMeta["receiver"]` and `LangMeta["receiver_ptr"]` capture the bound name and pointer flag). |
| **Rust** | The `impl` block's target type name. Trait-impl methods (`impl Trait for Type`) attach to the Type class -- the trait name lives in `attrs_json["trait"]`. Free `fn name(...)` outside any `impl` -> empty. | text between the outer `(` `)`. | `self.X(` -- captured separately. | `pub`, `pub(crate)`, `pub(super)`, `async`, `unsafe`, `const`, `extern`. |
| **PowerShell** | Enclosing class name when the method is declared inside `class Foo {}` (i.e. `FunctionMemberAst` under `TypeDefinitionAst`). Top-level `function Bar {...}` -> empty. | joined parameter list from the official PowerShell AST `FunctionDefinitionAst.Body.ParamBlock` -- see the `PowerShellAstParser` reference in Section 6.1. | `$this.X(` inside a class method. | `static`, `hidden` (class methods); none for free functions. |

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
params>)`) does NOT separate them by default, because:

- `QualifiedName` is `Foo.Bar` for both unless the parser
  marks one.
- `ParamSignature` excludes the receiver clause (it covers
  only the formal parameter list after the function name).

PINNED RULE (operator answer `go-receiver-pointer-fingerprint`,
applied here): the Go parser embeds the receiver-pointer marker
DIRECTLY in `QualifiedName`. Value-receiver methods get the
bare type name; pointer-receiver methods get a leading `*` on
the type name. `EnclosingClass` stays the bare type name (no
`*`) so the dispatcher's class-attachment lookup
(`classNodeID[m.EnclosingClass]`, `dispatcher.go` lines
392-396) finds the same `Foo` class node for both. The
boolean `LangMeta["receiver_ptr"]` is also set so consumers
can filter without re-parsing the canonical signature.

| Source declaration | `EnclosingClass` | `QualifiedName` | `ParamSignature` | Resulting canonical signature |
| --- | --- | --- | --- | --- |
| `func (r Foo) Bar(s string)` | `Foo` | `Foo.Bar` | `s string` | `<url>::method::<rel>#Foo.Bar(s string)` |
| `func (r *Foo) Bar(s string)` | `Foo` | `*Foo.Bar` | `s string` | `<url>::method::<rel>#*Foo.Bar(s string)` |

#### 4.5.1 Dispatcher integration consequences

Two dispatcher behaviours interact with the `*` prefix; this
story documents both and resolves them in the Go parser:

1. **Bare-name calls (A5 path).** `dispatcher.go::buildCalleeIndex`
   extracts the simple name with `q[LastIndexByte(q, '.')+1:]`.
   For `"*Foo.Bar"` this still yields `"Bar"`, so bare-name
   resolution of `Bar()` collides exactly as today and the
   existing drop-on-collision rule applies. No dispatcher
   change required.

2. **Receiver-qualified calls (A5 path).** The dispatcher
   builds the receiver-qualified key as
   `m.EnclosingClass + "." + callee` (`dispatcher.go` line
   601). Because we set `EnclosingClass="Foo"` for both
   variants, the key is `"Foo.Bar"` -- which matches the
   value-receiver method's `QualifiedName` but NOT the
   pointer-receiver one (`"*Foo.Bar"`). To keep `self.X` /
   `r.X` resolution working for pointer receivers, the Go
   parser ALSO registers each pointer-receiver method's
   QualifiedName WITHOUT the `*` prefix as a secondary
   resolution alias on `MethodDecl.ReceiverAliases`
   (`MethodDecl` first-class field; see Section 4.4
   addendum). The dispatcher's receiver-qualified pass
   consults `ReceiverAliases` after `methodNodeID` lookup;
   when both value and pointer receiver methods are present
   the alias collides and the drop-on-collision rule (A5)
   applies, matching today's behaviour for ambiguous
   call sites.

The `ReceiverAliases` field is the second and final additive
struct surface this story introduces (alongside `LangMeta`,
Section 4.4). Section 2.2 enumerates the exact additions.

The `*` prefix on the type name is the Go-specific marker.
Whitespace normalisation (`NormalizeSignature`) operates on
the `ParamSignature` payload, which is unchanged from existing
Go conventions; the `*` sits inside `QualifiedName` and is not
touched by parameter normalisation.

#### 4.5.2 Consequences

- Two distinct canonical signatures, hence two distinct
  fingerprints. A2 is satisfied -- the two methods produce
  two distinct Node rows.
- `methodNodeID[m.QualifiedName]` keys the value-receiver
  method under `Foo.Bar` and the pointer-receiver method
  under `*Foo.Bar`. There is NO collision and both methods
  remain individually addressable.
- The dispatcher's class-attachment lookup
  `classNodeID[m.EnclosingClass]` resolves `"Foo"` for both
  methods -> both `contains` edges attach to the same
  `Greeter` class node.
- Cross-file pointer/value collision is impossible (canonical
  signatures embed `relPath`).
- A pure bare-name call `Bar()` inside the same file with
  both receivers present resolves ambiguously and is dropped
  per A5 (`buildCalleeIndex` simple-name extraction yields
  `"Bar"` from both entries; the resolver drops on
  `len(ids) > 1`).
- Receiver-qualified calls (`r.Bar()` / `self.Bar()`) follow
  the `ReceiverAliases` mechanism described in 4.5.1.
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
   PowerShell `pwsh`-subprocess parser (Section 6 -- same
   implementation as the CGO=on build).
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

## 6. PowerShell Strategy (operator-pinned)

PowerShell is the one language in this story where the existing
smacker tree-sitter binding set provides NO grammar. Confirmed
by inspecting the vendored module
(`github.com/smacker/go-tree-sitter@v0.0.0-20240827094217...`):
the available bindings are bash, c, cpp, csharp, css, cue,
dockerfile, elixir, elm, golang, groovy, hcl, html, java,
javascript, kotlin, lua, markdown, ocaml, php, protobuf,
python, ruby, rust, scala, sql, svelte, swift, toml, typescript,
yaml. No PowerShell.

The operator answer to `powershell-grammar-strategy` (workstream
memory) is to follow the in-house C# reference example at
`E:\work\github\crp\workflow\src\ast\Ast.PowerShell` (verified
to exist on the dev machine; canonical files
`PowerShellAstParser.cs`, `PowerShellHyperedgeExtractor.cs`,
`PsAstNode.cs`). That example does NOT use a community
tree-sitter grammar; it uses the OFFICIAL PowerShell SDK
(`System.Management.Automation.Language.Parser`). v1 of THIS
story reproduces the same extraction shape in Go by invoking
the official `pwsh` runtime as a subprocess.

### 6.1 Reference extraction shape (from `Ast.PowerShell`)

`PowerShellAstParser.ExtractNodes` walks the
`System.Management.Automation.Language.Ast` tree and matches
exactly four node kinds:

| AST kind (.NET SDK) | Reference output (`PsAstNode`) | Mapped to our envelope (Section 4) |
| --- | --- | --- |
| `FunctionDefinitionAst` | `PsAstNode{NodeType=Function|Workflow, Name=func.Name, Parameters=[...]}` | `MethodDecl{QualifiedName=func.Name, EnclosingClass="", ParamSignature=joined param list}` (free function); when inside a class body the parent is the enclosing `TypeDefinitionAst` -> `MethodDecl{QualifiedName="Class.method", EnclosingClass="Class"}` |
| `TypeDefinitionAst` (class) | `PsAstNode{NodeType=Class, Name=..., BaseTypes=[...], Methods=[...], Properties=[...]}` | `ClassDecl{QualifiedName=type.Name, Kind="class" or "enum", Extends=BaseTypes (first), Implements=BaseTypes (interfaces), LangMeta={"properties":[...]}}` |
| `TypeDefinitionAst` (enum) | same with `IsEnum=true` | `ClassDecl{QualifiedName=type.Name, Kind="enum"}` |
| `ParamBlockAst` | `PsAstNode{NodeType=ParamBlock, Parameters=[...]}` | folded into the enclosing `MethodDecl.ParamSignature` (NOT a separate node -- our envelope has no peer for top-level param blocks) |
| `ScriptBlockAst` (top-level) | `PsAstNode{NodeType=ScriptBlock, Name=Path.GetFileName(...)}` | folded into the FILE-level container; the script body is treated as anonymous and only its TOP-LEVEL `Import-Module` / `using module` / dot-source statements are emitted as `Import` rows |

The reference also extracts a "hyperedge" set
(`PowerShellHyperedgeExtractor.cs`) for module/dot-source
relationships. v1 maps those onto the existing `Import`
envelope:

| Reference hyperedge | v1 `Import.Module` | `LangMeta` |
| --- | --- | --- |
| `Import-Module Foo` | `Foo` | `{"module_kind":"Import-Module"}` |
| `Import-Module ./Local.psm1` | `./Local.psm1` (relative -> dropped by `isRelativeImport`, persists raw) | `{"module_kind":"Import-Module"}` |
| `using module Foo` | `Foo` | `{"module_kind":"using_module"}` |
| `. ./helpers.ps1` (dot-source) | `./helpers.ps1` (relative -> dropped) | `{"module_kind":"dot_source"}` |

### 6.2 v1 implementation: subprocess to `pwsh`

The Go parser file is `parser_powershell.go` (NO CGO build
tag -- the implementation is pure subprocess invocation):

```go
// parser_powershell.go
//go:build !no_pwsh
// (build tag "no_pwsh" lets CI opt out on stripped images)

type powershellParser struct {
    pwshBin string  // resolved via exec.LookPath("pwsh")
}

func (p *powershellParser) Parse(relPath string, src []byte) (ParseResult, error) {
    // 1. Pipe `src` to `pwsh -NoProfile -NonInteractive -Command -`
    //    running an embedded extraction script that mirrors
    //    Ast.PowerShell's ExtractNodes.
    // 2. The script returns a JSON document with the shape
    //    {functions:[...], types:[...], imports:[...]}
    //    -- the EXACT same set the reference example yields.
    // 3. Map the JSON onto ClassDecl / MethodDecl / Import.
    // 4. When pwsh is not on PATH (CI without PowerShell),
    //    return ParseResult{} with no nodes and an empty
    //    error -- the dispatcher treats this as "parser
    //    skipped this file" without aborting the worker.
}
```

Extraction is one process per file. For a worker walking
many `.ps1` files, the implementation MAY (a follow-up
optimisation, not v1 scope) batch them via a single
long-lived `pwsh` host; v1 ships the simpler one-shot
invocation.

### 6.3 Build matrix

| Build tag | Production wiring | Behaviour for `.ps1` / `.psm1` / `.psd1` |
| --- | --- | --- |
| `//go:build cgo`   (`parsers_cgo.go`)   | `defaultParsers()` registers `powershellParser` | Parse via `pwsh` subprocess. If `pwsh` is absent on the host, file is skipped with `ast.dispatch.skip{reason:"pwsh_not_available"}` -- WARN-level log, the worker continues. |
| `//go:build !cgo`  (`parsers_nocgo.go`) | same registration -- the parser does NOT depend on CGO | identical behaviour to the CGO build (the subprocess approach is build-tag agnostic) |

This breaks the "tree-sitter requires CGO" pattern the other
languages follow -- intentionally. The reference example
sidesteps tree-sitter entirely, so the build-tag
asymmetry described in Section 7 (other-language matrix)
does NOT apply to PowerShell. The CGO=off `make test`
portable path runs the same parser if `pwsh` is installed;
otherwise files are skipped.

### 6.4 Fallback rules

- **No `pwsh` on host.** Files skipped, dispatcher emits one
  WARN log per skipped file (rate-limited via the existing
  `ast.dispatch.skip` mechanism). The worker does not abort.
  Tests under `parser_powershell_test.go` use `t.Skip` when
  `exec.LookPath("pwsh")` fails so the suite stays green on
  PowerShell-less CI.
- **`pwsh` returns a parse error.** The Go parser surfaces
  the error from the subprocess; `safeParse` (A6) logs it
  and the file is treated as a parse failure. Same behaviour
  as the existing tree-sitter parsers when the grammar
  rejects a malformed input.
- **`pwsh` subprocess hang.** A 10-second per-file timeout
  is applied via `context.WithTimeout`; on timeout the
  parser returns an error and `safeParse` does its job.
  Timeout value is the same constant used by the existing
  parsers' bounded work loops.

### 6.5 Tree-sitter PowerShell binding (explicitly OUT of v1)

Adding a community `tree-sitter-powershell` grammar binding
is NOT in scope for this story. The operator-pinned path is
the subprocess approach because it matches the in-house
reference example. Promotion to a tree-sitter binding is a
follow-up workstream only if (a) the subprocess overhead
becomes a measurable bottleneck or (b) the host-PowerShell
dependency becomes a deployment problem.

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
         QualifiedName="*Greeter.Greet",       # per Section 4.5
         EnclosingClass="Greeter",
         ParamSignature="name string",
         ReceiverAliases=["Greeter.Greet"],     # per Section 4.5.1
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
       sig = repoURL+"::method::internal/foo/foo.go#*Greeter.Greet
             (name string)"   # per Section 4.5
       AttrsJSON {language:"go", enclosing_class:"Greeter",
                  receiver:"g", receiver_ptr:true,
                  params_raw:"name string", ...}
       (receiver / receiver_ptr came from LangMeta via
        mergeLangMeta in methodAttrs; params_raw is the
        existing first-class key carrying ParamSignature.
        ReceiverAliases=["Greeter.Greet"] registers a
        secondary key for receiver-qualified resolution
        per Section 4.5.1.)
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
                                          *Greeter.Greet method,
                                          formatGreeting method,
                                          fmt package]}
```

### 7.2 Scenario: Rust trait impl with same-file static call, `use` import, and trait-default overrides edge

Setup: a Rust file `src/lib.rs` declaring `trait Greeter { fn
greet(&self, name: &str) -> String { String::new() } }` (a
default-bodied method on the trait), `struct GreeterImpl;`,
`impl Greeter for GreeterImpl { fn greet(&self,...) {...} }`
(impl-block method shadowing the trait default), free function
`fn format_greeting(...)`, and `use std::fmt::Display;`.

```
selectParser("src/lib.rs", [])
   extMap[".rs"]  ->  rustTreeSitterParser
       v
rustTreeSitterParser.Parse:
   - "trait_item"  ::  ClassDecl{
       QualifiedName="Greeter", Kind="trait", Extends=[],
       Implements=[]}
     plus, for the default-bodied method inside the trait:
       MethodDecl{
         QualifiedName="Greeter.greet",
         EnclosingClass="Greeter",
         ParamSignature="&self, name: &str",
         BodySource="String::new()",
         LangMeta={"trait_default":true} }
   - "struct_item" GreeterImpl  ::  ClassDecl{
       QualifiedName="GreeterImpl", Kind="struct"}
   - "impl_item" with trait `Greeter` for `GreeterImpl`  ::
       (i)  appends "Greeter" to GreeterImpl.Implements
       (ii) for each `function_item` in the impl body:
            emit MethodDecl{
              QualifiedName="GreeterImpl.greet",
              EnclosingClass="GreeterImpl",
              LangMeta={"trait":"Greeter"} }
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
                   Greeter.greet method (parent=Greeter, the
                     trait-default version),
                   GreeterImpl.greet method (parent=GreeterImpl,
                     the impl-block override),
                   format_greeting method (parent=file)
Dispatcher pass 2a:
   GreeterImpl.Extends=[]      -> no edges
   GreeterImpl.Implements=["Greeter"]
     -> classNodeID["Greeter"] resolves -> insert implements
        GreeterImpl -> Greeter
Dispatcher pass 2b:
   GreeterImpl.greet.Calls=["format_greeting"]
     -> resolves -> static_calls edge
Dispatcher pass 2d: overrides   # NEW pass for this story
   For each method M where M.LangMeta["trait"] is set:
     traitID = classNodeID[M.LangMeta["trait"]]
     traitMethodID = methodNodeID[traitName + "." + simpleName(M)]
     when BOTH resolve same-file:
       insert overrides edge M -> traitMethodID
   Here:
     M = GreeterImpl.greet, trait = "Greeter",
     simpleName = "greet" -> methodNodeID["Greeter.greet"]
     resolves to the trait-default node
     -> insert overrides GreeterImpl.greet -> Greeter.greet
   (See Section 9 R4 for the operator-pinned rule.)
Dispatcher pass 0:
   std::fmt is non-relative -> external package node
   imports edge file -> std::fmt with attrs.symbols=["Display"]
```

The Pass 2d step is the only addition this story makes to
the dispatcher's emit loop; it runs after Pass 2c (reads /
writes) and before `EmitResult` assembly. All other languages
in this story leave `LangMeta["trait"]` unset so the pass is
a no-op for them; existing TS / JS / Python parsers likewise
leave the key unset and remain byte-identical in output.

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
   empty body wins. This is the PINNED v1 rule (Section 9
   R1) -- no operator question is pending.
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
`pwsh` subprocess parser is the SINGLE implementation across
both build tags, so the constraint is trivially satisfied.

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
  import (C `#include "..."`, PowerShell dot-source and
  `./Local.psm1` references, Rust local `mod` declarations --
  which are NOT emitted as Imports at all in v1).

### 8.4 Test surface

Per the story description's tests section:

- Each language gets a fixture-driven test in
  `parser_treesitter_<lang>_test.go` (CGO=on) plus
  `parser_powershell_test.go` for the `pwsh` subprocess
  parser (skipped via `t.Skip` when `exec.LookPath("pwsh")`
  fails -- see Section 6.4).
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
| PowerShell | `pwsh` subprocess (Section 6) | `pwsh` subprocess (same impl) | requires `pwsh` on PATH; absent -> files skipped with `ast.dispatch.skip{reason:"pwsh_not_available"}` |

The "tree-sitter-backed only" caveat and the PowerShell
`pwsh`-subprocess requirement are quoted in the table.

## 9. Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| **R1** -- C++ declaration vs definition double-count. A class member function declared in a header (no body) and defined in a `.cpp` file (with body) would create two `MethodDecl` entries with the same QualifiedName. | The C++ parser deduplicates by QualifiedName within a single file at the end of `Parse`. Cross-file duplicates (header + cpp in separate files) are a future cross-file resolver problem and stay as two `method` nodes whose canonical signatures differ in `relPath` -- correct per A2. |
| **R2** -- Go method receiver pointer / value variants. `func (r *Foo) Bar()` and `func (r Foo) Bar()` are distinct methods at the language level. The receiver clause is OUTSIDE the formal `ParamSignature`, so without intervention the two methods collide on `<rel>::method#Foo.Bar()`. | **PINNED RULE per Section 4.5** (operator answer `go-receiver-pointer-fingerprint`): the Go parser embeds the receiver-pointer marker in `QualifiedName` -- value-receiver methods key as `Foo.Bar`, pointer-receiver methods key as `*Foo.Bar`. `EnclosingClass` stays `Foo` (bare) so class-attachment via `classNodeID[m.EnclosingClass]` works for both. `LangMeta["receiver_ptr"]` carries the boolean. Receiver-qualified calls (`r.Bar()`) use the `ReceiverAliases` mechanism (Section 4.5.1) -- the pointer-receiver method also registers the bare `Foo.Bar` key as a secondary lookup so `self.X` / `r.X` calls keep resolving. If both value and pointer receiver methods exist on the same type and name, the alias collides -> A5's drop-on-collision rule applies and `calls_raw` preserves intent. |
| **R3** -- C# `partial class` declarations across multiple files. v1 treats each partial file as its own ClassDecl with the same QualifiedName but different relPath -> different canonical signatures, hence distinct nodes. The cross-file resolver will stitch them via a future `partial_of` edge. | Documented as a future story; no v1 work required. The `LangMeta["partial"]=true` flag is set on each partial class so the consumer can group by name. |
| **R4** -- Rust trait method default impl. A trait declaration with a default-bodied method produces both a MethodDecl on the trait class (Kind="trait") AND, separately, an `impl Trait for Type` method shadows it on the Type class. | **PINNED RULE** (operator answer `rust-trait-overrides-edge`): when both the trait default-bodied method AND the impl-block method are present in the SAME FILE, the dispatcher's new Pass 2d emits an `overrides` edge from the impl method to the trait method. Resolution key is `methodNodeID[traitName + "." + simpleName(implMethod)]` where `traitName = LangMeta["trait"]`. Cross-file trait/impl pairs are deferred (per A4 same-file resolution) and the verbatim trait identity persists on `LangMeta["trait"]`. The implementation requires the dispatcher to accept the new edge kind `"overrides"`; Section 2.2.1 captures the dispatcher diff. The same-file ambiguity case (multiple impl methods with the same simple name) is handled by `buildCalleeIndex`'s drop-on-collision rule (A5) -- only same-name same-trait shadowing emits the edge. |
| **R5** -- PowerShell `pwsh` subprocess overhead / availability. | One process per file is acceptable for v1's per-file emission cadence; tests skip when `pwsh` is absent (Section 6.4). A future workstream may batch via a long-lived host. |
| **R6** -- `.h` ambiguity. C / C++ both use `.h`; the v1 routing sends `.h` to the C parser. | **PINNED PER SECTION 5.2**: `.h` routes to C unconditionally in v1; no hint-override mechanism is shipped. Repos with C++-only headers must use `.hpp` / `.hh` / `.hxx` / `.h++`. A follow-up story can add a per-repo extension-override knob that fires ahead of `extMap` in `selectParser`. |

## 10. Pinned Decisions and Out-of-Story Workstreams

This story makes the following v1 decisions in-doc. All four
prior operator questions are answered (see workstream memory);
nothing is pending.

- **`.h` routing** -- PINNED (operator answer
  `dot-h-extension-routing`): `.h` files route to the C parser
  unconditionally; no hint-override mechanism is shipped. See
  Section 5.2 for the rationale and Section 9 R6 for the risk
  acknowledgement. A follow-up story may add a per-repo
  extension-override knob.
- **PowerShell parser strategy** -- PINNED (operator answer
  `powershell-grammar-strategy`): follow the in-house example
  under `E:\work\github\crp\workflow\src\ast\Ast.PowerShell`,
  which uses the official
  `System.Management.Automation.Language.Parser` API. v1
  invokes `pwsh -NoProfile` as a subprocess to obtain the same
  AST and maps `FunctionDefinitionAst` / `TypeDefinitionAst` /
  `ParamBlockAst` / `ScriptBlockAst` onto our envelope. See
  Section 6 for the strategy detail and Section 6.4 for the
  fallback rules.
- **Go pointer-receiver fingerprint disambiguation** -- PINNED
  (operator answer `go-receiver-pointer-fingerprint`): the Go
  parser embeds `*` in `QualifiedName` for pointer receivers
  (`*Foo.Bar`) while keeping value receivers bare (`Foo.Bar`).
  `EnclosingClass` stays the bare type name so class-attachment
  works uniformly. Receiver-qualified calls resolve through
  the new `ReceiverAliases` field (Section 4.5.1). See Section
  4.5 and Section 9 R2.
- **Rust trait `overrides` edge** -- PINNED (operator answer
  `rust-trait-overrides-edge`): when an impl-block method
  shadows a trait default-bodied method AND both are present
  in the same file, the dispatcher's new Pass 2d emits an
  `overrides` edge from the impl method to the trait method.
  See Section 7.2 sequence, Section 9 R4, and the A3 update
  describing the new pass.

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
  from the `pwsh`-subprocess approach to an in-process
  tree-sitter PowerShell binding is a separate workstream
  (Section 6.5).

## Iteration Summary

- Path: `docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md`
- Covers from the story description: (1) v1 extraction scope
  per language (Section 4 matrices), (2) parser library
  coverage for C / C++ / C# / Go / Rust and the
  operator-pinned PowerShell `pwsh`-subprocess approach
  modelled on `Ast.PowerShell` (Section 6), (3) new parser
  file plan (Section 2.1 table), (4) extension registration
  in `parsers_cgo.go` / `parsers_nocgo.go` (Section 3), (5)
  mapping of language constructs into existing graph model
  including the explicit struct + writer extensions for
  `LangMeta` and `ReceiverAliases` (Sections 4.1 - 4.5),
  (6) fixture-driven tests (Section 8.4), (7) cross-language
  dispatcher tests (Section 8.4), (8) support matrix
  documentation hand-off to `.claude/context/tests.md`
  (Section 8.5), (9) end-to-end sequences for the primary
  language scenarios (Section 7.1 Go with pointer-receiver
  emission, 7.2 Rust with the new `overrides` edge in Pass
  2d, 7.3 C++ with header dedup) and the failure case
  (Section 7.4).
- Not covered (deliberately, owned by sibling docs): per-step
  file creation order (implementation-plan.md), fixture text
  bodies and exact `pwsh` extraction-script source for the
  PowerShell parser (tech-spec.md), and operator-visible
  scenario walk-throughs framed as Given/When/Then
  (e2e-scenarios.md).

### Prior feedback resolution

- [x] 1. FIXED -- Sections 1.2 A1, 2.2 (split into 2.2 +
      2.2.1) -- A1 now explicitly allows the two additive,
      nilable struct surfaces (`LangMeta` and
      `ReceiverAliases`) and references Section 2.2.1.
      Section 2.2 was split: 2.2 enumerates what does NOT
      change (dispatcher emit, block.go, ast.go);
      2.2.1 is a new table that explicitly lists every
      additive field and writer hook this story DOES
      introduce (LangMeta on three envelopes, ReceiverAliases
      on MethodDecl, mergeLangMeta in three writers, receiver
      alias consult in the receiver-qualified resolver).
      Section 4.4 LangMeta is now consistent with both.
      Verification:
      ```
      $ grep -nF "never in new top-level fields" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "parser.go -- the `ParseResult` shape and `ClassDecl` /" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
- [x] 2. FIXED -- Section 2.1 (parsers table) and Section
      5.3 -- the `normalizeHints` row now reads "resolve
      files whose extension is NOT registered in `extMap`"
      and explicitly states extension-first match wins
      (referencing Section 5.2). Section 5.3 step (3) was
      already updated last iter; verified consistent.
      Verification:
      ```
      $ grep -nF "so `repo.language_hints[]` can override extension routing." docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
- [x] 3. FIXED -- Section 7.3 C++ sequence -- replaced the
      stale "See the open-question block on declaration/
      definition handling" reference with a forward link to
      Section 9 R1 ("This is the PINNED v1 rule (Section 9
      R1) -- no operator question is pending"). No other
      open-question references remain.
      Verification:
      ```
      $ grep -nF "See the open-question block on declaration/definition handling" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```
- [x] 4. FIXED -- Section 9 R4, Section 10 (Rust trait
      `overrides` edge entry), A3 in Section 1.2, and a new
      Pass 2d in the Section 7.2 Rust sequence -- v1 now
      DOES emit an `overrides` edge from impl method to
      trait method when both same-file resolve, per operator
      answer `rust-trait-overrides-edge`. The dispatcher
      gains a new Pass 2d (documented in A3 and Section
      2.2.1). The Rust sequence trace shows the trait
      default-bodied method emission, the impl method
      emission with `LangMeta["trait"]="Greeter"`, and the
      Pass 2d resolution rule that produces the
      `overrides` edge.
      Verification:
      ```
      $ grep -nF "v1 does NOT emit an `overrides` edge" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "v1 does NOT\n      emit an `overrides` edge" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty -- multi-line variant)
      ```
- [x] 5. FIXED -- Section 6 fully rewritten to incorporate
      the operator-specified reference at
      `E:\work\github\crp\workflow\src\ast\Ast.PowerShell`.
      The file was inspected on disk:
      `PowerShellAstParser.cs` (6837 bytes) uses
      `System.Management.Automation.Language.Parser`
      directly and matches four AST kinds
      (`FunctionDefinitionAst`, `TypeDefinitionAst`,
      `ParamBlockAst`, `ScriptBlockAst`). v1 reproduces the
      same extraction in Go by invoking `pwsh -NoProfile`
      as a subprocess; the JSON output maps 1:1 onto
      `ClassDecl` / `MethodDecl` / `Import`. Section 6.1
      gives the mapping table sourced directly from the
      reference, 6.2 sketches the Go wrapper, 6.3 the
      build matrix (no CGO dependence), 6.4 the fallback
      rules, 6.5 explicitly defers any tree-sitter
      promotion. Section 10 pin is updated. The "Option A
      scanner-only" text is gone.
      Verification:
      ```
      $ grep -nF "Ship Option A (scanner-only)" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "Option A -- Scanner-only PowerShell (RECOMMENDED for v1)" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      $ grep -nF "PowerShell scanner-only" docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/architecture.md
      (empty)
      ```

All four operator-pinned answers are now applied in-doc; no
open-questions block is emitted. ASCII-clean check passed
(no characters outside `\x00-\x7E`).
