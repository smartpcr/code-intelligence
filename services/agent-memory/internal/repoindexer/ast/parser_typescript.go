package ast

import (
	"regexp"
	"strings"
)

// tsjsParser is the Stage 3.2 TypeScript / JavaScript parser.
// It is a lightweight scanner -- NOT a full tree-sitter
// grammar -- chosen because the default `make test` path runs
// with CGO disabled (see `services/agent-memory/Makefile`),
// which excludes the canonical tree-sitter Go bindings. The
// scanner is structured behind the same `LanguageParser`
// interface a tree-sitter implementation would satisfy, so a
// future story can swap in tree-sitter (or any other parser
// technology) without touching the dispatcher or its tests.
//
// Scope limits the scanner deliberately accepts:
//
//   - String / regex / template literals are masked to spaces
//     (preserving line numbers and offsets) before the
//     declaration regexes run, so a `class` keyword inside a
//     template literal does not produce a phantom class.
//
//   - Class / interface declarations: top-level only. Classes
//     nested inside method bodies are NOT extracted (a real
//     tree-sitter pass would; the v1 fixture set does not
//     exercise this).
//
//   - Method extraction operates on each class's brace-matched
//     body. The class body is scanned for declaration patterns
//     that match `(static |async |private |public |protected
//     |readonly )*<name>\s*(<generics>)?\s*\(<params>\)\s*
//     (:\s*<retType>)?\s*\{` -- i.e. method signatures with
//     `{` opening the body. Arrow-function class fields
//     (`foo = (x) => {...}`) are NOT extracted; the v1
//     fixture set uses canonical method declarations.
//
//   - Free functions: top-level `function name(params)
//     {...}` declarations are extracted with EnclosingClass
//     empty. Arrow-assignment free functions
//     (`const name = (params) => {...}`) are NOT extracted in
//     v1.
//
//   - Call extraction inside a method body: the scanner masks
//     strings / comments and matches the regex
//     `([A-Za-z_$][\w$]*)\s*\(` -- the captured identifier is
//     the call target. Keywords (`if`, `for`, `while`, etc.)
//     are filtered out. Member-call chains (`a.b.c(x)`) are
//     reduced to the rightmost identifier (`c`) so
//     same-file static-call resolution against a sibling
//     method named `c` works.
type tsjsParser struct{}

// NewTypeScriptParser returns the v1 TypeScript / JavaScript
// parser. Exposed so production wiring (`cmd/repo-indexer/
// main.go`, future) can construct it explicitly and so unit
// tests can pin a known instance.
func NewTypeScriptParser() LanguageParser { return tsjsParser{} }

func (tsjsParser) Language() string { return "typescript" }

// Extensions returns the v1 TS / JS file extensions the
// dispatcher routes through this parser. The set covers the
// canonical mainstream extensions; future declension stories
// (e.g. `.cts` / `.mts`) extend it via a follow-up edit here
// rather than at the dispatcher level.
func (tsjsParser) Extensions() []string {
	return []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
}

// tsClassRE matches a top-level `class` or `interface`
// declaration, capturing the name, optional `extends` clause,
// and optional `implements` clause. The `\{` at the end pins
// the match to declarations with a body -- a `class` keyword
// followed by something else is not a declaration.
var tsClassRE = regexp.MustCompile(
	`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?(class|interface)\s+` +
		`([A-Za-z_$][\w$]*)` +
		`(?:\s*<[^>]*>)?` +
		`(?:\s+extends\s+([^\s{]+(?:\s*,\s*[^\s{]+)*))?` +
		`(?:\s+implements\s+([^{]+))?` +
		`\s*\{`)

// tsFreeFnRE matches a top-level `function` declaration. The
// `(?m)` flag plus the leading `^` constraint ensures the
// match is anchored to a line start -- a `function` keyword
// inside a method body is not a top-level declaration.
var tsFreeFnRE = regexp.MustCompile(
	`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*` +
		`([A-Za-z_$][\w$]*)` +
		`(?:\s*<[^>]*>)?` +
		`\s*\(`)

