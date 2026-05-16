package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Length is the byte length of a fingerprint. It is the SHA-256
// digest size and matches the `octet_length(fingerprint) = 32`
// CHECK constraint in migration 0003 (tech-spec §8.7.1).
const Length = 32

// Sum is a deterministic G2 fingerprint as produced by
// NodeFingerprint or EdgeFingerprint.
type Sum [Length]byte

// Bytes returns a copy of the underlying byte slice. Callers must
// treat the returned slice as read-only; mutating it does not
// affect the receiver but is wasteful.
func (s Sum) Bytes() []byte {
	out := make([]byte, Length)
	copy(out, s[:])
	return out
}

// Hex returns the lowercase hexadecimal encoding of the
// fingerprint (64 characters). The hex form is what the
// structured-logging middleware emits for audit and what the
// `bytea` `decode(..., 'hex')` form in test fixtures expects.
func (s Sum) Hex() string { return hex.EncodeToString(s[:]) }

// String makes Sum implement fmt.Stringer; identical to Hex.
func (s Sum) String() string { return s.Hex() }

// Equal reports whether two fingerprints are byte-identical.
// Provided as a method for readability at call sites; the
// language-level == operator is equivalent.
func (s Sum) Equal(other Sum) bool { return s == other }

// IsZero reports whether the fingerprint is the all-zeros
// sentinel. A real SHA-256 digest is overwhelmingly unlikely to
// hit the zero pattern by chance, so the test exists mainly to
// catch uninitialised values.
func (s Sum) IsZero() bool { return s == Sum{} }

// SumFromBytes constructs a Sum from a 32-byte slice. Returns an
// error if the slice is not exactly 32 bytes long, matching the
// schema-level CHECK in migration 0003.
func SumFromBytes(b []byte) (Sum, error) {
	var s Sum
	if len(b) != Length {
		return s, fmt.Errorf(
			"fingerprint: invalid sum length %d (want %d)",
			len(b), Length,
		)
	}
	copy(s[:], b)
	return s, nil
}

// SumFromHex parses the lowercase hexadecimal form of a
// fingerprint. The case is not normalized — callers wanting to
// accept mixed-case input must lowercase first; this matches the
// "callers pass canonical input" contract the package commits to.
func SumFromHex(h string) (Sum, error) {
	var s Sum
	if len(h) != 2*Length {
		return s, fmt.Errorf(
			"fingerprint: invalid hex length %d (want %d)",
			len(h), 2*Length,
		)
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return s, fmt.Errorf("fingerprint: hex decode: %w", err)
	}
	copy(s[:], b)
	return s, nil
}

// RepoID is the canonical 16-byte form of a Repo row's UUID
// primary key. The hash domain for NodeFingerprint /
// EdgeFingerprint is keyed on the raw bytes (RFC 4122
// network-byte-order layout) so the fingerprint is independent of
// the textual UUID format the surrounding code happens to use.
type RepoID [16]byte

// String returns the canonical 36-character UUID representation
// (`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`). Bytes are emitted in
// the standard 8-4-4-4-12 grouping.
func (r RepoID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], r[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], r[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], r[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], r[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], r[10:16])
	return string(buf[:])
}

// IsZero reports whether the RepoID is the all-zeros sentinel.
func (r RepoID) IsZero() bool { return r == RepoID{} }

