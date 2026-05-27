package fingerprint

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

// TestNodeFingerprint_determinism is the Stage 2.1 acceptance
// scenario "fingerprint determinism": two calls to
// NodeFingerprint with identical inputs produce byte-identical
// 32-byte outputs.
func TestNodeFingerprint_determinism(t *testing.T) {
	t.Parallel()
	repoID := MustParseRepoID("11111111-2222-3333-4444-555555555555")
	const (
		kind = "method"
		sig  = "pkg.Foo#bar(int)"
		sha  = "deadbeef"
	)
	a, err := NodeFingerprint(repoID, kind, sig, sha)
	if err != nil {
		t.Fatalf("NodeFingerprint(a): %v", err)
	}
	b, err := NodeFingerprint(repoID, kind, sig, sha)
	if err != nil {
		t.Fatalf("NodeFingerprint(b): %v", err)
	}
	if a != b {
		t.Errorf("NodeFingerprint not deterministic:\n a=%s\n b=%s", a, b)
	}
	if len(a.Bytes()) != Length {
		t.Errorf("NodeFingerprint length = %d, want %d", len(a.Bytes()), Length)
	}
	if a.IsZero() {
		t.Error("NodeFingerprint produced zero sum")
	}
}

// TestEdgeFingerprint_determinism mirrors the Node scenario for
// edges. Both endpoint fingerprints are non-zero literals.
func TestEdgeFingerprint_determinism(t *testing.T) {
	t.Parallel()
	repoID := RepoID{}
	src := must(SumFromHex(strings.Repeat("0a", Length)))
	dst := must(SumFromHex(strings.Repeat("0b", Length)))
	const (
		kind = "observed_calls"
		sha  = "deadbeef"
	)
	a, err := EdgeFingerprint(repoID, kind, src, dst, sha)
	if err != nil {
		t.Fatalf("EdgeFingerprint(a): %v", err)
	}
	b, err := EdgeFingerprint(repoID, kind, src, dst, sha)
	if err != nil {
		t.Fatalf("EdgeFingerprint(b): %v", err)
	}
	if a != b {
		t.Errorf("EdgeFingerprint not deterministic:\n a=%s\n b=%s", a, b)
	}
}

// TestNodeFingerprint_goldenVector pins the exact byte encoding
// of the hash pre-image. If a future refactor changes the
// framing scheme, this test fails loudly so the cross-process
// determinism guarantee is not broken silently.
func TestNodeFingerprint_goldenVector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		repoIDStr string
		kind      string
		sig       string
		sha       string
		wantHex   string
	}{
		{
			// Pre-image (NUL-framed concatenation per fingerprint.go):
			//   16 zero bytes
			// ‖ "method"
			// ‖ 0x00
			// ‖ "pkg.Foo#bar(int)"
			// ‖ 0x00
			// ‖ "deadbeef"
			// SHA-256 of the above = wantHex.
			name:      "zero repo / method / pkg.Foo#bar(int) / deadbeef",
			repoIDStr: "00000000-0000-0000-0000-000000000000",
			kind:      "method",
			sig:       "pkg.Foo#bar(int)",
			sha:       "deadbeef",
			wantHex:   "1aec00b695f9d36fe8ac05364e6f5fa1ccfd9dc24c56e1823a7e36a2c1b74efe",
		},
		{
			name:      "real uuid / class / pkg.Foo / aa00aa00",
			repoIDStr: "11111111-2222-3333-4444-555555555555",
			kind:      "class",
			sig:       "pkg.Foo",
			sha:       "aa00aa00",
			wantHex:   "7775f09d34da9c3905daedb1d7e93b2eb3b7be40fac54e76137dc10fc8446cc7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoID := MustParseRepoID(tc.repoIDStr)
			got, err := NodeFingerprint(repoID, tc.kind, tc.sig, tc.sha)
			if err != nil {
				t.Fatalf("NodeFingerprint: %v", err)
			}
			if got.Hex() != tc.wantHex {
				t.Errorf("NodeFingerprint(%q) = %s, want %s",
					tc.name, got.Hex(), tc.wantHex)
			}
		})
	}
}

// TestEdgeFingerprint_goldenVector pins the edge pre-image
// encoding, mirroring the node golden test. The src/dst
// fingerprints are 32 bytes of 0x0a / 0x0b respectively.
func TestEdgeFingerprint_goldenVector(t *testing.T) {
	t.Parallel()
	repoID := MustParseRepoID("00000000-0000-0000-0000-000000000000")
	src := must(SumFromHex(strings.Repeat("0a", Length)))
	dst := must(SumFromHex(strings.Repeat("0b", Length)))
	got, err := EdgeFingerprint(repoID, "observed_calls", src, dst, "deadbeef")
	if err != nil {
		t.Fatalf("EdgeFingerprint: %v", err)
	}
	const want = "2afb4060b6be9d0f1df291e6b321ee19d944126ba5f52a2998ac6d2923e606ed"
	if got.Hex() != want {
		t.Errorf("EdgeFingerprint = %s, want %s", got.Hex(), want)
	}
}