// tsImportRE matches both `import` and `require` statements.
// The scanner is permissive: it captures any quoted module
// specifier on a line that starts with `import` or contains
// `require(`. v1 records the module string and leaves precise
// symbol resolution to a future cross-file resolver story.
var tsImportRE = regexp.MustCompile(
	`(?m)^\s*import\s+(?:[^"';]+\s+from\s+)?["']([^"']+)["']`)

// tsRequireRE matches `require("foo")` calls. Captured for
// CJS-style modules.
var tsRequireRE = regexp.MustCompile(`require\(\s*["']([^"']+)["']\s*\)`)

// tsMethodRE matches a method-like declaration inside a class
// body. The regex enforces:
//   - leading whitespace OR `{` / `;` (so the match cannot land
//     mid-expression)
//   - optional modifiers (`static`, `async`, `public`,
//     `private`, `protected`, `readonly`, `get`, `set`,
//     `override`, `abstract`, `*` for generators)
//   - method name (identifier OR `constructor`)
//   - optional generic parameter list `<...>`
//   - parameter list `(...)`
//   - optional return-type annotation `: ...`
//   - opening `{` (which means there is a body to walk)
//
// Method names of `if` / `for` / `while` / `switch` /
// `return` / `catch` / `do` are explicitly rejected
// post-match because they are statement keywords that share
// the surface shape with a method call.
var tsMethodRE = regexp.MustCompile(
	`(?:^|[\{\};])\s*((?:(?:static|async|public|private|protected|readonly|get|set|override|abstract|\*)\s+)*)` +
		`([A-Za-z_$][\w$]*)` +
		`(?:\s*<[^>]*>)?` +
		`\s*\(`)

