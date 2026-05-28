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

	_, err := p.Parse(filename, src)
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

	return EmitResult{}, nil
}