// TestNodeFingerprint_distinctInputs_produceDistinctOutputs is
// the cheapest collision-resistance smoke test: changing any
// single field of the pre-image changes the output. This is not
// a substitute for SHA-256's collision resistance proof; it
// guards against an accidental refactor that drops a field from
// the pre-image entirely.
func TestNodeFingerprint_distinctInputs_produceDistinctOutputs(t *testing.T) {
	t.Parallel()
	base := must(NodeFingerprint(
		MustParseRepoID("11111111-1111-1111-1111-111111111111"),
		"method", "pkg.Foo#bar()", "abc123",
	))
	mods := []struct {
		name string
		fn   func() (Sum, error)
	}{
		{"diff repo_id", func() (Sum, error) {
			return NodeFingerprint(
				MustParseRepoID("22222222-2222-2222-2222-222222222222"),
				"method", "pkg.Foo#bar()", "abc123",
			)
		}},
		{"diff kind", func() (Sum, error) {
			return NodeFingerprint(
				MustParseRepoID("11111111-1111-1111-1111-111111111111"),
				"class", "pkg.Foo#bar()", "abc123",
			)
		}},
		{"diff signature", func() (Sum, error) {
			return NodeFingerprint(
				MustParseRepoID("11111111-1111-1111-1111-111111111111"),
				"method", "pkg.Foo#baz()", "abc123",
			)
		}},
		{"diff from_sha", func() (Sum, error) {
			return NodeFingerprint(
				MustParseRepoID("11111111-1111-1111-1111-111111111111"),
				"method", "pkg.Foo#bar()", "def456",
			)
		}},
	}
	for _, m := range mods {
		t.Run(m.name, func(t *testing.T) {
			got, err := m.fn()
			if err != nil {
				t.Fatalf("variant %q: %v", m.name, err)
			}
			if got == base {
				t.Errorf("variant %q produced identical fingerprint to base", m.name)
			}
		})
	}
}

// TestNodeFingerprint_orderedFieldsMatter is a smoke test that
// swapping field order changes the output. Under the plain `‖`
// concatenation contract the ordering of (kind, signature,
// from_sha) is what disambiguates inputs; this test guards
// against an accidental refactor that reorders them.
func TestNodeFingerprint_orderedFieldsMatter(t *testing.T) {
	t.Parallel()
	repoID := RepoID{}
	a := must(NodeFingerprint(repoID, "method", "pkg.Foo#bar()", "abc"))
	b := must(NodeFingerprint(repoID, "pkg.Foo#bar()", "method", "abc"))
	if a == b {
		t.Errorf("swapping kind/signature did not change fingerprint: %s == %s",
			a.Hex(), b.Hex())
	}
}

// TestNodeFingerprint_emptyFieldsRejected enforces the contract
// that the closed-set discriminator and the SHA pin are required.
func TestNodeFingerprint_emptyFieldsRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		kind, sig, sha string
		wantErr        error
	}{
		{"empty kind", "", "pkg.Foo", "abc", ErrEmptyKind},
		{"empty signature", "method", "", "abc", ErrEmptySignature},
		{"empty sha", "method", "pkg.Foo", "", ErrEmptySHA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NodeFingerprint(RepoID{}, tc.kind, tc.sig, tc.sha)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestEdgeFingerprint_emptyFieldsRejected enforces the contract
// for edges. Endpoint fingerprints must be non-zero — the all-zero
// sum is used as the "uninitialised" sentinel by NodeFingerprint
// failures and must not be silently propagated.
func TestEdgeFingerprint_emptyFieldsRejected(t *testing.T) {
	t.Parallel()
	nz := must(SumFromHex(strings.Repeat("0a", Length)))
	cases := []struct {
		name          string
		kind          string
		src, dst      Sum
		sha           string
		wantSubstring string
	}{
		{"empty kind", "", nz, nz, "abc", "kind"},
		{"empty sha", "observed_calls", nz, nz, "", "from_sha"},
		{"zero src", "observed_calls", Sum{}, nz, "abc", "src"},
		{"zero dst", "observed_calls", nz, Sum{}, "abc", "dst"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EdgeFingerprint(RepoID{}, tc.kind, tc.src, tc.dst, tc.sha)
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", tc.wantSubstring)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSubstring)
			}
		})
	}
}

// TestNodeFingerprint_matchesHandRolledConcatenation is a
// defence-in-depth equivalence test: it computes the hash a
// SECOND way (assembling the concatenated pre-image bytes by
// hand and feeding them to sha256.Sum256) and compares to
// NodeFingerprint. If the implementation drifts from the
// documented NUL-framed concatenation scheme this test fails loudly
// without anyone needing to recompute the golden hex.
func TestNodeFingerprint_matchesHandRolledConcatenation(t *testing.T) {
	t.Parallel()
	repoID := MustParseRepoID("c0ffee00-c0ff-ee00-c0ff-ee00c0ffee00")
	const (
		kind = "method"
		sig  = "pkg.Bar#qux(int,int)"
		sha  = "0123456789abcdef"
	)

	var buf bytes.Buffer
	buf.Write(repoID[:])
	buf.WriteString(kind)
	buf.WriteByte(0)
	buf.WriteString(sig)
	buf.WriteByte(0)
	buf.WriteString(sha)
	want := sha256.Sum256(buf.Bytes())

	got, err := NodeFingerprint(repoID, kind, sig, sha)
	if err != nil {
		t.Fatalf("NodeFingerprint: %v", err)
	}
	if got != Sum(want) {
		t.Errorf("NodeFingerprint = %x, hand-rolled = %x", got, want)
	}
}

