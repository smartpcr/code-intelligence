package ast

import (
	"fmt"
	"strings"
)

// Dispatcher routes source files to the appropriate parser by extension,
// emits nodes and edges through the Writer, and runs post-parse passes
// (Pass2d) for derived edges like overrides.
type Dispatcher struct {
	parsers map[string]Parser // extension → parser
	writer  Writer
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

// NewDispatcher creates a Dispatcher with the given options.
func NewDispatcher(opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{parsers: make(map[string]Parser)}
	for _, o := range opts {
		o(d)
	}
	return d
}

// EmitFile parses a single file and emits nodes and edges to the Writer.
// It runs the full pipeline: parse → emit nodes → emit edges → Pass2d.
func (d *Dispatcher) EmitFile(filename string, src []byte) (EmitResult, error) {
	ext := ""
	if idx := strings.LastIndex(filename, "."); idx >= 0 {
		ext = filename[idx:]
	}

	p, ok := d.parsers[ext]
	if !ok {
		return EmitResult{}, fmt.Errorf("no parser registered for extension %q", ext)
	}

	pr, err := p.Parse(filename, src)
	if err != nil {
		return EmitResult{}, err
	}

	nodeCount := 0
	edgeCount := 0

	// Emit package node (one per file)
	if err := d.writer.InsertNode(Node{Kind: "package", Name: filename}); err != nil {
		return EmitResult{}, fmt.Errorf("insert package node: %w", err)
	}
	nodeCount++

	// Emit class nodes
	for _, c := range pr.Classes {
		if err := d.writer.InsertNode(Node{Kind: "class", Name: c.Name}); err != nil {
			return EmitResult{}, fmt.Errorf("insert class node %q: %w", c.Name, err)
		}
		nodeCount++

		// Emit implements edges from class LangMeta
		if c.LangMeta != nil {
			if iface, ok := c.LangMeta["implements"]; ok {
				if err := d.writer.InsertEdge(Edge{Kind: "implements", Source: c.Name, Target: iface}); err != nil {
					return EmitResult{}, fmt.Errorf("insert implements edge: %w", err)
				}
				edgeCount++
			}
		}
	}

	// Emit method nodes and build methodNodeID for Pass2d
	methodNodeID := make(map[string]string)
	for _, m := range pr.Methods {
		key := m.Name
		if m.ClassName != "" {
			key = m.ClassName + "." + m.Name
		}
		if err := d.writer.InsertNode(Node{Kind: "method", Name: key}); err != nil {
			return EmitResult{}, fmt.Errorf("insert method node %q: %w", key, err)
		}
		nodeCount++
		methodNodeID[key] = key
	}

	// Emit import edges
	for _, imp := range pr.Imports {
		if err := d.writer.InsertEdge(Edge{Kind: "imports", Source: filename, Target: imp.Path}); err != nil {
			return EmitResult{}, fmt.Errorf("insert imports edge: %w", err)
		}
		edgeCount++
	}

	// Emit static_calls edges from call sites recorded in methods
	for _, m := range pr.Methods {
		if m.LangMeta == nil {
			continue
		}
		if calls, ok := m.LangMeta["calls"]; ok && calls != "" {
			callerKey := m.Name
			if m.ClassName != "" {
				callerKey = m.ClassName + "." + m.Name
			}
			for _, call := range strings.Split(calls, ",") {
				call = strings.TrimSpace(call)
				if call != "" {
					if err := d.writer.InsertEdge(Edge{Kind: "static_calls", Source: callerKey, Target: call}); err != nil {
						return EmitResult{}, fmt.Errorf("insert static_calls edge: %w", err)
					}
					edgeCount++
				}
			}
		}
	}

	// Pass2d: emit overrides edges for trait impl methods
	overrideEdges, _ := Pass2dOverrides(pr, methodNodeID)
	for _, e := range overrideEdges {
		if err := d.writer.InsertEdge(e); err != nil {
			return EmitResult{}, fmt.Errorf("insert overrides edge: %w", err)
		}
		edgeCount++
	}

	return EmitResult{NodeCount: nodeCount, EdgeCount: edgeCount}, nil
}
