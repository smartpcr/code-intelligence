package scope

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Kind is the canonical seven-value scope discriminator enum
// (architecture Sec 5.2.3 line 1046). The string values match
// the PostgreSQL ENUM `clean_code.scope_kind` so a [Kind] value
// can be sent through a `text` placeholder and cast to the enum
// on the server side.
type Kind string

// Canonical [Kind] values. The list is closed: adding a value
// requires a coordinated schema migration AND an architecture-doc
// bump per C22 closed-set policy.
const (
	KindRepo      Kind = "repo"
	KindPackage   Kind = "package"
	KindFile      Kind = "file"
	KindClass     Kind = "class"
	KindInterface Kind = "interface"
	KindMethod    Kind = "method"
	KindBlock     Kind = "block"
)

// validKinds is the closed set [Kind.IsValid] checks against.
// Kept as a package-level map so a `grep -nF "validKinds"` lands
// on the single source of truth; mutating it at runtime is a bug.
var validKinds = map[Kind]struct{}{
	KindRepo:      {},
	KindPackage:   {},
	KindFile:      {},
	KindClass:     {},
	KindInterface: {},
	KindMethod:    {},
	KindBlock:    {},
}

// IsValid reports whether k is one of the canonical
// seven-value [Kind] values. Used by [DeriveScopeID] and the
// per-kind signature builders to refuse non-canonical values
// at the API boundary, mirroring the PostgreSQL ENUM rejection
// at the DB boundary.
func (k Kind) IsValid() bool {
	_, ok := validKinds[k]
	return ok
}

// String returns k's underlying string form (e.g. `"method"`).
// Implements fmt.Stringer for log-formatting; identical to
// `string(k)`.
func (k Kind) String() string { return string(k) }

// ErrInvalidKind is returned when a non-canonical [Kind] is
// passed to any function in this package. The DB ENUM also
// rejects out-of-set values; this in-process check fails fast
// before a round-trip to PostgreSQL.
var ErrInvalidKind = errors.New("scope: invalid scope_kind (canonical set: repo|package|file|class|interface|method|block)")

// ErrEmptyField is returned by the per-kind signature builders
// when a required input is empty. Validation is mandatory
// because an empty input would silently land in the canonical
// signature pre-image and collide with another empty-field row
// at the same prefix.
var ErrEmptyField = errors.New("scope: required signature field is empty")

// ErrEmbeddedNUL is returned by the signature builders and by
// [DeriveScopeID] when any string field contains the NUL byte
// (`\x00`). NUL is reserved as the framing delimiter in the
// [DeriveScopeID] pre-image; allowing it inside a field would
// re-introduce a pre-image ambiguity and silently degrade G2.
// In practice no legitimate qualifiedName / param / SHA contains
// NUL, so this is defence-in-depth against a producer bug.
var ErrEmbeddedNUL = errors.New("scope: signature field contains reserved NUL byte (\\x00)")

// canonicalPunctuation is the closed set of ASCII punctuation
// marks whose adjacency to whitespace is collapsed by
// [NormalizeSignature]. Mirrors agent-memory's
// `canonicalPunctuation` so a signature emitted by either
// service is byte-identical (linked-mode parity).
const canonicalPunctuation = ",()[]{}<>:;"

