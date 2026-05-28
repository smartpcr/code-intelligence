package ast

import (
	"strings"
)

// ---------------------------------------------------------------------------
// Emitter — resolves method calls against the multimap of declared
// methods, implementing Pass 2b collision-drop (A5) and pointer-only
// resolution via ReceiverAliases.
// ---------------------------------------------------------------------------

// Emitter converts a ParseResult into edges and raw-call records.
type Emitter struct {
	result ParseResult
}

// NewEmitter creates an Emitter for the given parse result.
func NewEmitter(pr ParseResult) *Emitter {
	return &Emitter{result: pr}
}

// Emit resolves method calls and returns (edges, callsRaw).
//
// Multimap rules:
//   - Build a map from short method name → set of qualified targets.
//   - If set size > 1 for a call target: drop (no static_calls edge),
//     but keep the name in callsRaw.
//   - If set size == 1: emit a static_calls edge.
//   - If set size == 0 but an alias exists in ReceiverAliases: resolve
//     via the alias and emit a static_calls edge.
func (em *Emitter) Emit() ([]Edge, []string) {
	// Build multimap: short name → set of qualified "ClassName.Name" targets.
	methodTargets := make(map[string]map[string]struct{})
	for _, m := range em.result.Methods {
		qualName := m.ClassName + "." + m.Name
		if _, ok := methodTargets[m.Name]; !ok {
			methodTargets[m.Name] = make(map[string]struct{})
		}
		methodTargets[m.Name][qualName] = struct{}{}
	}

	// Build alias map from ReceiverAliases across all methods.
	aliasMap := make(map[string]string)
	for _, m := range em.result.Methods {
		for alias, target := range m.ReceiverAliases {
			aliasMap[alias] = target
		}
	}

	var edges []Edge
	var callsRaw []string

	for _, caller := range em.result.Methods {
		callerQualName := caller.ClassName + "." + caller.Name

		// Iterate only over call-sites declared by the parser, not all methods.
		for _, shortName := range caller.Calls {
			callsRaw = append(callsRaw, shortName)

			targets := methodTargets[shortName]

			if len(targets) > 1 {
				// A5: set size > 1 → drop, do NOT emit static_calls edge.
				continue
			}

			if len(targets) == 1 {
				for tgt := range targets {
					edges = append(edges, Edge{
						Kind:   "static_calls",
						Source: callerQualName,
						Target: tgt,
					})
				}
				continue
			}

			// Check alias map for pointer-only resolution.
			if canonical, ok := aliasMap[shortName]; ok {
				edges = append(edges, Edge{
					Kind:   "static_calls",
					Source: callerQualName,
					Target: canonical,
				})
			}
		}
	}

	return edges, callsRaw
}

// ---------------------------------------------------------------------------
// NewGoParser returns a stub Go parser. In the full implementation this
// uses tree-sitter; the stub satisfies the Parser interface.
// ---------------------------------------------------------------------------

type goParser struct{}

// NewGoParser creates a Go language parser.
func NewGoParser() Parser {
	return &goParser{}
}

func (p *goParser) Parse(filename string, src []byte) (ParseResult, error) {
	// Stub — real implementation uses tree-sitter.
	return ParseResult{}, nil
}

func (p *goParser) Language() string        { return "go" }
func (p *goParser) Extensions() []string    { return []string{".go"} }

// ---------------------------------------------------------------------------
// NewTypeScriptParser returns a TypeScript parser stub.
// ---------------------------------------------------------------------------

type tsParser struct{}

// NewTypeScriptParser creates a TypeScript language parser.
func NewTypeScriptParser() Parser {
	return &tsParser{}
}

func (p *tsParser) Parse(filename string, src []byte) (ParseResult, error) {
	_ = strings.Contains(filename, ".ts") // stub
	return ParseResult{}, nil
}

func (p *tsParser) Language() string     { return "typescript" }
func (p *tsParser) Extensions() []string { return []string{".ts", ".tsx"} }

// ---------------------------------------------------------------------------
// NewPythonParser returns a Python parser stub.
// ---------------------------------------------------------------------------

type pyParser struct{}

// NewPythonParser creates a Python language parser.
func NewPythonParser() Parser {
	return &pyParser{}
}

func (p *pyParser) Parse(filename string, src []byte) (ParseResult, error) {
	_ = strings.Contains(filename, ".py") // stub
	return ParseResult{}, nil
}

func (p *pyParser) Language() string     { return "python" }
func (p *pyParser) Extensions() []string { return []string{".py"} }
