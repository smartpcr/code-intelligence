package ast

import (
	"path/filepath"
	"strings"
)

// LogEntry represents a single structured log event captured by the dispatcher.
type LogEntry struct {
	Message string
	Attrs   map[string]string
}

// Logger receives structured log events from the dispatcher.
type Logger interface {
	Log(msg string, attrs map[string]string)
}

// NodeEdgeWriter accepts Node and Edge inserts from the dispatcher.
type NodeEdgeWriter interface {
	InsertNode(n Node) error
	InsertEdge(e Edge) error
}

// Node represents a graph node emitted by the dispatcher.
type Node struct {
	Kind string
	Name string
}

// Edge represents a graph edge emitted by the dispatcher.
type Edge struct {
	Kind   string
	Source string
	Target string
}

// EmitResult summarises what the dispatcher wrote for one file.
type EmitResult struct {
	NodeCount int
	EdgeCount int
}

// Dispatcher routes source files to registered parsers and writes
// the resulting nodes/edges through a NodeEdgeWriter.
type Dispatcher struct {
	extMap map[string]Parser
	writer NodeEdgeWriter
	logger Logger
}

// NewDispatcher creates a Dispatcher from the given parser set.
func NewDispatcher(parsers []Parser, w NodeEdgeWriter, l Logger) *Dispatcher {
	em := make(map[string]Parser, len(parsers)*2)
	for _, p := range parsers {
		for _, ext := range p.Extensions() {
			em[ext] = p
		}
	}
	return &Dispatcher{extMap: em, writer: w, logger: l}
}

