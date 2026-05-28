package ast

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Dispatcher — routes files to parsers by extension, handles sentinel
// errors (ErrParserUnavailable), and delegates to the writer.
// ---------------------------------------------------------------------------

// Dispatcher routes source files to the appropriate parser.
type Dispatcher struct {
	parsers map[string]Parser // extension → parser
	writer  Writer
	logger  Logger
}

// DispatcherOption configures a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithParser registers a parser for all of its declared extensions.
func WithParser(p Parser) DispatcherOption {
	return func(d *Dispatcher) {
		for _, ext := range p.Extensions() {
			d.parsers[ext] = p
		}
	}
}

// WithWriter sets the graph writer.
func WithWriter(w Writer) DispatcherOption {
	return func(d *Dispatcher) { d.writer = w }
}

// WithLogger sets the structured logger.
func WithLogger(l Logger) DispatcherOption {
	return func(d *Dispatcher) { d.logger = l }
}

// NewDispatcher creates a Dispatcher with the given options.
func NewDispatcher(opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{parsers: make(map[string]Parser)}
	for _, o := range opts {
		o(d)
	}
	return d
}

// reasonPattern extracts (reason=...) from an error message.
var reasonPattern = regexp.MustCompile(`\(reason=([^)]+)\)`)

// EmitFile parses a single file and emits nodes/edges to the writer.
// When the parser returns ErrParserUnavailable the dispatcher logs
// an ast.dispatch.skip event and returns (EmitResult{}, nil).
func (d *Dispatcher) EmitFile(filename string, src []byte) (EmitResult, error) {
	ext := ""
	if idx := strings.LastIndex(filename, "."); idx >= 0 {
		ext = filename[idx:]
	}

	p, ok := d.parsers[ext]
	if !ok {
		return EmitResult{}, fmt.Errorf("no parser registered for extension %q", ext)
	}

	result, err := p.Parse(filename, src)
	if err != nil {
		if errors.Is(err, ErrParserUnavailable) {
			reason := "unknown"
			if m := reasonPattern.FindStringSubmatch(err.Error()); len(m) > 1 {
				reason = m[1]
			}
			if d.logger != nil {
				d.logger.Log("ast.dispatch.skip", map[string]string{
					"file":   filename,
					"reason": reason,
				})
			}
			return EmitResult{}, nil
		}
		return EmitResult{}, err
	}

	var nodeCount, edgeCount int

	// Emit class nodes.
	for _, c := range result.Classes {
		if err := d.writer.InsertNode(Node{Kind: "class", Name: c.Name}); err != nil {
			return EmitResult{}, fmt.Errorf("insert class node %q: %w", c.Name, err)
		}
		nodeCount++
	}

	// Emit method nodes and class→method edges.
	for _, m := range result.Methods {
		if err := d.writer.InsertNode(Node{Kind: "method", Name: m.Name}); err != nil {
			return EmitResult{}, fmt.Errorf("insert method node %q: %w", m.Name, err)
		}
		nodeCount++

		if m.ClassName != "" {
			if err := d.writer.InsertEdge(Edge{Kind: "has_method", Source: m.ClassName, Target: m.Name}); err != nil {
				return EmitResult{}, fmt.Errorf("insert has_method edge %q→%q: %w", m.ClassName, m.Name, err)
			}
			edgeCount++
		}
	}

	// Emit import nodes and file→import edges.
	for _, imp := range result.Imports {
		if err := d.writer.InsertNode(Node{Kind: "import", Name: imp.Path}); err != nil {
			return EmitResult{}, fmt.Errorf("insert import node %q: %w", imp.Path, err)
		}
		nodeCount++

		if err := d.writer.InsertEdge(Edge{Kind: "imports", Source: filename, Target: imp.Path}); err != nil {
			return EmitResult{}, fmt.Errorf("insert imports edge %q→%q: %w", filename, imp.Path, err)
		}
		edgeCount++
	}

	return EmitResult{NodeCount: nodeCount, EdgeCount: edgeCount}, nil
}
