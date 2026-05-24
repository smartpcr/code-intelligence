package steward

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// scopeGlobMatches reports whether `signature` matches the
// glob `pattern` per the Stage 5.3 candidate-scope read
// semantics (architecture Sec 5.3.6 + tech-spec Sec 10A):
//
//   - `*` matches zero or more of ANY character (including
//     `.`, `/`, and whitespace). Unlike `path.Match`, there
//     is no implicit segment separator -- scope signatures
//     use `.` (java package) or `/` (path) freely and a
//     single `*` MUST be able to cross both.
//   - `?` matches exactly one character.
//   - All other characters are literals, INCLUDING regex
//     metacharacters such as `(`, `)`, `[`, `]`, `$`, `+`,
//     `.`, `|`, `^`, `\`, `{`, `}`. They are quoted before
//     the resulting regex is compiled.
//
// The match is full-string anchored (^...$) and case-
// sensitive. An empty `pattern` matches only the empty
// `signature`; the steward validator forbids empty globs so
// this case is defensive (empty patterns never reach this
// function via the verb).
//
// Returns a non-nil error only when the pattern cannot be
// compiled (defensive -- the translator below always emits
// valid regex syntax). Callers SHOULD propagate the error
// (don't fail-open the gate) -- a malformed pattern is a
// configuration bug worth surfacing.
//
// The compiled regex is cached because every evaluator gate
// call hits this path -- a single rule's overrides can be
// re-scanned hundreds of times per minute and regex
// compilation is hot. Cache keys are the raw glob strings;
// `sync.Map` is fine because the keyspace is bounded by the
// distinct globs registered by operators (rarely more than a
// few hundred per deployment).
func scopeGlobMatches(pattern, signature string) (bool, error) {
	re, err := scopeGlobCompile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(signature), nil
}

// scopeGlobToRegex translates a scope-filter glob to a Go
// regex with full-string anchors. Lower-case so it stays
// package-private; the test file in the same package
// verifies the translation character-by-character.
//
// Translation rules:
//
//   - `*` -> `.*` (zero or more of any character)
//   - `?` -> `.` (exactly one of any character)
//   - every other character -> regexp.QuoteMeta(char)
//
// We do NOT support character classes (`[abc]`), curly
// alternations (`{a,b}`), or any other path/filepath glob
// extension -- the architecture-pinned glob vocabulary is
// `*` and `?` only.
func scopeGlobToRegex(pattern string) string {
	var b strings.Builder
	b.Grow(len(pattern) + 2)
	b.WriteByte('^')
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	return b.String()
}

var (
	scopeGlobCache   sync.Map // map[string]*regexp.Regexp
	scopeGlobMisses  sync.Map // map[string]error -- negative cache for malformed patterns (defensive)
	scopeGlobCacheMu sync.Mutex
)

// scopeGlobCompile returns the cached or freshly-compiled
// regex for `pattern`. A double-checked locking pattern
// guards against two concurrent callers both compiling the
// same pattern; the lock is only taken on cache miss so the
// hot path is lock-free.
func scopeGlobCompile(pattern string) (*regexp.Regexp, error) {
	if cached, ok := scopeGlobCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	if cachedErr, ok := scopeGlobMisses.Load(pattern); ok {
		return nil, cachedErr.(error)
	}
	scopeGlobCacheMu.Lock()
	defer scopeGlobCacheMu.Unlock()
	if cached, ok := scopeGlobCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	if cachedErr, ok := scopeGlobMisses.Load(pattern); ok {
		return nil, cachedErr.(error)
	}
	re, err := regexp.Compile(scopeGlobToRegex(pattern))
	if err != nil {
		wrapped := fmt.Errorf("steward: scopeGlobMatches: compile %q: %w", pattern, err)
		scopeGlobMisses.Store(pattern, wrapped)
		return nil, wrapped
	}
	scopeGlobCache.Store(pattern, re)
	return re, nil
}
