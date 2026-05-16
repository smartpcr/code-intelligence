package ast

import (
	"regexp"
	"strings"
)

// pythonParser is the Stage 3.2 Python parser. Like
// `tsjsParser` it is a lightweight scanner that emits the
// language-agnostic `ParseResult` -- a future story can swap
// in a tree-sitter-backed implementation behind the same
// `LanguageParser` interface without touching the dispatcher.
//
// Scope limits the scanner deliberately accepts:
//
//   - Triple-quoted strings (`"""..."""` and `”'...”'`)
//     and single-quoted strings are masked to spaces before
//     declaration regexes run.
//
//   - Decorators (`@decorator`) above class / def declarations
//     are tolerated but NOT captured in v1.
//
//   - Nested class definitions are NOT extracted (top-level
//     and direct-child-of-class only).
//
//   - Free-function `def name(params):` declarations at
//     module scope (column 0 indent) are extracted with
//     EnclosingClass empty.
//
//   - Method body detection uses Python's indentation rules:
//     a method's body is every contiguous line indented
//     STRICTLY MORE than the `def` line itself, up to the
//     next line at less-or-equal indent OR end of file.
type pythonParser struct{}

// NewPythonParser returns the v1 Python parser. See pythonParser
// doc for scope limits.
func NewPythonParser() LanguageParser { return pythonParser{} }

func (pythonParser) Language() string { return "python" }

// Extensions returns the v1 Python file extensions the
// dispatcher routes through this parser. `.pyi` (stub files)
// is included because stub-file methods are legitimate Nodes
// even though they have no body.
func (pythonParser) Extensions() []string {
	return []string{".py", ".pyi"}
}

// pyClassRE matches a `class Name(bases):` line. The line
// must start at column 0 in the masked source (the parser
// only extracts top-level class declarations in v1).
var pyClassRE = regexp.MustCompile(
	`(?m)^class\s+([A-Za-z_][\w]*)\s*(?:\(([^)]*)\))?\s*:`)

// pyDefRE matches a `def name(params)` line at any indent.
// The dispatcher uses the captured indent to decide
// EnclosingClass.
var pyDefRE = regexp.MustCompile(
	`(?m)^([ \t]*)(?:async\s+)?def\s+([A-Za-z_][\w]*)\s*\(`)

// pyImportRE matches both `import foo` / `import foo as f`
// and `from foo import a, b`. The captured group is the
// module specifier.
var pyImportRE = regexp.MustCompile(
	`(?m)^(?:from\s+([A-Za-z_][\w.]*)\s+import\s+([^#\n]+)|import\s+([A-Za-z_][\w.]*)(?:\s+as\s+([A-Za-z_][\w]*))?)`)

