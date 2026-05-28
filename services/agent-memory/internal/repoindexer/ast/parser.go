package ast

// LanguageParser is the per-language hook the Stage 3.2
// dispatcher delegates each file to. Stage 3.2 ships
// implementations for the v1 language set -- TypeScript /
// JavaScript (`tsjsParser`) and Python (`pythonParser`).
// Additional grammars (Go, Java, Rust, etc.) are added by
// dropping in a new implementation and registering its file
// extensions through `Dispatcher.Register`; no other code
// changes.
//
// The interface is intentionally narrow: it returns a
// language-agnostic `ParseResult` value rather than a stream of
// SAX-style callbacks. This keeps the dispatcher's two-pass
// insert protocol (first all Nodes, then all Edges) trivial to
// implement -- the parser does syntactic work only; the
// dispatcher does the writer interaction.
//
// Per-call semantics:
//   - Parsing errors that affect only part of the file MUST be
//     swallowed by the parser and reflected in the returned
//     `ParseResult` with the affected declarations omitted. The
//     dispatcher logs and continues; one malformed file does
//     NOT abort the ingest (see
//     `repoindexer.NoopASTEmitter.EmitFile` doc for the
//     contract).
//   - Parser crashes (panics) propagate to the dispatcher,
//     which recovers them, logs `ast.parse.panic`, and returns
//     nil so the surrounding worker keeps processing.
type LanguageParser interface {
	// Language returns the stable language id this parser
	// emits (`typescript`, `python`, ...). Used in the
	// dispatcher's structured logs and persisted on every
	// Class / Method / Block node's `attrs_json["language"]`
	// so downstream tooling can route per-language without
	// re-parsing the canonical signature.
	Language() string

	// Extensions returns the lower-case file extensions
	// (each beginning with `.`) this parser claims. The
	// dispatcher registers them in `extMap` at construction
	// time. Multiple extensions per parser are supported
	// (`.ts`, `.tsx`, `.js`, `.jsx`, ...).
	Extensions() []string

	// Parse interprets src and returns the declared Classes,
	// Methods, and Imports. The returned slices are owned by
	// the caller after Parse returns; the parser must not
	// retain references to them. `relPath` is the
	// workspace-relative path of the file -- the parser is
	// free to embed it in attrs or error messages but MUST
	// NOT use it to read the file (the caller has already
	// done so and handed `src` over).
	Parse(relPath string, src []byte) (ParseResult, error)
}

// ParseResult is the language-agnostic output of a
// `LanguageParser.Parse` call. The dispatcher consumes it to
// drive its two-pass insert protocol.
type ParseResult struct {
	// Classes is every class / interface / record-like
	// declaration found at file scope or inside another
	// declaration. Order is source order (top to bottom);
	// the dispatcher uses the order verbatim when minting
	// `<repoURL>::class::<relPath>#<qualifiedName>`
	// signatures.
	Classes []ClassDecl
	// Methods is every method declared inside a Class plus
	// every free-function declaration. A method's
	// `EnclosingClass` field is the empty string for free
	// functions and the parent class's QualifiedName
	// otherwise.
	Methods []MethodDecl
	// Imports is the structured form of every import-like
	// statement in the file (TS/JS `import` and `require`,
	// Python `import` / `from ... import`). The dispatcher
	// materialises each non-relative import as a synthetic
	// external-package Node + a file->package `imports`
	// edge; relative imports (`./util`, `../foo`) are
	// dropped pending the cross-file resolver story (a
	// later workstream stitches them to in-repo File
	// nodes).
	Imports []Import
}

