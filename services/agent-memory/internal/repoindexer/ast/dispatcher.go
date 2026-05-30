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

// WithParsers registers multiple parsers in declaration order.
// Later parsers override earlier ones for overlapping extensions (last-wins).
func WithParsers(parsers ...Parser) DispatcherOption {
	return func(d *Dispatcher) {
		for _, p := range parsers {
			for _, ext := range p.Extensions() {
				d.parsers[ext] = p
			}
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

// SelectParser returns the parser registered for the given file's extension.
// The hints parameter is reserved for future disambiguation (e.g. language
// preference when multiple parsers could claim an extension); it is accepted
// but currently unused — extension-based registration is authoritative.
func (d *Dispatcher) SelectParser(filename string, hints []string) Parser {
	ext := ""
	if idx := strings.LastIndex(filename, "."); idx >= 0 {
		ext = filename[idx:]
	}
	p, _ := d.parsers[ext]
	return p
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

	// Run emitter pass (Pass 2b multimap resolution).
	em := NewEmitter(result)
	edges, callsRaw := em.Emit()

	// Write nodes.
	nodeCount := 0
	for _, c := range result.Classes {
		if d.writer != nil {
			_ = d.writer.InsertNode(Node{Kind: "class", Name: c.Name})
			nodeCount++
		}
	}
	for _, m := range result.Methods {
		if d.writer != nil {
			_ = d.writer.InsertNode(Node{Kind: "method", Name: m.ClassName + "." + m.Name})
			nodeCount++
		}
	}

	// Write edges.
	edgeCount := 0
	for _, e := range edges {
		if d.writer != nil {
			_ = d.writer.InsertEdge(e)
			edgeCount++
		}
	}

	return EmitResult{NodeCount: nodeCount, EdgeCount: edgeCount, CallsRaw: callsRaw}, nil
}
