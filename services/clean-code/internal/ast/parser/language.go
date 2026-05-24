package parser

import (
	"bytes"
	"path/filepath"
	"strings"
)

// Canonical v1 language tags. These four strings are the
// authoritative `AstFile.language` values; the registry
// rejects any other value at registration AND lookup time.
const (
	LanguageGo         = "go"
	LanguagePython     = "python"
	LanguageTypeScript = "typescript"
	LanguageJava       = "java"
)

// SupportedLanguages enumerates the v1-pinned languages in the
// canonical order documented in tech-spec Sec 8.6 lines
// 1005-1016. The slice is what `Registry.Languages()` returns
// and what consumers iterate to enumerate the pin.
//
// Adding a language to this slice is intentionally a code
// change (not config) so a v2 language audit reads as a single
// `grep -F SupportedLanguages` hit.
var SupportedLanguages = []string{
	LanguageGo,
	LanguagePython,
	LanguageTypeScript,
	LanguageJava,
}

// isSupportedLanguage is the registry guard predicate.
// Centralising the membership test keeps the v1 pin enforced
// in exactly one place.
func isSupportedLanguage(lang string) bool {
	switch lang {
	case LanguageGo, LanguagePython, LanguageTypeScript, LanguageJava:
		return true
	default:
		return false
	}
}

// extensionLanguage is the file-extension to language map used
// by `Registry.Detect`. Keys are stored without the leading dot
// and lower-cased; values are canonical `Language*` constants.
//
// `.tsx`/`.jsx`/`.mts`/`.cts`/`.mjs`/`.cjs` are mapped to
// `typescript` even though `.jsx`/`.mjs`/`.cjs` are syntactically
// JavaScript -- the v1 TypeScript adapter parses both because
// TypeScript is a superset and the org's top-4 language pin
// rolls them up as one (tech-spec Sec 8.6).
var extensionLanguage = map[string]string{
	"go":   LanguageGo,
	"py":   LanguagePython,
	"pyi":  LanguagePython,
	"ts":   LanguageTypeScript,
	"tsx":  LanguageTypeScript,
	"mts":  LanguageTypeScript,
	"cts":  LanguageTypeScript,
	"js":   LanguageTypeScript,
	"jsx":  LanguageTypeScript,
	"mjs":  LanguageTypeScript,
	"cjs":  LanguageTypeScript,
	"java": LanguageJava,
}

// shebangLanguage covers extensionless scripts. The keys are
// the executable name as it appears in `#!/usr/bin/env <name>`
// or `#!/path/to/<name>`; values are canonical `Language*`
// constants. The set is intentionally small -- only the v1-pinned
// languages -- so a `.sh` script does not silently match the
// Python parser.
var shebangLanguage = map[string]string{
	"python":  LanguagePython,
	"python2": LanguagePython,
	"python3": LanguagePython,
}

// DetectLanguage returns the canonical language tag for `path`
// based on file extension first and a Linguist-style content
// sniff (currently just the shebang line) second.
// Empty string + `ErrUnsupportedLanguage`-style is signalled by
// the second return value being `false`.
func DetectLanguage(path string, content []byte) (string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext != "" {
		if lang, ok := extensionLanguage[ext]; ok {
			return lang, true
		}
	}
	if lang, ok := sniffShebang(content); ok {
		return lang, true
	}
	return "", false
}

// sniffShebang inspects the first line of `content` for a
// `#!/usr/bin/env <interpreter>` or `#!/path/to/<interpreter>`
// shebang and returns the matching language. Returns
// (`""`, false) when no shebang is present or the interpreter
// is outside the v1 pin.
func sniffShebang(content []byte) (string, bool) {
	if len(content) < 2 || content[0] != '#' || content[1] != '!' {
		return "", false
	}
	// First line, less the `#!`.
	nl := bytes.IndexByte(content, '\n')
	var line []byte
	if nl < 0 {
		line = content[2:]
	} else {
		line = content[2:nl]
	}
	// Trim trailing CR (Windows line endings) and whitespace.
	line = bytes.TrimRight(line, "\r\t ")
	parts := strings.Fields(string(line))
	if len(parts) == 0 {
		return "", false
	}
	interpreter := parts[0]
	// `/usr/bin/env <name>` form -- the interpreter is the
	// second token.
	if strings.HasSuffix(interpreter, "/env") && len(parts) >= 2 {
		interpreter = parts[1]
	}
	// Take the basename so `/usr/bin/python3` -> `python3`.
	interpreter = filepath.Base(interpreter)
	if lang, ok := shebangLanguage[interpreter]; ok {
		return lang, true
	}
	return "", false
}