// pyCallRE matches a bare-name call site `name(`. The
// preceding character must be a non-identifier byte so
// `self.name(` (method-on-self) and `foo.name(` are excluded
// from THIS regex -- `self.<name>(` is captured separately by
// `pySelfCallRE` so the dispatcher can resolve it against the
// enclosing class's methods (per evaluator finding #5).
var pyCallRE = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_.])([A-Za-z_][\w]*)\s*\(`)

// pySelfCallRE matches `self.<name>(` -- receiver-qualified
// method calls in Python. Receiver-qualified calls are
// unambiguous (the receiver scopes the lookup to the
// enclosing class) so the dispatcher resolves them against
// `<EnclosingClass>.<name>` without the bare-name ambiguity
// check that drops collisions from `pyCallRE`.
var pySelfCallRE = regexp.MustCompile(
	`\bself\.([A-Za-z_][\w]*)\s*\(`)

// pySelfWriteRE matches `self.<name> =` -- a write to an
// instance attribute. The negative look-ahead excludes
// `==` to avoid matching equality tests as writes.
var pySelfWriteRE = regexp.MustCompile(
	`\bself\.([A-Za-z_][\w]*)\s*=(?:[^=]|$)`)

// pySelfReadRE matches `self.<name>` -- any receiver-
// qualified instance attribute reference. The dispatcher
// subtracts writes from this set to derive pure reads.
var pySelfReadRE = regexp.MustCompile(
	`\bself\.([A-Za-z_][\w]*)`)

var pyKeywords = map[string]struct{}{
	"if": {}, "elif": {}, "else": {}, "for": {}, "while": {},
	"with": {}, "try": {}, "except": {}, "finally": {}, "raise": {},
	"return": {}, "yield": {}, "lambda": {}, "def": {}, "class": {},
	"and": {}, "or": {}, "not": {}, "is": {}, "in": {},
	"as": {}, "from": {}, "import": {}, "global": {}, "nonlocal": {},
	"pass": {}, "break": {}, "continue": {}, "assert": {}, "async": {},
	"await": {}, "print": {},
}

// Parse implements LanguageParser. See package and pythonParser
// doc for scope limits.
func (p pythonParser) Parse(_ string, src []byte) (ParseResult, error) {
	source := string(src)
	masked := maskPyStringsAndComments(source)

	var res ParseResult
	res.Imports = p.parseImports(masked)
	res.Classes, res.Methods = p.parseClassesAndMethods(source, masked)
	res.Methods = append(res.Methods, p.parseFreeFunctions(source, masked)...)
	return res, nil
}

func (pythonParser) parseImports(masked string) []Import {
	var imports []Import
	for _, m := range pyImportRE.FindAllStringSubmatchIndex(masked, -1) {
		line := lineNumberAt(masked, m[0])
		if m[2] >= 0 {
			// `from X import a, b`
			mod := masked[m[2]:m[3]]
			rawSyms := masked[m[4]:m[5]]
			var symbols []string
			for _, s := range strings.Split(rawSyms, ",") {
				name := strings.TrimSpace(strings.Split(s, " as ")[0])
				if name != "" && name != "*" {
					symbols = append(symbols, name)
				}
			}
			imports = append(imports, Import{
				Module:  mod,
				Symbols: symbols,
				Line:    line,
			})
			continue
		}
		// `import X` / `import X as Y`
		mod := masked[m[6]:m[7]]
		alias := ""
		if m[8] >= 0 {
			alias = masked[m[8]:m[9]]
		}
		imports = append(imports, Import{
			Module: mod,
			Alias:  alias,
			Line:   line,
		})
	}
	return imports
}

func (p pythonParser) parseClassesAndMethods(source, masked string) ([]ClassDecl, []MethodDecl) {
	var classes []ClassDecl
	var methods []MethodDecl
	lines := strings.Split(masked, "\n")
	rawLines := strings.Split(source, "\n")

	for _, m := range pyClassRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		var bases []string
		if m[4] >= 0 {
			for _, b := range strings.Split(masked[m[4]:m[5]], ",") {
				if t := strings.TrimSpace(b); t != "" {
					bases = append(bases, baseIdent(t))
				}
			}
		}
		startLine := lineNumberAt(masked, m[0])
		endLine := pyBlockEnd(lines, startLine-1, 0)
		classes = append(classes, ClassDecl{
			QualifiedName: name,
			Kind:          "class",
			Extends:       bases,
			StartLine:     startLine,
			EndLine:       endLine,
		})

		// Walk methods inside this class. A method is any
		// `def` whose indent is STRICTLY greater than the
		// class's indent (0 in v1) AND whose line is in the
		// class's body range.
		for i := startLine; i < endLine && i < len(lines); i++ {
			line := lines[i]
			if !strings.Contains(line, "def ") &&
				!strings.Contains(line, "def\t") {
				continue
			}
			sub := pyDefRE.FindStringSubmatchIndex(line)
			if sub == nil {
				continue
			}
			indent := line[sub[2]:sub[3]]
			if indent == "" {
				continue // top-level def, not a method
			}
			methodName := line[sub[4]:sub[5]]
			paramOpen := sub[1] - 1
			paramClose, ok := matchParens(line, paramOpen)
			if !ok {
				continue
			}
			params := line[paramOpen+1 : paramClose]
			defLineIdx := i
			methodIndentSize := len(indent)
			bodyStart := defLineIdx + 1
			bodyEnd := pyBlockEnd(lines, defLineIdx, methodIndentSize)
			bodyLines := rawLines[bodyStart:min(bodyEnd, len(rawLines))]
			methodBody := strings.Join(bodyLines, "\n")
			methods = append(methods, MethodDecl{
				QualifiedName:  name + "." + methodName,
				EnclosingClass: name,
				ParamSignature: params,
				BodySource:     methodBody,
				StartLine:      defLineIdx + 1,
				EndLine:        bodyEnd,
				BodyStartLine:  bodyStart + 1,
				BodyEndLine:    bodyEnd,
				BodyStartByte:  byteOffsetOfLineInSource(source, bodyStart+1),
				BodyEndByte:    byteOffsetOfLineInSource(source, bodyEnd+1) - 1,
				Calls:          extractPyCalls(methodBody),
				ReceiverCalls:  extractPySelfCalls(methodBody),
				MemberAccesses: extractPySelfAccesses(methodBody),
			})
		}
	}
	return classes, methods
}

func (p pythonParser) parseFreeFunctions(source, masked string) []MethodDecl {
	var out []MethodDecl
	lines := strings.Split(masked, "\n")
	rawLines := strings.Split(source, "\n")

	for _, m := range pyDefRE.FindAllStringSubmatchIndex(masked, -1) {
		indent := masked[m[2]:m[3]]
		if indent != "" {
			continue // method, not a free function
		}
		name := masked[m[4]:m[5]]
		paramOpen := m[1] - 1
		paramClose, ok := matchParens(masked, paramOpen)
		if !ok {
			continue
		}
		params := masked[paramOpen+1 : paramClose]
		startLine := lineNumberAt(masked, m[0])
		defLineIdx := startLine - 1
		endLine := pyBlockEnd(lines, defLineIdx, 0)
		bodyStart := defLineIdx + 1
		bodyLines := rawLines[bodyStart:min(endLine, len(rawLines))]
		methodBody := strings.Join(bodyLines, "\n")
		out = append(out, MethodDecl{
			QualifiedName:  name,
			EnclosingClass: "",
			ParamSignature: params,
			BodySource:     methodBody,
			StartLine:      startLine,
			EndLine:        endLine,
			BodyStartLine:  bodyStart + 1,
			BodyEndLine:    endLine,
			BodyStartByte:  byteOffsetOfLineInSource(source, bodyStart+1),
			BodyEndByte:    byteOffsetOfLineInSource(source, endLine+1) - 1,
			Calls:          extractPyCalls(methodBody),
			ReceiverCalls:  extractPySelfCalls(methodBody),
			MemberAccesses: extractPySelfAccesses(methodBody),
		})
	}
	return out
}

func extractPyCalls(body string) []string {
	masked := maskPyStringsAndComments(body)
	seen := map[string]struct{}{}
	var out []string
	for _, m := range pyCallRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		if _, isKW := pyKeywords[name]; isKW {
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

// extractPySelfCalls returns the deduped (insertion-order)
// list of `self.<name>(` call targets. Per evaluator finding
// #5 -- the most common kind of intra-class call in Python.
func extractPySelfCalls(body string) []string {
	masked := maskPyStringsAndComments(body)
	seen := map[string]struct{}{}
	var out []string
	for _, m := range pySelfCallRE.FindAllStringSubmatchIndex(masked, -1) {
		name := masked[m[2]:m[3]]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// extractPySelfAccesses returns the per-method list of
// `self.<name>` member accesses. Mirrors
// `extractTSMemberAccesses`; see its doc comment for the
// classification rule.
func extractPySelfAccesses(body string) []MemberAccess {
	masked := maskPyStringsAndComments(body)
	writes := map[string]struct{}{}
	for _, m := range pySelfWriteRE.FindAllStringSubmatchIndex(masked, -1) {
		writes[masked[m[2]:m[3]]] = struct{}{}
	}
	seen := map[string]bool{}
	var out []MemberAccess
	for _, m := range pySelfReadRE.FindAllStringSubmatchIndex(masked, -1) {
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

// byteOffsetOfLineInSource returns the 0-based byte offset of
// the start of the (1-based) line within s. Used by the
// Python parser to compute body-byte boundaries for
// `MethodDecl.BodyStartByte` / `BodyEndByte`. Returns
// `len(s)` for lines past the end of the source.
func byteOffsetOfLineInSource(s string, line int) int {
	if line <= 1 {
		return 0
	}
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			count++
			if count == line-1 {
				return i + 1
			}
		}
	}
	return len(s)
}

// pyBlockEnd returns the 1-based exclusive end line of a
// Python block whose header is at lines[headerIdx] and whose
// indent depth is headerIndent (count of leading spaces /
// tabs). The block ends at the first non-blank line whose
// indent is <= headerIndent (or end of file).
func pyBlockEnd(lines []string, headerIdx, headerIndent int) int {
	for i := headerIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		ind := leadingIndent(line)
		if ind <= headerIndent {
			return i // exclusive end (line above the dedent)
		}
	}
	return len(lines)
}

func leadingIndent(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return i
		}
	}
	return len(line)
}

// maskPyStringsAndComments returns src with triple-quoted
// strings, single-quoted strings, and `#` line comments
// replaced by spaces of equal length. Line counts and offsets
// are preserved.
func maskPyStringsAndComments(src string) string {
	out := []byte(src)
	i := 0
	for i < len(out) {
		c := out[i]
		// Triple-quoted string.
		if (c == '"' || c == '\'') && i+2 < len(out) &&
			out[i+1] == c && out[i+2] == c {
			triple := string([]byte{c, c, c})
			end := strings.Index(string(out[i+3:]), triple)
			if end < 0 {
				maskRange(out, i+3, len(out))
				return string(out)
			}
			maskRange(out, i+3, i+3+end)
			i = i + 3 + end + 3
			continue
		}
		// Single-quoted string.
		if c == '"' || c == '\'' {
			end := scanPyString(out, i, c)
			maskRange(out, i+1, end)
			i = end + 1
			if i > len(out) {
				i = len(out)
			}
			continue
		}
		// Line comment.
		if c == '#' {
			nl := indexByteFrom(out, '\n', i)
			if nl < 0 {
				maskRange(out, i, len(out))
				return string(out)
			}
			maskRange(out, i, nl)
			i = nl
			continue
		}
		i++
	}
	return string(out)
}

func scanPyString(buf []byte, start int, quote byte) int {
	for j := start + 1; j < len(buf); j++ {
		if buf[j] == '\\' {
			j++
			continue
		}
		if buf[j] == '\n' {
			return j
		}
		if buf[j] == quote {
			return j
		}
	}
	return len(buf) - 1
}