// ParseRepoID parses the canonical 36-character UUID form into
// the raw 16-byte representation. Hyphens are required at the
// 8/13/18/23 positions per RFC 4122; the variant and version
// nibbles are NOT validated because PostgreSQL's `gen_random_uuid`
// emits v4 UUIDs and any tighter check would reject legitimate
// future variants.
func ParseRepoID(s string) (RepoID, error) {
	var r RepoID
	if len(s) != 36 {
		return r, fmt.Errorf(
			"fingerprint: uuid length %d (want 36) for %q",
			len(s), s,
		)
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return r, fmt.Errorf(
			"fingerprint: uuid hyphens malformed in %q "+
				"(expect 8-4-4-4-12 grouping)",
			s,
		)
	}
	hexBuf := make([]byte, 32)
	copy(hexBuf[0:8], s[0:8])
	copy(hexBuf[8:12], s[9:13])
	copy(hexBuf[12:16], s[14:18])
	copy(hexBuf[16:20], s[19:23])
	copy(hexBuf[20:32], s[24:36])
	if _, err := hex.Decode(r[:], hexBuf); err != nil {
		return RepoID{}, fmt.Errorf("fingerprint: uuid hex decode: %w", err)
	}
	return r, nil
}

// MustParseRepoID is the panic-on-error variant of ParseRepoID.
// Use only for known-good literals (test fixtures, hard-coded
// constants); never on caller-controlled strings.
func MustParseRepoID(s string) RepoID {
	r, err := ParseRepoID(s)
	if err != nil {
		panic(err)
	}
	return r
}

// ErrEmptyKind is returned when NodeFingerprint or EdgeFingerprint
// is called with an empty `kind` string. The `kind` field is the
// closed-set discriminator that prevents a Method node and a Class
// node sharing the same canonical_signature from colliding on the
// hash pre-image; allowing the empty string would silently degrade
// G2.
var ErrEmptyKind = errors.New("fingerprint: kind must be non-empty")

// ErrEmptySignature is returned when NodeFingerprint is called
// with an empty canonical_signature. The signature is the
// language-stable identifier (e.g. `pkg.Foo#bar(int)`); an empty
// value is almost always a bug at the dispatcher layer.
var ErrEmptySignature = errors.New(
	"fingerprint: canonical_signature must be non-empty",
)

// ErrEmptySHA is returned when NodeFingerprint or EdgeFingerprint
// is called with an empty `from_sha` (the first SHA at which the
// entity appeared). The architecture pins from_sha into the hash
// pre-image precisely so a renamed-or-moved member produces a NEW
// fingerprint linked to the old by a `renamed_to` Edge.
var ErrEmptySHA = errors.New("fingerprint: from_sha must be non-empty")

// ErrEmbeddedNUL is returned when NodeFingerprint or
// EdgeFingerprint is called with a string field that contains a
// NUL (`\x00`) byte. NUL is reserved as the framing delimiter
// between variable-length string fields in the hash pre-image
// (see NodeFingerprint / EdgeFingerprint doc comments); allowing
// NUL inside a field would re-introduce the ambiguous-pre-image
// failure mode and silently degrade G2.
//
// In practice no valid `kind`, `canonical_signature`, or
// `from_sha` contains NUL — they are human-readable identifiers
// and lowercase hex SHAs — so the check is defence-in-depth
// against a bug at the dispatcher layer feeding raw bytes into
// these helpers.
var ErrEmbeddedNUL = errors.New(
	"fingerprint: string field contains reserved NUL byte (\\x00)",
)

