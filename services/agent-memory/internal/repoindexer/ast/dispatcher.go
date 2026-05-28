package ast

import "fmt"

// Dispatcher maintains a registry of language parsers keyed by file
// extension and routes incoming files to the correct parser.
type Dispatcher struct {
	byExt map[string]LanguageParser
}

// NewDispatcher creates an empty Dispatcher ready to accept parser
// registrations.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{byExt: make(map[string]LanguageParser)}
}

// Register adds a parser for every extension it declares.  Returns an
// error if any extension is already claimed by another parser.
func (d *Dispatcher) Register(p LanguageParser) error {
	for _, ext := range p.Extensions() {
		if existing, ok := d.byExt[ext]; ok {
			return fmt.Errorf("extension %q already registered by %s", ext, existing.Language())
		}
		d.byExt[ext] = p
	}
	return nil
}

// Dispatch returns the parser registered for the given file extension,
// or (nil, false) if no parser handles that extension.
func (d *Dispatcher) Dispatch(ext string) (LanguageParser, bool) {
	p, ok := d.byExt[ext]
	return p, ok
}
