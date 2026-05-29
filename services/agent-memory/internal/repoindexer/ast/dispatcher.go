package ast

import "path/filepath"

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

// EmitFile parses a single file and emits nodes/edges to the writer.
// When no parser is registered for the file's extension the dispatcher
// logs an ast.dispatch.skip event with reason "no_parser" and returns
// a zero EmitResult with nil error.
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

	_ = result // stub: real dispatcher would write nodes/edges
	return EmitResult{}, nil
}