// ClassDecl describes one class-like declaration the parser
// extracted. Field meanings are language-specific:
//
//   - TypeScript / JavaScript: classes (`class Foo {...}`) and
//     interfaces (`interface Foo {...}`); `Kind` carries
//     `"class"` or `"interface"`.
//   - Python: classes (`class Foo:`); `Kind` is always
//     `"class"`.
type ClassDecl struct {
	// QualifiedName is the dotted name of the class within
	// its file (e.g. `Outer.Inner` for a nested class).
	// Stable across re-ingests of the same source file.
	QualifiedName string
	// Kind is the language-specific declaration kind
	// (`"class"` or `"interface"`). Persisted in
	// `attrs_json["decl_kind"]`.
	Kind string
	// Extends lists the parent classes named in the
	// `extends` clause (TS/JS, Python multiple inheritance
	// via the base list). Names are raw identifiers; the
	// dispatcher resolves each one against the same file's
	// declared classes and emits an `extends` edge per
	// resolved entry, dropping unresolved bare names (no
	// edge is materialised for them). The verbatim list is
	// always persisted on the class node's
	// `attrs_json["extends_raw"]` so consumers can still
	// inspect what the parser saw before the resolver
	// filter -- including names that did not produce an
	// edge because the parent class lives in a different
	// file (cross-file resolution lands in a later
	// workstream).
	Extends []string
	// Implements lists the interfaces named in the TS/JS
	// `implements` clause. Empty for Python.
	Implements []string
	// StartLine is the 1-based source line the declaration
	// begins on. Persisted in `attrs_json["start_line"]`.
	StartLine int
	// EndLine is the 1-based source line the declaration
	// ends on (the line containing the closing `}` for
	// TS/JS; the last indented body line for Python).
	EndLine int
	// LangMeta carries per-language attrs the dispatcher
	// folds into `attrs_json` via `mergeLangMeta` (architecture
	// Section 4.4.2). A nil map means "no per-language attrs"
	// -- the merge is a no-op and the existing TS/JS/Python
	// parsers leave it nil, so dispatcher output for those
	// languages is byte-identical across this surface.
	//
	// LangMeta is DESCRIPTIVE, NOT IDENTIFYING (architecture
	// invariant C12): two `ClassDecl`s that differ ONLY in
	// `LangMeta` values MUST collide on the same canonical
	// signature. Parsers MUST NOT route language-specific
	// data into `QualifiedName` (the Go pointer-receiver `*`
	// prefix is the one operator-pinned exception described
	// in architecture Section 4.5).
	//
	// Parsers MUST NOT set keys whose names collide with the
	// dispatcher's first-class attrs keys (e.g. `language`,
	// `decl_kind`, `extends_raw`, `start_line`, `end_line`)
	// -- the merge helper's first-class-key-wins rule
	// silently drops them. Well-known per-language keys for
	// classes (`namespace`, `embeds`, `partial`,
	// `template_params`, `base_access`) are catalogued in
	// architecture Section 4.4.3.
	LangMeta map[string]any
}

// MethodDecl describes one method or free-function
// declaration.
type MethodDecl struct {
	// QualifiedName is the dotted name of the method within
	// its file (e.g. `Foo.bar` for a method `bar` inside
	// class `Foo`, or `bar` for a free function).
	QualifiedName string
	// EnclosingClass is the QualifiedName of the class this
	// method belongs to, or the empty string for free
	// functions.
	EnclosingClass string
	// ParamSignature is the raw parameter list text as
	// extracted from the source (the `(...)` contents). The
	// dispatcher runs it through `NormalizeSignature` before
	// embedding it in the canonical signature; the raw form
	// is also persisted in `attrs_json["params_raw"]` for
	// diagnostics.
	ParamSignature string
	// BodySource is the verbatim source of the method body
	// (TS/JS: the contents between `{` and the matching
	// `}`; Python: the indented block lines). The dispatcher
	// passes it to `SubdivideMethod` for the §8.2 logical-
	// line threshold check.
	BodySource string
	// StartLine is the 1-based source line the method's
	// signature begins on.
	StartLine int
	// EndLine is the 1-based source line the method's body
	// ends on.
	EndLine int
	// BodyStartLine / BodyEndLine are the 1-based file lines
	// the body opens / closes on. For TS/JS this is the line
	// of `{` and `}`; for Python it's the first / last
	// indented body line. The dispatcher passes these to
	// `SubdivideMethod` so Block boundaries are emitted in
	// file-relative coordinates -- a future span-to-block
	// resolver matches observed-stack-frame line numbers
	// against these ranges (per evaluator finding #6).
	BodyStartLine int
	BodyEndLine   int
	// BodyStartByte / BodyEndByte are the 0-based file byte
	// offsets the body spans. Persisted alongside the line
	// numbers so consumers that work in byte offsets (e.g.
	// LSP-based tools) can match without re-counting lines.
	BodyStartByte int
	BodyEndByte   int
	// Calls is the list of bare-name call sites the parser
	// found inside BodySource. The dispatcher resolves each
	// against the file's declared methods and emits a
	// `static_calls` edge per unambiguously resolved entry;
	// ambiguous bare names (multiple matching method decls
	// in the same file) and unresolved names are dropped --
	// no edge is materialised for them, because a memory
	// store prefers missing edges over wrong ones (see
	// `dispatcher.go::resolveBareCalls`). The verbatim list
	// is always persisted on the method node's
	// `attrs_json["calls_raw"]` so consumers can still see
	// what the parser observed before the resolver filter,
	// including names that will be stitched up by the
	// later cross-file resolver workstream. Order is the
	// source order of the first occurrence of each call
	// target; duplicates are removed (all four parsers --
	// the TS/JS and Python scanners as well as the
	// tree-sitter variants -- dedupe in insertion order)
	// so a method that calls `helper()` ten times produces
	// a single `static_calls` edge rather than ten. Call-
	// frequency metrics cannot be derived from this slice;
	// downstream consumers that need them must walk
	// `BodySource` directly.
	Calls []string
	// ReceiverCalls is the list of receiver-qualified call
	// targets the parser extracted: `this.foo()` in TS/JS,
	// `self.foo()` in Python. The dispatcher resolves each
	// against `<EnclosingClass>.<name>` in the same file,
	// producing unambiguous `static_calls` edges (per
	// evaluator finding #5). Receiver-qualified calls cannot
	// be confused with sibling-class methods, so resolution
	// never drops them on ambiguity (unlike bare `Calls`).
	// Like `Calls`, the slice is in source order of first
	// occurrence with duplicates removed; one edge per
	// distinct target.
	ReceiverCalls []string
	// MemberAccesses records every receiver-qualified field
	// access the parser found inside the body. Reads and
	// writes are NOT deduped here -- the dispatcher folds
	// them into a single `reads` / `writes` edge per
	// (method, enclosingClass) pair, but the member names
	// are persisted on the edge's `attrs_json["members"]`
	// so consumers can see WHICH fields the method touched
	// (per rubber-duck #4: lossy edges without member
	// names defeat the purpose of these edges).
	MemberAccesses []MemberAccess
	// Modifiers is the language-specific decoration list
	// (`async`, `static`, `private`, etc.). Persisted in
	// `attrs_json["modifiers"]`.
	Modifiers []string
	// LangMeta carries per-language attrs the dispatcher
	// folds into `attrs_json` via `mergeLangMeta` (architecture
	// Section 4.4.2). A nil map means "no per-language attrs"
	// -- the merge is a no-op and the existing TS/JS/Python
	// parsers leave it nil, so dispatcher output for those
	// languages is byte-identical across this surface.
	//
	// LangMeta is DESCRIPTIVE, NOT IDENTIFYING (architecture
	// invariant C12): two `MethodDecl`s that differ ONLY in
	// `LangMeta` values MUST collide on the same canonical
	// signature. Parsers MUST NOT route language-specific
	// data into `QualifiedName` or `ParamSignature` (the Go
	// pointer-receiver `*` prefix in `QualifiedName` is the
	// one operator-pinned exception described in architecture
	// Section 4.5).
	//
	// Parsers MUST NOT set keys whose names collide with the
	// dispatcher's first-class attrs keys (e.g. `language`,
	// `enclosing_class`, `params_raw`, `calls_raw`,
	// `modifiers`, `start_line`, `end_line`, `start_byte`,
	// `end_byte`) -- the merge helper's first-class-key-wins
	// rule silently drops them. Well-known per-language keys
	// for methods (`receiver`, `receiver_ptr`, `trait`) are
	// catalogued in architecture Section 4.4.3.
	LangMeta map[string]any
}

