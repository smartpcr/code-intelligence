package ast

import (
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TestNormalizeSignature_StableAcrossFormatters pins the
// acceptance scenario "whitespace normalisation -- Given the
// same method reformatted only by adding spaces, When the
// canonical signature is computed, Then the resulting
// fingerprint matches the unformatted version's fingerprint
// exactly." It is the §9.7 / §9.9 mitigation in action.
func TestNormalizeSignature_StableAcrossFormatters(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		variant string
	}{
		{
			name:    "extra spaces around punctuation",
			raw:     "Map<K,V>",
			variant: "Map < K , V >",
		},
		{
			name:    "tabs and newlines collapse to single space",
			raw:     "Foo.bar(int,string)",
			variant: "Foo.bar(int,\tstring)",
		},
		{
			name:    "line comments are stripped",
			raw:     "Foo.bar(int,string)",
			variant: "Foo.bar(int, // a comment\nstring)",
		},
		{
			name:    "block comments are stripped",
			raw:     "Foo.bar(int,string)",
			variant: "Foo.bar(int, /* block */ string)",
		},
		{
			name:    "mixed: comments + extra whitespace",
			raw:     "ns.Foo.bar(int,Map<K,V>,string)",
			variant: "ns.Foo.bar(  int ,  Map < K , V > ,  // trailing\n  string )",
		},
		{
			name:    "python-style hash comment is stripped",
			raw:     "Foo.bar(a,b)",
			variant: "Foo.bar(a, # comment\nb)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normRaw := NormalizeSignature(tc.raw)
			normVariant := NormalizeSignature(tc.variant)
			if normRaw != normVariant {
				t.Fatalf("normalisation diverges:\n  raw     = %q\n  variant = %q\n  normRaw     = %q\n  normVariant = %q",
					tc.raw, tc.variant, normRaw, normVariant)
			}
		})
	}
}

// TestNormalizeSignature_FingerprintsIdentical proves the
// scenario end-to-end through the actual NodeFingerprint helper
// that production callers use. A reformatted method MUST land
// on a byte-identical fingerprint or §9.7 / §9.9 mitigation is
// broken.
func TestNormalizeSignature_FingerprintsIdentical(t *testing.T) {
	repoID := fingerprint.MustParseRepoID("11111111-2222-3333-4444-555555555555")
	repoURL := "https://example.com/acme/svc"
	relPath := "src/foo.ts"
	qual := "Foo.bar"
	paramsTight := "int,Map<K,V>,string"
	paramsLoose := "  int ,  Map < K , V > ,  // c\n  string "

	sigTight := methodSignature(repoURL, relPath, qual, paramsTight)
	sigLoose := methodSignature(repoURL, relPath, qual, paramsLoose)
	if sigTight != sigLoose {
		t.Fatalf("methodSignature diverges:\n  tight = %q\n  loose = %q", sigTight, sigLoose)
	}

	fpTight, err := fingerprint.NodeFingerprint(repoID, "method", sigTight, "abc123")
	if err != nil {
		t.Fatalf("fingerprint tight: %v", err)
	}
	fpLoose, err := fingerprint.NodeFingerprint(repoID, "method", sigLoose, "abc123")
	if err != nil {
		t.Fatalf("fingerprint loose: %v", err)
	}
	if fpTight != fpLoose {
		t.Fatalf("fingerprint divergence:\n  tight = %s\n  loose = %s",
			fpTight.Hex(), fpLoose.Hex())
	}
}

// TestNormalizeSignature_RejectsCollisionForDistinctSignatures
// is the negative control: signatures that ARE semantically
// different MUST normalise to different strings. Without this
// the normaliser would silently merge `bar(int)` and `bar()`.
func TestNormalizeSignature_RejectsCollisionForDistinctSignatures(t *testing.T) {
	cases := []struct{ a, b string }{
		{"Foo.bar()", "Foo.baz()"},
		{"Foo.bar(int)", "Foo.bar(string)"},
		{"Foo.bar(int)", "Foo.bar(int,int)"},
		{"Foo<K>", "Foo<V>"},
	}
	for _, tc := range cases {
		a := NormalizeSignature(tc.a)
		b := NormalizeSignature(tc.b)
		if a == b {
			t.Fatalf("distinct signatures collapsed to same normalised form: %q == %q", tc.a, tc.b)
		}
	}
}

// TestCountLogicalLines_IgnoresBlankAndComments verifies the
// counter the block-subdivision threshold consumes does not
// drift on formatter-only edits.
func TestCountLogicalLines_IgnoresBlankAndComments(t *testing.T) {
	body := strings.Join([]string{
		"const x = 1;",
		"",
		"// a line comment",
		"  ",
		"/* a block ",
		"   comment */",
		"const y = 2;",
		"# python-style comment",
		"const z = 3;",
	}, "\n")
	got := CountLogicalLines(body)
	if got != 3 {
		t.Fatalf("CountLogicalLines = %d; want 3", got)
	}
}

// methodSignature builds the architecture-pinned method canonical
// signature (`<repoURL>::method::<relPath>#<QualifiedName>(<params>)`)
// using `NormalizeSignature` on the params so the produced string
// is stable across whitespace-only / comment-only formatter
// reformats. It is defined here (rather than in production code)
// because the helper exists solely to feed the whitespace
// stability tests in this file; production callers build the
// signature inline via the `repoindexer/signatures` package.
func methodSignature(repoURL, relPath, qual, params string) string {
	return repoURL + "::method::" + relPath + "#" + qual + "(" + NormalizeSignature(params) + ")"
}
