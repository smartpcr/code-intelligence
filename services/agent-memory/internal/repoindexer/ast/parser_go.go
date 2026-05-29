package ast

import (
	"regexp"
	"strings"
)

// goTreeSitterParser implements LanguageParser for Go source files.
// It extracts top-level type declarations, function/method declarations,
// and import statements.
type goTreeSitterParser struct{}

// NewTreeSitterGoParser returns a Go LanguageParser.
func NewTreeSitterGoParser() LanguageParser {
	return &goTreeSitterParser{}
}

func (p *goTreeSitterParser) Language() string   { return "go" }
func (p *goTreeSitterParser) Extensions() []string { return []string{".go"} }

var (
	// funcRe matches top-level func declarations with optional receivers.
	// Groups: 1=receiver block, 2=receiver var, 3=pointer star, 4=receiver type, 5=func name, 6=params
	funcRe = regexp.MustCompile(`(?m)^func\s+(?:\(\s*(\w+)\s+(\*?)(\w+)\s*\)\s+)?(\w+)\s*\(([^)]*)\)\s*(?:\S[^\{]*)?\{`)

	// typeRe matches type declarations (struct / interface).
	typeRe = regexp.MustCompile(`(?m)^type\s+(\w+)\s+(struct|interface)\s*\{`)

	// importRe matches single-line imports: import "path"
	singleImportRe = regexp.MustCompile(`(?m)^import\s+"([^"]+)"`)

	// importBlockRe matches multi-line import blocks.
	importBlockRe = regexp.MustCompile(`(?ms)^import\s*\((.*?)\)`)

	// importPathRe matches individual paths within an import block.
	importPathRe = regexp.MustCompile(`"([^"]+)"`)
)

func (p *goTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	code := string(src)
	var result ParseResult

	// --- imports ---
	for _, m := range singleImportRe.FindAllStringSubmatch(code, -1) {
		result.Imports = append(result.Imports, Import{Path: m[1]})
	}
	for _, block := range importBlockRe.FindAllStringSubmatch(code, -1) {
		for _, pm := range importPathRe.FindAllStringSubmatch(block[1], -1) {
			result.Imports = append(result.Imports, Import{Path: pm[1]})
		}
	}

	// --- types ---
	for _, m := range typeRe.FindAllStringSubmatch(code, -1) {
		result.Classes = append(result.Classes, ClassDecl{
			QualifiedName: m[1],
			Kind:          m[2],
		})
	}

	// --- functions / methods ---
	for _, m := range funcRe.FindAllStringSubmatch(code, -1) {
		recVar := m[1]   // receiver variable name (empty for free funcs)
		ptrStar := m[2]  // "*" if pointer receiver
		recType := m[3]  // receiver type name
		funcName := m[4] // function name
		params := m[5]   // parameter list

		md := MethodDecl{
			LangMeta: make(map[string]any),
		}

		if recType != "" {
			// method with receiver
			isPtr := ptrStar == "*"
			if isPtr {
				md.QualifiedName = "*" + recType + "." + funcName
			} else {
				md.QualifiedName = recType + "." + funcName
			}
			md.EnclosingClass = recType
			md.LangMeta["receiver"] = recVar
			md.LangMeta["receiver_type"] = recType

			if isPtr {
				md.LangMeta["receiver_ptr"] = true
				md.ReceiverAliases = []string{recType + "." + funcName}
			} else {
				md.LangMeta["receiver_ptr"] = false
			}
		} else {
			md.QualifiedName = funcName
		}

		md.ParamSignature = normaliseParams(params)
		result.Methods = append(result.Methods, md)
	}

	return result, nil
}

// normaliseParams strips parameter names, keeping only types.
func normaliseParams(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	var types []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		tokens := strings.Fields(p)
		if len(tokens) >= 2 {
			types = append(types, tokens[len(tokens)-1])
		} else if len(tokens) == 1 {
			types = append(types, tokens[0])
		}
	}
	return strings.Join(types, ", ")
}