// TestEdgeFingerprint_matchesHandRolledConcatenation is the
// edge-side equivalence test: it asserts EdgeFingerprint is the
// SHA-256 of the literal byte concatenation
// (repo_id ‖ kind ‖ 0x00 ‖ src ‖ dst ‖ from_sha).
func TestEdgeFingerprint_matchesHandRolledConcatenation(t *testing.T) {
	t.Parallel()
	repoID := MustParseRepoID("c0ffee00-c0ff-ee00-c0ff-ee00c0ffee00")
	src := must(SumFromHex(strings.Repeat("0a", Length)))
	dst := must(SumFromHex(strings.Repeat("0b", Length)))
	const (
		kind = "observed_calls"
		sha  = "0123456789abcdef"
	)

	var buf bytes.Buffer
	buf.Write(repoID[:])
	buf.WriteString(kind)
	buf.WriteByte(0)
	buf.Write(src[:])
	buf.Write(dst[:])
	buf.WriteString(sha)
	want := sha256.Sum256(buf.Bytes())

	got, err := EdgeFingerprint(repoID, kind, src, dst, sha)
	if err != nil {
		t.Fatalf("EdgeFingerprint: %v", err)
	}
	if got != Sum(want) {
		t.Errorf("EdgeFingerprint = %x, hand-rolled = %x", got, want)
	}
}

// TestRepoID_roundTrip asserts ParseRepoID(r.String()) == r for
// a representative sample of UUIDs (zeros, all-Fs, mixed bytes,
// canonical UUID v4). Round-trip stability is required because
// the structured-logging middleware emits the textual form for
// audit and downstream queries parse it back.
func TestRepoID_roundTrip(t *testing.T) {
	t.Parallel()
	cases := []string{
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		"11111111-2222-3333-4444-555555555555",
		"01234567-89ab-cdef-0123-456789abcdef",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			r, err := ParseRepoID(s)
			if err != nil {
				t.Fatalf("ParseRepoID(%q): %v", s, err)
			}
			if got := r.String(); got != s {
				t.Errorf("round-trip: ParseRepoID(%q).String() = %q", s, got)
			}
		})
	}
}

// TestRepoID_parseRejectsMalformed pins the negative space:
// wrong length, missing hyphens, non-hex characters, mixed-case
// hex (we accept lowercase per the canonical contract — uppercase
// is normalized via hex.Decode but the round-trip isn't preserved
// so we let it through; the test asserts behaviour, not policy).
func TestRepoID_parseRejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"too short", "1111-2222-3333-4444-5555"},
		{"missing hyphens", "111111112222333344445555555555555555"},
		{"non-hex", "zzzzzzzz-2222-3333-4444-555555555555"},
		{"empty", ""},
		// Hyphens at the wrong positions still trip the strict
		// hyphen check even though the overall length is 36.
		{"misplaced hyphens", "11111111223333-3-4444-5555555555555555"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRepoID(tc.in); err == nil {
				t.Errorf("ParseRepoID(%q) accepted malformed input", tc.in)
			}
		})
	}
}

// TestSumFromHex_roundTrip exercises the Sum hex parser/encoder.
func TestSumFromHex_roundTrip(t *testing.T) {
	t.Parallel()
	hexes := []string{
		strings.Repeat("0", 64),
		strings.Repeat("f", 64),
		strings.Repeat("ab", 32),
		"6942b4e8a12a08727174ed5fcf0eac1a535faf2213b00f05b9b6d5babd32719d",
	}
	for _, h := range hexes {
		t.Run(h[:8]+"...", func(t *testing.T) {
			s, err := SumFromHex(h)
			if err != nil {
				t.Fatalf("SumFromHex(%q): %v", h, err)
			}
			if s.Hex() != h {
				t.Errorf("round-trip: SumFromHex(%q).Hex() = %q", h, s.Hex())
			}
		})
	}
}

// TestSumFromBytes_lengthCheck guards the schema-level invariant
// in Go: a Sum can only be constructed from exactly 32 bytes.
func TestSumFromBytes_lengthCheck(t *testing.T) {
	t.Parallel()
	if _, err := SumFromBytes(make([]byte, Length-1)); err == nil {
		t.Error("SumFromBytes accepted a too-short slice")
	}
	if _, err := SumFromBytes(make([]byte, Length+1)); err == nil {
		t.Error("SumFromBytes accepted a too-long slice")
	}
	if _, err := SumFromBytes(make([]byte, Length)); err != nil {
		t.Errorf("SumFromBytes rejected exactly-32-byte slice: %v", err)
	}
}

// must is a tiny test-only helper that fails the test on any
// non-nil error from a (T, error) returner. Hand-rolled to avoid
// pulling in a "testify" style dep in this package.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
