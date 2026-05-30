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

// Bytes returns a copy of the underlying byte slice.
func (s Sum) Bytes() []byte {
	out := make([]byte, Length)
	copy(out, s[:])
	return out
}

// Hex returns the lowercase hexadecimal encoding of the
// fingerprint (64 characters).
func (s Sum) Hex() string { return hex.EncodeToString(s[:]) }

// String makes Sum implement fmt.Stringer; identical to Hex.
func (s Sum) String() string { return s.Hex() }

// Equal reports whether two fingerprints are byte-identical.
func (s Sum) Equal(other Sum) bool { return s == other }

// IsZero reports whether the fingerprint is the all-zeros sentinel.
func (s Sum) IsZero() bool { return s == Sum{} }

// SumFromBytes constructs a Sum from a 32-byte slice.
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

// SumFromHex parses the lowercase hexadecimal form of a fingerprint.
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

// RepoID is the canonical 16-byte form of a Repo row's UUID primary key.
type RepoID [16]byte

// String returns the canonical 36-character UUID representation.
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

// ParseRepoID parses the canonical 36-character UUID form.
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
			"fingerprint: uuid hyphens malformed in %q",
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
func MustParseRepoID(s string) RepoID {
	r, err := ParseRepoID(s)
	if err != nil {
		panic(err)
	}
	return r
}

// ErrEmptyKind is returned when kind is empty.
var ErrEmptyKind = errors.New("fingerprint: kind must be non-empty")

// ErrEmptySignature is returned when canonical_signature is empty.
var ErrEmptySignature = errors.New(
	"fingerprint: canonical_signature must be non-empty",
)

// ErrEmptySHA is returned when from_sha is empty.
var ErrEmptySHA = errors.New("fingerprint: from_sha must be non-empty")

// ErrEmbeddedNUL is returned when a string field contains NUL.
var ErrEmbeddedNUL = errors.New(
	"fingerprint: string field contains reserved NUL byte (\\x00)",
)

// NodeFingerprint computes the 32-byte G2 fingerprint of a Node.
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

// EdgeFingerprint computes the 32-byte G2 fingerprint of an Edge.
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