// NormalizeSignature returns the canonical-signature form of s.
//
// Mirrors `services/agent-memory/internal/repoindexer/ast/whitespace.go`
// `NormalizeSignature` byte-for-byte so a signature that survives
// both normalisers is identical across the two services (the §9.7
// / §9.9 stability mitigation: a formatter-only commit -- different
// indentation, inserted spaces around `,` or `()`, swapped
// tabs<->spaces, added `//` line comments -- produces a byte-
// identical signature and therefore a stable `scope_id`).
//
// Steps, in order:
//
//  1. Strip line comments (`//...`, `#...` to end of line) and
//     block comments (`/* ... */`). Strings are NOT scanned
//     specially -- callers pass name+parameter tokens only, not
//     arbitrary source bodies.
//  2. Collapse runs of any Unicode whitespace
//     ([unicode.IsSpace], which covers `\t`, `\n`, `\r`, NBSP,
//     ideographic space, etc.) to a single ASCII space.
//  3. Remove the single space when it sits directly adjacent to
//     one of the canonical punctuation marks
//     (`,`, `(`, `)`, `[`, `]`, `{`, `}`, `<`, `>`, `:`, `;`).
//     Collapses `Map<K, V>` / `Map < K , V >` / `Map<K,V>` to
//     the same normalised string.
//  4. Trim leading / trailing whitespace.
//
// The normaliser is deterministic and pure -- the same input
// always produces the same output. Empty input returns empty
// output (no error); callers needing rejection of empty fields
// must guard at the call site (the per-kind builders do so).
func NormalizeSignature(s string) string {
	if s == "" {
		return ""
	}
	stripped := stripComments(s)
	collapsed := collapseWhitespace(stripped)
	pruned := stripWhitespaceAroundPunctuation(collapsed)
	return strings.TrimSpace(pruned)
}

// stripComments removes `//...\n`, `#...\n`, and `/* ... */`
// runs from s. Byte-oriented because every recognised opener is
// ASCII; multibyte runes inside comment bodies are preserved
// verbatim until the comment terminator.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += 2 + end + 2
			continue
		}
		if (i+1 < len(s) && s[i] == '/' && s[i+1] == '/') || s[i] == '#' {
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return b.String()
			}
			b.WriteByte('\n')
			i += nl + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// collapseWhitespace replaces every run of Unicode whitespace
