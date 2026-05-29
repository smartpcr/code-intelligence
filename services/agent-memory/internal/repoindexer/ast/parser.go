package ast

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ErrParserUnavailable is the sentinel error a parser returns when its
// backing runtime (e.g. tree-sitter CGO binding, pwsh subprocess) is
// not available in the current build or environment.
var ErrParserUnavailable = errors.New("parser unavailable")

// UnavailableError wraps ErrParserUnavailable with a parser-specific reason.
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string         { return e.Reason + ": parser unavailable" }
func (e *UnavailableError) Is(target error) bool   { return target == ErrParserUnavailable }

// Parser is implemented by each language-specific parser.
type Parser interface {
	Language() string
	Extensions() []string
	Parse(filename string, src []byte) (ParseResult, error)
}

// ParseResult is the output of a single-file parse pass.
type ParseResult struct {
	Classes []ClassDecl
	Methods []MethodDecl
	Imports []Import
}

// ClassDecl represents a class, struct, interface, trait, or enum.
type ClassDecl struct {
	Name     string
	LangMeta map[string]string
}

// MethodDecl represents a method or free function.
type MethodDecl struct {
	Name      string
	ClassName string
	LangMeta  map[string]string
}

// Import represents an import, include, use, or using directive.
type Import struct {
	Path     string
	LangMeta map[string]string
}

// Edge represents a directed relationship between two nodes.
type Edge struct {
	Kind   string
	Source string
	Target string
}

// Node represents an AST-derived graph node.
type Node struct {
	Kind string
	Name string
}

// EmitResult summarises the output of an EmitFile call.
type EmitResult struct {
	NodeCount int
	EdgeCount int
}

// Writer receives nodes and edges produced by the emitter.
type Writer interface {
	InsertNode(n Node) error
	InsertEdge(e Edge) error
}

// Logger receives structured log events from the dispatcher.
type Logger interface {
	Log(event string, fields map[string]string)
}

// ---------------------------------------------------------------------------
// Dispatcher — routes files to parsers by extension
// ---------------------------------------------------------------------------

// Dispatcher routes source files to the appropriate parser.
type Dispatcher struct {
	parsers map[string]Parser
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

// EmitFile parses a single file and emits nodes/edges to the writer.
func (d *Dispatcher) EmitFile(filename string, src []byte) (EmitResult, error) {
	ext := ""
	if idx := strings.LastIndex(filename, "."); idx >= 0 {
		ext = filename[idx:]
	}

	p, ok := d.parsers[ext]
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
		if errors.Is(err, ErrParserUnavailable) {
			reason := "unavailable"
			var ue *UnavailableError
			if errors.As(err, &ue) {
				reason = ue.Reason
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

	nodeCount := 0
	edgeCount := 0
	if d.writer != nil {
		for _, c := range result.Classes {
			if wErr := d.writer.InsertNode(Node{Kind: "class", Name: c.Name}); wErr != nil {
				return EmitResult{}, fmt.Errorf("InsertNode: %w", wErr)
			}
			nodeCount++
		}
		for _, m := range result.Methods {
			if wErr := d.writer.InsertNode(Node{Kind: "method", Name: m.Name}); wErr != nil {
				return EmitResult{}, fmt.Errorf("InsertNode: %w", wErr)
			}
			nodeCount++
		}
	}

	return EmitResult{NodeCount: nodeCount, EdgeCount: edgeCount}, nil
}

// ---------------------------------------------------------------------------
// Global registry
// ---------------------------------------------------------------------------

var (
	mu     sync.RWMutex
	extMap = map[string]Parser{}
)

// RegisterParser adds a parser for each of its declared extensions.
func RegisterParser(p Parser) {
	mu.Lock()
	defer mu.Unlock()
	for _, ext := range p.Extensions() {
		extMap[ext] = p
	}
}

// SelectParser returns the registered parser for the given filename's
// extension, or nil if no parser is registered for that extension.
func SelectParser(filename string, src []byte) Parser {
	ext := filepath.Ext(filename)
	mu.RLock()
	defer mu.RUnlock()
	return extMap[ext]
}

// DefaultParsers returns the build-appropriate parser list.
func DefaultParsers() []Parser {
	return defaultParsers()
}

// NewDefaultDispatcher creates a Dispatcher populated with the given
// parser list plus the provided writer and logger.
func NewDefaultDispatcher(parsers []Parser, w Writer, l Logger) *Dispatcher {
	opts := []DispatcherOption{
		WithWriter(w),
		WithLogger(l),
	}
	for _, p := range parsers {
		opts = append(opts, WithParser(p))
	}
	return NewDispatcher(opts...)
}