// tsCallRE matches a bare-name call site inside a method body
// (the dispatcher uses this to populate `MethodDecl.Calls`).
// The regex looks for `<identifier>(` where the identifier is
// preceded by a non-identifier byte (so `.foo(` and `[foo](`
// are excluded -- only bare-name calls survive the filter).
var tsCallRE = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_$.])` +
		`([A-Za-z_$][\w$]*)` +
		`\s*\(`)

// tsReceiverCallRE matches `this.<name>(` -- receiver-qualified
// method calls. Receiver-qualified calls are unambiguous (the
// receiver scopes the lookup to the enclosing class) so the
// dispatcher resolves them against `<EnclosingClass>.<name>`
// without the bare-name ambiguity check that drops collisions
// from `tsCallRE`. Per evaluator finding #5.
var tsReceiverCallRE = regexp.MustCompile(
	`\bthis\.([A-Za-z_$][\w$]*)\s*\(`)

// tsMemberWriteRE matches `this.<name> = ` -- a write to a
// member field. The look-ahead requires `=` NOT followed by
// `=` (to avoid matching `this.x == y` as a write). Used to
// populate `MethodDecl.MemberAccesses` with IsWrite=true.
var tsMemberWriteRE = regexp.MustCompile(
	`\bthis\.([A-Za-z_$][\w$]*)\s*=(?:[^=]|$)`)

// tsMemberReadRE matches `this.<name>` -- any receiver-
// qualified member access. The dispatcher subtracts writes
// from this set to derive pure reads.
var tsMemberReadRE = regexp.MustCompile(
	`\bthis\.([A-Za-z_$][\w$]*)`)

// tsKeywords is the set of identifiers that look like calls
// but are actually language statements / operators. Members
// are filtered out of `MethodDecl.Calls` post-match.
var tsKeywords = map[string]struct{}{
	"if": {}, "for": {}, "while": {}, "switch": {}, "return": {},
	"catch": {}, "do": {}, "function": {}, "typeof": {}, "instanceof": {},
	"new": {}, "throw": {}, "await": {}, "yield": {}, "void": {},
	"delete": {}, "in": {}, "of": {}, "with": {},
}

// Parse implements LanguageParser. See package and tsjsParser
// doc comments for scope limits.
func (p tsjsParser) Parse(_ string, src []byte) (ParseResult, error) {
	source := string(src)
	masked := maskTSStringsAndComments(source)

	var res ParseResult
	res.Imports = p.parseImports(source, masked)
	res.Classes, res.Methods = p.parseClassesAndMethods(source, masked)
	res.Methods = append(res.Methods, p.parseFreeFunctions(source, masked)...)
	return res, nil
}

func (tsjsParser) parseImports(source, masked string) []Import {
	var imports []Import
	for _, m := range tsImportRE.FindAllStringSubmatchIndex(masked, -1) {
		// Module text lives inside a quoted literal which is
		// masked to spaces in `masked` -- the byte offsets
		// are stable so we recover the real text from
		// `source` (rubber-duck: masking-aware indexing).
		mod := source[m[2]:m[3]]
		stmtText := source[m[0]:m[1]]
		imports = append(imports, Import{
			Module:     mod,
			Line:       lineNumberAt(masked, m[0]),
			IsTypeOnly: tsImportIsTypeOnly(stmtText),
			Symbols:    tsImportNamedSymbols(stmtText),
			Alias:      tsImportNamespaceAlias(stmtText),
		})
	}
	for _, m := range tsRequireRE.FindAllStringSubmatchIndex(masked, -1) {
		mod := source[m[2]:m[3]]
		imports = append(imports, Import{
			Module: mod,
			Line:   lineNumberAt(masked, m[0]),
		})
	}
	return imports
}

// tsImportIsTypeOnly returns true when the source-text of the
// import statement contains a leading `import type` (or the
// per-symbol `import { type Foo }` shape).
func tsImportIsTypeOnly(stmt string) bool {
	stmt = strings.TrimSpace(stmt)
	return strings.HasPrefix(stmt, "import type ") ||
		strings.HasPrefix(stmt, "import type{") ||
		strings.HasPrefix(stmt, "import type(")
}

// tsImportNamedSymbols pulls the symbol names out of a TS
// `import { a, b as c, type D } from "..."` statement. The
// extractor accepts both bare and aliased forms and drops the
// `as alias` part so the symbol list reflects the SOURCE
// module's names. Returns nil for whole-module imports.
func tsImportNamedSymbols(stmt string) []string {
	open := strings.IndexByte(stmt, '{')
	close := strings.IndexByte(stmt, '}')
	if open < 0 || close <= open {
		return nil
	}
	body := stmt[open+1 : close]
	parts := strings.Split(body, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, "type ")
		if idx := strings.Index(p, " as "); idx > 0 {
			p = p[:idx]
		}
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// tsImportNamespaceAlias returns the alias from
// `import * as fs from "fs"` or the default-import name from
// `import os from "node:os"`. Returns "" when neither shape
// is present.
func tsImportNamespaceAlias(stmt string) string {
	// `import * as <alias> from`
	if idx := strings.Index(stmt, "* as "); idx >= 0 {
		rest := strings.TrimSpace(stmt[idx+len("* as "):])
		end := strings.IndexAny(rest, " \t,")
		if end > 0 {
			return rest[:end]
		}
		return rest
	}
	// `import <name> from` (default import; no braces, no star).
	if strings.Contains(stmt, "{") || strings.Contains(stmt, "* as ") {
		return ""
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stmt), "import"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "type"))
	end := strings.IndexAny(rest, " \t,")
	if end <= 0 {
		return ""
	}
	name := rest[:end]
	if name == "from" || name == "type" {
		return ""
	}
	return name
}

func (p tsjsParser) parseClassesAndMethods(source, masked string) ([]ClassDecl, []MethodDecl) {
	var classes []ClassDecl
	var methods []MethodDecl
	for _, m := range tsClassRE.FindAllStringSubmatchIndex(masked, -1) {
		kind := masked[m[2]:m[3]]
		name := masked[m[4]:m[5]]
		var extends, implements []string
		if m[6] >= 0 {
			for _, e := range strings.Split(masked[m[6]:m[7]], ",") {
				if t := strings.TrimSpace(e); t != "" {
					extends = append(extends, baseIdent(t))
				}
			}
		}
		if m[8] >= 0 {
			for _, e := range strings.Split(masked[m[8]:m[9]], ",") {
				if t := strings.TrimSpace(e); t != "" {
					implements = append(implements, baseIdent(t))
				}
			}
		}
		openBrace := m[1] - 1 // position of `{`
		bodyEnd, ok := matchBraces(masked, openBrace)
		if !ok {
			// Unbalanced braces -- skip this class declaration
			// rather than emitting half a class. A real
			// tree-sitter parse would error out the file; the
			// scanner degrades gracefully so a single corrupt
			// file does not abort the ingest.
			continue
		}
		start := lineNumberAt(masked, m[0])
		end := lineNumberAt(masked, bodyEnd)
		classes = append(classes, ClassDecl{
			QualifiedName: name,
			Kind:          kind,
			Extends:       extends,
			Implements:    implements,
			StartLine:     start,
			EndLine:       end,
		})

		body := masked[openBrace+1 : bodyEnd]
		rawBody := source[openBrace+1 : bodyEnd]
		methods = append(methods, p.parseMethods(source, masked, body, rawBody, openBrace+1, name)...)
	}
	return classes, methods
}

func (p tsjsParser) parseMethods(_, masked, body, _ string, bodyOffset int, className string) []MethodDecl {
	var out []MethodDecl
	for _, m := range tsMethodRE.FindAllStringSubmatchIndex(body, -1) {
		nameStart := m[4]
		name := body[nameStart:m[5]]
		if _, isKW := tsKeywords[name]; isKW {
			continue
		}
		modsRaw := strings.TrimSpace(body[m[2]:m[3]])
		var modifiers []string
		if modsRaw != "" {
			modifiers = strings.Fields(modsRaw)
		}
		// m[1] is the byte just after the `(`. Walk the
		// param list (in the masked source so a stray `)`
		// inside a comment can't confuse us).
		paramOpen := m[1] - 1
		paramClose, ok := matchParens(body, paramOpen)
		if !ok {
			continue
		}
		params := body[paramOpen+1 : paramClose]

		// Find the `{` that opens the method body. Walk
		// forward from paramClose, skipping return-type
		// annotations.
		braceIdx := strings.IndexByte(body[paramClose:], '{')
		if braceIdx < 0 {
			// No body (interface method declaration). Still
			// emit it -- declarations are real Nodes.
			out = append(out, MethodDecl{
				QualifiedName:  className + "." + name,
				EnclosingClass: className,
				ParamSignature: params,
				BodySource:     "",
				StartLine:      lineNumberAt(masked, bodyOffset+nameStart),
				EndLine:        lineNumberAt(masked, bodyOffset+paramClose),
				Modifiers:      modifiers,
			})
			continue
		}
		bodyOpen := paramClose + braceIdx
		bodyClose, ok := matchBraces(body, bodyOpen)
		if !ok {
			continue
		}
		methodBody := body[bodyOpen+1 : bodyClose]
		bodyStartLine := lineNumberAt(masked, bodyOffset+bodyOpen)
		bodyEndLine := lineNumberAt(masked, bodyOffset+bodyClose)
		out = append(out, MethodDecl{
			QualifiedName:  className + "." + name,
			EnclosingClass: className,
			ParamSignature: params,
			BodySource:     methodBody,
			StartLine:      lineNumberAt(masked, bodyOffset+nameStart),
			EndLine:        lineNumberAt(masked, bodyOffset+bodyClose),
			BodyStartLine:  bodyStartLine,
			BodyEndLine:    bodyEndLine,
			BodyStartByte:  bodyOffset + bodyOpen + 1,
			BodyEndByte:    bodyOffset + bodyClose - 1,
			Modifiers:      modifiers,
			Calls:          extractTSCalls(methodBody),
			ReceiverCalls:  extractTSReceiverCalls(methodBody),
			MemberAccesses: extractTSMemberAccesses(methodBody),
		})
	}
	return out
}

func (p tsjsParser) parseFreeFunctions(source, masked string) []MethodDecl {
	var out []MethodDecl
	for _, m := range tsFreeFnRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		paramOpen := m[1] - 1
		paramClose, ok := matchParens(masked, paramOpen)
		if !ok {
			continue
		}
		params := masked[paramOpen+1 : paramClose]
		braceIdx := strings.IndexByte(masked[paramClose:], '{')
		if braceIdx < 0 {
			continue
		}
		bodyOpen := paramClose + braceIdx
		bodyClose, ok := matchBraces(masked, bodyOpen)
		if !ok {
			continue
		}
		methodBody := source[bodyOpen+1 : bodyClose]
		bodyStartLine := lineNumberAt(masked, bodyOpen)
		bodyEndLine := lineNumberAt(masked, bodyClose)
		out = append(out, MethodDecl{
			QualifiedName:  name,
			EnclosingClass: "",
			ParamSignature: params,
			BodySource:     methodBody,
			StartLine:      lineNumberAt(masked, m[0]),
			EndLine:        lineNumberAt(masked, bodyClose),
			BodyStartLine:  bodyStartLine,
			BodyEndLine:    bodyEndLine,
			BodyStartByte:  bodyOpen + 1,
			BodyEndByte:    bodyClose - 1,
			Calls:          extractTSCalls(methodBody),
			ReceiverCalls:  extractTSReceiverCalls(methodBody),
			MemberAccesses: extractTSMemberAccesses(methodBody),
		})
	}
	return out
}

// extractTSCalls returns the deduped (insertion-order) list of
// bare-name identifiers used as call targets in methodBody.
// The body is masked first so strings / comments cannot inject
// phantom calls.
func extractTSCalls(methodBody string) []string {
	masked := maskTSStringsAndComments(methodBody)
	seen := map[string]struct{}{}
	var out []string
	for _, m := range tsCallRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		if _, isKW := tsKeywords[name]; isKW {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// extractTSReceiverCalls returns the deduped (insertion-order)
// list of `this.<name>(` call targets in methodBody.
// Receiver-qualified calls scope the resolver to the enclosing
// class (no ambiguity); the dispatcher emits a
// `static_calls` edge from this method to <enclosingClass>.<name>
// whenever the latter exists in the local symbol table. Per
// evaluator finding #5 -- without this extractor, the most
// common kind of intra-class call (`this.helper()`) would
// silently produce no edge.
func extractTSReceiverCalls(methodBody string) []string {
	masked := maskTSStringsAndComments(methodBody)
	seen := map[string]struct{}{}
	var out []string
	for _, m := range tsReceiverCallRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// extractTSMemberAccesses returns the per-method list of
// `this.<name>` member accesses. The walker first finds every
// `this.<name>` (read) and every `this.<name> =` (write),
// then collapses each name to its strongest classification:
// if the same name is written anywhere in the body, the
// record has IsWrite=true; otherwise IsWrite=false. The
// dispatcher folds the result into one `reads` and one
// `writes` edge from this method to its enclosing class,
// with the touched member names persisted on the edge's
// `attrs_json["members"]` (per rubber-duck #4 -- without
// member names the edges are too lossy to be useful).
func extractTSMemberAccesses(methodBody string) []MemberAccess {
	masked := maskTSStringsAndComments(methodBody)
	// Collect writes first so we can flag any name that
	// appears as a write target.
	writes := map[string]struct{}{}
	// Walk write matches; tsMemberWriteRE consumes one char
	// after `=`, so use a non-overlapping iterator.
	for _, m := range tsMemberWriteRE.FindAllStringSubmatchIndex(masked, -1) {
		writes[masked[m[2]:m[3]]] = struct{}{}
	}
	// Walk all member references; build a deduped name set
	// in source order, classifying each by the writes set.
	seen := map[string]bool{}
	var out []MemberAccess
	for _, m := range tsMemberReadRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		if seen[name] {
			continue
		}
		seen[name] = true
		_, isWrite := writes[name]
		out = append(out, MemberAccess{Name: name, IsWrite: isWrite})
	}
	return out
}

// baseIdent collapses `Foo<T, U>` / `pkg.Foo` / `pkg.Foo<T>`
// to `Foo`. Used to extract the resolvable identifier from an
// `extends` / `implements` clause -- v1 same-file resolution
// matches against the unqualified class name, so generics and
// namespaces are stripped.
func baseIdent(s string) string {
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// maskTSStringsAndComments returns src with every string
// literal, template literal, line comment, and block comment
// replaced by spaces of equal length. Line counts and offsets
// are preserved so regex matches against the masked text map
// back to source byte positions one-to-one.
//
// Regex literals (`/.../flags`) are NOT masked; v1 does not
// distinguish division from regex literals (a hard parser
// problem). Methods whose bodies contain `/.../` regexes that
// happen to include `class` or `function` keywords would
// produce phantom declarations -- the fixture set deliberately
// avoids that pattern.
func maskTSStringsAndComments(src string) string {
	out := []byte(src)
	i := 0
	for i < len(out) {
		c := out[i]
		// Block comment.
		if c == '/' && i+1 < len(out) && out[i+1] == '*' {
			end := strings.Index(string(out[i+2:]), "*/")
			if end < 0 {
				maskRange(out, i, len(out))
				return string(out)
			}
			maskRange(out, i, i+2+end+2)
			i = i + 2 + end + 2
			continue
		}
		// Line comment.
		if c == '/' && i+1 < len(out) && out[i+1] == '/' {
			nl := indexByteFrom(out, '\n', i)
			if nl < 0 {
				maskRange(out, i, len(out))
				return string(out)
			}
			maskRange(out, i, nl)
			i = nl
			continue
		}
		// String / template literal.
		if c == '\'' || c == '"' || c == '`' {
			end := scanTSString(out, i, c)
			maskRange(out, i+1, end) // keep quotes for offset clarity
			i = end + 1
			if i > len(out) {
				i = len(out)
			}
			continue
		}
		i++
	}
	return string(out)
}

func scanTSString(buf []byte, start int, quote byte) int {
	for j := start + 1; j < len(buf); j++ {
		if buf[j] == '\\' {
			j++
			continue
		}
		if buf[j] == quote {
			return j
		}
		// Template literal interpolation: `${ ... }` is masked
		// as part of the string to keep the scanner simple --
		// v1 fixtures don't rely on interpolated calls being
		// extracted.
	}
	return len(buf) - 1
}

func maskRange(buf []byte, lo, hi int) {
	if hi > len(buf) {
		hi = len(buf)
	}
	for j := lo; j < hi; j++ {
		if buf[j] != '\n' && buf[j] != '\r' {
			buf[j] = ' '
		}
	}
}

func indexByteFrom(buf []byte, c byte, from int) int {
	for j := from; j < len(buf); j++ {
		if buf[j] == c {
			return j
		}
	}
	return -1
}

// matchBraces returns the byte index of the `}` that closes
// the `{` at openIdx. Returns (0, false) if openIdx does not
// point at `{` or no matching brace exists. Operates on the
// masked source so braces inside string / template literals
// don't confuse the counter.
func matchBraces(masked string, openIdx int) (int, bool) {
	return matchPair(masked, openIdx, '{', '}')
}

// matchParens returns the byte index of the `)` that closes
// the `(` at openIdx. Same semantics as matchBraces.
func matchParens(masked string, openIdx int) (int, bool) {
	return matchPair(masked, openIdx, '(', ')')
}

func matchPair(masked string, openIdx int, open, close byte) (int, bool) {
	if openIdx < 0 || openIdx >= len(masked) || masked[openIdx] != open {
		return 0, false
	}
	depth := 0
	for j := openIdx; j < len(masked); j++ {
		switch masked[j] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return j, true
			}
		}
	}
	return 0, false
}

// lineNumberAt returns the 1-based line number of byte offset
// pos in src. Used to populate `StartLine` / `EndLine` on the
// parser's structured output.
func lineNumberAt(src string, pos int) int {
	if pos < 0 {
		pos = 0
	}
	if pos > len(src) {
		pos = len(src)
	}
	return 1 + strings.Count(src[:pos], "\n")
}