// NodeFingerprint computes the 32-byte G2 fingerprint of a Node
// per architecture.md §1.3.
//
// The hash pre-image is:
//
//	sha256( repo_id ‖ kind ‖ 0x00 ‖ canonical_signature ‖ 0x00 ‖ from_sha )
//
// where `‖` is byte-string concatenation. `repo_id` is the raw
// 16-byte UUID and needs no separator because its length is
// fixed. A single NUL byte (`0x00`) terminates each variable-
// length string field so the (kind, canonical_signature, from_sha)
// boundaries are unambiguous regardless of the individual field
// lengths.
//
// Without these separators, distinct logical tuples could share a
// byte-identical pre-image and therefore an identical fingerprint
// — e.g. (kind="method", sig="pkg.Foo()a", sha="bc") and
// (kind="method", sig="pkg.Foo()", sha="abc") would both produce
// the byte string "methodpkg.Foo()abc" and collide on the hash.
// That class of collision would silently violate G2's
// "fingerprint uniquely identifies a logical entity" invariant.
//
// The function is deterministic: identical inputs always produce
// byte-identical output. Validation rejects empty `kind`,
// `canonical_signature`, and `from_sha` (each is a G2 invariant
// the schema assumes is non-empty) and rejects any string field
// that contains a NUL byte (NUL is reserved as the framing
// delimiter; see ErrEmbeddedNUL).
func NodeFingerprint(
	repoID RepoID,
	kind string,
	canonicalSignature string,
	fromSHA string,
) (Sum, error) {
	if kind == "" {
		return Sum{}, ErrEmptyKind
	}
	if canonicalSignature == "" {
		return Sum{}, ErrEmptySignature
	}
	if fromSHA == "" {
		return Sum{}, ErrEmptySHA
	}
	if strings.IndexByte(kind, 0) >= 0 {
		return Sum{}, fmt.Errorf("%w (field: kind)", ErrEmbeddedNUL)
	}
	if strings.IndexByte(canonicalSignature, 0) >= 0 {
		return Sum{}, fmt.Errorf(
			"%w (field: canonical_signature)", ErrEmbeddedNUL,
		)
	}
	if strings.IndexByte(fromSHA, 0) >= 0 {
		return Sum{}, fmt.Errorf("%w (field: from_sha)", ErrEmbeddedNUL)
	}
	h := sha256.New()
	_, _ = h.Write(repoID[:])
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(canonicalSignature))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fromSHA))
	var out Sum
	copy(out[:], h.Sum(nil))
	return out, nil
}

// EdgeFingerprint computes the 32-byte G2 fingerprint of an Edge
// per architecture.md §1.3.
//
// The hash pre-image is:
//
//	sha256( repo_id ‖ kind ‖ 0x00 ‖ src_fingerprint ‖ dst_fingerprint ‖ from_sha )
//
// where `‖` is byte-string concatenation. `repo_id` (16 bytes)
// and `src_fingerprint` / `dst_fingerprint` (32 bytes each) are
// fixed-length and need no separator; their boundaries are fixed
// by their known lengths. The single NUL byte after `kind`
// disambiguates the kind→src boundary (kind is variable-length,
// so without the separator the same byte string could parse as
// either a longer kind with shorter following fields or vice
// versa). `from_sha` appears last so the dst→from_sha boundary is
// fixed by dst's known length and no trailing separator is
// required.
//
// `src` and `dst` MUST be the fingerprints of the Node rows the
// edge connects (NOT the node UUIDs) — keying the edge identity
// by endpoint fingerprints is what makes the edge fingerprint
// stable across re-ingests of the same commit even when the
// surrogate node UUIDs change.
func EdgeFingerprint(
	repoID RepoID,
	kind string,
	src Sum,
	dst Sum,
	fromSHA string,
) (Sum, error) {
	if kind == "" {
		return Sum{}, ErrEmptyKind
	}
	if fromSHA == "" {
		return Sum{}, ErrEmptySHA
	}
	if src.IsZero() {
		return Sum{}, errors.New("fingerprint: src fingerprint must be non-zero")
	}
	if dst.IsZero() {
		return Sum{}, errors.New("fingerprint: dst fingerprint must be non-zero")
	}
	if strings.IndexByte(kind, 0) >= 0 {
		return Sum{}, fmt.Errorf("%w (field: kind)", ErrEmbeddedNUL)
	}
	if strings.IndexByte(fromSHA, 0) >= 0 {
		return Sum{}, fmt.Errorf("%w (field: from_sha)", ErrEmbeddedNUL)
	}
	h := sha256.New()
	_, _ = h.Write(repoID[:])
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(src[:])
	_, _ = h.Write(dst[:])
	_, _ = h.Write([]byte(fromSHA))
	var out Sum
	copy(out[:], h.Sum(nil))
	return out, nil
}