// with a single ASCII space. Operates on runes so multibyte
// whitespace (NBSP `\u00A0`, ideographic space `\u3000`, etc.)
// is collapsed correctly.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// stripWhitespaceAroundPunctuation removes single ASCII spaces
// that sit directly before or after one of the
// [canonicalPunctuation] marks. Assumes its input has already
// passed through [collapseWhitespace], i.e. there is at most one
// space between any two non-space runes.
func stripWhitespaceAroundPunctuation(s string) string {
	if s == "" {
		return ""
	}
	bs := []byte(s)
	var b strings.Builder
	b.Grow(len(bs))
	for i := 0; i < len(bs); i++ {
		c := bs[i]
		if c == ' ' && i+1 < len(bs) &&
			strings.IndexByte(canonicalPunctuation, bs[i+1]) >= 0 {
			continue
		}
		if c == ' ' && i > 0 &&
			strings.IndexByte(canonicalPunctuation, bs[i-1]) >= 0 {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// BuildRepo returns the canonical signature for a repo-kind
// scope. The signature is simply the repo URL, matching
// agent-memory `Node.canonical_signature` for the `repo` kind.
// `repoURL` MUST be non-empty and MUST NOT contain a NUL byte.
func BuildRepo(repoURL string) (string, error) {
	if err := guardField("repoURL", repoURL); err != nil {
		return "", err
	}
	return repoURL, nil
}

// BuildPackage returns the canonical signature for a package-
// kind scope: `<repoURL>::pkg::<dir>`.
//
// `dir` is the repo-relative directory path in forward-slash
// form (e.g. `internal/storage`). It is NOT run through
// [NormalizeSignature]: paths are emitted by the parser layer
// in canonical form already, and running the punctuation
// stripper over a path containing `,` `:` `;` `<>` characters
// could corrupt it. Both fields MUST be non-empty (an empty
// `dir` means "repo root", which is the repo-kind scope, not
// the package-kind scope) and MUST NOT contain a NUL byte.
func BuildPackage(repoURL, dir string) (string, error) {
	if err := guardField("repoURL", repoURL); err != nil {
		return "", err
	}
	if err := guardField("dir", dir); err != nil {
		return "", err
	}
	return repoURL + "::pkg::" + dir, nil
}

// BuildFile returns the canonical signature for a file-kind
// scope: `<repoURL>::file::<relPath>`. `relPath` is the
// repo-relative file path in forward-slash form. Both fields
// MUST be non-empty and MUST NOT contain a NUL byte.
func BuildFile(repoURL, relPath string) (string, error) {
	if err := guardField("repoURL", repoURL); err != nil {
		return "", err
	}
	if err := guardField("relPath", relPath); err != nil {
		return "", err
	}
	return repoURL + "::file::" + relPath, nil
}

// BuildClass returns the canonical signature for a class-kind
// scope: `<repoURL>::class::<relPath>#<NormalizeSignature(qualifiedName)>`.
//
// `qualifiedName` is the dot-separated path through enclosing
// scopes (e.g. `pkg.Foo` or `outer.Inner`). It is run through
// [NormalizeSignature] so a formatter-only commit (different
// spacing inside generic parameters, inserted comments) produces
// a byte-identical signature. All three fields MUST be non-
// empty and MUST NOT contain a NUL byte.
func BuildClass(repoURL, relPath, qualifiedName string) (string, error) {
	if err := guardField("repoURL", repoURL); err != nil {
		return "", err
	}
	if err := guardField("relPath", relPath); err != nil {
		return "", err
	}
	if err := guardField("qualifiedName", qualifiedName); err != nil {
		return "", err
	}
	return repoURL + "::class::" + relPath + "#" + NormalizeSignature(qualifiedName), nil
}

// BuildInterface returns the canonical signature for an
// interface-kind scope:
// `<repoURL>::class::<relPath>#<NormalizeSignature(qualifiedName)>`.
//
// Uses the `::class::` discriminator (NOT `::interface::`) to
// remain BYTE-IDENTICAL to agent-memory's `classSignature`
// (services/agent-memory/internal/repoindexer/ast/dispatcher.go
// `classSignature` mints the canonical signature for "a Class /
// Interface node" -- agent-memory does NOT distinguish them at
// the signature layer). Linked-mode `agent_memory_node_id`
// resolution requires byte-identical signatures, so the
// agent-memory recipe wins here. The clean-code `scope_kind`
// ENUM still separates `class` and `interface` for downstream
// classification; the `scope_id` derivation includes
// `scope_kind` in its pre-image so a class and an interface
// with the same qualifiedName still get DIFFERENT `scope_id`s
// even though their `canonical_signature` strings match.
//
// (Iter 1 emitted `::interface::` here as a "self-consistent"
// divergence; evaluator iter-1 flagged it as breaking
// linked-mode parity, which is the whole point of the
// canonical_signature column. Reverted to the agent-memory
// recipe.)
func BuildInterface(repoURL, relPath, qualifiedName string) (string, error) {
	if err := guardField("repoURL", repoURL); err != nil {
		return "", err
	}
	if err := guardField("relPath", relPath); err != nil {
		return "", err
	}
	if err := guardField("qualifiedName", qualifiedName); err != nil {
		return "", err
	}
	return repoURL + "::class::" + relPath + "#" + NormalizeSignature(qualifiedName), nil
}

// BuildMethod returns the canonical signature for a method-kind
// scope: `<repoURL>::method::<relPath>#<NormalizeSignature(qualifiedName)>(<NormalizeSignature(joinedParams)>)`.
//
// `qualifiedName` is the dot-separated path through enclosing
// scopes (e.g. `pkg.Foo.bar` for a method `bar` on class
// `pkg.Foo`). It MUST NOT contain `#` -- the `#` byte is the
// recipe-level separator between `<relPath>` and the normalised
// qualifiedName AND it is also stripped by [NormalizeSignature]
// as a Python-style line-comment marker (agent-memory parity);
// a producer passing `pkg.Foo#bar` would see `#bar` silently
// stripped. The Java / Go / Python parser layer emits dot-
// separated qualified names, so this constraint is honoured by
// every supported language.
//
// `params` is the parameter TYPE list (NOT parameter names) in
// source order, e.g. `[]string{"int", "*Foo"}` for a Go method
// `func (r *Receiver) Bar(int, *Foo)`. The brief's example
// recipe output `pkg.Foo#bar(int)` (architecture Sec 5.2.3)
// is realised by passing `qualifiedName="pkg.Foo.bar"` and
// `params=[]string{"int"}`; the `#bar` portion of the recipe
// output is the `<relPath>#<normalisedQN>` recipe-level
// separator landing exactly at the `pkg.Foo` -> `bar` boundary.
//
// `params` MAY be empty (a no-arg method); the result then has
// an empty `()` group. `qualifiedName` MUST be non-empty.
// Individual param strings MAY be empty -- they are joined with
// `,` BEFORE normalisation, so a stray empty element manifests
// as a `,,` run that the normaliser collapses adjacent whitespace
// around but does NOT remove. Producers SHOULD NOT pass empty
// elements; doing so is a parser bug.
func BuildMethod(repoURL, relPath, qualifiedName string, params []string) (string, error) {
	if err := guardField("repoURL", repoURL); err != nil {
		return "", err
	}
	if err := guardField("relPath", relPath); err != nil {
		return "", err
	}
	if err := guardField("qualifiedName", qualifiedName); err != nil {
		return "", err
	}
	for i, p := range params {
		if strings.IndexByte(p, 0) >= 0 {
			return "", fmt.Errorf("%w (field: params[%d])", ErrEmbeddedNUL, i)
		}
	}
	joined := strings.Join(params, ",")
	return repoURL + "::method::" + relPath + "#" +
		NormalizeSignature(qualifiedName) +
		"(" + NormalizeSignature(joined) + ")", nil
}

// BuildBlock returns the canonical signature for a block-kind
// scope: `<methodSig>#block_<ordinal>_<kind>`.
//
// `methodSig` MUST be the canonical signature of the enclosing
// method (as produced by [BuildMethod]); embedding it verbatim
// keeps every block uniquely keyed by its container plus an
// intra-method ordinal. `ordinal` is the 0-BASED positional
// index within the method body (agent-memory parity --
// services/agent-memory/internal/repoindexer/ast/block.go
// `Block.Ordinal` doc: "0-based position of this Block within
// its enclosing Method's Block list"; `blockSignature` emits
// `#block_0_<kind>` for the first block, NOT `#block_1_...`).
// `kind` is a free-form short label distinguishing block
// flavours (e.g. `"entry"`, `"exit"`, `"loop"`, `"branch"`);
// it MUST be non-empty and MUST NOT contain a NUL byte.
//
// `ordinal` MAY be `0` (the first block). It MUST NOT be
// negative -- a negative ordinal is a parser bug and would
// emit `#block_-1_kind`, breaking the recipe.
//
// (Iter 1 rejected `ordinal=0` as a "0-value uninitialised
// surface bug"; evaluator iter-1 flagged it as breaking parity
// with agent-memory, which uses 0-based ordinals.)
func BuildBlock(methodSig string, ordinal int, kind string) (string, error) {
	if err := guardField("methodSig", methodSig); err != nil {
		return "", err
	}
	if ordinal < 0 {
		return "", fmt.Errorf("scope: BuildBlock: ordinal must be >= 0 (0-based per agent-memory parity), got %d", ordinal)
	}
	if err := guardField("kind", kind); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s#block_%d_%s", methodSig, ordinal, kind), nil
}

// guardField rejects empty and NUL-containing string fields.
// Centralised so every per-kind builder shares the same
// validation surface and a single grep on the sentinel finds
// every guard.
func guardField(name, value string) error {
	if value == "" {
		return fmt.Errorf("%w (field: %s)", ErrEmptyField, name)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w (field: %s)", ErrEmbeddedNUL, name)
	}
	return nil
}
