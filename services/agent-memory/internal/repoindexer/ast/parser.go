package ast

import (
	"path/filepath"
	"sync"
)

// Parser can parse source files of a particular language.
type Parser interface {
	Language() string
	Extensions() []string
	Parse(filename string, src []byte) (*ParseResult, error)
}

// ParseResult holds the output of parsing a single source file.
type ParseResult struct {
	Language string
}

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