// EmitFile parses a single file and emits nodes/edges to the writer
// following the documented Pass contract (architecture Section 4 +
// walker docstring at parser_treesitter_rust.go:1-126):
//
//   - Pass 0  (imports):       one `package` Node + one `imports`
//     Edge per non-relative ParseResult.Import. Relative module
//     specifiers (`./...`, `../...`) are dropped here because they
//     map onto in-repo File nodes that a later cross-file resolver
//     workstream will stitch (see ParseResult.Imports docstring).
//   - Pass 1a (classes):       one `class` Node per ClassDecl.
//   - Pass 1b (methods):       one `method` Node per MethodDecl,
//     plus a simple-name multimap used by Pass 2b's ambiguity-aware
//     bare-name resolver. ReceiverAliases are registered ONLY into
//     the scoped QN lookup map (used by Pass 2b's receiver-qualified
//     path and Pass 2d), NOT into the bare-name multimap, to avoid
//     creating artificial ambiguity for sibling parsers that emit
//     pointer aliases (e.g. Go's `*Foo.Bar` → `Foo.Bar` alias).
//   - Pass 2a (extends + implements): one edge per
//     ClassDecl.Extends/Implements entry whose target is in the
//     file's local class set (cross-file targets are dropped per
//     architecture A4 silent-drop rule).
//   - Pass 2b (static_calls):  AMBIGUITY-AWARE — a bare call
//     target is emitted as an edge ONLY when exactly one local
//     method has a matching simple name (per parser.go's Calls
//     docstring: "ambiguous bare names ... are dropped").
//     Receiver-qualified calls are scoped to
//     `<EnclosingClass>.<name>` and emitted when the scoped
//     target exists locally (including via ReceiverAliases).
//   - Pass 2d (overrides):     one `overrides` edge per impl
//     method (LangMeta["trait"] set) whose trait-side method
//     exists locally AND carries LangMeta["trait_default"]=true
//     (architecture Section 7.2 A4 cross-file silent-drop rule).
//
// When no parser is registered for the file's extension the dispatcher
// logs an ast.dispatch.skip event with reason "no_parser" and returns
// a zero EmitResult with nil error.
//
// When the writer is nil (used by tests that exercise the routing
// surface only) the dispatcher walks the ParseResult per the Pass
// contract but skips every `InsertNode`/`InsertEdge` call, returning
// a zero EmitResult with nil error. Writer errors propagate; the
// caller decides whether to abort the surrounding ingest.
func (d *Dispatcher) EmitFile(filename string, src []byte) (EmitResult, error) {
	ext := filepath.Ext(filename)

	p, ok := d.extMap[ext]
	if !ok {
		if d.logger != nil {
			d.logger.Log("ast.dispatch.skip", map[string]string{
				"file":   filename,
				"reason": "no_parser",
			})
		}
		return EmitResult{}, nil
	}

	result, err := p.Parse(filename, src)
	if err != nil {
		return EmitResult{}, err
	}

	var (
		nodes int
		edges int
	)

	insertNode := func(n Node) error {
		nodes++
		if d.writer == nil {
			return nil
		}
		return d.writer.InsertNode(n)
	}
	insertEdge := func(e Edge) error {
		edges++
		if d.writer == nil {
			return nil
		}
		return d.writer.InsertEdge(e)
	}

	// Pass 0: imports → package nodes + imports edges
	// (skip workspace-relative module specifiers).
	for _, imp := range result.Imports {
		if isRelativeImportSpecifier(imp.Module) {
			continue
		}
		if err := insertNode(Node{Kind: "package", Name: imp.Module}); err != nil {
			return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
		}
		if err := insertEdge(Edge{Kind: "imports", Source: filename, Target: imp.Module}); err != nil {
			return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
		}
	}

	// Pass 1a: classes (build local-class set for Pass 2a).
	localClasses := make(map[string]struct{}, len(result.Classes))
	for _, c := range result.Classes {
		if err := insertNode(Node{Kind: "class", Name: c.QualifiedName}); err != nil {
			return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
		}
		localClasses[c.QualifiedName] = struct{}{}
	}

	// Pass 1b: methods (build scoped-QN map for receiver-qualified
	// resolution AND simple-name multimap for ambiguity-aware
	// bare-name resolution).
	localMethodsByQN := make(map[string]MethodDecl, len(result.Methods))
	simpleNameToQNs := make(map[string]map[string]struct{}, len(result.Methods))
	registerSimpleName := func(simple, qn string) {
		set, ok := simpleNameToQNs[simple]
		if !ok {
			set = make(map[string]struct{}, 1)
			simpleNameToQNs[simple] = set
		}
		set[qn] = struct{}{}
	}
	for _, m := range result.Methods {
		if err := insertNode(Node{Kind: "method", Name: m.QualifiedName}); err != nil {
			return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
		}
		localMethodsByQN[m.QualifiedName] = m
		registerSimpleName(lastDottedSegment(m.QualifiedName), m.QualifiedName)
		// Register receiver aliases ONLY in the scoped QN map
		// so receiver-qualified call resolution and Pass 2d can
		// see them; NOT in the bare-name multimap (avoids
		// creating artificial bare-name ambiguity for Go's
		// pointer-receiver aliases).
		for _, alias := range m.ReceiverAliases {
			localMethodsByQN[alias] = m
		}
	}

	// Pass 2a: extends + implements edges (local targets only;
	// cross-file targets silently dropped per architecture A4).
	for _, c := range result.Classes {
		for _, parent := range c.Extends {
			if _, ok := localClasses[parent]; ok {
				if err := insertEdge(Edge{Kind: "extends", Source: c.QualifiedName, Target: parent}); err != nil {
					return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
				}
			}
		}
		for _, iface := range c.Implements {
			if _, ok := localClasses[iface]; ok {
				if err := insertEdge(Edge{Kind: "implements", Source: c.QualifiedName, Target: iface}); err != nil {
					return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
				}
			}
		}
	}

	// Pass 2b: static_calls — ambiguity-aware bare-name + scoped
	// receiver-qualified resolution.
	for _, m := range result.Methods {
		for _, callee := range m.Calls {
			simple := lastDottedSegment(callee)
			candidates := simpleNameToQNs[simple]
			if len(candidates) != 1 {
				continue
			}
			var target string
			for qn := range candidates {
				target = qn
			}
			if err := insertEdge(Edge{Kind: "static_calls", Source: m.QualifiedName, Target: target}); err != nil {
				return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
			}
		}
		if m.EnclosingClass != "" {
			for _, callee := range m.ReceiverCalls {
				target := m.EnclosingClass + "." + callee
				if _, ok := localMethodsByQN[target]; !ok {
					continue
				}
				if err := insertEdge(Edge{Kind: "static_calls", Source: m.QualifiedName, Target: target}); err != nil {
					return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
				}
			}
		}
	}

	// Pass 2d: overrides (impl method → trait-default trait
	// method, same file only; cross-file misses silently
	// dropped per architecture A4).
	for _, m := range result.Methods {
		if m.LangMeta == nil {
			continue
		}
		traitName, ok := m.LangMeta["trait"].(string)
		if !ok || traitName == "" {
			continue
		}
		traitMethodQN := traitName + "." + lastDottedSegment(m.QualifiedName)
		traitMethod, ok := localMethodsByQN[traitMethodQN]
		if !ok {
			continue
		}
		if traitMethod.LangMeta == nil || traitMethod.LangMeta["trait_default"] != true {
			continue
		}
		if err := insertEdge(Edge{Kind: "overrides", Source: m.QualifiedName, Target: traitMethodQN}); err != nil {
			return EmitResult{NodeCount: nodes, EdgeCount: edges}, err
		}
	}

	return EmitResult{NodeCount: nodes, EdgeCount: edges}, nil
}

// lastDottedSegment returns the right-most dotted segment of a
// qualified name (e.g. "Foo.bar" → "bar", "free_fn" → "free_fn").
// Used by Pass 2b's bare-name multimap and Pass 2d's trait-method
// lookup.
func lastDottedSegment(qn string) string {
	if i := strings.LastIndexByte(qn, '.'); i >= 0 {
		return qn[i+1:]
	}
	return qn
}

// isRelativeImportSpecifier reports whether a module specifier
// is a workspace-relative path that Pass 0 should skip. Matches
// the prefixes used by every v1 parser language whose grammar
// supports dot/up-tree relative imports (`./foo`, `../bar`).
func isRelativeImportSpecifier(mod string) bool {
	return strings.HasPrefix(mod, "./") || strings.HasPrefix(mod, "../")
}
