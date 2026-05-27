package parser

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Factory builds a fresh `Parser` instance. Per-language
// implementations may be stateless (`Factory` returns a shared
// singleton) or stateful (`Factory` returns a new instance per
// call); the registry MUST NOT assume one shape over the other.
type Factory func() Parser

// Registry maps language tags to parser factories. The registry
// enforces the v1 language pin (tech-spec Sec 8.6 lines
// 1005-1016) at registration time: any attempt to register a
// factory for a language outside `SupportedLanguages` returns
// `ErrUnsupportedLanguage`.
//
// Registries are safe for concurrent use; the factory map is
// guarded by an RWMutex so per-request `For` calls do not
// serialise.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty Registry. The default registry
// returned by `DefaultRegistry()` is pre-populated with the
// four v1-pinned languages; tests can build a fresh empty
// registry to exercise the v1 pin guard.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register installs `factory` under `language`. The registry
// refuses to register a language outside the v1 pin
// (`ErrUnsupportedLanguage`). It also rejects nil factories so
// a typo at wiring time surfaces immediately rather than at
// the next `Parse` call.
//
// Re-registering an already-registered language is treated as
// an error so a duplicate `Register` in two `init`s reads as a
// loud build-time failure rather than a silent last-writer-wins.
func (r *Registry) Register(language string, factory Factory) error {
	if !isSupportedLanguage(language) {
		return fmt.Errorf("%w: %q", ErrUnsupportedLanguage, language)
	}
	if factory == nil {
		return fmt.Errorf("clean-code/parser: nil factory for language %q", language)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[language]; exists {
		return fmt.Errorf("clean-code/parser: language %q already registered", language)
	}
	r.factories[language] = factory
	return nil
}

// For returns a parser instance for `language` or
// `ErrUnsupportedLanguage` if the language is not registered.
// The instance is whatever the registered factory returns; the
// caller takes ownership.
func (r *Registry) For(language string) (Parser, error) {
	if !isSupportedLanguage(language) {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedLanguage, language)
	}
	r.mu.RLock()
	factory, ok := r.factories[language]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: language %q is in the v1 pin but no factory has been registered", ErrUnsupportedLanguage, language)
	}
	return factory(), nil
}

// Languages returns the registered language tags in sorted
// order. Used by `/healthz` to surface the active parser fleet.
func (r *Registry) Languages() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for lang := range r.factories {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// Parse routes `(path, content)` through `DetectLanguage` and
// dispatches to the matching parser. Convenience wrapper for
// callers that have a file path + bytes but no pre-resolved
// language tag (the Metric Ingestor's typical shape).
func (r *Registry) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	lang, ok := DetectLanguage(path, content)
	if !ok {
		return nil, fmt.Errorf("%w: cannot detect language for path %q", ErrUnsupportedLanguage, path)
	}
	p, err := r.For(lang)
	if err != nil {
		return nil, err
	}
	return p.Parse(ctx, path, content)
}

// defaultRegistry is the process-wide registry populated by the
// per-language `init()` functions. Tests that need an isolated
// registry MUST build one via `NewRegistry()`; mutating the
// default at test time risks cross-test interference.
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry that the
// per-language adapters wire themselves into via `init()`. The
// instance is shared; callers MUST treat the returned pointer
// as read-mostly (Register is allowed at boot but discouraged
// at request time).
func DefaultRegistry() *Registry { return defaultRegistry }

// registerInDefault is a small helper invoked from per-language
// `init()` functions. The helper panics on failure -- a
// per-language init that cannot register itself is a programmer
// error (wrong language tag, double-init) that MUST not be
// papered over at runtime.
func registerInDefault(language string, factory Factory) {
	if err := defaultRegistry.Register(language, factory); err != nil {
		panic(fmt.Sprintf("clean-code/parser: registerInDefault(%q): %v", language, err))
	}
}