// MemberAccess is one receiver-qualified field touch the
// parser extracted. `Name` is the bare field name (e.g.
// `prefix` for `this.prefix`). `IsWrite` is true when the
// access appears on the LHS of an assignment, false for a
// pure read. Receiver type (`this` vs `self`) is implicit
// in the enclosing parser's language.
type MemberAccess struct {
	Name    string
	IsWrite bool
}

// Import is the structured form of one import statement.
type Import struct {
	// Module is the imported module specifier (e.g.
	// `"./utils"`, `"os"`, `"@scope/pkg"`).
	Module string
	// Symbols lists the named symbols imported from Module.
	// Empty for whole-module imports (`import os`,
	// `import "./utils"`).
	Symbols []string
	// Alias is the local alias for whole-module imports
	// (`import os as o` -> "o"; `import * as fs from "fs"`
	// -> "fs"). Empty when no alias was specified.
	Alias string
	// Line is the 1-based source line of the import
	// statement.
	Line int
	// IsTypeOnly is true for TS `import type` statements
	// (and the per-symbol `import { type Foo } from ...`
	// equivalent). Type-only imports do not produce a
	// runtime dependency, so consumers may want to weight
	// them differently in relevance scoring; the dispatcher
	// records the flag on the `imports` edge attrs but does
	// NOT skip the edge. Always false for Python.
	IsTypeOnly bool
	// LangMeta carries per-language attrs the dispatcher
	// folds into the `imports` edge `attrs_json` via
	// `mergeLangMeta` (architecture Section 4.4.2). A nil map
	// means "no per-language attrs" -- the merge is a no-op
	// and the existing TS/JS/Python parsers leave it nil, so
	// dispatcher output for those languages is byte-identical
	// across this surface.
	//
	// LangMeta is DESCRIPTIVE, NOT IDENTIFYING (architecture
	// invariant C12): two `Import`s that differ ONLY in
	// `LangMeta` values describe the same dependency edge and
	// are NOT routed into the import's identity (Module +
	// Symbols + Alias). Parsers MUST NOT push language-
	// specific data into those identifying fields.
	//
	// Parsers MUST NOT set keys whose names collide with the
	// dispatcher's first-class import-edge attrs keys (e.g.
	// `module`, `line`, `symbols`, `alias`, `is_type_only`,
	// `language`) -- the merge helper's first-class-key-wins
	// rule silently drops them. Well-known per-language keys
	// for imports (`dot_import`, `blank_import`, `is_static`,
	// `cmdlet_verb`, `module_kind`) are catalogued in
	// architecture Section 4.4.3.
	LangMeta map[string]any
}
